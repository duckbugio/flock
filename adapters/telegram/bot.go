// Package telegram is the Telegram transport adapter for the core/chat
// conversation layer. It wraps a *bot.Bot, implements core/chat.Transport
// (converting Telegram's native int64 chat id / int message id to and from the
// string ChatID/MessageID the neutral layer uses), renders the Stop and star
// inline markup, classifies Telegram's 429 rate-limit error, downloads inbound
// files (with token redaction), and converts the assistant's Markdown to
// Telegram HTML. The transport-neutral run loop lives in core/chat.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/duckbugio/flock/core/chat"
)

// stopCallbackPrefix is the inline-button callback_data prefix carrying the run
// id so a Stop press maps back to the right in-flight run.
const stopCallbackPrefix = "stop:"

// telegramMaxMessageRunes is Telegram's per-message length cap in runes, reported
// via Capabilities so the neutral chunker is parameterized rather than hardcoded.
const telegramMaxMessageRunes = 4096

// botChat implements core/chat.Transport over a *bot.Bot.
type botChat struct {
	b *bot.Bot
	// enableRich reports the Capabilities().CanSendRich flag, sourced from the
	// ENABLE_RICH_MESSAGES config (see docs/rich-messages-plan.md).
	enableRich bool
	// rich serializes and sends Bot API 10.1 rich messages. It is non-nil only when
	// enableRich is set; every call site treats a rich error as "fall back to the
	// legacy MarkdownToHTML/plain path", so the rich path is never load-bearing.
	rich richTransport
	// richFailures counts CONSECUTIVE rich failures (reset to 0 on any success).
	// richDisabledUntil holds a unix-nano deadline: while now < it, the rich path is
	// suppressed (0 = enabled). Together they form a circuit breaker behind the
	// default-on flag: after richFailureThreshold consecutive failures the rich path
	// is suppressed for richCooldown, then a single probe is allowed — so an API that
	// does not serve the rich methods stops costing a failed round-trip per message,
	// while a transient blip self-heals instead of disabling rich until restart.
	richFailures      atomic.Int32
	richDisabledUntil atomic.Int64
	// now is an injectable clock for the breaker cooldown (tests). nil means time.Now.
	now func() time.Time
}

const (
	// richFailureThreshold is how many consecutive rich failures trip the breaker.
	// Small enough to stop wasted round-trips quickly when the API lacks the methods,
	// large enough to ride out a transient blip (any success resets the count).
	richFailureThreshold = 5
	// richCooldown is how long the rich path stays suppressed after the breaker trips
	// before a single probe is allowed.
	richCooldown = time.Minute
)

// NewBotChat adapts a *bot.Bot to the core/chat.Transport interface used by the
// Service. enableRich sets Capabilities().CanSendRich (from ENABLE_RICH_MESSAGES)
// and, when set, wires the rich HTTP transport over the bot's token; pass false to
// keep the legacy MarkdownToHTML/plain rendering.
func NewBotChat(b *bot.Bot, enableRich bool) chat.Transport {
	c := &botChat{b: b, enableRich: enableRich}
	if enableRich {
		c.rich = newHTTPRichTransport(b.Token(), defaultTelegramAPIBase, nil)
	}
	return c
}

// Capabilities reports Telegram's full feature set: it can send documents, its
// message cap is 4096 runes, and it can render rich messages when enableRich is
// set. With these flags the neutral Service behaves exactly as before when rich
// is off.
func (c *botChat) Capabilities() chat.Capabilities {
	return chat.Capabilities{
		CanSendDocument: true,
		MaxMessageRunes: telegramMaxMessageRunes,
		CanSendRich:     c.enableRich,
	}
}

// parseChatID converts the neutral string chat id back to Telegram's int64. A
// non-numeric id (which Telegram never produces) maps to 0, which the Bot API
// rejects — surfacing the misuse rather than silently targeting chat 0.
func parseChatID(chatID chat.ChatID) int64 {
	id, _ := strconv.ParseInt(chatID, 10, 64)
	return id
}

// parseMessageID converts the neutral string message id back to Telegram's int.
func parseMessageID(messageID chat.MessageID) int {
	id, _ := strconv.Atoi(messageID)
	return id
}

func (c *botChat) Send(ctx context.Context, chatID chat.ChatID, text, stopRunID string, asMarkdown bool) (chat.MessageID, error) {
	// Rich path first (when enabled and the message is markdown-rendered). Any
	// failure falls through to the legacy HTML/plain path below — the rich render
	// never costs the user the answer.
	if id, ok := c.trySendRich(ctx, parseChatID(chatID), text, stopRunID, asMarkdown); ok {
		return id, nil
	}
	params := &bot.SendMessageParams{
		ChatID:      parseChatID(chatID),
		Text:        text,
		ReplyMarkup: stopMarkup(stopRunID),
	}
	return c.sendParams(ctx, params, text, asMarkdown)
}

