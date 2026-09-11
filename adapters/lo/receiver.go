package lo

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/duckbugio/flock/core/agent"
	"github.com/duckbugio/flock/core/chat"
	"github.com/duckbugio/flock/core/goal"
	"github.com/duckbugio/flock/core/schedule"
	"github.com/duckbugio/flock/internal/fsutil"
)

const privateChatType = "private"

// Service is the transport-neutral inbound seam; no Telegram handlers are reused.
type Service interface {
	Handle(ctx context.Context, chatID chat.ChatID, userID int64, messageID chat.MessageID, text string)
	// HandleMedia starts a run whose prompt carries IMAGES as well as words. Separate from
	// Handle because the model is shown the picture rather than told where to find it: a path
	// alone makes the agent open a tool to look at what it was already sent, and the Telegram
	// and VK adapters both take this seam for exactly this case.
	HandleMedia(
		ctx context.Context, chatID chat.ChatID, userID int64, messageID chat.MessageID,
		prompt string, images []agent.ImageInput,
	)
	StopChat(chatID chat.ChatID) bool
	NewSession(chatID chat.ChatID) error
	ArmGoal(chatID chat.ChatID, criterion string) (goal.Goal, bool)
	GoalStatus(chatID chat.ChatID) (goal.Goal, bool)
	DisarmGoal(chatID chat.ChatID) bool
}

// ReceiverConfig owns the LO allow-list separately from Telegram/VK identities.
type ReceiverConfig struct {
	Service   Service
	Client    *Client
	Transport *Transport
	Username  string
	BotID     int64
	// ConflictDelay overrides retry timing; nonpositive values use the production default.
	ConflictDelay  time.Duration
	IsAllowed      func(int64) bool
	Guards         func(int64) (bool, string)
	RequireMention bool
	Scheduler      *schedule.Manager
	// Uploads saves inbound photos into the per-chat uploads directory so the agent can
	// open them. Nil disables the download and every attachment gets the same explanatory
	// notice it got before — the adapter must still run where no workspace is wired.
	Uploads *Uploader
	Logger  *slog.Logger
}

// Receiver serially admits updates while the shared dispatcher runs chats concurrently.
type Receiver struct{ cfg ReceiverConfig }

// NewReceiver defaults to denying users unless an allow-list is wired.
func NewReceiver(cfg ReceiverConfig) *Receiver {
	if cfg.ConflictDelay <= 0 {
		cfg.ConflictDelay = pollingConflictDelay
	}
	if cfg.IsAllowed == nil {
		cfg.IsAllowed = func(int64) bool { return false }
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Receiver{cfg: cfg}
}

// Run uses normal positive-offset acknowledgement, retaining queued updates on startup.
// Only one polling process should run for a bot token; persistent conflicts stop polling.
func (r *Receiver) Run(ctx context.Context) error {
	var offset int64
	conflicts := 0
	for ctx.Err() == nil {
		updates, err := r.cfg.Client.GetUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if r.stopPolling(err, &conflicts) {
				return err
			}
			delay := r.pollingRetryDelay(err)
			r.cfg.Logger.Warn("lo: polling failed; retrying", "error", err)
			if !wait(ctx, delay) {
				return ctx.Err()
			}
			continue
		}
		conflicts = 0
		previousOffset := offset
		sort.SliceStable(updates, func(i, j int) bool { return updates[i].ID < updates[j].ID })
		for _, update := range updates {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if update.ID < offset || update.ID < 0 {
				continue
			}
			if update.ID == math.MaxInt64 {
				return errors.New("LO update ID exceeds acknowledgement range")
			}
			r.HandleUpdate(ctx, update)
			offset = update.ID + 1
		}
		// Empty or stale-only immediate responses must not become a busy polling loop.
		if offset == previousOffset && !wait(ctx, time.Second) {
			return ctx.Err()
		}
	}
	return ctx.Err()
}

const (
	maxPollingConflicts  = 5
	pollingConflictDelay = 10 * time.Second
)

// stopPolling tolerates an old in-flight poll after restart, but bounds conflicts
// with four ten-second waits (40s > our 30s long poll), while bounding contention.
func (r *Receiver) stopPolling(err error, conflicts *int) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == 409 {
		*conflicts++
		r.cfg.Logger.Error("lo: polling conflict", "attempt", *conflicts)
		return *conflicts >= maxPollingConflicts
	}
	*conflicts = 0
	return fatalAPIError(err)
}

