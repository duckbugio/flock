package vk

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/duckbugio/flock/core/agent"
	"github.com/duckbugio/flock/core/chat"
	"github.com/duckbugio/flock/core/codexauth"
	"github.com/duckbugio/flock/core/goal"
	"github.com/duckbugio/flock/core/schedule"
)

// loginNotifyTimeout bounds one background Codex-login notice delivery. The
// device flow reports minutes after the /login update's own context is gone, so
// each notice is sent on a fresh, bounded context of its own.
const loginNotifyTimeout = 30 * time.Second

// scheduleDisabledText is the reply when /schedule is used but the scheduler is
// turned off (the default). It mirrors the Telegram adapter's notice.
const scheduleDisabledText = "Scheduler is disabled. Set ENABLE_SCHEDULER=true to enable it."

// newSessionText is the static reply to /new: confirms the session was reset so
// the next message starts a brand-new conversation.
const newSessionText = "Started a fresh session. Your next message begins a new conversation."

// Service is the subset of *core/chat.Service the receiver drives. The real
// Service satisfies it; tests use a fake to assert the mapped calls.
type Service interface {
	Handle(ctx context.Context, chatID chat.ChatID, userID int64, msgID chat.MessageID, prompt string)
	HandleMedia(
		ctx context.Context, chatID chat.ChatID, userID int64, msgID chat.MessageID, prompt string, images []agent.ImageInput,
	)
	Stop(runID string) bool
	StopChat(chatID chat.ChatID) bool
	NewSession(chatID chat.ChatID) error
	StarPress() (toast string, ok bool)
	ArmGoal(chatID chat.ChatID, criterion string) (goal.Goal, bool)
	GoalStatus(chatID chat.ChatID) (goal.Goal, bool)
	DisarmGoal(chatID chat.ChatID) bool
}

// guardChecker decides whether an allowed user's accepted message may run (rate
// limit + cost cap), mirroring the Telegram adapter's CheckGuards call. It
// returns a reason to send back on denial. A nil checker means "always allow"
// (the cmd always wires one).
type guardChecker func(userID int64) (allow bool, reason string)

// transcriber transcribes a VK voice attachment from its (self-keyed) URL.
// *VoiceTranscriber satisfies it; nil disables voice.
type transcriber interface {
	Transcribe(ctx context.Context, url string) (string, error)
}

// uploader saves an inbound attachment from its URL to the per-chat uploads dir.
// *Uploader satisfies it; nil disables uploads.
type uploader interface {
	Save(ctx context.Context, chatID int64, url, fileName string) (string, error)
	MaxBytes() int64
}

// NoticeSender posts a short user-facing notice (a guard denial, a failed
// transcription, an oversize/undownloadable upload) to a VK conversation. The
// cmd wires a client-backed implementation (NewNoticeSender); tests use a fake.
type NoticeSender interface {
	Notify(ctx context.Context, peerID int64, text string)
}

// clientNotice is the production NoticeSender: it sends a plain message via the
// VK client, generating a unique random_id per notice (VK dedupes a repeated
// random_id within ~1h). Delivery failures are non-fatal and only logged at debug.
type clientNotice struct {
	api    *Client
	seed   int64
	seq    atomic.Uint64
	logger *slog.Logger
}

// NewNoticeSender builds a client-backed NoticeSender. seed seeds the per-sender
// random_id generator (the cmd passes a monotonic seed); a nil logger defaults to
// slog.Default().
func NewNoticeSender(api *Client, seed int64, logger *slog.Logger) NoticeSender {
	if logger == nil {
		logger = slog.Default()
	}
	return &clientNotice{api: api, seed: seed, logger: logger}
}

// Notify sends text to peerID as a plain message. Best-effort: a failure is logged
// at debug and swallowed (a notice must never crash the receive loop).
func (n *clientNotice) Notify(ctx context.Context, peerID int64, text string) {
	//nolint:gosec // G115: deliberate wrap into the signed random_id space; uniqueness, not range, matters.
	randomID := n.seed + int64(n.seq.Add(1))
	if _, err := n.api.MessagesSend(ctx, peerID, text, randomID, "", "", 0); err != nil {
		n.logger.Debug("vk: send notice", "peer_id", peerID, "error", err)
	}
}

