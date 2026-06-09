package telegram

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/go-telegram/bot/models"
)

// starCallbackPrefix is the inline-button callback_data prefix for the star
// nudge's confirm button. It mirrors stopCallbackPrefix: a press routes to the
// Service via the bot's callback handler, which calls StarPress.
const starCallbackPrefix = "star:"

// starCallbackData is the full callback_data carried by the star button. The
// nudge is a single global account action (no per-run id), so a fixed token is
// enough — unlike the Stop button which embeds a run id.
const starCallbackData = starCallbackPrefix + "confirm"

// nudgeText is the post-task invitation to star the project. It is short and
// lightly Duck-flavoured; the duck touch is optional seasoning, not required.
const nudgeText = "Если flock пригодился — можно поддержать проект звездой на GitHub 🦆"

// starButtonText labels the confirm button. Pressing it triggers the server-side
// star (a callback button, not a URL button).
const starButtonText = "⭐ Поставить звезду"

// starDoneText replaces the nudge message after a successful star.
const starDoneText = "⭐ Спасибо! Звезда поставлена"

// starFailToast is the soft callback answer shown when starring fails (e.g. the
// token lacks scope); it never crashes the run.
const starFailToast = "Не удалось поставить звезду"

// starOpTimeout bounds a single GitHub stars API call made off the hot path.
const starOpTimeout = 20 * time.Second

// starrer checks and sets the deployment account's star on a repo. *ghstar.Client
// satisfies it; tests use a fake. Methods take owner/repo so the same client can
// serve any target.
type starrer interface {
	IsStarred(ctx context.Context, owner, repo string) (bool, error)
	Star(ctx context.Context, owner, repo string) error
}

// nudgeStore persists the global "starred resolved" flag so the nudge stops once
// the repo is known starred. *nudge.Store satisfies it; tests use a fake. A nil
// store is a safe no-op (treated as not-yet-starred).
type nudgeStore interface {
	Starred() bool
	MarkStarred() error
}

// StarNudgeConfig configures the post-task GitHub star nudge injected into the
// Service. When Enabled is false the whole path is inert (the default for
// non-GitHub deploys); Client and Store may then be nil.
type StarNudgeConfig struct {
	// Enabled gates the entire feature. The cmd wires this from
	// config.StarNudgeEnabled(), so a non-GitHub deploy or a missing token leaves
	// it false and every nudge call is a no-op.
	Enabled bool
	// Owner and Repo identify the repository to star (from STAR_NUDGE_REPO).
	Owner string
	Repo  string
	// Client performs the GitHub stars API calls. May be nil when disabled.
	Client starrer
	// Store persists the resolved-starred flag. May be nil when disabled.
	Store nudgeStore
}

// starNudge holds the resolved runtime state for the post-task star nudge. A nil
// *starNudge is a valid no-op (all methods guard on it), so callers need not nil-
// check before invoking it.
type starNudge struct {
	enabled bool
	owner   string
	repo    string
	client  starrer
	store   nudgeStore
	chat    chat
	log     *slog.Logger
}

// newStarNudge builds a starNudge from cfg. When cfg.Enabled is false (or its
// dependencies are missing) it returns a disabled nudge whose methods no-op, so
// the Service can wire it unconditionally.
func newStarNudge(cfg StarNudgeConfig, c chat, log *slog.Logger) *starNudge {
	enabled := cfg.Enabled && cfg.Client != nil && cfg.Store != nil && cfg.Owner != "" && cfg.Repo != ""
	return &starNudge{
		enabled: enabled,
		owner:   cfg.Owner,
		repo:    cfg.Repo,
		client:  cfg.Client,
		store:   cfg.Store,
		chat:    c,
		log:     log,
	}
}

// active reports whether the nudge should do any work: it is enabled and the repo
// is not already known starred. A nil receiver is inactive.
func (n *starNudge) active() bool {
	return n != nil && n.enabled && !n.store.Starred()
}

