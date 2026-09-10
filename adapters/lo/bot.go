package lo

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/duckbugio/flock/core/chat"
)

// ErrUnsupported identifies an operation absent from LO's supported bot surface.
var ErrUnsupported = errors.New("operation is not supported by the LO adapter")

const (
	maxTextUnits    = 4096
	maxUnitsPerRune = 2
	maxBMPRune      = 0xffff
)

// Transport implements text delivery and optional ephemeral progress for LO.
// Markdown is kept as plain source until LO's formatter is released and verified.
type Transport struct {
	api    *Client
	drafts bool
}

// NewTransport enables drafts only when the deployment explicitly advertises them.
func NewTransport(api *Client, drafts bool) *Transport { return &Transport{api: api, drafts: drafts} }

// Capabilities deliberately disables file outbox and rich messages. A half-size
// rune budget guarantees the 4096 UTF-16 unit limit even for all-emoji answers.
func (t *Transport) Capabilities() chat.Capabilities {
	return chat.Capabilities{MaxMessageRunes: maxTextUnits / maxUnitsPerRune, CanSendDraft: t.drafts}
}

func numericID(value string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id == 0 {
		return 0, errors.New("invalid LO identifier")
	}
	return id, nil
}

func messageText(text string) error {
	if !utf8.ValidString(text) || strings.TrimSpace(text) == "" {
		return errors.New("LO text is empty or invalid UTF-8")
	}
	units := 0
	for _, r := range text {
		units++
		if r > maxBMPRune {
			units++
		}
	}
	if units > maxTextUnits {
		return errors.New("LO text exceeds 4096 UTF-16 units")
	}
	return nil
}

// Send posts only fields implemented by LO; /stop replaces the unavailable Stop keyboard.
func (t *Transport) Send(ctx context.Context, chatID, text, _ string, _ bool) (chat.MessageID, error) {
	id, err := numericID(chatID)
	if err != nil {
		return "", err
	}
	if err := messageText(text); err != nil {
		return "", err
	}
	var result Message
	if err := t.api.call(ctx, "sendMessage", map[string]any{"chat_id": id, "text": text}, &result); err != nil {
		return "", err
	}
	if result.ID <= 0 {
		return "", errors.New("LO sendMessage returned no message ID")
	}
	return strconv.FormatInt(result.ID, 10), nil
}

// SendReply degrades to a plain send: LO rejects reply_parameters rather than ignoring them.
func (t *Transport) SendReply(ctx context.Context, chatID, _ chat.MessageID, text string, markdown bool) (chat.MessageID, error) {
	return t.Send(ctx, chatID, text, "", markdown)
}

// Edit updates the persistent anchor in deployments without sendMessageDraft.
func (t *Transport) Edit(ctx context.Context, chatID, messageID, text, _ string, _ bool) error {
	id, err := numericID(chatID)
	if err != nil {
		return err
	}
	msgID, err := numericID(messageID)
	if err != nil || msgID < 0 {
		return errors.New("invalid LO message ID")
	}
	if err := messageText(text); err != nil {
		return err
	}
	var result Message
	body := map[string]any{"chat_id": id, "message_id": msgID, "text": text}
	if err := t.api.call(ctx, "editMessageText", body, &result); err != nil {
		return err
	}
	if result.ID != msgID {
		return errors.New("LO editMessageText returned a different message ID")
	}
	return nil
}

// Delete removes the bot's own message.
func (t *Transport) Delete(ctx context.Context, chatID, messageID string) error {
	id, err := numericID(chatID)
	if err != nil {
		return err
	}
	msgID, err := numericID(messageID)
	if err != nil || msgID < 0 {
		return errors.New("invalid LO message ID")
	}
	var result bool
	if err := t.api.call(ctx, "deleteMessage", map[string]any{"chat_id": id, "message_id": msgID}, &result); err != nil {
		return err
	}
	if !result {
		return errors.New("LO did not confirm message deletion")
	}
	return nil
}

// SendDocument is unavailable; Capabilities prevents the core outbox from invoking it.
func (*Transport) SendDocument(context.Context, chat.ChatID, string, io.Reader) error {
	return ErrUnsupported
}

// SendStarNudge is unavailable because its confirmation callback is not implemented in LO.
func (*Transport) SendStarNudge(context.Context, chat.ChatID, string) (chat.MessageID, error) {
	return "", ErrUnsupported
}

// SendDraft maps one run to a stable, non-zero int64 draft ID. It never returns a
// synthetic message ID; the shared service persists the final text with Send.
func (t *Transport) SendDraft(ctx context.Context, chatID, runID, text string) error {
	if err := messageText(text); err != nil {
		return err
	}
	return t.sendDraft(ctx, chatID, runID, text)
}

// ClearDraft explicitly removes the run's ephemeral progress using LO's empty-text contract.
func (t *Transport) ClearDraft(ctx context.Context, chatID, runID string) error {
	return t.sendDraft(ctx, chatID, runID, "")
}

func (t *Transport) sendDraft(ctx context.Context, chatID, runID, text string) error {
	if !t.drafts {
		return ErrUnsupported
	}
	id, err := numericID(chatID)
	if err != nil {
		return err
	}
	if id < 0 {
		return ErrUnsupported
	} // LO drafts are private-chat only.
	digest := sha256.Sum256([]byte(runID))
	const positiveMask = uint64(1<<63 - 1)
	draftID := binary.BigEndian.Uint64(digest[:8]) & positiveMask
	if draftID == 0 {
		draftID = 1
	}
	var result bool
	body := map[string]any{"chat_id": id, "draft_id": draftID, "text": text}
	if err := t.api.call(ctx, "sendMessageDraft", body, &result); err != nil {
		return err
	}
	if !result {
		return errors.New("LO did not confirm draft delivery")
	}
	return nil
}

var (
	_ chat.Transport      = (*Transport)(nil)
	_ chat.DraftTransport = (*Transport)(nil)
)
