//nolint:testpackage // whitebox: tests the unexported rich gating/fallback in Send/Edit/StreamDraft
package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// fakeRich is a stub richTransport: it records calls and returns a configured id
// or error so the gating/fallback decisions can be tested without HTTP.
type fakeRich struct {
	sendID    int
	sendErr   error
	editErr   error
	draftErr  error
	sendN     int32
	editN     int32
	draftN    int32
	lastDraft inputRichMessage
}

func (f *fakeRich) send(_ context.Context, _ int64, _ inputRichMessage, _ models.ReplyMarkup) (int, error) {
	atomic.AddInt32(&f.sendN, 1)
	return f.sendID, f.sendErr
}

func (f *fakeRich) edit(_ context.Context, _ int64, _ int, _ inputRichMessage, _ models.ReplyMarkup) error {
	atomic.AddInt32(&f.editN, 1)
	return f.editErr
}

func (f *fakeRich) streamDraft(_ context.Context, _, _ int64, msg inputRichMessage) error {
	atomic.AddInt32(&f.draftN, 1)
	f.lastDraft = msg
	return f.draftErr
}

// legacyBotServer builds a *bot.Bot pointed at a fake Bot API that counts the
// legacy sendMessage/editMessageText/sendMessageDraft calls and always succeeds.
func legacyBotServer(t *testing.T, sendHits, editHits, draftHits *int32) *bot.Bot {
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
	mux.HandleFunc("/bot123:ABC/sendMessageDraft", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(draftHits, 1)
		if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true}); err != nil {
			t.Errorf("encode draft response: %v", err)
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
	var sendHits, editHits, draftHits int32
	b := legacyBotServer(t, &sendHits, &editHits, &draftHits)
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
	var sendHits, editHits, draftHits int32
	b := legacyBotServer(t, &sendHits, &editHits, &draftHits)
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
	var sendHits, editHits, draftHits int32
	b := legacyBotServer(t, &sendHits, &editHits, &draftHits)
	fr := &fakeRich{editErr: errors.New("rich boom")}
	c := &botChat{b: b, enableRich: true, rich: fr}

	if err := c.Edit(context.Background(), "555", "7", "progress", "run-1", true); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if atomic.LoadInt32(&editHits) != 1 {
		t.Errorf("legacy editMessageText hits = %d, want 1 (fallback)", editHits)
	}
}

// TestStreamDraftRichSuccessSkipsLegacy: a successful rich draft never touches the
// legacy sendMessageDraft endpoint and carries the Markdown frame.
func TestStreamDraftRichSuccessSkipsLegacy(t *testing.T) {
	var sendHits, editHits, draftHits int32
	b := legacyBotServer(t, &sendHits, &editHits, &draftHits)
	fr := &fakeRich{}
	c := &botChat{b: b, enableRich: true, rich: fr}

	if err := c.StreamDraft(context.Background(), "555", "run-1", "**frame**", true); err != nil {
		t.Fatalf("StreamDraft: %v", err)
	}
	if atomic.LoadInt32(&fr.draftN) != 1 {
		t.Errorf("rich streamDraft calls = %d, want 1", fr.draftN)
	}
	if fr.lastDraft.Markdown != "**frame**" {
		t.Errorf("draft markdown = %q, want the frame text", fr.lastDraft.Markdown)
	}
	if atomic.LoadInt32(&draftHits) != 0 {
		t.Errorf("legacy sendMessageDraft hits = %d, want 0 (rich succeeded)", draftHits)
	}
}

// TestStreamDraftRichErrorFallsBackToLegacy: a rich draft failure falls through to
// the legacy SendMessageDraft.
func TestStreamDraftRichErrorFallsBackToLegacy(t *testing.T) {
	var sendHits, editHits, draftHits int32
	b := legacyBotServer(t, &sendHits, &editHits, &draftHits)
	fr := &fakeRich{draftErr: errors.New("rich boom")}
	c := &botChat{b: b, enableRich: true, rich: fr}

	if err := c.StreamDraft(context.Background(), "555", "run-1", "frame", true); err != nil {
		t.Fatalf("StreamDraft: %v", err)
	}
	if atomic.LoadInt32(&draftHits) != 1 {
		t.Errorf("legacy sendMessageDraft hits = %d, want 1 (fallback)", draftHits)
	}
}