// maybeNudge runs the post-task nudge for chatID fully off the hot path: it spawns
// a detached goroutine (so it never delays the run), recovers any panic, and
// swallows every error at debug level (so it never breaks a run). It is called
// only after a cleanly successful run. When the repo is already known starred, or
// the feature is disabled, it does nothing.
func (n *starNudge) maybeNudge(chatID int64) {
	if !n.active() {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				n.log.Debug("star nudge panic recovered", "recover", r)
			}
		}()
		n.runNudge(chatID)
	}()
}

// runNudge checks the live star state and either records it (already starred →
// stop nudging, send nothing) or sends the invitation with a confirm button. All
// failures are logged at debug and swallowed.
func (n *starNudge) runNudge(chatID int64) {
	// A fresh root context is deliberate: the nudge runs in a detached goroutine
	// AFTER the run's own ctx may have been cancelled (Stop/timeout), and must not
	// be torn down with it. It is independently bounded by starOpTimeout.
	ctx, cancel := context.WithTimeout(context.Background(), starOpTimeout)
	defer cancel()

	starred, err := n.client.IsStarred(ctx, n.owner, n.repo)
	if err != nil {
		n.log.Debug("star nudge: check failed", "error", err)
		return
	}
	if starred {
		// Already starred out-of-band: record it so we never nudge or hit the API
		// again, and send nothing.
		if mErr := n.store.MarkStarred(); mErr != nil {
			n.log.Debug("star nudge: mark starred failed", "error", mErr)
		}
		return
	}
	if _, sErr := n.chat.SendStarNudge(ctx, chatID, nudgeText); sErr != nil {
		n.log.Debug("star nudge: send failed", "error", sErr)
	}
}

// StarPress handles a press of the star confirm button: it stars the repo from
// the deployment account, and on success records the resolved flag so the nudge
// stops. It returns a short toast for the callback answer and whether the star
// succeeded (so the caller can decide whether to edit the message into the
// confirmation). It never panics and never blocks the caller beyond starOpTimeout;
// a disabled/already-starred nudge reports success without an API call so a stale
// button press is handled gracefully.
func (s *Service) StarPress() (toast string, ok bool) {
	return s.nudge.handlePress()
}

// handlePress performs the star and resolves the flag. A nil/disabled nudge, or
// an already-resolved one, reports success with the done text so a late press of
// a lingering button still confirms cleanly. A token-scope failure (or any other
// error) reports the soft toast and false.
func (n *starNudge) handlePress() (toast string, ok bool) {
	if n == nil || !n.enabled {
		return starDoneText, true
	}
	if n.store.Starred() {
		return starDoneText, true
	}

	// A fresh root context is deliberate: the star must complete even if the short
	// per-callback context is already done. It is bounded by starOpTimeout.
	ctx, cancel := context.WithTimeout(context.Background(), starOpTimeout)
	defer cancel()

	if err := n.client.Star(ctx, n.owner, n.repo); err != nil {
		// The ghstar client surfaces a 401/403 (missing star-write scope) with a
		// descriptive wrapped error, so a single debug log is enough here; we never
		// crash the run on a failed star.
		n.log.Debug("star nudge: star failed", "error", err)
		return starFailToast, false
	}
	if mErr := n.store.MarkStarred(); mErr != nil {
		n.log.Debug("star nudge: mark starred failed", "error", mErr)
	}
	return starDoneText, true
}

// starMarkup returns the inline keyboard with the single confirm button used on a
// nudge message.
func starMarkup() models.ReplyMarkup {
	return &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{{
			{Text: starButtonText, CallbackData: starCallbackData},
		}},
	}
}

// IsStarCallback reports whether callback data is a star-confirm callback this
// Service should handle. Used as the prefix for the bot's callback handler, the
// same way CallbackMatch drives the Stop handler.
func IsStarCallback(data string) bool {
	return strings.HasPrefix(data, starCallbackPrefix)
}

// StarCallbackPrefix is the callback_data prefix the cmd registers the star
// handler against (bot.MatchTypePrefix), mirroring CallbackMatch for Stop.
func StarCallbackPrefix() string { return starCallbackPrefix }

// StarDoneText is the confirmation text the callback handler edits the nudge
// message into after a successful star (the button is removed at the same time).
func StarDoneText() string { return starDoneText }