// Receiver maps VK Long Poll updates onto the neutral Service. It owns the gate
// (allow-list + community mention), the guard check, and the inbound media/voice
// download seams, mirroring cmd/flock-telegram's handler shape but for VK.
type Receiver struct {
	svc            Service
	groupID        int64
	requireMention bool
	isAllowed      func(userID int64) bool
	guards         guardChecker
	voice          transcriber
	uploads        uploader
	notices        NoticeSender
	eventAck       eventAckFunc
	sched          *schedule.Manager
	auth           *codexauth.Manager
	logger         *slog.Logger
}

// ReceiverConfig bundles the Receiver's dependencies.
type ReceiverConfig struct {
	Service        Service
	GroupID        int64
	RequireMention bool
	IsAllowed      func(userID int64) bool
	Guards         func(userID int64) (allow bool, reason string)
	Voice          transcriber
	Uploads        uploader
	Notices        NoticeSender
	// EventAck acks a callback button press (messages.sendMessageEventAnswer) so VK
	// clears the button spinner. May be nil (the ack is then skipped).
	EventAck eventAckFunc
	// Scheduler serves the /schedule command. Nil when ENABLE_SCHEDULER is off, in
	// which case /schedule replies with the disabled notice.
	Scheduler *schedule.Manager
	// CodexAuth serves /login and blocks runs while a Codex subscription deploy has
	// no completed sign-in. A nil manager never blocks and reports /login as
	// unnecessary.
	CodexAuth *codexauth.Manager
	Logger    *slog.Logger
}

// NewReceiver builds a Receiver from cfg. A nil logger defaults to slog.Default();
// a nil IsAllowed denies everyone (fail-safe); a nil Guards allows every accepted
// message.
func NewReceiver(cfg ReceiverConfig) *Receiver {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	isAllowed := cfg.IsAllowed
	if isAllowed == nil {
		isAllowed = func(int64) bool { return false }
	}
	return &Receiver{
		svc:            cfg.Service,
		groupID:        cfg.GroupID,
		requireMention: cfg.RequireMention,
		isAllowed:      isAllowed,
		guards:         cfg.Guards,
		voice:          cfg.Voice,
		uploads:        cfg.Uploads,
		notices:        cfg.Notices,
		eventAck:       cfg.EventAck,
		sched:          cfg.Scheduler,
		auth:           cfg.CodexAuth,
		logger:         log,
	}
}

// Run drives the Bots Long Poll receive loop until ctx is cancelled: it fetches a
// server descriptor, polls a_check, dispatches updates, and handles the failed
// codes (1 → resume with the returned ts; 2 → re-fetch the key; 3 → re-fetch key
// + ts). Transient HTTP/transport errors back off and retry rather than crash the
// loop. It blocks until ctx is done and returns ctx.Err().
func (r *Receiver) Run(ctx context.Context, lp longPoller) error {
	server, err := r.refreshServer(ctx, lp)
	if err != nil {
		// Couldn't get the initial server; back off and let the loop retry below.
		r.logger.Warn("vk long poll: initial getLongPollServer failed", "error", err)
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if server.Server == "" || server.Key == "" {
			if !r.sleep(ctx, pollBackoff) {
				return ctx.Err()
			}
			if s, sErr := r.refreshServer(ctx, lp); sErr != nil {
				r.logger.Warn("vk long poll: getLongPollServer failed", "error", sErr)
			} else {
				server = s
			}
			continue
		}

		resp, err := lp.poll(ctx, server)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.logger.Warn("vk long poll: poll failed; backing off", "error", err)
			if !r.sleep(ctx, pollBackoff) {
				return ctx.Err()
			}
			continue
		}

		if resp.Failed != 0 {
			server = r.handleFailed(ctx, lp, server, resp)
			continue
		}

		if ts := resp.TS.String(); ts != "" {
			server.TS = ts
		}
		for i := range resp.Updates {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.dispatch(ctx, resp.Updates[i])
		}
	}
}

