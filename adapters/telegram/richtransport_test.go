//nolint:testpackage // whitebox: tests the unexported rich HTTP shim
package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// richRecordingServer is a fake Bot API that records the last method path and
// JSON body, and returns a canned envelope.
type richRecordingServer struct {
	srv      *httptest.Server
	lastPath string
	lastBody map[string]any
}

func newRichServer(t *testing.T, reply string) *richRecordingServer {
	t.Helper()
	rs := &richRecordingServer{}
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.lastPath = r.URL.Path
		data, _ := io.ReadAll(r.Body)
		rs.lastBody = map[string]any{}
		_ = json.Unmarshal(data, &rs.lastBody)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(rs.srv.Close)
	return rs
}

// richMessageField returns the rich_message sub-object of the recorded body.
func (rs *richRecordingServer) richMessageField(t *testing.T) map[string]any {
	t.Helper()
	rm, ok := rs.lastBody["rich_message"].(map[string]any)
	if !ok {
		t.Fatalf("body %v has no rich_message object", rs.lastBody)
	}
	return rm
}

func TestHTTPRichTransportSend(t *testing.T) {
	rs := newRichServer(t, `{"ok":true,"result":{"message_id":4242}}`)
	tr := newHTTPRichTransport("123:ABC", rs.srv.URL, rs.srv.Client())

	id, err := tr.send(context.Background(), 555, richMarkdown("# Title\n\nhi"), stopMarkup("run-1"))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if id != 4242 {
		t.Errorf("message id = %d, want 4242", id)
	}
	if !strings.HasSuffix(rs.lastPath, "/bot123:ABC/sendRichMessage") {
		t.Errorf("path = %q, want .../bot123:ABC/sendRichMessage", rs.lastPath)
	}
	if rs.richMessageField(t)["markdown"] != "# Title\n\nhi" {
		t.Errorf("rich_message.markdown = %v, want the passed text", rs.richMessageField(t)["markdown"])
	}
	if rs.lastBody["chat_id"].(float64) != 555 {
		t.Errorf("chat_id = %v, want 555", rs.lastBody["chat_id"])
	}
	if _, ok := rs.lastBody["reply_markup"]; !ok {
		t.Errorf("body %v missing reply_markup (Stop button)", rs.lastBody)
	}
}

func TestHTTPRichTransportEdit(t *testing.T) {
	rs := newRichServer(t, `{"ok":true,"result":{"message_id":7}}`)
	tr := newHTTPRichTransport("123:ABC", rs.srv.URL, rs.srv.Client())

	if err := tr.edit(context.Background(), 555, 7, richMarkdown("updated"), nil); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !strings.HasSuffix(rs.lastPath, "/editMessageText") {
		t.Errorf("path = %q, want .../editMessageText", rs.lastPath)
	}
	if rs.lastBody["message_id"].(float64) != 7 {
		t.Errorf("message_id = %v, want 7", rs.lastBody["message_id"])
	}
	if rs.richMessageField(t)["markdown"] != "updated" {
		t.Errorf("rich_message.markdown = %v, want updated", rs.richMessageField(t)["markdown"])
	}
}

func TestHTTPRichTransportStreamDraft(t *testing.T) {
	rs := newRichServer(t, `{"ok":true,"result":true}`)
	tr := newHTTPRichTransport("123:ABC", rs.srv.URL, rs.srv.Client())

	if err := tr.streamDraft(context.Background(), 555, 99, richMarkdown("frame")); err != nil {
		t.Fatalf("streamDraft: %v", err)
	}
	if !strings.HasSuffix(rs.lastPath, "/sendRichMessageDraft") {
		t.Errorf("path = %q, want .../sendRichMessageDraft", rs.lastPath)
	}
	// draft_id is an integer per the verified schema.
	if rs.lastBody["draft_id"].(float64) != 99 {
		t.Errorf("draft_id = %v, want 99", rs.lastBody["draft_id"])
	}
	if rs.richMessageField(t)["markdown"] != "frame" {
		t.Errorf("rich_message.markdown = %v, want frame", rs.richMessageField(t)["markdown"])
	}
}

// TestHTTPRichTransportRedactsToken asserts a transport-level error (net/http
// embeds the token-bearing request URL in its message) never leaks the token.
func TestHTTPRichTransportRedactsToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := srv.URL
	srv.Close() // now the base refuses connections → t.hc.Do returns an error

	tr := newHTTPRichTransport("123:SECRETTOKEN", base, srv.Client())
	_, err := tr.send(context.Background(), 1, richMarkdown("x"), nil)
	if err == nil {
		t.Fatal("send to a closed server returned nil error")
	}
	if strings.Contains(err.Error(), "SECRETTOKEN") {
		t.Errorf("error leaks the bot token: %v", err)
	}
}

// TestHTTPRichTransportAPIError asserts an ok:false envelope becomes an error
// (which the adapter turns into a legacy fallback).
func TestHTTPRichTransportAPIError(t *testing.T) {
	rs := newRichServer(t, `{"ok":false,"description":"RICH_MESSAGE_INVALID"}`)
	tr := newHTTPRichTransport("123:ABC", rs.srv.URL, rs.srv.Client())

	_, err := tr.send(context.Background(), 1, richMarkdown("x"), nil)
	if err == nil {
		t.Fatal("send with ok:false returned nil error, want failure")
	}
	if !strings.Contains(err.Error(), "RICH_MESSAGE_INVALID") {
		t.Errorf("error %v missing API description", err)
	}
}
