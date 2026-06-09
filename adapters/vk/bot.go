package vk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/duckbugio/flock/core/chat"
)

// vkMaxMessageRunes is VK's per-message length cap in runes, reported via
// Capabilities so the neutral chunker is parameterized rather than hardcoded.
// VK's documented limit is 4096; this is a §6 UNKNOWN to live-confirm with a
// real token (see the report / .team/memory.md).
const vkMaxMessageRunes = 4096

// floodBackoff is the fixed delay the RetryAfter seam asks the Service to wait
// after a VK flood/rate error (codes 6/9). VK returns no Retry-After header, so a
// small fixed back-off is used; the Service caps and retries terminal delivery.
const floodBackoff = time.Second

// botChat implements core/chat.Transport over a VK *Client. It converts VK's
// native int64 peer/message ids to and from the string ChatID/MessageID the
// neutral layer uses, renders the Stop inline keyboard, and generates a unique
// random_id per send.
type botChat struct {
	api      *Client
	groupID  int64
	randSeed int64
	randSeq  atomic.Uint64
	// kbDisabledOnce guards the one-shot WARN log emitted the first time the VK
	// "Bot abilities" toggle (error 912) forces a keyboard-less resend, so a
	// misconfigured community does not spam the log on every progress message.
	kbDisabledOnce sync.Once
}

// NewBotChat adapts a VK *Client to the core/chat.Transport interface. groupID is
// the community id (used elsewhere for the mention parse); randSeed seeds the
// per-adapter random_id generator — production passes a monotonic seed, tests
// pass a fixed seed for deterministic random_ids (VK dedupes a repeated random_id
// within ~1h, so it only needs to be unique, not unpredictable).
func NewBotChat(api *Client, groupID, randSeed int64) chat.Transport {
	return &botChat{api: api, groupID: groupID, randSeed: randSeed}
}

// Capabilities reports VK's feature set: it can send documents (the docs.*
// dance), and its message cap is 4096 runes. With these flags the neutral
// Service drives the full outbox path, exactly as on Telegram.
func (c *botChat) Capabilities() chat.Capabilities {
	return chat.Capabilities{
		CanSendDocument: true,
		MaxMessageRunes: vkMaxMessageRunes,
	}
}

// nextRandomID returns a process-unique int64 random_id: the per-adapter seed
// mixed with a monotonic counter, so two sends never collide (VK silently
// dedupes a repeated random_id within ~1h). It is deterministic given a fixed
// seed, which keeps tests reproducible.
func (c *botChat) nextRandomID() int64 {
	n := c.randSeq.Add(1)
	//nolint:gosec // G115: deliberate wrap into the signed random_id space; uniqueness, not range, is what matters.
	return c.randSeed + int64(n)
}

// parsePeerID converts the neutral string chat id back to VK's int64 peer id. A
// non-numeric id (which VK never produces) maps to 0, surfacing the misuse rather
// than silently targeting peer 0.
func parsePeerID(chatID chat.ChatID) int64 {
	id, _ := strconv.ParseInt(chatID, 10, 64)
	return id
}

// parseMessageID converts the neutral string message id back to VK's int64
// global message id.
func parseMessageID(messageID chat.MessageID) int64 {
	id, _ := strconv.ParseInt(messageID, 10, 64)
	return id
}

// Send posts a new message to chatID. When stopRunID is non-empty it renders the
// inline Stop keyboard bound to that run; empty sends no keyboard. asMarkdown is
// IGNORED — VK has no HTML/Markdown parse mode, so the text is always sent
// verbatim. It returns the GLOBAL message id (what Edit/Delete address).
func (c *botChat) Send(
	ctx context.Context, chatID chat.ChatID, text, stopRunID string, _ bool,
) (chat.MessageID, error) {
	peerID := parsePeerID(chatID)
	keyboard := stopKeyboard(stopRunID)
	msgID, err := c.api.MessagesSend(ctx, peerID, text, c.nextRandomID(), keyboard, "")
	// VK error 912: the community has "Bot abilities" OFF, so a keyboard'd send is
	// rejected while the same send WITHOUT a keyboard succeeds. Retry once without
	// the keyboard (Stop then falls back to the /stop text command); the message
	// must still be delivered. Only retry when a keyboard was actually attached.
	if err != nil && keyboard != "" && isBotFeatureDisabled(err) {
		c.warnKeyboardsDisabled()
		msgID, err = c.api.MessagesSend(ctx, peerID, text, c.nextRandomID(), "", "")
	}
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(msgID, 10), nil
}

// Edit updates messageID in place. A non-empty stopRunID keeps the Stop keyboard;
// empty clears it (the final answer carries no markup). asMarkdown is IGNORED
// (VK has no parse mode).
func (c *botChat) Edit(
	ctx context.Context, chatID chat.ChatID, messageID chat.MessageID, text, stopRunID string, _ bool,
) error {
	peerID := parsePeerID(chatID)
	msgID := parseMessageID(messageID)
	keyboard := stopKeyboard(stopRunID)
	err := c.api.MessagesEdit(ctx, peerID, msgID, text, keyboard)
	// VK error 912: see Send. Retry the edit once without the keyboard so progress
	// keeps updating even when the community's "Bot abilities" toggle is OFF. Only
	// retry when a keyboard was actually attached (an empty keyboard clears markup
	// and never triggers 912).
	if err != nil && keyboard != "" && isBotFeatureDisabled(err) {
		c.warnKeyboardsDisabled()
		err = c.api.MessagesEdit(ctx, peerID, msgID, text, "")
	}
	return err
}