// SendReply posts text as a native reply to replyToID, carrying no Stop markup. It
// reuses the legacy HTML/plain rendering path (the short completion notice does not
// need the rich path, which has no reply parameter), so it shares the same
// markdown→HTML render and parse-error plain-text fallback as Send via sendParams.
// AllowSendingWithoutReply is set so a deleted/missing target still delivers the
// notice as a plain message instead of erroring.
func (c *botChat) SendReply(
	ctx context.Context, chatID chat.ChatID, replyToID chat.MessageID, text string, asMarkdown bool,
) (chat.MessageID, error) {
	params := &bot.SendMessageParams{
		ChatID: parseChatID(chatID),
		Text:   text,
		ReplyParameters: &models.ReplyParameters{
			MessageID:                parseMessageID(replyToID),
			AllowSendingWithoutReply: true,
		},
	}
	return c.sendParams(ctx, params, text, asMarkdown)
}

// sendParams renders params.Text as HTML when asMarkdown is set and posts the
// message, retrying once as plain text if Telegram rejects the HTML markup so a
// formatting glitch never costs the user the message. text is the ORIGINAL
// (unrendered) body used for that plain retry. It is the shared body-building tail
// of Send and SendReply, which differ only in the markup/reply fields on params.
func (c *botChat) sendParams(
	ctx context.Context, params *bot.SendMessageParams, text string, asMarkdown bool,
) (chat.MessageID, error) {
	if asMarkdown {
		params.Text = MarkdownToHTML(text)
		params.ParseMode = models.ParseModeHTML
	}
	msg, err := c.b.SendMessage(ctx, params)
	if err != nil && asMarkdown && isParseError(err) {
		// Telegram rejected our HTML markup: resend the ORIGINAL text as plain so a
		// formatting glitch never costs the user the message.
		params.Text = text
		params.ParseMode = ""
		msg, err = c.b.SendMessage(ctx, params)
	}
	if err != nil {
		return "", fmt.Errorf("send telegram message: %w", err)
	}
	return strconv.Itoa(msg.ID), nil
}

func (c *botChat) Edit(
	ctx context.Context, chatID chat.ChatID, messageID chat.MessageID, text, stopRunID string, asMarkdown bool,
) error {
	// Rich path first; on any failure fall through to the legacy HTML/plain edit.
	if c.tryEditRich(ctx, parseChatID(chatID), parseMessageID(messageID), text, stopRunID, asMarkdown) {
		return nil
	}
	params := &bot.EditMessageTextParams{
		ChatID:      parseChatID(chatID),
		MessageID:   parseMessageID(messageID),
		Text:        text,
		ReplyMarkup: stopMarkup(stopRunID),
	}
	if asMarkdown {
		params.Text = MarkdownToHTML(text)
		params.ParseMode = models.ParseModeHTML
	}
	_, err := c.b.EditMessageText(ctx, params)
	if err != nil && asMarkdown && isParseError(err) {
		params.Text = text
		params.ParseMode = ""
		_, err = c.b.EditMessageText(ctx, params)
	}
	return err
}

// trySendRich attempts to send text as a Bot API 10.1 rich message. It returns
// (id, true) on success; ("", false) tells Send to fall back to the legacy
// HTML/plain path. It is a no-op (false) unless rich is enabled, asMarkdown is
// set, and the text is non-empty — so plain sends and the rich-off build keep
// their exact previous behaviour.
func (c *botChat) trySendRich(
	ctx context.Context, chatID int64, text, stopRunID string, asMarkdown bool,
) (chat.MessageID, bool) {
	if !c.richAttemptable(asMarkdown, text) {
		return "", false
	}
	id, err := c.rich.send(ctx, chatID, richMarkdown(text), stopMarkup(stopRunID))
	c.recordRich(err)
	if err != nil {
		return "", false // any rich failure → legacy fallback
	}
	return strconv.Itoa(id), true
}

// tryEditRich is the Edit counterpart of trySendRich: it returns true when the
// rich edit succeeded, false to fall back to the legacy HTML/plain edit.
func (c *botChat) tryEditRich(
	ctx context.Context, chatID int64, messageID int, text, stopRunID string, asMarkdown bool,
) bool {
	if !c.richAttemptable(asMarkdown, text) {
		return false
	}
	err := c.rich.edit(ctx, chatID, messageID, richMarkdown(text), stopMarkup(stopRunID))
	c.recordRich(err)
	return err == nil
}

