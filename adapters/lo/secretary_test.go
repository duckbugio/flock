package lo_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/duckbugio/flock/adapters/lo"
)

const (
	secretaryConnectionID = "15eac687-9383-4da1-9644-826693923e44" //nolint:gosec // Public UUID fixture, not a credential.
	secretaryContextJSON  = `{"conversation_id":"19","chat_id":"77","policy_version":"922337203685477` +
		`5807","source_message_id":"9223372036854775806","source_revision":"1"}`
	secretaryMessageJSON = `{"business_connection_id":"` + secretaryConnectionID +
		`","message_id":"9223372036854775806","from":{"id":77,"is_bot":false},"ch` +
		`at":{"id":77,"type":"private"},"date":1,"text":"hello","lo_event_id":"na` + `tive-event","lo_context":` +
		secretaryContextJSON + `}`
)

func secretaryScope() lo.SecretaryContext {
	return lo.SecretaryContext{
		ConversationID: 19, ChatID: 77, PolicyVersion: 9223372036854775807,
		SourceMessageID: 9223372036854775806, SourceRevision: 1,
	}
}

func TestSecretaryPollingDecodesNativeEventsWithoutLosingInt64(t *testing.T) {
	t.Parallel()
	api := client(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Allowed []string `json:"allowed_updates"` //nolint:tagliatelle // Native Bot API wire spelling.
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body.Allowed) != 6 {
			t.Errorf("allowed updates = %v", body.Allowed)
		}
		reply(w, `{"ok":true,"result":[{"update_id":"4","business_message":`+secretaryMessageJSON+`}]}`)
	}).WithSecretary(true)
	updates, err := api.GetUpdates(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || !updates[0].Delegated || updates[0].BusinessMessage == nil {
		t.Fatalf("updates: %+v", updates)
	}
	if updates[0].BusinessMessage.Context != secretaryScope() || updates[0].Message != nil {
		t.Fatal("native context was lost or routed as ordinary message")
	}
}

func TestSecretaryRejectsAmbiguousOrInvalidUpdatesWithoutBotFallback(t *testing.T) {
	t.Parallel()
	fixtures := []string{
		`{"business_message":` + secretaryMessageJSON + `,"message":{"message_id":9,"text":"/stop"}}`,
		`{"business_message":` + strings.Replace(secretaryMessageJSON, `"type":"private"`, `"type":"group"`, 1) + `}`,
		`{"business_message":` + strings.Replace(secretaryMessageJSON, `"source_revision":"1"`, `"source_revision":1.5`, 1) + `}`,
		`{"business_connection":{"id":"` + secretaryConnectionID +
			`","user":{"id":1},"date":1,"is_enabled":true,"can_reply":true}}`,
		`{"business_message":null,"message":{"message_id":9,"text":"/stop"}}`,
	}
	for i, fixture := range fixtures {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()
			api := client(t, func(w http.ResponseWriter, _ *http.Request) {
				reply(w, `{"ok":true,"result":[{"update_id":1,`+fixture[1:]+`]}`)
			})
			updates, err := api.GetUpdates(t.Context(), 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(updates) != 1 || !updates[0].Delegated || updates[0].BusinessMessage != nil ||
				updates[0].BusinessConnection != nil || updates[0].Message != nil {
				t.Fatalf("malformed update admitted: %+v", updates)
			}
		})
	}
}