// handleFailed advances the server descriptor per the Long Poll failed code and
// returns the descriptor to poll next.
func (r *Receiver) handleFailed(
	ctx context.Context, lp longPoller, server longPollServer, resp longPollResponse,
) longPollServer {
	switch resp.Failed {
	case failedOutdatedTS:
		// History outdated: continue from the returned ts, reuse the key.
		if ts := resp.TS.String(); ts != "" {
			server.TS = ts
		}
		return server
	case failedKeyExpired, failedInfoLost:
		// Key expired (2) or information lost (3): re-fetch a fresh server (new key,
		// and for 3 a fresh ts too — getLongPollServer returns both).
		s, err := r.refreshServer(ctx, lp)
		if err != nil {
			r.logger.Warn("vk long poll: refresh after failed code", "failed", resp.Failed, "error", err)
			server.Server = "" // force a backoff+retry on the next iteration
			return server
		}
		if resp.Failed == failedKeyExpired {
			// Keep our current ts (only the key expired); the fresh server's ts would
			// skip queued updates.
			s.TS = server.TS
		}
		return s
	default:
		r.logger.Warn("vk long poll: unknown failed code", "failed", resp.Failed)
		server.Server = ""
		return server
	}
}

// refreshServer fetches a fresh Long Poll server descriptor.
func (r *Receiver) refreshServer(ctx context.Context, lp longPoller) (longPollServer, error) {
	return lp.getLongPollServer(ctx, r.groupID)
}

// sleep waits d or until ctx is cancelled; it reports false when cancelled.
func (r *Receiver) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// dispatch routes a single update to its handler. Unhandled types are ignored.
func (r *Receiver) dispatch(ctx context.Context, u update) {
	switch u.Type {
	case "message_new":
		var obj messageNewObject
		if err := unmarshalObject(u.Object, &obj); err != nil {
			r.logger.Debug("vk: decode message_new", "error", err)
			return
		}
		r.onMessageNew(ctx, obj.Message)
	case "message_event":
		var ev messageEventObject
		if err := unmarshalObject(u.Object, &ev); err != nil {
			r.logger.Debug("vk: decode message_event", "error", err)
			return
		}
		r.onMessageEvent(ctx, ev)
	default:
		// message_edit and other update types are intentionally not wired: the
		// Service exposes no VK-specific edit entry point that maps cleanly, so an
		// edited VK message is left unhandled (documented in the P1 report).
	}
}

// onMessageNew maps a message_new update onto the Service: allow-list, reserved
// command dispatch (the canonical core/chat set), the community mention-gate, the
// guard check, then a text or media run. A non-reserved slash command (/loop, …)
// is NOT intercepted — it falls through with its full text so Claude Code runs it.
func (r *Receiver) onMessageNew(ctx context.Context, msg messageObject) {
	// Ignore messages from communities (negative from_id) and our own echoes.
	if msg.FromID <= 0 {
		return
	}
	if !r.isAllowed(msg.FromID) {
		r.logger.Debug("vk: ignoring message from disallowed user", "user_id", msg.FromID)
		return
	}

	peerID := msg.PeerID
	isGroup := isGroupPeer(peerID)

	// Reserved commands the bot handles itself (the single source of truth in
	// core/chat). A leading /start, /help, /new, /stop or /schedule is dispatched here and
	// never reaches Claude; every OTHER slash command (e.g. /loop) is left to fall
	// through with its full text so Claude Code's own slash-command system runs it.
	if name := commandName(msg.Text); chat.IsReservedCommand(name) {
		r.dispatchReserved(ctx, name, msg)
		return
	}

	handle, cleaned := shouldHandle(isGroup, msg.Text, r.groupID, r.requireMention)
	if !handle {
		r.logger.Debug("vk: group message not addressed to bot — ignoring", "peer_id", peerID)
		return
	}

	if r.guards != nil {
		if allow, reason := r.guards(msg.FromID); !allow {
			r.logger.Debug("vk: guardrail denied message", "peer_id", peerID, "user_id", msg.FromID, "reason", reason)
			r.notify(ctx, peerID, reason)
			return
		}
	}

	// Codex with no completed sign-in cannot run anything. Say so here — after the
	// mention gate (so an unaddressed community message stays silent) and after the
	// guards (an unauthorized deploy in a busy conversation would otherwise answer
	// every single message, unthrottled), but before any paid work: the guards spend
	// nothing, while transcription and downloads do. /login is dispatched above and
	// stays reachable while this gate is closed. core/chat gates the run itself;
	// this is the early, user-facing half.
	if notice, blocked := r.auth.BlockedNotice(); blocked {
		r.logger.Debug("vk: codex unauthorized — blocking run", "peer_id", peerID)
		r.notify(ctx, peerID, notice)
		return
	}

	text := strings.TrimSpace(cleaned)

	// Voice: transcribe the first audio_message attachment (gate already passed, so
	// we only pay for transcription on an accepted message).
	if att := firstAudioMessage(msg.Attachments); att != nil && r.voice != nil && text == "" {
		r.handleVoice(ctx, msg, att)
		return
	}

	// Inbound document / photo: save to the per-chat uploads dir (outside every git
	// tree) and submit a run referencing the saved path (a photo also attaches a
	// vision content block).
	if r.uploads != nil {
		if doc := firstDoc(msg.Attachments); doc != nil {
			r.handleDocument(ctx, msg, doc, text)
			return
		}
		if ph := firstPhoto(msg.Attachments); ph != nil {
			r.handlePhoto(ctx, msg, ph, text)
			return
		}
	}

	if text == "" {
		return
	}
	// Fold any quoted/replied-to/forwarded original into the prompt so the run sees
	// the context the user is referring to, not just their new text. QuotedPrompt is
	// a strict no-op when there is no quote, so a normal message stays unchanged.
	qAuthor, qText := quotedContext(msg)
	text = chat.QuotedPrompt(qAuthor, qText, text)
	r.svc.Handle(ctx, chatIDStr(peerID), msg.FromID, msgIDStr(msg.ConversationMessageID), text)
}

