//nolint:testpackage // intentionally whitebox to test unexported VK transport internals
package vk

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/duckbugio/flock/core/chat"
)

func TestCapabilities(t *testing.T) {
	c := NewBotChat(NewClient("tok"), 100, 1)
	caps := c.Capabilities()
	if !caps.CanEditMessages || !caps.CanInlineStop || !caps.CanSendDocument {
		t.Errorf("capabilities = %+v, want all-true feature flags", caps)
	}
	if caps.MaxMessageRunes != vkMaxMessageRunes {
		t.Errorf("MaxMessageRunes = %d, want %d", caps.MaxMessageRunes, vkMaxMessageRunes)
	}
}

func TestSendRendersStopKeyboardAndUniqueRandomID(t *testing.T) {
	rs := newRecordingServer(t, map[string]string{
		"messages.send": `{"response": 7}`,
	})
	api := rs.client("tok")
	c := NewBotChat(api, 100, 1000) // fixed seed → deterministic random_ids

	// With a stopRunID, a callback Stop keyboard must be rendered.
	if _, err := c.Send(context.Background(), "200", "working", "run-42", true); err != nil {
		t.Fatalf("Send: %v", err)
	}
	first := rs.last("messages.send")
	kb := first.Get("keyboard")
	if kb == "" {
		t.Fatal("Send with stopRunID rendered no keyboard")
	}
	var parsed vkKeyboard
	mustUnmarshal(t, kb, &parsed)
	if !parsed.Inline || len(parsed.Buttons) != 1 || len(parsed.Buttons[0]) != 1 {
		t.Fatalf("keyboard shape = %+v, want one inline callback button", parsed)
	}
	btn := parsed.Buttons[0][0]
	if btn.Action.Type != "callback" || btn.Color != "negative" {
		t.Errorf("button = %+v, want callback/negative", btn)
	}
	var pl callbackPayload
	mustUnmarshal(t, btn.Action.Payload, &pl)
	if pl.Stop != "run-42" {
		t.Errorf("payload.stop = %q, want run-42", pl.Stop)
	}
	firstRandom := first.Get("random_id")

	// A second send (empty stopRunID → no keyboard) must use a DIFFERENT random_id.
	if _, err := c.Send(context.Background(), "200", "final", "", true); err != nil {
		t.Fatalf("second Send: %v", err)
	}
	second := rs.last("messages.send")
	if second.Get("keyboard") != "" {
		t.Errorf("Send with empty stopRunID rendered a keyboard %q, want none", second.Get("keyboard"))
	}
	if second.Get("random_id") == firstRandom {
		t.Errorf("two sends reused random_id %q; must be unique", firstRandom)
	}
}

func TestEditClearsKeyboardOnFinal(t *testing.T) {
	rs := newRecordingServer(t, map[string]string{"messages.edit": `{"response": 1}`})
	c := NewBotChat(rs.client("tok"), 100, 1)

	if err := c.Edit(context.Background(), "200", "55", "final answer", "", true); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	got := rs.last("messages.edit")
	if got.Get("message_id") != "55" || got.Get("message") != "final answer" {
		t.Errorf("edit params = %v", got)
	}
	if got.Get("keyboard") != "" {
		t.Errorf("final edit keyboard = %q, want empty (markup cleared)", got.Get("keyboard"))
	}
}

func TestSendDocumentUploadsThenSends(t *testing.T) {
	// Reuse the three-step dance asserted in api_test, but through the Transport.
	rs := newRecordingServer(t, nil)
	uploadSrv := newUploadServer(t)
	rs.replies = map[string]string{
		"docs.getMessagesUploadServer": `{"response": {"upload_url": "` + uploadSrv + `"}}`,
		"docs.save":                    `{"response": {"type": "doc", "doc": {"id": 5, "owner_id": -100}}}`,
		"messages.send":                `{"response": 1}`,
	}
	c := NewBotChat(rs.client("tok"), 100, 1)

	if err := c.SendDocument(context.Background(), "200", "out.txt", strings.NewReader("DATA")); err != nil {
		t.Fatalf("SendDocument: %v", err)
	}
	send := rs.last("messages.send")
	if send.Get("attachment") != "doc-100_5" {
		t.Errorf("send attachment = %q, want doc-100_5", send.Get("attachment"))
	}
}

func TestStreamDraftUnsupported(t *testing.T) {
	c := NewBotChat(NewClient("tok"), 100, 1)
	err := c.StreamDraft(context.Background(), "200", "draft", "frame", true)
	if !errors.Is(err, errDraftUnsupported) {
		t.Errorf("StreamDraft error = %v, want errDraftUnsupported (Service falls back to Edit)", err)
	}
}

func TestRetryAfterClassifiesFloodErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		wantOK bool
	}{
		{"flood control 9", &apiError{Code: 9, Msg: "Flood control"}, true},
		{"too many requests 6", &apiError{Code: 6, Msg: "Too many requests"}, true},
		{"other api error", &apiError{Code: 100, Msg: "param error"}, false},
		{"non-api error", errors.New("boom"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, ok := RetryAfter(tc.err)
			if ok != tc.wantOK {
				t.Fatalf("RetryAfter ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && d != floodBackoff {
				t.Errorf("RetryAfter delay = %v, want %v", d, floodBackoff)
			}
		})
	}
}

// Compile-time assertion that botChat satisfies the Transport contract.
var _ chat.Transport = (*botChat)(nil)

// newUploadServer returns the URL of a fake VK doc upload endpoint that accepts a
// multipart "file" field and returns a fixed file token.
func newUploadServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, testBodyLimit)
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "no file field", http.StatusBadRequest)
			return
		}
		defer func() { _ = f.Close() }()
		_, _ = io.WriteString(w, `{"file": "uploaded-file-token"}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}
