//nolint:tagliatelle // LO Bot API uses snake_case wire keys, matching the native SDK 0.2 transport.
package lo

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Native SDK 0.2 bounds (human and bot namespaces differ):
// https://github.com/lo-ink/lo-platform-adapters/tree/3cd0095a27467ed4f494b329158a365c44951daa
// See packages/bot-http-lo/src/secretary.ts and the generated fixtures in testdata/.
const (
	maxSecretaryConversationID = 2147483647
	maxSecretaryUserID         = 999999999999999
	minSecretaryBotID          = 1000000000000000
	maxSecretarySafeInteger    = 9007199254740991
)

// SecretaryNumber decodes numeric or string int64 IDs without floating-point conversion.
type SecretaryNumber int64

// UnmarshalJSON accepts canonical nonnegative integers; native scope validation requires positive IDs.
func (n *SecretaryNumber) UnmarshalJSON(data []byte) error {
	text := string(data)
	if strings.HasPrefix(text, `"`) {
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value < 0 || strconv.FormatInt(value, 10) != text {
		return errors.New("invalid LO secretary integer")
	}
	*n = SecretaryNumber(value)
	return nil
}

// MarshalJSON keeps int64 IDs lossless across JSON gateways and the SDK 0.2 transport.
func (n SecretaryNumber) MarshalJSON() ([]byte, error) { return json.Marshal(n.String()) }

// String returns a lossless decimal ID.
func (n SecretaryNumber) String() string { return strconv.FormatInt(int64(n), 10) }

// SecretaryContext is the exact delegated source context, never an inferred chat history.
type SecretaryContext struct {
	ConversationID  SecretaryNumber `json:"conversation_id"`
	ChatID          SecretaryNumber `json:"chat_id"`
	PolicyVersion   SecretaryNumber `json:"policy_version"`
	SourceMessageID SecretaryNumber `json:"source_message_id"`
	SourceRevision  SecretaryNumber `json:"source_revision"`
}

// Valid checks the native SDK's ID bounds.
func (c SecretaryContext) Valid() bool {
	return c.ConversationID > 0 && c.ConversationID <= maxSecretaryConversationID &&
		c.ChatID > 0 && c.ChatID <= maxSecretaryUserID && c.PolicyVersion > 0 &&
		c.SourceMessageID > 0 && c.SourceRevision > 0
}

// SecretaryConnection contains only LO's independent rights; Telegram can_reply is not used.
type SecretaryConnection struct {
	ID            string          `json:"id"`
	User          User            `json:"user"`
	Enabled       bool            `json:"is_enabled"`
	SchemaVersion int             `json:"lo_schema_version"`
	PolicyVersion SecretaryNumber `json:"lo_policy_version"`
	Rights        []string        `json:"lo_rights"`
	Date          int64           `json:"date"`
}

// UnmarshalJSON requires the explicit native consent and human-owner flags.
func (c *SecretaryConnection) UnmarshalJSON(data []byte) error {
	type wire SecretaryConnection
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var flags struct {
		Enabled *bool `json:"is_enabled"`
		User    struct {
			IsBot *bool `json:"is_bot"`
		} `json:"user"`
	}
	if err := json.Unmarshal(data, &flags); err != nil {
		return err
	}
	if flags.Enabled == nil || flags.User.IsBot == nil || *flags.User.IsBot {
		return errors.New("invalid native LO consent flags")
	}
	*c = SecretaryConnection(decoded)
	return nil
}

var secretaryUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidSecretaryConnectionID rejects malformed and nil UUIDs.
func ValidSecretaryConnectionID(id string) bool {
	return secretaryUUID.MatchString(id) && id != "00000000-0000-0000-0000-000000000000"
}

// Valid rejects Telegram-shaped connections without the native LO consent contract.
func (c SecretaryConnection) Valid() bool {
	if !ValidSecretaryConnectionID(c.ID) || c.SchemaVersion != 1 || c.PolicyVersion <= 0 ||
		c.User.ID <= 0 || c.User.ID > maxSecretaryUserID || c.User.IsBot || c.Date <= 0 || c.Rights == nil {
		return false
	}
	seen := make(map[string]bool, len(c.Rights))
	for _, right := range c.Rights {
		if seen[right] || !slices.Contains([]string{
			"receive_messages", "send_messages", "mark_read", "edit_sent", "delete_sent", "delete_all",
		}, right) {
			return false
		}
		seen[right] = true
	}
	return true
}

// CanReply requires separately granted receive and send rights at this policy version.
func (c SecretaryConnection) CanReply(scope SecretaryContext) bool {
	return c.Valid() && c.Enabled && scope.Valid() && c.PolicyVersion == scope.PolicyVersion &&
		slices.Contains(c.Rights, "receive_messages") && slices.Contains(c.Rights, "send_messages")
}

// SecretaryAttachment is a delegated file reference, distinct from ordinary bot file IDs.
type SecretaryAttachment struct {
	Kind  string      `json:"kind"`
	Voice *Attachment `json:"voice"`
}

// SecretaryMessage carries the LO native metadata exposed through Business wire updates.
type SecretaryMessage struct {
	ID   SecretaryNumber `json:"message_id"`
	From User            `json:"from"`
	Chat struct {
		ID   SecretaryNumber `json:"id"`
		Type string          `json:"type"`
	} `json:"chat"`
	Date         int64                 `json:"date"`
	Text         string                `json:"text"`
	Caption      string                `json:"caption"`
	ConnectionID string                `json:"business_connection_id"`
	EventID      string                `json:"lo_event_id"`
	Context      SecretaryContext      `json:"lo_context"`
	BotID        SecretaryNumber       `json:"lo_secretary_bot_id"`
	Attachments  []SecretaryAttachment `json:"lo_attachments"`
	MediaStatus  string                `json:"lo_media_status"`
}

// UnmarshalJSON requires an explicit human sender, including delegated send receipts.
func (m *SecretaryMessage) UnmarshalJSON(data []byte) error {
	type wire SecretaryMessage
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var flags struct {
		From struct {
			IsBot *bool `json:"is_bot"`
		} `json:"from"`
	}
	if err := json.Unmarshal(data, &flags); err != nil {
		return err
	}
	if flags.From.IsBot == nil || *flags.From.IsBot {
		return errors.New("invalid native LO sender")
	}
	*m = SecretaryMessage(decoded)
	return nil
}

// Valid checks that routing and source identity agree with the trusted event context.
func (m SecretaryMessage) Valid() bool {
	return ValidSecretaryConnectionID(m.ConnectionID) && m.Context.Valid() && m.ID == m.Context.SourceMessageID &&
		m.Chat.ID == m.Context.ChatID && m.Chat.Type == privateChatType && m.From.ID > 0 &&
		m.From.ID <= maxSecretaryUserID && !m.From.IsBot && m.Date > 0 &&
		m.EventID != "" && len(m.EventID) <= 512 &&
		(m.BotID == 0 || (m.BotID >= minSecretaryBotID && m.BotID <= maxSecretarySafeInteger)) &&
		((m.MediaStatus == "" && m.Attachments == nil) ||
			(m.MediaStatus == "available" && len(m.Attachments) > 0 && len(m.Attachments) <= 10) ||
			((m.MediaStatus == "unavailable" || m.MediaStatus == "unsupported") && m.Attachments == nil))
}

// SecretaryDeletion invalidates the affected sources, not the whole account's messages.
type SecretaryDeletion struct {
	ConnectionID string            `json:"business_connection_id"`
	Context      SecretaryContext  `json:"lo_context"`
	MessageIDs   []SecretaryNumber `json:"message_ids"`
	EventID      string            `json:"lo_event_id"`
	Chat         struct {
		ID   SecretaryNumber `json:"id"`
		Type string          `json:"type"`
	} `json:"chat"`
}

// SecretaryAction preserves the source and a durable idempotency key on every delegated write.
type SecretaryAction struct {
	ConnectionID string           `json:"business_connection_id"`
	RequestID    string           `json:"lo_request_id"`
	Context      SecretaryContext `json:"lo_context"`
	ChatID       SecretaryNumber  `json:"chat_id"`
	Text         string           `json:"text"`
	Reason       string           `json:"lo_draft_reason,omitempty"`
}

// SecretaryDraft is reviewed by the account owner in LO, never by third-party callback buttons.
type SecretaryDraft struct {
	ID              string           `json:"lo_draft_id"`
	ConnectionID    string           `json:"business_connection_id"`
	ConversationID  SecretaryNumber  `json:"lo_conversation_id"`
	ChatID          SecretaryNumber  `json:"chat_id"`
	SourceMessageID SecretaryNumber  `json:"lo_source_message_id"`
	Text            string           `json:"text"`
	State           string           `json:"state"`
	Mode            string           `json:"mode"`
	Revision        SecretaryNumber  `json:"lo_revision"`
	BotID           SecretaryNumber  `json:"lo_secretary_bot_id"`
	Reason          string           `json:"reason"`
	Date            int64            `json:"date"`
	ExpiresAt       int64            `json:"expires_at"`
	MessageID       *SecretaryNumber `json:"message_id,omitempty"`
}

// WithSecretary enables native event subscription before polling starts.
func (c *Client) WithSecretary(enabled bool) *Client { c.secretaryEnabled = enabled; return c }

// GetBusinessConnection reads the native LO consent snapshot.
func (c *Client) GetBusinessConnection(ctx context.Context, id string) (SecretaryConnection, error) {
	var result SecretaryConnection
	if !ValidSecretaryConnectionID(id) {
		return result, errors.New("invalid LO secretary connection ID")
	}
	if err := c.call(ctx, "getBusinessConnection", map[string]string{"business_connection_id": id}, &result); err != nil {
		return result, err
	}
	if !result.Valid() || result.ID != id {
		return SecretaryConnection{}, errors.New("invalid LO secretary connection response")
	}
	return result, nil
}

// Valid checks the native delegated write contract.
func (action SecretaryAction) Valid() bool {
	return ValidSecretaryConnectionID(action.ConnectionID) && action.Context.Valid() && action.ChatID == action.Context.ChatID &&
		len(action.RequestID) >= 8 && len(action.RequestID) <= 128 &&
		strings.IndexFunc(action.RequestID, func(r rune) bool { return r < 33 || r > 126 }) == -1 &&
		messageText(action.Text) == nil
}

// ProposeBusinessDraft uses native server-side owner review; Flock cannot approve the draft.
func (c *Client) ProposeBusinessDraft(ctx context.Context, action SecretaryAction) (SecretaryDraft, error) {
	var result SecretaryDraft
	if !action.Valid() || action.Reason != "manual_review" {
		return result, errors.New("invalid LO secretary draft")
	}
	if err := c.call(ctx, "proposeBusinessDraft", action, &result); err != nil {
		return result, err
	}
	if !ValidSecretaryConnectionID(result.ID) || result.ConnectionID != action.ConnectionID ||
		result.ConversationID != action.Context.ConversationID || result.ChatID != action.ChatID ||
		result.SourceMessageID != action.Context.SourceMessageID || result.Revision <= 0 || result.BotID < minSecretaryBotID ||
		result.BotID > maxSecretarySafeInteger || result.Date <= 0 || result.ExpiresAt <= result.Date ||
		(result.State == "sent") != (result.MessageID != nil) ||
		!slices.Contains([]string{
			"manual_review", "owner_cancelled", "policy_changed", "source_changed",
			"manual_takeover", "superseded",
		}, result.Reason) || result.Text != action.Text || result.Mode != "review" ||
		!slices.Contains([]string{"draft", "approved", "sent", "cancelled", "expired"}, result.State) {
		return SecretaryDraft{}, errors.New("invalid LO secretary draft response")
	}
	return result, nil
}

// SendSecretaryText sends one idempotent message from the human account, not a normal bot chat reply.
func (c *Client) SendSecretaryText(ctx context.Context, action SecretaryAction, ownerID int64) error {
	if !action.Valid() || action.Reason != "" {
		return errors.New("invalid LO secretary send")
	}
	var result SecretaryMessage
	if err := c.call(ctx, "sendMessage", action, &result); err != nil {
		return err
	}
	if result.ID <= 0 || result.ConnectionID != action.ConnectionID || result.Chat.ID != action.ChatID ||
		result.Chat.Type != privateChatType || result.From.ID != ownerID || result.From.IsBot ||
		result.BotID < minSecretaryBotID || result.BotID > maxSecretarySafeInteger || result.Text != action.Text {
		return errors.New("invalid LO secretary send response")
	}
	return nil
}

func decodeSecretaryUpdate(raw json.RawMessage, update *Update) error {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return err
	}
	for _, key := range []string{"message", "edited_message", "callback_query", "inline_query"} {
		if _, exists := keys[key]; exists {
			return errors.New("ambiguous LO secretary update")
		}
	}
	kindCount := 0
	for _, key := range []string{"business_connection", "business_message", "edited_business_message", "deleted_business_messages"} {
		if _, exists := keys[key]; exists {
			kindCount++
		}
	}
	if kindCount != 1 {
		return errors.New("ambiguous LO secretary update kinds")
	}
	var fields struct {
		Connection *SecretaryConnection `json:"business_connection"`
		Message    *SecretaryMessage    `json:"business_message"`
		Edited     *SecretaryMessage    `json:"edited_business_message"`
		Deleted    *SecretaryDeletion   `json:"deleted_business_messages"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	count := 0
	for _, present := range []bool{fields.Connection != nil, fields.Message != nil, fields.Edited != nil, fields.Deleted != nil} {
		if present {
			count++
		}
	}
	if count != 1 || update.Message != nil || update.CallbackQuery != nil {
		return errors.New("ambiguous LO secretary update")
	}
	if fields.Connection != nil && !fields.Connection.Valid() {
		return errors.New("invalid LO secretary update connection")
	}
	if fields.Message != nil && !fields.Message.Valid() {
		return errors.New("invalid LO secretary update message")
	}
	if fields.Edited != nil && !fields.Edited.Valid() {
		return errors.New("invalid LO secretary update edit")
	}
	if fields.Deleted != nil && !fields.Deleted.Valid() {
		return errors.New("invalid LO secretary deletion")
	}
	update.BusinessConnection, update.BusinessMessage = fields.Connection, fields.Message
	update.EditedBusinessMessage, update.DeletedBusinessMessages = fields.Edited, fields.Deleted
	return nil
}

// Valid checks the native deletion context and unique source IDs.
func (deletion SecretaryDeletion) Valid() bool {
	if !ValidSecretaryConnectionID(deletion.ConnectionID) || !deletion.Context.Valid() ||
		deletion.Chat.Type != privateChatType ||
		deletion.Chat.ID != deletion.Context.ChatID || deletion.EventID == "" || len(deletion.EventID) > 512 ||
		len(deletion.MessageIDs) == 0 || len(deletion.MessageIDs) > 100 ||
		!slices.Contains(deletion.MessageIDs, deletion.Context.SourceMessageID) {
		return false
	}
	seen := make(map[SecretaryNumber]bool)
	for _, id := range deletion.MessageIDs {
		if seen[id] || id <= 0 {
			return false
		}
		seen[id] = true
	}
	return true
}