// nowTime reads the breaker clock, defaulting to time.Now when none is injected.
func (c *botChat) nowTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// richAttemptable reports whether a rich attempt should be made for this call:
// rich is enabled and wired, the breaker's cooldown is not currently active, the
// text is markdown-rendered, and it is non-empty (the empty-text draft clear stays
// on the legacy path).
func (c *botChat) richAttemptable(asMarkdown bool, text string) bool {
	if !c.enableRich || c.rich == nil || !asMarkdown || text == "" {
		return false
	}
	until := c.richDisabledUntil.Load()
	return until == 0 || c.nowTime().UnixNano() >= until
}

// recordRich feeds the circuit breaker: a success clears the failure count and any
// cooldown; a failure increments the count and, on reaching richFailureThreshold,
// suppresses the rich path for richCooldown (logged WARN only on the first trip,
// not on each re-arm). The error is already token-redacted by the transport.
func (c *botChat) recordRich(err error) {
	if err == nil {
		c.richFailures.Store(0)
		c.richDisabledUntil.Store(0)
		return
	}
	n := c.richFailures.Add(1)
	if n < richFailureThreshold {
		return
	}
	until := c.nowTime().Add(richCooldown).UnixNano()
	if prev := c.richDisabledUntil.Swap(until); prev == 0 {
		slog.Default().Warn(
			"telegram: suppressing rich rendering after consecutive failures; using legacy",
			"failures", n, "cooldown", richCooldown, "error", err,
		)
	}
}

// isParseError reports whether err is Telegram rejecting our parse_mode markup
// (a 400 "can't parse entities"). Only such errors warrant the plain-text retry;
// a transport/Not-Found error would fail identically, so retrying it risks a
// double-send for no gain.
func isParseError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "parse")
}

func (c *botChat) Delete(ctx context.Context, chatID chat.ChatID, messageID chat.MessageID) error {
	_, err := c.b.DeleteMessage(ctx, &bot.DeleteMessageParams{
		ChatID:    parseChatID(chatID),
		MessageID: parseMessageID(messageID),
	})
	return err
}

func (c *botChat) SendDocument(ctx context.Context, chatID chat.ChatID, name string, data io.Reader) error {
	// The go-telegram lib uploads the InputFileUpload via multipart form-data, so
	// no token-bearing URL is constructed for the file — the token only ever rides
	// the (header/path) API call the lib makes internally, never logged here.
	_, err := c.b.SendDocument(ctx, &bot.SendDocumentParams{
		ChatID:   parseChatID(chatID),
		Document: &models.InputFileUpload{Filename: name, Data: data},
	})
	return err
}

// SendStarNudge posts the post-task star-nudge message with the inline confirm
// button (a callback button). It sends plain text (the nudge copy carries no
// markup) and returns the new message id.
func (c *botChat) SendStarNudge(ctx context.Context, chatID chat.ChatID, text string) (chat.MessageID, error) {
	msg, err := c.b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      parseChatID(chatID),
		Text:        text,
		ReplyMarkup: starMarkup(),
	})
	if err != nil {
		return "", err
	}
	return strconv.Itoa(msg.ID), nil
}

// RetryAfter reports the server-requested back-off when err is a Telegram 429
// (rate limit), flooring a missing/zero value to 1s so callers still wait. It is
// wired into core/chat.Config.RetryAfter so the neutral run loop can throttle
// its edit fallback and back off terminal delivery without knowing about
// Telegram's error types.
func RetryAfter(err error) (time.Duration, bool) {
	var tmr *bot.TooManyRequestsError
	if errors.As(err, &tmr) {
		if d := time.Duration(tmr.RetryAfter) * time.Second; d > 0 {
			return d, true
		}
		return time.Second, true
	}
	return 0, false
}

// stopMarkup returns an inline keyboard with a single Stop button bound to
// runID, or nil when runID is empty (final messages carry no markup).
func stopMarkup(runID string) models.ReplyMarkup {
	if runID == "" {
		return nil
	}
	return &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{{
			{Text: "⏹ Stop", CallbackData: stopCallbackPrefix + runID},
		}},
	}
}

// StopRunID extracts the run id from a Stop callback's data, or "" if the data
// is not a Stop callback.
func StopRunID(data string) string {
	if !strings.HasPrefix(data, stopCallbackPrefix) {
		return ""
	}
	return strings.TrimPrefix(data, stopCallbackPrefix)
}

// CallbackMatch reports whether callback data is a Stop callback this Service
// should handle. Used as the prefix for the bot's callback handler.
func CallbackMatch() string { return stopCallbackPrefix }
