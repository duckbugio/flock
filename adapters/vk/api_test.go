//nolint:testpackage // intentionally whitebox to test unexported VK client internals
package vk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// testBodyLimit bounds request bodies in the fake servers (keeps gosec G120 happy
// and matches the tiny payloads the tests send).
const testBodyLimit = 4 << 20 // 4 MiB

// recordingServer is a fake api.vk.com that records each method call's form
// params and replies with a canned JSON envelope keyed by method name.
type recordingServer struct {
	srv     *httptest.Server
	mu      chan struct{} // serializes access to calls (1-buffered as a mutex)
	calls   map[string][]url.Values
	replies map[string]string
}

func newRecordingServer(t *testing.T, replies map[string]string) *recordingServer {
	t.Helper()
	rs := &recordingServer{
		calls:   map[string][]url.Values{},
		replies: replies,
		mu:      make(chan struct{}, 1),
	}
	rs.mu <- struct{}{}
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/")
		r.Body = http.MaxBytesReader(w, r.Body, testBodyLimit)
		_ = r.ParseForm()
		<-rs.mu
		rs.calls[method] = append(rs.calls[method], r.Form)
		reply, ok := rs.replies[method]
		rs.mu <- struct{}{}
		if !ok {
			http.Error(w, "no canned reply for "+method, http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(rs.srv.Close)
	return rs
}

// client returns a Client pointed at the fake server (trailing slash so method
// names append cleanly).
func (rs *recordingServer) client(token string) *Client {
	return &Client{Token: token, BaseURL: rs.srv.URL + "/", HTTPClient: rs.srv.Client()}
}

// last returns the most recent recorded params for method.
func (rs *recordingServer) last(method string) url.Values {
	<-rs.mu
	defer func() { rs.mu <- struct{}{} }()
	c := rs.calls[method]
	if len(c) == 0 {
		return nil
	}
	return c[len(c)-1]
}

func (rs *recordingServer) count(method string) int {
	<-rs.mu
	defer func() { rs.mu <- struct{}{} }()
	return len(rs.calls[method])
}

func TestMessagesSendParams(t *testing.T) {
	rs := newRecordingServer(t, map[string]string{
		"messages.send": `{"response": 4242}`,
	})
	c := rs.client("secret-token")

	msgID, err := c.MessagesSend(context.Background(), 100, "hello", 777, `{"inline":true}`, "")
	if err != nil {
		t.Fatalf("MessagesSend: %v", err)
	}
	if msgID != 4242 {
		t.Errorf("message id = %d, want 4242", msgID)
	}
	got := rs.last("messages.send")
	if got.Get("peer_id") != "100" {
		t.Errorf("peer_id = %q, want 100", got.Get("peer_id"))
	}
	if got.Get("message") != "hello" {
		t.Errorf("message = %q, want hello", got.Get("message"))
	}
	if got.Get("random_id") != "777" {
		t.Errorf("random_id = %q, want 777", got.Get("random_id"))
	}
	if got.Get("keyboard") != `{"inline":true}` {
		t.Errorf("keyboard = %q, want the inline JSON", got.Get("keyboard"))
	}
	if got.Get("access_token") != "secret-token" {
		t.Errorf("access_token form field = %q, want secret-token", got.Get("access_token"))
	}
	if got.Get("v") != apiVersion {
		t.Errorf("v = %q, want %s", got.Get("v"), apiVersion)
	}
}

func TestMessagesEditAndDeleteParams(t *testing.T) {
	rs := newRecordingServer(t, map[string]string{
		"messages.edit":   `{"response": 1}`,
		"messages.delete": `{"response": {"123": 1}}`,
	})
	c := rs.client("tok")

	if err := c.MessagesEdit(context.Background(), 100, 55, "edited", ""); err != nil {
		t.Fatalf("MessagesEdit: %v", err)
	}
	edit := rs.last("messages.edit")
	if edit.Get("peer_id") != "100" || edit.Get("message_id") != "55" || edit.Get("message") != "edited" {
		t.Errorf("edit params = %v, want peer_id=100 message_id=55 message=edited", edit)
	}
	// Empty keyboard must still be SENT (clears markup on the final answer).
	if _, ok := edit["keyboard"]; !ok {
		t.Error("edit did not send a keyboard field; empty keyboard must be sent to clear markup")
	}

	if err := c.MessagesDelete(context.Background(), 100, 55); err != nil {
		t.Fatalf("MessagesDelete: %v", err)
	}
	del := rs.last("messages.delete")
	if del.Get("message_ids") != "55" || del.Get("delete_for_all") != "1" {
		t.Errorf("delete params = %v, want message_ids=55 delete_for_all=1", del)
	}
}

func TestAPIErrorEnvelope(t *testing.T) {
	rs := newRecordingServer(t, map[string]string{
		"messages.send": `{"error": {"error_code": 9, "error_msg": "Flood control"}}`,
	})
	c := rs.client("tok")
	_, err := c.MessagesSend(context.Background(), 1, "x", 1, "", "")
	if err == nil {
		t.Fatal("MessagesSend on error envelope = nil, want *apiError")
	}
	var ae *apiError
	if !errors.As(err, &ae) {
		t.Fatalf("error = %T, want *apiError", err)
	}
	if ae.Code != 9 {
		t.Errorf("error code = %d, want 9", ae.Code)
	}
}

func TestUploadDocumentThreeStepDance(t *testing.T) {
	rs := newRecordingServer(t, nil)
	// The upload server is a separate endpoint; point docs.getMessagesUploadServer
	// at it.
	uploadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Assert the multipart "file" field is present and streamed.
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
	t.Cleanup(uploadSrv.Close)

	rs.replies = map[string]string{
		"docs.getMessagesUploadServer": `{"response": {"upload_url": "` + uploadSrv.URL + `"}}`,
		"docs.save":                    `{"response": {"type": "doc", "doc": {"id": 555, "owner_id": -100}}}`,
		"messages.send":                `{"response": 9001}`,
	}
	c := rs.client("tok")

	attachment, err := c.UploadDocument(context.Background(), 200, "report.pdf", strings.NewReader("FILEDATA"))
	if err != nil {
		t.Fatalf("UploadDocument: %v", err)
	}
	if attachment != "doc-100_555" {
		t.Errorf("attachment = %q, want doc-100_555", attachment)
	}
	if rs.count("docs.getMessagesUploadServer") != 1 || rs.count("docs.save") != 1 {
		t.Errorf("expected one getMessagesUploadServer + one docs.save, got %d + %d",
			rs.count("docs.getMessagesUploadServer"), rs.count("docs.save"))
	}
	if got := rs.last("docs.save").Get("file"); got != "uploaded-file-token" {
		t.Errorf("docs.save file token = %q, want uploaded-file-token", got)
	}
}

// mustUnmarshal is a guard that JSON we build parses back to the expected shape
// (used by bot_test for the keyboard JSON).
func mustUnmarshal(t *testing.T, data string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(data), v); err != nil {
		t.Fatalf("unmarshal %q: %v", data, err)
	}
}
