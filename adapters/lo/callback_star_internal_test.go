package lo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStarCallbackEditsConfirmationOnlyOnSuccess(t *testing.T) {
	t.Parallel()
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "refused", true: "accepted"}[accepted], func(t *testing.T) {
			t.Parallel()
			requests := make(chan string, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
				requests <- method
				w.Header().Set("Content-Type", "application/json")
				if method == editTextMethod {
					var body struct {
						Text   string         `json:"text"`
						Markup inlineKeyboard `json:"reply_markup"` //nolint:tagliatelle // Bot API wire spelling.
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body.Text == "" || body.Markup.Rows == nil || len(body.Markup.Rows) != 0 {
						t.Error("confirmation did not atomically clear button")
					}
					_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":9}}`))
					return
				}
				_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
			}))
			defer server.Close()
			api, err := NewClient(server.URL, "123:secret", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			spy := &callbackSpy{starAccepted: accepted}
			receiver := NewReceiver(ReceiverConfig{
				Client: api, Transport: NewTransport(api, false).WithKeyboards(true),
				Callbacks: spy, BotID: 1000000000000001, IsAllowed: func(id int64) bool { return id == 77 },
			})
			msg := &Message{ID: 9, From: &User{ID: 1000000000000001, IsBot: true}}
			msg.Chat.ID = 77
			msg.Chat.Type = "private"
			receiver.HandleUpdate(t.Context(), Update{ID: 1, CallbackQuery: &CallbackQuery{
				ID: "query", From: &User{ID: 77}, Message: msg, Data: "star:confirm",
			}})
			if spy.stars != 1 || spy.run != "" {
				t.Fatal("wrong service action")
			}
			if accepted && <-requests != editTextMethod {
				t.Fatal("missing confirmation")
			}
			if <-requests != "answerCallbackQuery" || len(requests) != 0 {
				t.Fatal("unexpected callback requests")
			}
		})
	}
}
