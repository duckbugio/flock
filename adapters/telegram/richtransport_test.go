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

	"github.com/duckbugio/flock/core/chat/rich"
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

func TestHTTPRichTransportSend(t *testing.T) {
	rs := newRichServer(t, `{"ok":true,"result":{"message_id":4242}}`)
	tr := newHTTPRichTransport("123:ABC", rs.srv.URL, rs.srv.Client())

	msg := toInputRichMessage(rich2(t))
	id, err := tr.send(context.Background(), 555, msg, stopMarkup("run-1"))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if id != 4242 {
		t.Errorf("message id = %d, want 4242", id)
	}
	if !strings.HasSuffix(rs.lastPath, "/bot123:ABC/sendRichMessage") {
		t.Errorf("path = %q, want .../bot123:ABC/sendRichMessage", rs.lastPath)
	}
	if _, ok := rs.lastBody["rich_message"]; !ok {
		t.Errorf("body %v missing rich_message", rs.lastBody)
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

	err := tr.edit(context.Background(), 555, 7, toInputRichMessage(rich2(t)), nil)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !strings.HasSuffix(rs.lastPath, "/editMessageText") {
		t.Errorf("path = %q, want .../editMessageText", rs.lastPath)
	}
	if rs.lastBody["message_id"].(float64) != 7 {
		t.Errorf("message_id = %v, want 7", rs.lastBody["message_id"])
	}
}

// TestHTTPRichTransportAPIError asserts an ok:false envelope becomes an error
// (which the adapter turns into a legacy fallback).
func TestHTTPRichTransportAPIError(t *testing.T) {
	rs := newRichServer(t, `{"ok":false,"description":"RICH_MESSAGE_INVALID"}`)
	tr := newHTTPRichTransport("123:ABC", rs.srv.URL, rs.srv.Client())

	_, err := tr.send(context.Background(), 1, toInputRichMessage(rich2(t)), nil)
	if err == nil {
		t.Fatal("send with ok:false returned nil error, want failure")
	}
	if !strings.Contains(err.Error(), "RICH_MESSAGE_INVALID") {
		t.Errorf("error %v missing API description", err)
	}
}

// rich2 builds a small representative message for transport tests.
func rich2(t *testing.T) rich.Message {
	t.Helper()
	return rich.FromMarkdown("# Title\n\nhello **world**")
}