// admits answers whether this message is for this bot at all, and hands back the three values
// the rest of HandleUpdate needs.
//
// Split out of HandleUpdate rather than inlined, because the two halves answer different
// questions: everything here is "is this addressed to me", and everything after is "what do I
// do about it". Keeping them together also put the whole routine past the repository's
// complexity gate.
func (r *Receiver) admits(msg *Message) (text string, replyToBot, ok bool) {
	if msg == nil || msg.From == nil || msg.From.IsBot || msg.From.ID <= 0 || msg.ID <= 0 ||
		msg.Chat.ID == 0 || !r.cfg.IsAllowed(msg.From.ID) {
		return "", false, false
	}
	if msg.Chat.Type != privateChatType && msg.Chat.Type != "group" && msg.Chat.Type != "supergroup" {
		return "", false, false
	}
	text = strings.TrimSpace(msg.Text)
	if text == "" {
		text = strings.TrimSpace(msg.Caption)
	}
	cleaned, addressed := addressedText(text, r.cfg.Username, msg.Chat.Type != privateChatType)
	if !addressed {
		return "", false, false
	}
	name, _ := command(cleaned)
	replyToBot = r.cfg.BotID > 0 && msg.Reply != nil && msg.Reply.From != nil && msg.Reply.From.ID == r.cfg.BotID
	if msg.Chat.Type != privateChatType && r.cfg.RequireMention &&
		cleaned == text && !replyToBot && !chat.IsReservedCommand(name) {
		return "", false, false
	}
	return cleaned, replyToBot, true
}

// HandleUpdate gates every command and message before invoking the shared service.
func (r *Receiver) HandleUpdate(ctx context.Context, update Update) {
	msg := update.Message
	text, replyToBot, ok := r.admits(msg)
	if !ok {
		return
	}
	name, args := command(text)
	chatID := strconv.FormatInt(msg.Chat.ID, 10)
	if chat.IsReservedCommand(name) {
		r.reserved(ctx, chatID, msg.From.ID, name, args)
		return
	}
	// An attachment this platform will not hand over is answered FIRST, before the guards.
	// It costs no network call, no disk write and no agent run, and core/chat's contract is
	// that a message which produces no work spends no limiter budget. Charging for it would
	// not even save a message: the guard refusal sends one too.
	if note := unservedNotice(msg); note != "" {
		r.notify(ctx, chatID, note)
		return
	}
	// Guards run BEFORE any attachment work that remains. A rate limit or a spent cost cap
	// must be paid with a refusal, not with a download: doing the network call and the disk
	// write first means a capped user still costs bandwidth and storage on every message.
	if hasServedMedia(msg) || text != "" {
		if r.cfg.Guards != nil {
			if allowed, reason := r.cfg.Guards(msg.From.ID); !allowed {
				r.notify(ctx, chatID, reason)
				return
			}
		}
	}
	// Attachments are answered BEFORE the empty-text check: a photo with no caption is a
	// complete request ("look at this"), and dropping it silently is what made the bot
	// look broken.
	photo, document, confirmed, note := r.attachments(ctx, msg, chatID)
	if note != "" {
		// The file the request rests on never reached the agent, so the caption must not be
		// answered on its own: "review this file" without the file invites a confident answer
		// about nothing. One notice, no run.
		r.notify(ctx, chatID, note)
		return
	}
	if text == "" && photo == "" && document == "" {
		return
	}
	if photo != "" {
		r.handlePhoto(ctx, msg, chatID, replyToBot, text, photo, confirmed)
		return
	}
	if document != "" {
		// core/chat's own wording, shared with the Telegram adapter: the caption belongs
		// INSIDE the sentence that names the file, not appended after it, so the agent reads
		// one request rather than a path followed by an unrelated line.
		text = chat.DocumentPrompt(document, text)
	}
	r.cfg.Service.Handle(ctx, chatID, msg.From.ID, strconv.FormatInt(msg.ID, 10),
		replyPrompt(msg, replyToBot, text))
}

