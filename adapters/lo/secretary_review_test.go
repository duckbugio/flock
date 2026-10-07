package lo_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestSecretaryReviewNoticeUsesOwnerDialogAndExactText(t *testing.T) {
	t.Parallel()
	token := strings.Repeat("a", 26)
	text := "Exact **unformatted** reply."
	incoming := "From @sender (LO #77)\nIncoming: Question"
	requests := 0
	api := client(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			t.Error("native draft API used")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		requests++
		if requests == 1 {
			if len(body) != 2 || body["chat_id"] != float64(1) || body["text"] != incoming {
				t.Errorf("wrong incoming context: %+v", body)
			}
			result, _ := json.Marshal(map[string]any{"ok": true, "result": map[string]any{
				"message_id": 98, "chat": map[string]any{"id": 1, "type": "private"},
				"from": map[string]any{"id": 1000000000000001, "is_bot": true}, "text": incoming,
			}})
			reply(w, string(result))
			return
		}
		if len(body) != 3 || body["chat_id"] != float64(1) || body["text"] != text {
			t.Errorf("wrong owner preview: %+v", body)
		}
		markup := body["reply_markup"].(map[string]any)
		buttons := markup["inline_keyboard"].([]any)[0].([]any)
		send, discard := buttons[0].(map[string]any), buttons[1].(map[string]any)
		if send["text"] != "Send to chat 77" || send["callback_data"] != "lo-secretary:send:"+token ||
			discard["callback_data"] != "lo-secretary:discard:"+token {
			t.Error("wrong review buttons")
		}
		reply(w, `{"ok":true,"result":{"message_id":99,"chat":{"id":1,"type":"private"},"from":{"id":1000000000000001,`+
			`"is_bot":true},"text":"Exact **unformatted** reply."}}`)
	})
	contextID, err := api.SendSecretaryReviewContext(t.Context(), 1, 1000000000000001, incoming)
	if err != nil || contextID != 98 {
		t.Fatalf("context = %d, %v", contextID, err)
	}
	id, err := api.SendSecretaryReviewNotice(t.Context(), 1, 1000000000000001, 77, text, token)
	if err != nil || id != 99 {
		t.Fatalf("notice = %d, %v", id, err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want context then exact reply", requests)
	}
}

func TestSecretaryReviewNoticeRejectsForeignResponseIdentity(t *testing.T) {
	t.Parallel()
	original := `{"ok":true,"result":{"message_id":99,"chat":{"id":1,"type":"private"},"from":{"id":1000000000000001,` +
		`"is_bot":true},"text":"reply"}}`
	for _, fixture := range []string{
		strings.Replace(original, `"message_id":99`, `"message_id":0`, 1),
		strings.Replace(original, `"id":1,`, `"id":2,`, 1),
		strings.Replace(original, `"private"`, `"group"`, 1),
		strings.Replace(original, `1000000000000001`, `1000000000000002`, 1),
		strings.Replace(original, `"is_bot":true`, `"is_bot":false`, 1),
		strings.Replace(original, `"text":"reply"`, `"text":"changed"`, 1),
	} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()
			api := client(t, func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["text"] == "Context" {
					reply(w, strings.Replace(original, `"text":"reply"`, `"text":"Context"`, 1))
					return
				}
				reply(w, fixture)
			})
			if _, err := api.SendSecretaryReviewNotice(t.Context(), 1, 1000000000000001, 77, "reply",
				strings.Repeat("a", 26)); err == nil {
				t.Fatal("foreign preview accepted")
			}
		})
	}
}

func TestSecretaryReviewContextFailureHasNoApprovalControls(t *testing.T) {
	requests := 0
	api := client(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["reply_markup"] != nil {
			t.Error("approval notice sent without context")
		}
		reply(w, `{"ok":false,"error_code":503,"description":"unavailable"}`)
	})
	_, err := api.SendSecretaryReviewContext(t.Context(), 1, 1000000000000001, "Context")
	if err == nil || requests != 1 {
		t.Fatalf("context failure: requests=%d error=%v", requests, err)
	}
}

func TestSecretaryReviewReceiptAlwaysClearsButtons(t *testing.T) {
	api := client(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/editMessageText") {
			t.Error("receipt is not an edit")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		markup := body["reply_markup"].(map[string]any)
		if len(markup["inline_keyboard"].([]any)) != 0 || body["message_id"] != float64(99) {
			t.Error("consumed buttons retained")
		}
		reply(w, `{"ok":true,"result":{"message_id":99,"chat":{"id":1,"type":"private"},`+
			`"from":{"id":1000000000000001,"is_bot":true},"text":"Sent."}}`)
	})
	if err := api.CloseSecretaryReviewNotice(t.Context(), 1, 1000000000000001, 99, "Sent."); err != nil {
		t.Fatal(err)
	}
}
