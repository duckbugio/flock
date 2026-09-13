package lo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type callbackSpy struct {
	finished     bool
	starAccepted bool
	run          string
	stars        int
}

func (s *callbackSpy) Stop(runID string) bool    { s.run = runID; return !s.finished }
func (s *callbackSpy) StarPress() (string, bool) { s.stars++; return "Done", s.starAccepted }

func TestCallbackAdmissionProtectsRunControl(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"valid", "disallowed", "foreign-bot", "foreign-chat", "restarted",
		"inaccessible", "bot-user", "disabled", groupChatType, "positive-group",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var answer string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Text string `json:"text"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				answer = body.Text
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
			}))
			defer server.Close()
			api, err := NewClient(server.URL, "123:secret", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			transport := NewTransport(api, false).WithKeyboards(true)
			service := &callbackSpy{}
			receiver := NewReceiver(ReceiverConfig{
				Client: api, Transport: transport, Callbacks: service,
				BotID: 1000000000000001, IsAllowed: func(id int64) bool { return id == 77 },
			})
			msg := &Message{ID: 9, From: &User{ID: 1000000000000001, IsBot: true}}
			msg.Chat.ID = 77
			msg.Chat.Type = "private"
			query := &CallbackQuery{ID: "query", From: &User{ID: 77}, Message: msg, Data: transport.stopButtonData("77", "1")}
			switch name {
			case "disallowed":
				query.From.ID = 88
			case "foreign-bot":
				msg.From.ID++
			case "foreign-chat":
				msg.Chat.ID = 88
			case "restarted":
				query.Data = NewTransport(nil, false).stopButtonData("77", "1")
			case "inaccessible":
				query.Message = nil
			case "bot-user":
				query.From.IsBot = true
			case "disabled":
				transport.WithKeyboards(false)
			case groupChatType:
				msg.Chat.Type = supergroupChatType
				msg.Chat.ID = -99
				query.Data = transport.stopButtonData("-99", "1")
			case "positive-group":
				msg.Chat.Type = groupChatType
			}
			receiver.HandleUpdate(t.Context(), Update{ID: 1, CallbackQuery: query})
			if name == "valid" || name == groupChatType {
				if service.run != "1" || answer != "Stopping…" {
					t.Fatalf("run=%q answer=%q", service.run, answer)
				}
			} else if service.run != "" || answer != callbackUnavailable {
				t.Fatalf("unauthorized action: run=%q answer=%q", service.run, answer)
			}
			if service.stars != 0 {
				t.Fatal("stop callback triggered account action")
			}
		})
	}
}