// dispatchReserved acts on a reserved command (caller already confirmed the name
// is in the canonical set). /start and /help reply with the static notices; /new
// resets the chat's session and confirms; /stop cancels the in-flight run;
// /schedule manages cron jobs via the shared Manager (or replies with the disabled
// notice when the scheduler is off). None of these starts a Claude run. An unknown
// name (should not happen — the caller gates on IsReservedCommand) is a no-op so
// the set stays the single authority.
func (r *Receiver) dispatchReserved(ctx context.Context, name string, msg messageObject) {
	peerID := msg.PeerID
	switch name {
	case "start":
		r.notify(ctx, peerID, welcomeText(r.auth.Applicable()))
	case "help":
		r.notify(ctx, peerID, HelpText(r.auth.Applicable()))
	case "new":
		if err := r.svc.NewSession(chatIDStr(peerID)); err != nil {
			r.logger.Error("vk: reset session", "peer_id", peerID, "error", err)
		}
		r.notify(ctx, peerID, newSessionText)
	case "stop":
		r.svc.StopChat(chatIDStr(peerID))
	case "schedule":
		r.dispatchSchedule(ctx, msg)
	case "goal":
		r.dispatchGoal(ctx, msg)
	case "login":
		r.dispatchLogin(ctx, msg)
	}
}

// dispatchLogin serves /login: start, report, or cancel the Codex device-code
// sign-in. It stays available while the deployment is unauthorized — it is the
// only way out of that state. The sender is already allow-list gated by
// onMessageNew, which returns before any reserved command is dispatched.
//
// Whether a one-time code may be printed here is decided by codexauth from the
// Private flag, not by this adapter inspecting the arguments: /login status also
// re-shows a pending code, so an argument-based guard would let it straight past.
// A VK conversation has a peer id distinct from the sender's, which is exactly
// what makes it non-private.
//
// The sign-in outlives this call: Dispatch replies immediately and keeps working
// in the background, delivering the link, the one-time code, and the verdict to
// its subscribers. The callback builds its OWN context, because this update's is
// long gone by the time a user finishes in a browser.
func (r *Receiver) dispatchLogin(ctx context.Context, msg messageObject) {
	peerID := msg.PeerID
	sub := codexauth.Subscriber{
		ID: chatIDStr(peerID),
		// VK states the 1:1 invariant directly in the update: in a direct message the
		// peer IS the sender. Deriving it from the peer-id range instead would call
		// every non-conversation peer private, community peers included — too loose a
		// test for a flag that decides who may take over the bot's account.
		Private: peerID == msg.FromID,
		Notify: func(text string) {
			sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loginNotifyTimeout)
			defer cancel()
			r.notify(sendCtx, peerID, text)
		},
	}
	// An empty reply means Dispatch already delivered it through sub, in order.
	if reply := r.auth.Dispatch(ctx, commandArgs(msg.Text), sub); reply != "" {
		r.notify(ctx, peerID, reply)
	}
}

