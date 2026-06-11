//nolint:testpackage // whitebox: tests the unexported rich gating/fallback in Send/Edit
package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// fakeRich is a stub richTransport: it records calls and returns a configured id
// or error so the gating/fallback decisions can be tested without HTTP.
type fakeRich struct {
	sendID  int
	sendErr error
	editErr error
	sendN   int32
	editN   int32
}

func (f *fakeRich) send(_ context.Context, _ int64, _ inputRichMessage, _ models.ReplyMarkup) (int, error) {
	atomic.AddInt32(&f.sendN, 1)
	return f.sendID, f.sendErr
}

func (f *fakeRich) edit(_ context.Context, _ int64, _ int, _ inputRichMessage, _ models.ReplyMarkup) error {
	atomic.AddInt32(&f.editN, 1)
	return f.editErr
}

// legacyBotServer builds a *bot.Bot pointed at a fake Bot API that counts the
// legacy sendMessage/editMessageText calls and always succeeds.
func legacyBotServer(t *testing.T, sendHits, editHits *int32) *bot.Bot {
	t.Helper()
	ok := func(w http.ResponseWriter, _ *http.Request) {
		if err := json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": 99, "chat": map[string]any{"id": 1}, "date": 0},
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/bot123:ABC/sendMessage", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(sendHits, 1)
		ok(w, r)
	})
	mux.HandleFunc("/bot123:ABC/editMessageText", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(editHits, 1)
		ok(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	b, err := bot.New("123:ABC", bot.WithSkipGetMe(), bot.WithServerURL(srv.URL))
	if err != nil {
		t.Fatalf("bot.New: %v", err)
	}
	return b
}

func TestTrySendRichGating(t *testing.T) {
	fr := &fakeRich{sendID: 11}
	tests := []struct {
		name       string
		enableRich bool
		rich       richTransport
		asMarkdown bool
		text       string
		wantOK     bool
	}{
		{"rich disabled", false, fr, true, "hi", false},
		{"nil transport", true, nil, true, "hi", false},
		{"not markdown", true, fr, false, "hi", false},
		{"empty text", true, fr, true, "", false},
		{"all set", true, fr, true, "hi", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &botChat{enableRich: tc.enableRich, rich: tc.rich}
			id, ok := c.trySendRich(context.Background(), 1, tc.text, "", tc.asMarkdown)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && id != "11" {
				t.Errorf("id = %q, want 11", id)
			}
		})
	}
}

// TestSendRichSuccessSkipsLegacy: a successful rich send returns the rich id and
// never touches the legacy sendMessage endpoint.
func TestSendRichSuccessSkipsLegacy(t *testing.T) {
	var sendHits, editHits int32
	b := legacyBotServer(t, &sendHits, &editHits)
	c := &botChat{b: b, enableRich: true, rich: &fakeRich{sendID: 4242}}

	id, err := c.Send(context.Background(), "555", "# Hi\n\nbody", "", true)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if id != "4242" {
		t.Errorf("id = %q, want 4242 (rich id)", id)
	}
	if atomic.LoadInt32(&sendHits) != 0 {
		t.Errorf("legacy sendMessage called %d times, want 0 (rich succeeded)", sendHits)
	}
}

// TestSendRichErrorFallsBackToLegacy: a rich failure falls through to the legacy
// sendMessage, which delivers the answer.
func TestSendRichErrorFallsBackToLegacy(t *testing.T) {
	var sendHits, editHits int32
	b := legacyBotServer(t, &sendHits, &editHits)
	fr := &fakeRich{sendErr: errors.New("rich boom")}
	c := &botChat{b: b, enableRich: true, rich: fr}

	id, err := c.Send(context.Background(), "555", "hello", "", true)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if id != "99" {
		t.Errorf("id = %q, want 99 (legacy fallback)", id)
	}
	if atomic.LoadInt32(&fr.sendN) != 1 {
		t.Errorf("rich send attempts = %d, want 1", fr.sendN)
	}
	if atomic.LoadInt32(&sendHits) != 1 {
		t.Errorf("legacy sendMessage hits = %d, want 1 (fallback)", sendHits)
	}
}

// TestEditRichErrorFallsBackToLegacy mirrors the Send fallback for Edit.
func TestEditRichErrorFallsBackToLegacy(t *testing.T) {
	var sendHits, editHits int32
	b := legacyBotServer(t, &sendHits, &editHits)
	fr := &fakeRich{editErr: errors.New("rich boom")}
	c := &botChat{b: b, enableRich: true, rich: fr}

	if err := c.Edit(context.Background(), "555", "7", "progress", "run-1", true); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if atomic.LoadInt32(&editHits) != 1 {
		t.Errorf("legacy editMessageText hits = %d, want 1 (fallback)", editHits)
	}
}

// TestSendRichDisabledUsesLegacy: with the flag off, Send takes the legacy path
// untouched (no rich transport configured).
func TestSendRichDisabledUsesLegacy(t *testing.T) {
	var sendHits, editHits int32
	b := legacyBotServer(t, &sendHits, &editHits)
	c := &botChat{b: b, enableRich: false} // rich nil — exactly today's behaviour

	if _, err := c.Send(context.Background(), "555", "hello", "", true); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if atomic.LoadInt32(&sendHits) != 1 {
		t.Errorf("legacy sendMessage hits = %d, want 1", sendHits)
	}
}
