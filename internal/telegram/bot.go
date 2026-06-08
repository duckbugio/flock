package telegram

import (
	"context"
	"io"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/duckbugio/flock/internal/tgui"
)

// botChat implements chat over a *bot.Bot.
type botChat struct {
	b *bot.Bot
}

// NewBotChat adapts a *bot.Bot to the chat interface used by Service.
func NewBotChat(b *bot.Bot) chat { //nolint:revive // returns unexported interface by design
	return &botChat{b: b}
}

func (c *botChat) Send(ctx context.Context, chatID int64, text, stopRunID string, asMarkdown bool) (int, error) {
	params := &bot.SendMessageParams{
		ChatID:      chatID,
		Text:        text,
		ReplyMarkup: stopMarkup(stopRunID),
	}
	if asMarkdown {
		params.Text = tgui.MarkdownToHTML(text)
		params.ParseMode = models.ParseModeHTML
	}
	msg, err := c.b.SendMessage(ctx, params)
	if err != nil && asMarkdown && isParseError(err) {
		// Telegram rejected our HTML markup: resend the ORIGINAL text as plain so a
		// formatting glitch never costs the user the answer.
		params.Text = text
		params.ParseMode = ""
		msg, err = c.b.SendMessage(ctx, params)
	}
	if err != nil {
		return 0, err
	}
	return msg.ID, nil
}

func (c *botChat) Edit(ctx context.Context, chatID int64, messageID int, text, stopRunID string, asMarkdown bool) error {
	params := &bot.EditMessageTextParams{
		ChatID:      chatID,
		MessageID:   messageID,
		Text:        text,
		ReplyMarkup: stopMarkup(stopRunID),
	}
	if asMarkdown {
		params.Text = tgui.MarkdownToHTML(text)
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

// StreamDraft streams text as an ephemeral live preview keyed by draftID (repeated
// calls update the same preview; an empty text clears it). It is rate-limit-free
// relative to EditMessageText, so it carries the live run progress while the answer
// is persisted with Send/Edit. A draft can't hold an inline keyboard, which is why
// the Stop button rides a separate anchor message.
func (c *botChat) StreamDraft(ctx context.Context, chatID int64, draftID, text string) error {
	_, err := c.b.SendMessageDraft(ctx, &bot.SendMessageDraftParams{
		ChatID:  chatID,
		DraftID: draftID,
		Text:    text,
	})
	return err
}

// isParseError reports whether err is Telegram rejecting our parse_mode markup
// (a 400 "can't parse entities"). Only such errors warrant the plain-text retry;
// a transport/Not-Found error would fail identically, so retrying it risks a
// double-send for no gain.
func isParseError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "parse")
}

func (c *botChat) Delete(ctx context.Context, chatID int64, messageID int) error {
	_, err := c.b.DeleteMessage(ctx, &bot.DeleteMessageParams{
		ChatID:    chatID,
		MessageID: messageID,
	})
	return err
}

func (c *botChat) SendDocument(ctx context.Context, chatID int64, name string, data io.Reader) error {
	// The go-telegram lib uploads the InputFileUpload via multipart form-data, so
	// no token-bearing URL is constructed for the file — the token only ever rides
	// the (header/path) API call the lib makes internally, never logged here.
	_, err := c.b.SendDocument(ctx, &bot.SendDocumentParams{
		ChatID:   chatID,
		Document: &models.InputFileUpload{Filename: name, Data: data},
	})
	return err
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
