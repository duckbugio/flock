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
	api := client(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			t.Error("native draft API used")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
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
	id, err := api.SendSecretaryReviewNotice(t.Context(), 1, 1000000000000001, 77, text, token)
	if err != nil || id != 99 {
		t.Fatalf("notice = %d, %v", id, err)
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
			api := client(t, func(w http.ResponseWriter, _ *http.Request) { reply(w, fixture) })
			if _, err := api.SendSecretaryReviewNotice(t.Context(), 1, 1000000000000001, 77, "reply",
				strings.Repeat("a", 26)); err == nil {
				t.Fatal("foreign preview accepted")
			}
		})
	}
}