// dispatchGoal serves /goal: arm, show, or disarm the calling chat's goal via
// the shared Service, mirroring the Telegram adapter's goalHandler.
func (r *Receiver) dispatchGoal(ctx context.Context, msg messageObject) {
	peerID := msg.PeerID
	args := commandArgs(msg.Text)
	switch strings.ToLower(args) {
	case "":
		if g, armed := r.svc.GoalStatus(chatIDStr(peerID)); armed {
			r.notify(ctx, peerID, fmt.Sprintf(
				"Armed goal (evaluation round %d/%d):\n%s\n\n%s", g.Attempts, g.MaxAttempts, g.Criterion, goalUsageText))
			return
		}
		r.notify(ctx, peerID, "No goal armed.\n\n"+goalUsageText)
	case "off", "clear", "stop":
		if r.svc.DisarmGoal(chatIDStr(peerID)) {
			r.notify(ctx, peerID, "Goal disarmed.")
			return
		}
		r.notify(ctx, peerID, "No goal was armed.")
	default:
		g, armed := r.svc.ArmGoal(chatIDStr(peerID), args)
		if !armed {
			r.notify(ctx, peerID, "The goal evaluator is disabled on this deployment.")
			return
		}
		r.notify(ctx, peerID, fmt.Sprintf(
			"🎯 Goal armed (up to %d evaluation rounds). After every completed run an independent "+
				"evaluator will judge:\n%s", g.MaxAttempts, g.Criterion))
	}
}

// dispatchSchedule serves /schedule: when the scheduler is disabled it replies
// with the disabled notice; otherwise it strips the leading /schedule token and
// hands the remaining args, the chat id, and the sender's user id to the
// transport-neutral Manager.Dispatch, replying with its result.
func (r *Receiver) dispatchSchedule(ctx context.Context, msg messageObject) {
	if r.sched == nil {
		r.notify(ctx, msg.PeerID, scheduleDisabledText)
		return
	}
	args := commandArgs(msg.Text)
	reply := r.sched.Dispatch(chatIDStr(msg.PeerID), msg.FromID, args)
	r.notify(ctx, msg.PeerID, reply)
}

// commandName parses the reserved-command name from a VK message: it trims the
// text and, when it starts with "/", returns the first whitespace-delimited token
// with the leading "/" dropped and lower-cased; otherwise it returns "". The
// match is exact, so "/news" yields "news" (not reserved -> passes through) and
// "/loop 5m" yields "loop". VK has no @botname suffix, so none is stripped.
func commandName(text string) string {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return ""
	}
	token := text[1:]
	if i := strings.IndexFunc(token, isSpace); i >= 0 {
		token = token[:i]
	}
	return strings.ToLower(token)
}

// isSpace reports whether r is one of the ASCII whitespace runes that delimit a
// VK command token (space, tab, newline, carriage return).
func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}

// commandArgs returns the text following the leading "/command" token of a
// slash-command message, i.e. the arguments. It drops the first whitespace-
// delimited token (the command itself) and trims the surrounding whitespace, so
// "/schedule add x" yields "add x" and a bare "/schedule" yields "". VK has no
// @botname suffix to strip.
func commandArgs(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.IndexFunc(text, isSpace); i >= 0 {
		return strings.TrimSpace(text[i+1:])
	}
	return ""
}

// handleVoice downloads + transcribes a voice attachment and submits the
// transcript as a text run. An empty/failed transcript gets one notice.
func (r *Receiver) handleVoice(ctx context.Context, msg messageObject, att *audioMsgAttach) {
	url := att.LinkOGG
	if url == "" {
		url = att.LinkMP3
	}
	transcript, err := r.voice.Transcribe(ctx, url)
	if err != nil {
		r.logger.Warn("vk: voice transcription failed", "peer_id", msg.PeerID, "error", err)
		r.notify(ctx, msg.PeerID, "Sorry, I couldn't transcribe that voice message.")
		return
	}
	text := strings.TrimSpace(transcript)
	if text == "" {
		r.notify(ctx, msg.PeerID, "Sorry, I couldn't make out that voice message.")
		return
	}
	// VK voice does not flow through the text path, so fold any quoted original in
	// here too, keeping voice replies uniform with text/media replies.
	qAuthor, qText := quotedContext(msg)
	text = chat.QuotedPrompt(qAuthor, qText, text)
	r.svc.Handle(ctx, chatIDStr(msg.PeerID), msg.FromID, msgIDStr(msg.ConversationMessageID), text)
}

