//nolint:testpackage // intentionally whitebox to test unexported VK transport internals
package vk

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/duckbugio/flock/core/chat"
)

func TestCapabilities(t *testing.T) {
	c := NewBotChat(NewClient("tok"), 100, 1)
	caps := c.Capabilities()
	if !caps.CanSendDocument {
		t.Errorf("capabilities = %+v, want CanSendDocument true", caps)
	}
	if caps.MaxMessageRunes != vkMaxMessageRunes {
		t.Errorf("MaxMessageRunes = %d, want %d", caps.MaxMessageRunes, vkMaxMessageRunes)
	}
	// VK has no Bot API 10.1 rich support: the additive CanSendRich field must stay
	// at its zero value so the VK adapter keeps the legacy rendering with no change
	// (docs/rich-messages-plan.md §6).
	if caps.CanSendRich {
		t.Errorf("capabilities = %+v, want CanSendRich false (VK has no rich support)", caps)
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

// keyboardGatedServer is a fake api.vk.com that rejects any call carrying a
// non-empty "keyboard" form field with VK error 912 ("Bot abilities" toggle OFF)
// and succeeds otherwise — modelling the live finding that a keyboard'd send/edit
// fails while the same call without a keyboard goes through. It records every
// call so a test can assert the retry happened and dropped the keyboard.
func keyboardGatedServer(t *testing.T, successReply string) *recordingServer {
	t.Helper()
	rs := &recordingServer{
		calls: map[string][]url.Values{},
		mu:    make(chan struct{}, 1),
	}
	rs.mu <- struct{}{}
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/")
		r.Body = http.MaxBytesReader(w, r.Body, testBodyLimit)
		_ = r.ParseForm()
		<-rs.mu
		rs.calls[method] = append(rs.calls[method], r.Form)
		rs.mu <- struct{}{}
		if r.Form.Get("keyboard") != "" {
			_, _ = io.WriteString(w, `{"error": {"error_code": 912, "error_msg": "This is a chat bot feature"}}`)
			return
		}
		_, _ = io.WriteString(w, successReply)
	}))
	t.Cleanup(rs.srv.Close)
	return rs
}

func TestSendRetriesWithoutKeyboardOn912(t *testing.T) {
	rs := keyboardGatedServer(t, `{"response": 77}`)
	c := NewBotChat(rs.client("tok"), 100, 1)

	id, err := c.Send(context.Background(), "200", "working", "run-1", true)
	if err != nil {
		t.Fatalf("Send after 912 fallback = %v, want nil", err)
	}
	if id != "77" {
		t.Errorf("delivered message id = %q, want 77 (the keyboard-less resend)", id)
	}
	if got := rs.count("messages.send"); got != 2 {
		t.Fatalf("messages.send call count = %d, want 2 (keyboard'd attempt + keyboard-less retry)", got)
	}
	first := rs.calls["messages.send"][0]
	if first.Get("keyboard") == "" {
		t.Error("first send carried no keyboard; the 912 path requires a keyboard'd attempt")
	}
	second := rs.calls["messages.send"][1]
	if second.Get("keyboard") != "" {
		t.Errorf("retry send keyboard = %q, want empty (Stop button dropped)", second.Get("keyboard"))
	}
	if second.Get("message") != "working" {
		t.Errorf("retry send message = %q, want the original text", second.Get("message"))
	}
}

func TestEditRetriesWithoutKeyboardOn912(t *testing.T) {
	rs := keyboardGatedServer(t, `{"response": 1}`)
	c := NewBotChat(rs.client("tok"), 100, 1)

	if err := c.Edit(context.Background(), "200", "55", "progress", "run-1", true); err != nil {
		t.Fatalf("Edit after 912 fallback = %v, want nil", err)
	}
	if got := rs.count("messages.edit"); got != 2 {
		t.Fatalf("messages.edit call count = %d, want 2 (keyboard'd attempt + keyboard-less retry)", got)
	}
	if got := rs.calls["messages.edit"][1].Get("keyboard"); got != "" {
		t.Errorf("retry edit keyboard = %q, want empty (Stop button dropped)", got)
	}
}

func TestSendDoesNotRetryNon912(t *testing.T) {
	// A non-912 error from a keyboard'd send must propagate unchanged, with no
	// second attempt.
	rs := newRecordingServer(t, map[string]string{
		"messages.send": `{"error": {"error_code": 100, "error_msg": "param error"}}`,
	})
	c := NewBotChat(rs.client("tok"), 100, 1)

	if _, err := c.Send(context.Background(), "200", "working", "run-1", true); err == nil {
		t.Fatal("Send on non-912 error = nil, want the error propagated")
	}
	if got := rs.count("messages.send"); got != 1 {
		t.Errorf("messages.send call count = %d, want 1 (no retry for non-912)", got)
	}
}

func TestSendDoesNotRetryWhenNoKeyboard(t *testing.T) {
	// A keyboard-less send that somehow returns 912 must NOT trigger a pointless
	// double-send (there is no keyboard to drop).
	rs := newRecordingServer(t, map[string]string{
		"messages.send": `{"error": {"error_code": 912, "error_msg": "This is a chat bot feature"}}`,
	})
	c := NewBotChat(rs.client("tok"), 100, 1)

	if _, err := c.Send(context.Background(), "200", "final", "", true); err == nil {
		t.Fatal("keyboard-less Send on 912 = nil, want the error propagated")
	}
	if got := rs.count("messages.send"); got != 1 {
		t.Errorf("messages.send call count = %d, want 1 (no retry without a keyboard)", got)
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