// TestRecordRichBreaker exercises the circuit-breaker logic: threshold consecutive
// failures suppress rich, and any success in between resets the count + cooldown.
func TestRecordRichBreaker(t *testing.T) {
	c := &botChat{}
	// One short of the threshold: not suppressed.
	for range richFailureThreshold - 1 {
		c.recordRich(errors.New("x"))
	}
	if c.richDisabledUntil.Load() != 0 {
		t.Fatal("breaker suppressed rich before reaching the threshold")
	}
	// A success resets the consecutive count.
	c.recordRich(nil)
	if c.richFailures.Load() != 0 {
		t.Fatalf("failure count = %d after success, want 0", c.richFailures.Load())
	}
	// A full threshold of consecutive failures now arms the cooldown.
	for range richFailureThreshold {
		c.recordRich(errors.New("x"))
	}
	if c.richDisabledUntil.Load() == 0 {
		t.Error("breaker did not suppress rich after threshold consecutive failures")
	}
	// A later success clears the cooldown again.
	c.recordRich(nil)
	if c.richDisabledUntil.Load() != 0 {
		t.Error("success did not clear the cooldown")
	}
}

// TestRichBreakerStopsAttempts: once the breaker trips, Send stops attempting the
// rich path during the cooldown (no further calls to the rich transport).
func TestRichBreakerStopsAttempts(t *testing.T) {
	var sendHits, editHits, draftHits int32
	b := legacyBotServer(t, &sendHits, &editHits, &draftHits)
	fr := &fakeRich{sendErr: errors.New("unsupported")}
	c := &botChat{b: b, enableRich: true, rich: fr}

	for range richFailureThreshold {
		if _, err := c.Send(context.Background(), "555", "hi", "", true); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	tripped := atomic.LoadInt32(&fr.sendN)
	if tripped != int32(richFailureThreshold) {
		t.Fatalf("rich attempts before trip = %d, want %d", tripped, richFailureThreshold)
	}
	if c.richDisabledUntil.Load() == 0 {
		t.Fatal("breaker not armed after threshold failures")
	}
	// Subsequent sends must not touch the rich transport during cooldown, only legacy.
	if _, err := c.Send(context.Background(), "555", "hi", "", true); err != nil {
		t.Fatalf("Send after trip: %v", err)
	}
	if atomic.LoadInt32(&fr.sendN) != tripped {
		t.Errorf("rich attempted during cooldown (%d > %d)", fr.sendN, tripped)
	}
}

// TestRichBreakerCooldownProbe: after the cooldown elapses, a single rich probe is
// allowed again (self-healing from a transient failure burst).
func TestRichBreakerCooldownProbe(t *testing.T) {
	now := time.Unix(1_000, 0)
	c := &botChat{enableRich: true, rich: &fakeRich{}, now: func() time.Time { return now }}

	for range richFailureThreshold {
		c.recordRich(errors.New("x"))
	}
	if c.richAttemptable(true, "hi") {
		t.Fatal("rich attempted during the cooldown window")
	}
	// Advance the clock past the cooldown → a probe is permitted again.
	now = now.Add(richCooldown + time.Second)
	if !c.richAttemptable(true, "hi") {
		t.Error("rich not probed after the cooldown elapsed")
	}
}

// TestSendRichDisabledUsesLegacy: with the flag off, Send takes the legacy path
// untouched (no rich transport configured).
func TestSendRichDisabledUsesLegacy(t *testing.T) {
	var sendHits, editHits, draftHits int32
	b := legacyBotServer(t, &sendHits, &editHits, &draftHits)
	c := &botChat{b: b, enableRich: false} // rich nil — exactly today's behaviour

	if _, err := c.Send(context.Background(), "555", "hello", "", true); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if atomic.LoadInt32(&sendHits) != 1 {
		t.Errorf("legacy sendMessage hits = %d, want 1", sendHits)
	}
}

// TestDraftIDToInt asserts the run-id → non-zero int64 mapping is stable and
// distinguishes different ids (sendRichMessageDraft requires a non-zero integer).
func TestDraftIDToInt(t *testing.T) {
	if a, b := draftIDToInt("run-1"), draftIDToInt("run-1"); a != b {
		t.Errorf("not stable: %d != %d", a, b)
	}
	if draftIDToInt("run-1") == draftIDToInt("run-2") {
		t.Error("distinct run ids collided")
	}
	for _, s := range []string{"", "0", "run-1", "duck/-5238983644/x"} {
		if draftIDToInt(s) == 0 {
			t.Errorf("draftIDToInt(%q) = 0, want non-zero", s)
		}
		if draftIDToInt(s) < 0 {
			t.Errorf("draftIDToInt(%q) = %d, want positive", s, draftIDToInt(s))
		}
	}
}