// handleDocument downloads + saves a document attachment and submits a run
// referencing the saved path plus the message text.
func (r *Receiver) handleDocument(ctx context.Context, msg messageObject, doc *docAttachment, caption string) {
	savedPath, err := r.uploads.Save(ctx, msg.PeerID, doc.URL, docFileName(doc))
	if err != nil {
		r.logger.Warn("vk: save document failed", "peer_id", msg.PeerID, "error", err)
		r.notify(ctx, msg.PeerID, "Sorry, I couldn't save that file.")
		return
	}
	prompt := chat.DocumentPrompt(savedPath, caption)
	qAuthor, qText := quotedContext(msg)
	prompt = chat.QuotedPrompt(qAuthor, qText, prompt)
	r.svc.HandleMedia(ctx, chatIDStr(msg.PeerID), msg.FromID, msgIDStr(msg.ConversationMessageID), prompt, nil)
}

// handlePhoto downloads + saves the largest photo size and submits a vision run
// (the saved path + an attached image content block). A vision-load failure falls
// back to a path-only run.
func (r *Receiver) handlePhoto(ctx context.Context, msg messageObject, ph *photoAttachment, caption string) {
	size := largestPhoto(ph.Sizes)
	if size == nil {
		return
	}
	savedPath, err := r.uploads.Save(ctx, msg.PeerID, size.URL, "photo.jpg")
	if err != nil {
		r.logger.Warn("vk: save photo failed", "peer_id", msg.PeerID, "error", err)
		r.notify(ctx, msg.PeerID, "Sorry, I couldn't save that image.")
		return
	}
	prompt := chat.PhotoPrompt(savedPath, caption)
	qAuthor, qText := quotedContext(msg)
	prompt = chat.QuotedPrompt(qAuthor, qText, prompt)
	images, err := chat.LoadPhotoImage(savedPath)
	if err != nil {
		r.logger.Warn("vk: load photo image failed; path-only", "peer_id", msg.PeerID, "error", err)
		images = nil
	}
	r.svc.HandleMedia(ctx, chatIDStr(msg.PeerID), msg.FromID, msgIDStr(msg.ConversationMessageID), prompt, images)
}

// onMessageEvent maps a callback button press onto the Service: a Stop payload
// cancels the run, a star payload triggers the star, and either way the event is
// acked (sendMessageEventAnswer) by the caller-provided ack func.
func (r *Receiver) onMessageEvent(ctx context.Context, ev messageEventObject) {
	if !r.isAllowed(ev.UserID) {
		r.logger.Debug("vk: ignoring callback from disallowed user", "user_id", ev.UserID)
		r.ack(ctx, ev)
		return
	}
	payload := parsePayload(ev.Payload)
	switch {
	case payload.Stop != "":
		r.svc.Stop(payload.Stop)
	case payload.Star:
		r.svc.StarPress()
	}
	r.ack(ctx, ev)
}

// ack acknowledges a message_event so VK clears the button's loading spinner.
// Best-effort: a failed ack only leaves the spinner, it never affects the run.
func (r *Receiver) ack(ctx context.Context, ev messageEventObject) {
	if r.eventAck == nil {
		return
	}
	if err := r.eventAck(ctx, ev.EventID, ev.UserID, ev.PeerID); err != nil {
		r.logger.Debug("vk: sendMessageEventAnswer", "error", err)
	}
}

// eventAckFunc acks a callback press (messages.sendMessageEventAnswer). Wired by
// the cmd to the VK client's SendMessageEventAnswer; tests inject a fake to
// assert the ack.
type eventAckFunc func(ctx context.Context, eventID string, userID, peerID int64) error

// unmarshalObject decodes a Long Poll update object into out.
func unmarshalObject(raw json.RawMessage, out any) error {
	return json.Unmarshal(raw, out)
}

// notify posts a short user-facing notice when a notice sender is configured.
func (r *Receiver) notify(ctx context.Context, peerID int64, text string) {
	if r.notices == nil {
		return
	}
	r.notices.Notify(ctx, peerID, text)
}

// isGroupPeer reports whether a VK peer id is a group/chat conversation. VK
// multi-user chats use peer ids >= 2000000000 (2_000_000_000 + chat_id); a peer
// id below that is a 1:1 user DM (never gated as a group).
func isGroupPeer(peerID int64) bool {
	return peerID >= chatPeerBase
}