// warnKeyboardsDisabled logs a single WARN line the first time error 912 forces a
// keyboard-less resend, pointing at the community-side fix. The sync.Once keeps a
// misconfigured community from spamming the log on every progress message.
func (c *botChat) warnKeyboardsDisabled() {
	c.kbDisabledOnce.Do(func() {
		slog.Default().Warn("vk: bot keyboards disabled for this community (error 912); " +
			"sending without Stop button — enable 'Bot abilities' in community settings")
	})
}

// StreamDraft is unsupported on VK: it has no ephemeral live-draft primitive, so
// this returns an error unconditionally. The Service treats that as "drafts
// unsupported" and falls back to editing the anchor message (the rate-limited
// progress path), exactly as on a platform without drafts.
func (c *botChat) StreamDraft(_ context.Context, _ chat.ChatID, _, _ string, _ bool) error {
	return errDraftUnsupported
}

// errDraftUnsupported signals the Service that VK cannot stream an ephemeral
// draft, so it should drive progress via Edit instead.
var errDraftUnsupported = errors.New("vk: live draft preview unsupported; use message edits")

// Delete removes messageID for everyone; failures are non-fatal to the caller.
func (c *botChat) Delete(ctx context.Context, chatID chat.ChatID, messageID chat.MessageID) error {
	return c.api.MessagesDelete(ctx, parsePeerID(chatID), parseMessageID(messageID))
}

// SendDocument uploads a local file to chatID as a VK document attachment via the
// three-step docs.* dance, then sends a message carrying it. The data is streamed
// (no token-bearing URL constructed). Used by the outbox.
func (c *botChat) SendDocument(ctx context.Context, chatID chat.ChatID, name string, data io.Reader) error {
	peerID := parsePeerID(chatID)
	attachment, err := c.api.UploadDocument(ctx, peerID, name, data)
	if err != nil {
		return err
	}
	_, err = c.api.MessagesSend(ctx, peerID, "", c.nextRandomID(), "", attachment)
	return err
}

// SendStarNudge posts the post-task star-nudge message with the inline confirm
// button (a callback keyboard whose payload triggers the star action), mirroring
// the Telegram nudge's separate-seam shape. It returns the new global message id.
func (c *botChat) SendStarNudge(ctx context.Context, chatID chat.ChatID, text string) (chat.MessageID, error) {
	msgID, err := c.api.MessagesSend(ctx, parsePeerID(chatID), text, c.nextRandomID(), starKeyboard(), "")
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(msgID, 10), nil
}

// RetryAfter reports a fixed back-off when err is a VK flood/rate error (code 6
// "too many requests" or code 9 "flood control"). VK sends no Retry-After header,
// so a small fixed delay is used. It is wired into core/chat.Config.RetryAfter so
// the neutral run loop throttles its edit fallback and backs off terminal
// delivery without importing VK's error types.
func RetryAfter(err error) (time.Duration, bool) {
	var ae *apiError
	if errors.As(err, &ae) && (ae.Code == errCodeTooManyRequests || ae.Code == errCodeFloodControl) {
		return floodBackoff, true
	}
	return 0, false
}

// stopKeyboard returns the JSON for a VK inline keyboard carrying a single
// callback Stop button bound to runID, or "" when runID is empty (final messages
// carry no keyboard). The payload is {"stop":"<runID>"}, parsed back by the
// message_event handler into a Service Stop.
func stopKeyboard(runID string) string {
	if runID == "" {
		return ""
	}
	return keyboardJSON("⏹ Stop", stopPayload(runID), "negative")
}

// starKeyboard returns the JSON for the star-nudge confirm keyboard: a single
// callback button whose payload triggers the star action.
func starKeyboard() string {
	return keyboardJSON(starButtonText, starPayloadJSON(), "positive")
}

// vkKeyboard / vkButton / vkAction model VK's inline keyboard JSON so it is built
// with encoding/json (correct escaping) rather than fragile string concatenation.
type vkKeyboard struct {
	Inline  bool         `json:"inline"`
	Buttons [][]vkButton `json:"buttons"`
}

type vkButton struct {
	Action vkAction `json:"action"`
	Color  string   `json:"color"`
}

type vkAction struct {
	Type    string `json:"type"`
	Label   string `json:"label"`
	Payload string `json:"payload"`
}

// keyboardJSON builds a one-button inline callback keyboard. The payload is a
// JSON string (VK requires the payload be valid JSON; we pass a JSON object
// encoded as a string).
func keyboardJSON(label, payload, color string) string {
	kb := vkKeyboard{
		Inline: true,
		Buttons: [][]vkButton{{
			{Action: vkAction{Type: "callback", Label: label, Payload: payload}, Color: color},
		}},
	}
	b, err := json.Marshal(kb)
	if err != nil {
		// The struct is fixed-shape and always marshalable; on the impossible error
		// path return "" so the message still sends without a keyboard.
		return ""
	}
	return string(b)
}
