package lo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type keyboardRequest struct {
	Method string          `json:"-"`
	Text   string          `json:"text"`
	Markup *inlineKeyboard `json:"reply_markup"` //nolint:tagliatelle // Bot API wire spelling.
}

func TestKeyboardLifecycleUsesAtomicEdits(t *testing.T) {
	t.Parallel()
	requests := make(chan keyboardRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request keyboardRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		request.Method = r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		requests <- request
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":9}}`))
	}))
	defer server.Close()
	api, err := NewClient(server.URL, "123:secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	transport := NewTransport(api, false).WithKeyboards(true)
	id, err := transport.Send(t.Context(), "77", "Starting", "1", false)
	if err != nil || id != "9" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	sent := <-requests
	if sent.Method != "sendMessage" || sent.Markup == nil || len(sent.Markup.Rows) != 1 {
		t.Fatalf("send: %+v", sent)
	}
	data := sent.Markup.Rows[0][0].Data
	if transport.stopButtonRun("77", data) != "1" {
		t.Fatal("button does not control the originating run")
	}
	if err := transport.Edit(t.Context(), "77", id, "Working", "1", false); err != nil {
		t.Fatal(err)
	}
	edited := <-requests
	if edited.Method != editTextMethod || edited.Text != "Working" || edited.Markup.Rows[0][0].Data != data {
		t.Fatalf("progress edit: %+v", edited)
	}
	if err := transport.Edit(t.Context(), "77", id, "Done", "", false); err != nil {
		t.Fatal(err)
	}
	final := <-requests
	if final.Method != editTextMethod || final.Text != "Done" || final.Markup == nil || len(final.Markup.Rows) != 0 {
		t.Fatalf("final text and keyboard were not changed together: %+v", final)
	}
	if _, err := transport.SendStarNudge(t.Context(), "77", "Star this project?"); err != nil {
		t.Fatal(err)
	}
	star := <-requests
	if star.Method != "sendMessage" || star.Markup.Rows[0][0].Data != "star:confirm" {
		t.Fatalf("star: %+v", star)
	}
}