// chatPeerBase is the offset VK adds to a multi-user chat id to form its peer id.
const chatPeerBase = 2_000_000_000

// chatIDStr renders a VK int64 peer id as the transport-neutral string chat id.
func chatIDStr(id int64) string { return strconv.FormatInt(id, 10) }

// msgIDStr renders a VK int64 conversation_message_id as the neutral string id.
func msgIDStr(id int64) string { return strconv.FormatInt(id, 10) }

// docFileName derives a filename for a document attachment: its title, or a
// synthesized name from the extension.
func docFileName(doc *docAttachment) string {
	if strings.TrimSpace(doc.Title) != "" {
		return doc.Title
	}
	if doc.Ext != "" {
		return "document." + doc.Ext
	}
	return "document"
}

// firstDoc returns the first doc attachment, or nil.
func firstDoc(atts []attachment) *docAttachment {
	for i := range atts {
		if atts[i].Type == "doc" && atts[i].Doc != nil {
			return atts[i].Doc
		}
	}
	return nil
}

// firstPhoto returns the first photo attachment, or nil.
func firstPhoto(atts []attachment) *photoAttachment {
	for i := range atts {
		if atts[i].Type == "photo" && atts[i].Photo != nil {
			return atts[i].Photo
		}
	}
	return nil
}

// firstAudioMessage returns the first audio_message (voice) attachment, or nil.
func firstAudioMessage(atts []attachment) *audioMsgAttach {
	for i := range atts {
		if atts[i].Type == "audio_message" && atts[i].AudioMessage != nil {
			return atts[i].AudioMessage
		}
	}
	return nil
}

// largestPhoto returns the highest-resolution size from a VK photo set
// (Width*Height), or nil for an empty set.
func largestPhoto(sizes []photoSize) *photoSize {
	var best *photoSize
	var bestScore int
	for i := range sizes {
		score := sizes[i].Width * sizes[i].Height
		if best == nil || score > bestScore {
			best = &sizes[i]
			bestScore = score
		}
	}
	return best
}

// quotedAssistantLabel names a community/bot's own quoted message; quotedUserLabel
// is the coarse label for a human participant. Both keep the folded context author
// coarse — VK gives us only a from_id, and we deliberately avoid an extra users.get
// call to resolve a name.
const (
	quotedAssistantLabel = "the assistant"
	quotedUserLabel      = "a participant"
)

// quotedContext extracts a quoted original from a VK message so it can be folded
// into the prompt (via chat.QuotedPrompt). It prefers the replied-to message
// (reply_message), else the first forwarded message (fwd_messages), reading ONE
// level only — nested reply/forward chains are not traversed. The quoted text is the
// source message's text, or a generic "[media]" placeholder when that message
// carries only attachments, so the reference is never silently dropped. An empty
// text means the message is neither a reply nor a forward, which keeps a normal
// message byte-for-byte unchanged. The author is coarse (no users.get lookup): a
// community/bot source (from_id < 0) is the assistant, a human (from_id > 0) is a
// participant, and an unknown source yields an empty label.
func quotedContext(msg messageObject) (author, text string) {
	quoted, ok := firstQuoted(msg)
	if !ok {
		return "", ""
	}
	text = strings.TrimSpace(quoted.Text)
	if text == "" && len(quoted.Attachments) > 0 {
		text = "[media]"
	}
	return quotedAuthorLabel(quoted.FromID), text
}

// firstQuoted returns the single quoted source of a message — the replied-to
// message if present, else the first forwarded message — and whether one exists.
// It intentionally does not recurse into the quoted message's own reply/forwards.
func firstQuoted(msg messageObject) (messageObject, bool) {
	if msg.ReplyMessage != nil {
		return *msg.ReplyMessage, true
	}
	if len(msg.FwdMessages) > 0 {
		return msg.FwdMessages[0], true
	}
	return messageObject{}, false
}

// quotedAuthorLabel maps a quoted message's from_id to a coarse author label with
// no users.get lookup: a negative id (a community / the bot) is the assistant, a
// positive id is a human participant, and zero is unknown (empty).
func quotedAuthorLabel(fromID int64) string {
	switch {
	case fromID < 0:
		return quotedAssistantLabel
	case fromID > 0:
		return quotedUserLabel
	default:
		return ""
	}
}