// handlePhoto starts the run for a message whose picture reached disk.
//
// The image is SHOWN to the model, not described to it. Both other adapters do this — the seam
// (chat.PhotoPrompt, chat.LoadPhotoImage, Service.HandleMedia) exists in core/chat for exactly
// this case — and the difference is not cosmetic: handed a path alone, the agent has to spend a
// tool call opening a file it was already sent, and a model that never looks answers about the
// caption instead of the picture.
//
// A failed read falls back to the path alone rather than refusing: the file IS on disk and the
// agent can still open it, so losing vision is a degradation, not a dead request. VK makes the
// same fallback, in the same place.
//
// confirmed is the same fallback for a different reason: the vision block carries a media type
// derived from the saved NAME, so it may only be built when the bytes were identified and the
// name agrees with them. Bytes nothing could identify travel as a path, never as a claim.
func (r *Receiver) handlePhoto(
	ctx context.Context, msg *Message, chatID string, replyToBot bool, text, saved string, confirmed bool,
) {
	prompt := replyPrompt(msg, replyToBot, chat.PhotoPrompt(saved, text))
	var images []agent.ImageInput
	if confirmed {
		loaded, err := chat.LoadPhotoImage(saved)
		if err != nil {
			r.cfg.Logger.Warn("lo: load photo for vision failed; sending the path only", "error", err)
		}
		images = loaded
	}
	r.cfg.Service.HandleMedia(ctx, chatID, msg.From.ID, strconv.FormatInt(msg.ID, 10), prompt, images)
}

func hasMedia(msg *Message) bool {
	if len(msg.Photo) > 0 {
		return true
	}
	if msg.Document != nil {
		return true
	}
	for _, raw := range []json.RawMessage{
		msg.Voice, msg.Audio, msg.Video, msg.Animation, msg.Sticker, msg.VideoNote,
	} {
		if present(raw) {
			return true
		}
	}
	return false
}

// unservedKinds names the attachments LO accepts from a user but will not hand to a bot,
// in the order a message is inspected. Each carries its own sentence: "attachments are not
// supported" taught users nothing about which of their files the bot could actually read,
// and photos now CAN be read.
//
//nolint:gochecknoglobals // A fixed table, read-only, kept beside the function that uses it.
var unservedKinds = []struct {
	has    func(*Message) bool
	notice string
}{
	{
		has:    func(m *Message) bool { return present(m.Voice) },
		notice: "I cannot listen to voice messages on LO yet. Please send the request as text.",
	},
	{
		has:    func(m *Message) bool { return present(m.VideoNote) },
		notice: "I cannot open video notes on LO yet. Please send the request as text.",
	},
	{
		has: func(m *Message) bool { return present(m.Video) },
		notice: "I cannot download video on LO: the platform hands bots a reference, not the bytes. " +
			"Describe what it shows, or send a screenshot as a photo.",
	},
	{
		has: func(m *Message) bool { return present(m.Audio) },
		notice: "I cannot download audio on LO: the platform hands bots a reference, not the bytes. " +
			"Please send the request as text.",
	},
	{
		// Before any arm that could also match it. Bot API fills `document` ALONGSIDE
		// `animation` for a GIF, and this adapter READS documents — so without this order a
		// GIF would be downloaded and handed to the agent as a file it cannot act on, instead
		// of getting the sentence that says what is missing.
		has:    func(m *Message) bool { return present(m.Animation) },
		notice: "I cannot open animations on LO yet. A still screenshot sent as a photo works.",
	},
	{
		has:    func(m *Message) bool { return present(m.Sticker) },
		notice: "Stickers carry nothing I can act on. Please send the request as text.",
	},
}

