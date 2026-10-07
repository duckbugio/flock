package lo

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

// SendSecretaryReviewNotice previews the exact reply in the owner's ordinary bot
// dialog. The buttons are owned by Flock, never a native LO draft approval API.
func (c *Client) SendSecretaryReviewNotice(ctx context.Context, ownerID, botID, peerID int64, text, token string) (int64, error) {
	if ownerID <= 0 || botID < minSecretaryBotID || peerID <= 0 || len(token) != 26 ||
		strings.ContainsAny(token, ": \n\r\t") || messageText(text) != nil {
		return 0, errors.New("invalid secretary review notice")
	}
	var result Message
	body := map[string]any{
		"chat_id": ownerID, "text": text,
		"reply_markup": &inlineKeyboard{Rows: [][]inlineButton{{
			{Text: "Send to chat " + strconv.FormatInt(peerID, 10), Data: "lo-secretary:send:" + token},
			{Text: "Discard", Data: "lo-secretary:discard:" + token},
		}}},
	}
	if err := c.call(ctx, "sendMessage", body, &result); err != nil {
		return 0, err
	}
	if result.ID <= 0 || result.Chat.ID != ownerID || result.Chat.Type != privateChatType || result.Text != text ||
		result.From == nil || !result.From.IsBot || result.From.ID != botID {
		return 0, errors.New("invalid secretary review notice response")
	}
	return result.ID, nil
}
