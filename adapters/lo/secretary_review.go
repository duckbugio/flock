package lo

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

// Secretary callback wire constants are shared with the approval manager.
const (
	SecretaryCallbackPrefix    = "lo-secretary:"
	SecretaryActionSend        = "send"
	SecretaryActionDiscard     = "discard"
	SecretaryReviewTokenLength = 26
)

// ValidSecretaryReviewToken checks the bounded callback token wire alphabet.
func ValidSecretaryReviewToken(token string) bool {
	if len(token) != SecretaryReviewTokenLength {
		return false
	}
	for _, character := range token {
		if character < 'a' || character > 'z' {
			if character < 'A' || character > 'Z' {
				if character < '0' || character > '9' {
					return false
				}
			}
		}
	}
	return true
}

// SendSecretaryReviewContext posts source context without approval controls.
func (c *Client) SendSecretaryReviewContext(ctx context.Context, ownerID, botID int64, incoming string) (int64, error) {
	if ownerID <= 0 || botID < minSecretaryBotID || strings.TrimSpace(incoming) == "" || messageText(incoming) != nil {
		return 0, errors.New("invalid secretary review context")
	}
	return c.secretaryOwnerMessage(ctx, "sendMessage", map[string]any{"chat_id": ownerID, "text": incoming},
		ownerID, botID, incoming)
}

// SendSecretaryReviewNotice previews the exact reply in the owner's ordinary bot
// dialog. The buttons are owned by Flock, never a native LO draft approval API.
func (c *Client) SendSecretaryReviewNotice(ctx context.Context, ownerID, botID, peerID int64,
	text, token string,
) (int64, error) {
	if ownerID <= 0 || botID < minSecretaryBotID || peerID <= 0 || !ValidSecretaryReviewToken(token) ||
		messageText(text) != nil {
		return 0, errors.New("invalid secretary review notice")
	}
	body := map[string]any{
		"chat_id": ownerID, "text": text,
		"reply_markup": &inlineKeyboard{Rows: [][]inlineButton{{
			{Text: "Send to chat " + strconv.FormatInt(peerID, 10), Data: SecretaryCallbackPrefix + SecretaryActionSend + ":" + token},
			{Text: "Discard", Data: SecretaryCallbackPrefix + SecretaryActionDiscard + ":" + token},
		}}},
	}
	return c.secretaryOwnerMessage(ctx, "sendMessage", body, ownerID, botID, text)
}

// CloseSecretaryReviewNotice replaces consumed approval buttons with a receipt.
func (c *Client) CloseSecretaryReviewNotice(ctx context.Context, ownerID, botID, noticeID int64, status string) error {
	if ownerID <= 0 || botID < minSecretaryBotID || noticeID <= 0 || messageText(status) != nil {
		return errors.New("invalid secretary review receipt")
	}
	id, err := c.secretaryOwnerMessage(ctx, editTextMethod, map[string]any{
		"chat_id": ownerID, "message_id": noticeID, "text": status,
		"reply_markup": &inlineKeyboard{Rows: [][]inlineButton{}},
	}, ownerID, botID, status)
	if err == nil && id != noticeID {
		return errors.New("invalid secretary review receipt identity")
	}
	return err
}

func (c *Client) secretaryOwnerMessage(ctx context.Context, method string, body map[string]any,
	ownerID, botID int64, text string,
) (int64, error) {
	var result Message
	if err := c.call(ctx, method, body, &result); err != nil {
		return 0, err
	}
	if result.ID <= 0 || result.Chat.ID != ownerID || result.Chat.Type != privateChatType || result.Text != text ||
		result.From == nil || !result.From.IsBot || result.From.ID != botID {
		return 0, errors.New("invalid secretary review notice response")
	}
	return result.ID, nil
}
