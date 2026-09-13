package lo

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
)

const (
	editTextMethod     = "editMessageText"
	groupChatType      = "group"
	supergroupChatType = "supergroup"
)

// CallbackQuery identifies the user pressing a button on a bot-owned message.
// Inaccessible or inline-only messages are retained as nil and rejected by admission.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    *User    `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

// AnswerCallbackQuery dismisses the client's progress indicator with a short toast.
func (c *Client) AnswerCallbackQuery(ctx context.Context, queryID, text string) error {
	if strings.TrimSpace(queryID) == "" || !utf8.ValidString(queryID) {
		return errors.New("invalid LO callback query ID")
	}
	if !utf8.ValidString(text) || utf8.RuneCountInString(text) > 200 {
		return errors.New("LO callback answer exceeds 200 characters or is invalid UTF-8")
	}
	var result bool
	body := map[string]any{"callback_query_id": queryID, "text": text}
	if err := c.call(ctx, "answerCallbackQuery", body, &result); err != nil {
		return err
	}
	if !result {
		return errors.New("LO did not confirm callback answer")
	}
	return nil
}

// inlineKeyboard uses the public Telegram-compatible Bot API field names.
type inlineKeyboard struct {
	Rows [][]inlineButton `json:"inline_keyboard"` //nolint:tagliatelle // Bot API wire spelling.
}
type inlineButton struct {
	Text string `json:"text"`
	Data string `json:"callback_data"` //nolint:tagliatelle // Bot API wire spelling.
}

func buttonKeyboard(text, data string) *inlineKeyboard {
	return &inlineKeyboard{Rows: [][]inlineButton{{{Text: text, Data: data}}}}
}
