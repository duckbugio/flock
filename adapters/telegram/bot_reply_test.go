//nolint:testpackage // whitebox: tests the unexported SendReply rendering/reply-parameter wiring
package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/go-telegram/bot"
)

// captureBotServer builds a *bot.Bot pointed at a fake Bot API that records the
// last sendMessage request's form fields and always succeeds, so a test can assert
// the reply parameters and rendered text the adapter sent. The go-telegram lib
// encodes sendMessage as multipart form-data; reply_parameters rides as a single
// JSON-encoded form field.
func captureBotServer(t *testing.T, last *url.Values) *bot.Bot {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/bot123:ABC/sendMessage", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, testBodyLimit)
		if err := r.ParseMultipartForm(testBodyLimit); err != nil {
			t.Errorf("parse multipart form: %v", err)
		}
		*last = r.Form
		if err := json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": 7, "chat": map[string]any{"id": 1}, "date": 0},
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	b, err := bot.New("123:ABC", bot.WithSkipGetMe(), bot.WithServerURL(srv.URL))
	if err != nil {
		t.Fatalf("bot.New: %v", err)
	}
	return b
}

// testBodyLimit bounds the parsed multipart form so the fake server stays within
// gosec's request-size guard.
const testBodyLimit = 4 << 20 // 4 MiB

// replyParameters is the subset of the Bot API ReplyParameters the test inspects.
type replyParameters struct {
	MessageID                int  `json:"message_id"`                  //nolint:tagliatelle // Bot API uses snake_case.
	AllowSendingWithoutReply bool `json:"allow_sending_without_reply"` //nolint:tagliatelle // Bot API uses snake_case.
}

// TestSendReplySetsReplyParameters (AC4) asserts SendReply targets the given
// message id with AllowSendingWithoutReply set, and reuses the markdown→HTML
// rendering shared with Send.
func TestSendReplySetsReplyParameters(t *testing.T) {
	var last url.Values
	b := captureBotServer(t, &last)
	c := &botChat{b: b} // rich off → legacy HTML path, which SendReply always uses

	id, err := c.SendReply(context.Background(), "555", "42", "*done*", true)
	if err != nil {
		t.Fatalf("SendReply: %v", err)
	}
	if id != "7" {
		t.Errorf("id = %q, want 7", id)
	}
	raw := last.Get("reply_parameters")
	if raw == "" {
		t.Fatalf("no reply_parameters in request form: %v", last)
	}
	var rp replyParameters
	if err := json.Unmarshal([]byte(raw), &rp); err != nil {
		t.Fatalf("decode reply_parameters %q: %v", raw, err)
	}
	if rp.MessageID != 42 {
		t.Errorf("reply message_id = %d, want 42", rp.MessageID)
	}
	if !rp.AllowSendingWithoutReply {
		t.Error("allow_sending_without_reply = false, want true (deleted target must still deliver)")
	}
	// The markdown was rendered to HTML via the shared Send path (parse_mode HTML).
	if pm := last.Get("parse_mode"); pm != "HTML" {
		t.Errorf("parse_mode = %q, want HTML (shared markdown rendering)", pm)
	}
}