// attachments turns a message's files into (prompt addition, user notice). Both may be
// empty. A photo is downloaded into the chat's uploads directory and the agent is told the
// path; everything else gets the sentence that says what this platform withholds.
//
// A download failure is a NOTICE, never a dropped message: the user watched their file
// arrive and deserves to know it did not reach the agent.
func (r *Receiver) attachments(
	ctx context.Context, msg *Message, chatID string,
) (photo, document string, confirmed bool, notice string) {
	// LargestPhoto is the ONLY reader of msg.Photo here, deliberately. hasServedMedia counts a
	// non-empty array as work and spends guard budget on it, so a ladder whose every rung
	// lacks a file_id must still end in a sentence — otherwise the message is dropped in
	// silence, or worse, its caption reaches the agent without the picture it refers to. That
	// is the "confident answer about nothing" every other arm here exists to prevent.
	if len(msg.Photo) > 0 {
		size, ok := LargestPhoto(msg.Photo)
		if !ok {
			return "", "", false, "I could not read that image. Please send it again."
		}
		saved, note := r.download(ctx, chatID, size.FileID, photoFileName(msg.ID), imageKind)
		shown := false
		if saved != "" {
			var refusal string
			if saved, shown, refusal = r.checkPhotoBytes(saved); refusal != "" {
				note = refusal
			}
		}
		return saved, "", shown, note
	}
	if msg.Document != nil {
		// The name comes from the chat and is sanitised on the way to disk; keeping it is what
		// lets the agent see "spec.pdf" rather than an opaque id. LO fills it only "usually",
		// and a nameless document would land as an extensionless "upload" — so an absent name
		// is rebuilt from the declared type instead.
		//
		// A document is returned SEPARATELY from a photo even though both are just a saved
		// path, because the caller does different things with them: a picture is shown to the
		// model as a vision block, a document becomes words the agent opens with a tool. Its
		// name is the sender's, so nothing sniffs it — that correction is for the name this
		// adapter invented, not for one a person chose.
		// The guard asks the SANITISER what the name will become, rather than re-deriving its
		// rule here. A TrimSpace test passes ".", "..", "..." and "/" — all of which fsutil
		// reduces to its own placeholder or to a bare separator, leaving the agent the
		// extensionless path this fallback exists to prevent — and a rule copied by hand would
		// drift the first time fsutil's changes.
		name := msg.Document.FileName
		if sanitized := fsutil.SanitizeUploadName(name); sanitized == fsutil.DefaultUploadName ||
			strings.Trim(sanitized, `./\ `) == "" {
			name = documentFileName(msg.ID, msg.Document.MimeType)
		}
		saved, note := r.download(ctx, chatID, msg.Document.FileID, name, fileKind)
		return "", saved, false, note
	}
	// No unserved-kind arm here any more: those are answered before the guards, without
	// touching the network. See unservedNotice.
	return "", "", false, ""
}

// checkPhotoBytes names a saved photo by what its bytes ARE, and decides whether the picture
// can be shown to the model at all.
//
// The name was a GUESS until the bytes existed, and it is not decoration: core/chat reads the
// vision block's media type out of the saved path, so a PNG saved as .jpg would be declared to
// the model as a JPEG. The three outcomes below are that same rule carried through:
//
//   - A format the vision block CAN carry is renamed to it and shown.
//   - A picture in a format it cannot carry (bmp, ico, tiff…) is refused BY NAME. Renaming is
//     impossible — core/chat answers image/jpeg for every extension it does not know — so the
//     choice is a false claim about the bytes or a sentence the user can act on.
//   - Bytes that are not a picture at all (a storage error page served with 200, an empty
//     body) get the sentence a failed download already has: to the person who sent it, the
//     file arrived here and not at the agent, which is the same event.
//
// Anything else is UNKNOWN rather than disproven — the sniff failed, or the bytes are a format
// DetectContentType does not recognise — and the file stays with the name it had. Deleting a
// picture because this adapter could not identify it would lose a file that may be perfectly
// readable.
func (r *Receiver) checkPhotoBytes(path string) (saved string, confirmed bool, notice string) {
	path, detected := nameByContent(path, r.cfg.Logger)
	switch {
	case photoExtensions[detected] != "" && strings.HasSuffix(path, photoExtensions[detected]):
		// The only case the model is SHOWN: the bytes were identified and the name on disk
		// agrees with them, so the media type core/chat derives from that name is true.
		return path, true, ""
	case detected == "" || detected == "application/octet-stream":
		// Unidentified, or the rename failed. The file is real and may well be readable — the
		// agent can open it — but nothing here knows what it IS, and core/chat would tell the
		// model "image/jpeg" on the strength of a name this adapter invented. The run goes
		// ahead on the PATH alone.
		return path, false, ""
	case strings.HasPrefix(detected, "image/"):
		r.removeUnreadable(path)
		return "", false, "I cannot read " + detected + " images. Please send it as PNG or JPEG."
	case detected == emptyFileType:
		r.removeUnreadable(path)
		return "", false, "That image arrived empty. Please try sending it again."
	}
	r.removeUnreadable(path)
	return "", false, "I could not download that image. Please try sending it again."
}

// removeUnreadable drops a saved file the agent will never be shown, so a refused picture does
// not sit in the uploads directory forever.
func (r *Receiver) removeUnreadable(path string) {
	if err := os.Remove(path); err != nil {
		r.cfg.Logger.Warn("lo: could not remove a photo the agent will not be shown", "error", err)
	}
}