func TestSecretaryDelegatedWritesUseExactContextAndIdempotency(t *testing.T) {
	t.Parallel()
	for _, draft := range []bool{false, true} {
		t.Run(strconv.FormatBool(draft), func(t *testing.T) {
			t.Parallel()
			action := lo.SecretaryAction{
				ConnectionID: secretaryConnectionID, RequestID: "flock:durable-key",
				Context: secretaryScope(), ChatID: 77, Text: "reply",
			}
			if draft {
				action.Reason = "manual_review"
			}
			api := client(t, func(w http.ResponseWriter, r *http.Request) {
				var received lo.SecretaryAction
				if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
					t.Error(err)
				}
				if received != action {
					t.Errorf("wrong delegated body: %+v", received)
				}
				if draft {
					if !strings.HasSuffix(r.URL.Path, "/proposeBusinessDraft") {
						t.Error("wrong method")
					}
					reply(w, `{"ok":true,"result":{"lo_draft_id":"69417813-99f5-4dc5-a6a8-4c45151b1e5c`+
						`","business_connection_id":"`+secretaryConnectionID+
						`","lo_conversation_id":19,"chat_id":77,"lo_source_message_id":"922337203`+
						`6854775806","text":"reply","state":"draft","mode":"review","lo_revision"`+
						`:1,"lo_secretary_bot_id":1000000000000001,"reason":"manual_review","date`+`":1,"expires_at":2}}`)
				} else {
					if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
						t.Error("wrong method")
					}
					reply(w, `{"ok":true,"result":{"message_id":2,"business_connection_id":"`+secretaryConnectionID+
						`","chat":{"id":77,"type":"private"},"from":{"id":1,"is_bot":false},"lo_s`+
						`ecretary_bot_id":1000000000000001,"text":"reply"}}`)
				}
			})
			if draft {
				if _, err := api.ProposeBusinessDraft(t.Context(), action); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := api.SendSecretaryText(t.Context(), action, 1); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestSecretaryConsentRequiresIndependentRights(t *testing.T) {
	t.Parallel()
	conn := lo.SecretaryConnection{
		ID: secretaryConnectionID, User: lo.User{ID: 1}, Enabled: true, SchemaVersion: 1,
		PolicyVersion: 9223372036854775807, Date: 1,
	}
	for _, rights := range [][]string{nil, {"send_messages"}, {"receive_messages"}, {
		"receive_messages",
		"receive_messages", "send_messages",
	}, {"can_reply"}} {
		conn.Rights = rights
		if conn.CanReply(secretaryScope()) {
			t.Fatalf("wrong rights admitted: %v", rights)
		}
	}
	conn.Rights = []string{"receive_messages", "send_messages"}
	if !conn.CanReply(secretaryScope()) {
		t.Fatal("native rights rejected")
	}
	conn.PolicyVersion--
	if conn.CanReply(secretaryScope()) {
		t.Fatal("stale policy admitted")
	}
}

func TestSecretaryPollingDoesNotAcknowledgeFailedPersistence(t *testing.T) {
	t.Parallel()
	calls := 0
	api := client(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		reply(w, `{"ok":true,"result":[{"update_id":1,"business_message":`+secretaryMessageJSON+`}]}`)
	})
	failure := errors.New("disk unavailable")
	receiver := lo.NewReceiver(lo.ReceiverConfig{Client: api, Secretary: func(_ context.Context, update lo.Update) error {
		if update.BusinessMessage == nil {
			t.Error("native update not routed")
		}
		return failure
	}})
	if err := receiver.Run(t.Context()); !errors.Is(err, failure) {
		t.Fatalf("poll did not stop on persistence error: %v", err)
	}
	if calls != 1 {
		t.Fatal("poll acknowledged a nonpersisted update")
	}
}

func TestSecretaryVoiceUsesNestedNativeAttachment(t *testing.T) {
	t.Parallel()
	wire := strings.TrimSuffix(secretaryMessageJSON, "}") +
		`,"lo_media_status":"available","lo_attachments":[{"kind":"voice","voice"` +
		`:{"file_id":"secretary-v1:payload:abcdefghijklmnopqrstuv","file_unique_i` + `d":"secretary:hash","duration":3}}]}`
	api := client(t, func(w http.ResponseWriter, _ *http.Request) {
		reply(w, `{"ok":true,"result":[{"update_id":1,"business_message":`+wire+`}]}`)
	})
	updates, err := api.GetUpdates(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0].BusinessMessage == nil {
		t.Fatal("voice update lost")
	}
	attachments := updates[0].BusinessMessage.Attachments
	if len(attachments) != 1 || attachments[0].Voice == nil ||
		attachments[0].Voice.FileID != "secretary-v1:payload:abcdefghijklmnopqrstuv" {
		t.Fatal("native voice reference lost")
	}
}