// unservedNotice is the whole of the attachment path that needs no I/O — the kinds this
// platform will not hand over, each with its own sentence.
//
// It answers "" when the message carries something that CAN be read, so a photo that arrives
// alongside a sticker is still work rather than a refusal. That is the precedence attachments
// applies, kept here rather than restated.
func unservedNotice(msg *Message) string {
	if hasServedMedia(msg) {
		return ""
	}
	for _, kind := range unservedKinds {
		if kind.has(msg) {
			return kind.notice
		}
	}
	return ""
}

// hasServedMedia reports an attachment this adapter can actually turn into work. Distinct from
// hasMedia, which answers "carries an attachment of any kind": the two differ exactly on the
// kinds that produce a sentence and nothing else, and that difference is what keeps a refusal
// from spending guard budget.
func hasServedMedia(msg *Message) bool {
	return len(msg.Photo) > 0 || msg.Document != nil
}

// attachmentKind is what a refusal needs to say about one kind of attachment: the noun the
// user sees, and what they can do instead.
//
// The advice cannot be shared even though the sentence around it can. "Paste the contents" is
// something the sender of a specification can do and the sender of a photograph cannot, and a
// refusal that ends in impossible advice reads as a bot that did not look at what it got.
type attachmentKind struct{ noun, advice string }

//nolint:gochecknoglobals // Two fixed values, read-only, beside the function that uses them.
var (
	imageKind = attachmentKind{noun: "image", advice: "Describe what it shows, or send it as a file."}
	fileKind  = attachmentKind{
		noun:   "file",
		advice: "Paste the contents, or point me at the file in a repository.",
	}
)

// download saves one inbound attachment and phrases the outcome for both readers: the agent,
// which needs a path it can open, and the user, who needs to know if their file did not arrive.
//
// kind is a parameter rather than a branch because every outcome below reads the same for
// both — only the noun changes, and in one case the advice after it.
func (r *Receiver) download(
	ctx context.Context, chatID, fileID, name string, kind attachmentKind,
) (saved, notice string) {
	if r.cfg.Uploads == nil {
		return "", "I cannot read attachments in this deployment: no uploads directory is configured."
	}
	path, err := r.cfg.Uploads.Save(ctx, chatID, fileID, name)
	switch {
	case errors.Is(err, ErrUploadTooLarge):
		return "", "That " + kind.noun + " is too large for me to open. Please send a smaller one."
	case errors.Is(err, ErrNoBytes):
		// The platform answered the reference and withheld the bytes — for a document that
		// means this LO has not shipped document downloads yet.
		return "", "This LO does not hand bots the bytes of that " + kind.noun + ", so I cannot open it. " +
			kind.advice
	case err != nil:
		r.cfg.Logger.Warn("lo: attachment download failed", "kind", kind.noun, "error", err)
		return "", "I could not download that " + kind.noun + ". Please try sending it again."
	}
	// The SAVED PATH, not a sentence: a photo becomes a vision block and a document becomes
	// words, and only the caller knows which. Phrasing here would have to guess.
	return path, ""
}

func (r *Receiver) notify(ctx context.Context, chatID, text string) {
	for _, chunk := range chat.ChunkFencedSize(text, r.cfg.Transport.Capabilities().MaxMessageRunes) {
		if _, err := r.cfg.Transport.Send(ctx, chatID, chunk, "", false); err != nil {
			r.cfg.Logger.Warn("lo: notice delivery failed", "error", err)
			return
		}
	}
}

func wait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// pollingRetryDelay keeps a faulty server hint from suspending reception for hours.
// Match the delivery loop's maximum wait while retaining shorter server hints.
func (r *Receiver) pollingRetryDelay(err error) time.Duration {
	const maxWait = 30 * time.Second
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == http.StatusConflict {
		return r.cfg.ConflictDelay
	}
	delay, ok := RetryAfter(err)
	if !ok {
		return time.Second
	}
	return min(delay, maxWait)
}

func replyPrompt(msg *Message, replyToBot bool, text string) string {
	if msg.Reply == nil {
		return text
	}
	quoted := msg.Reply.Text
	if quoted == "" {
		quoted = msg.Reply.Caption
	}
	if hasMedia(msg.Reply) {
		quoted += "\n[Quoted attachment is unavailable to this bot; ask the user for its contents if needed.]"
	}
	if quoted == "" {
		quoted = "[media]"
	}
	author := ""
	if msg.Reply.From != nil {
		author = msg.Reply.From.Username
	}
	if replyToBot {
		author = chat.AssistantAuthorLabel
	}
	return chat.QuotedPrompt(author, quoted, text)
}
