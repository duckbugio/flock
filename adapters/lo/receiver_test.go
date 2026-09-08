package lo_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/duckbugio/flock/adapters/lo"
	"github.com/duckbugio/flock/core/goal"
)

type serviceSpy struct {
	prompts     []string
	stopped     int
	newSessions int
}

func (s *serviceSpy) Handle(_ context.Context, _ string, _ int64, _, prompt string) {
	s.prompts = append(s.prompts, prompt)
}
func (s *serviceSpy) StopChat(string) bool                   { s.stopped++; return true }
func (s *serviceSpy) NewSession(string) error                { s.newSessions++; return nil }
func (*serviceSpy) ArmGoal(string, string) (goal.Goal, bool) { return goal.Goal{}, false }
func (*serviceSpy) GoalStatus(string) (goal.Goal, bool)      { return goal.Goal{}, false }
func (*serviceSpy) DisarmGoal(string) bool                   { return false }

func inbound(text string) *lo.Message {
	msg := &lo.Message{ID: 1, From: &lo.User{ID: 77}, Text: text}
	msg.Chat.ID = 77
	msg.Chat.Type = "private"
	return msg
}

func TestReceiverGatesCommandsAndPreservesProviderCommands(t *testing.T) {
	t.Parallel()
	svc := &serviceSpy{}
	api := client(t, func(w http.ResponseWriter, _ *http.Request) { reply(w, `{"ok":true,"result":{"message_id":1}}`) })
	receiver := lo.NewReceiver(lo.ReceiverConfig{
		Service: svc, Transport: lo.NewTransport(api, false), Username: "flock",
		IsAllowed: func(id int64) bool { return id == 77 }, RequireMention: true,
	})
	for _, text := range []string{"/help", "/stop", "/new", "/loop 5m"} {
		receiver.HandleUpdate(t.Context(), lo.Update{Message: inbound(text)})
	}
	if svc.stopped != 1 || svc.newSessions != 1 || len(svc.prompts) != 1 || svc.prompts[0] != "/loop 5m" {
		t.Fatalf("spy=%+v", svc)
	}
	other := inbound("/stop")
	other.From.ID = 88
	receiver.HandleUpdate(t.Context(), lo.Update{Message: other})
	receiver.HandleUpdate(t.Context(), lo.Update{Message: inbound("/stop@other")})
	if svc.stopped != 1 {
		t.Fatal("unauthorized stop")
	}
	group := inbound("hello")
	group.Chat.ID = -42
	group.Chat.Type = "group"
	receiver.HandleUpdate(t.Context(), lo.Update{Message: group})
	group.Text = "/stop"
	receiver.HandleUpdate(t.Context(), lo.Update{Message: group})
	group.Text = "/new"
	receiver.HandleUpdate(t.Context(), lo.Update{Message: group})
	group.Text = "/stop@other"
	receiver.HandleUpdate(t.Context(), lo.Update{Message: group})
	if svc.stopped != 2 || svc.newSessions != 2 {
		t.Fatalf("group commands must bypass mention gating but reject other bots: %+v", svc)
	}
	group.Text = "@flock hello"
	receiver.HandleUpdate(t.Context(), lo.Update{Message: group})
	if len(svc.prompts) != 2 || svc.prompts[1] != "hello" {
		t.Fatal(svc.prompts)
	}
}

func TestReceiverDoesNotDropAttachmentsOrQuoteContextSilently(t *testing.T) {
	t.Parallel()
	svc := &serviceSpy{}
	var notices atomic.Int32
	api := client(t, func(w http.ResponseWriter, _ *http.Request) {
		notices.Add(1)
		reply(w, `{"ok":true,"result":{"message_id":1}}`)
	})
	receiver := lo.NewReceiver(lo.ReceiverConfig{
		Service: svc, Transport: lo.NewTransport(api, false), IsAllowed: func(int64) bool { return true },
	})
	media := inbound("review this file")
	media.Document = []byte(`{"file_id":"lo-file"}`)
	receiver.HandleUpdate(t.Context(), lo.Update{Message: media})
	if len(svc.prompts) != 0 || notices.Load() != 1 {
		t.Fatal("attachment silently discarded")
	}
	quoted := inbound("explain")
	quoted.Reply = inbound("original code")
	receiver.HandleUpdate(t.Context(), lo.Update{Message: quoted})
	if len(svc.prompts) != 1 || !strings.Contains(svc.prompts[0], "original code") {
		t.Fatal(svc.prompts)
	}
}

func TestReceiverStopBypassesNewWorkGuards(t *testing.T) {
	t.Parallel()
	svc := &serviceSpy{}
	api := client(t, func(w http.ResponseWriter, _ *http.Request) {
		reply(w, `{"ok":true,"result":{"message_id":1}}`)
	})
	receiver := lo.NewReceiver(lo.ReceiverConfig{
		Service: svc, Transport: lo.NewTransport(api, false), IsAllowed: func(int64) bool { return true },
		Guards: func(int64) (bool, string) { return false, "Daily cap reached." },
	})
	receiver.HandleUpdate(t.Context(), lo.Update{Message: inbound("do work")})
	receiver.HandleUpdate(t.Context(), lo.Update{Message: inbound("/stop")})
	if len(svc.prompts) != 0 || svc.stopped != 1 {
		t.Fatalf("spy=%+v", svc)
	}
}

func TestPollingAcknowledgesInOrderAndStopsOnConflict(t *testing.T) {
	t.Parallel()
	const lastID = int64(9007199254740995)
	var calls atomic.Int32
	api := client(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Offset int64 `json:"offset"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if calls.Add(1) == 1 {
			if body.Offset != 0 {
				t.Errorf("initial offset=%d", body.Offset)
			}
			a, b := inbound("second"), inbound("first")
			data, err := json.Marshal(map[string]any{"ok": true, "result": []lo.Update{
				{ID: lastID, Message: a}, {ID: lastID - 1, Message: b}, {ID: lastID - 1, Message: b},
			}})
			if err != nil {
				t.Error(err)
			}
			reply(w, string(data))
			return
		}
		if body.Offset != lastID+1 {
			t.Errorf("ack offset=%d", body.Offset)
		}
		w.WriteHeader(http.StatusConflict)
		reply(w, `{"ok":false,"error_code":409,"description":"another poller"}`)
	})
	svc := &serviceSpy{}
	receiver := lo.NewReceiver(lo.ReceiverConfig{
		Service: svc, Client: api, Transport: lo.NewTransport(api, false), IsAllowed: func(int64) bool { return true },
	})
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	err := receiver.Run(ctx)
	var apiErr *lo.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != http.StatusConflict {
		t.Fatal(err)
	}
	if strings.Join(svc.prompts, ",") != "first,second" {
		t.Fatal(svc.prompts)
	}
}

func TestMentionRemovalPreservesEarlierPrefixAndWhitespace(t *testing.T) {
	t.Parallel()
	svc := &serviceSpy{}
	receiver := lo.NewReceiver(lo.ReceiverConfig{
		Service: svc, Username: "flock", IsAllowed: func(int64) bool { return true },
	})
	receiver.HandleUpdate(t.Context(), lo.Update{Message: inbound("@flockbot  and\n@FLOCK fix this")})
	if len(svc.prompts) != 1 || svc.prompts[0] != "@flockbot  and\n fix this" {
		t.Fatal(svc.prompts)
	}
}

func TestLongCommandNoticeIsChunkedWithoutLosingText(t *testing.T) {
	t.Parallel()
	const reasonRunes = 5000
	reason := strings.Repeat("😀", reasonRunes)
	var mu sync.Mutex
	var parts []string
	api := client(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if utf8.RuneCountInString(body.Text) > 2048 {
			t.Error("oversized chunk")
		}
		mu.Lock()
		parts = append(parts, body.Text)
		mu.Unlock()
		reply(w, `{"ok":true,"result":{"message_id":1}}`)
	})
	receiver := lo.NewReceiver(lo.ReceiverConfig{
		Service: &serviceSpy{}, Transport: lo.NewTransport(api, false), IsAllowed: func(int64) bool { return true },
		Guards: func(int64) (bool, string) { return false, reason },
	})
	receiver.HandleUpdate(t.Context(), lo.Update{Message: inbound("work")})
	mu.Lock()
	defer mu.Unlock()
	if len(parts) < 2 || strings.Join(parts, "") != reason {
		t.Fatal("long notice was lost or truncated")
	}
}

func TestIgnoredMessagesDoNotSpendGuardBudget(t *testing.T) {
	t.Parallel()
	svc := &serviceSpy{}
	api := client(t, func(w http.ResponseWriter, _ *http.Request) { reply(w, `{"ok":true,"result":{"message_id":1}}`) })
	calls := 0
	receiver := lo.NewReceiver(lo.ReceiverConfig{
		Service: svc, Transport: lo.NewTransport(api, false), Username: "flock",
		IsAllowed: func(int64) bool { return true }, RequireMention: true,
		Guards: func(int64) (bool, string) { calls++; return true, "" },
	})
	media := inbound("photo")
	media.Photo = json.RawMessage(`[{}]`)
	receiver.HandleUpdate(t.Context(), lo.Update{Message: media})
	empty := inbound("@flock")
	empty.Chat.ID, empty.Chat.Type = -42, "group"
	receiver.HandleUpdate(t.Context(), lo.Update{Message: empty})
	receiver.HandleUpdate(t.Context(), lo.Update{Message: inbound("")})
	if calls != 0 {
		t.Fatalf("ignored messages spent %d guard calls", calls)
	}
	receiver.HandleUpdate(t.Context(), lo.Update{Message: inbound("hello")})
	if calls != 1 || len(svc.prompts) != 1 {
		t.Fatalf("calls=%d prompts=%v", calls, svc.prompts)
	}
}

func TestStaleOnlyPollingBatchBacksOff(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	stale := make(chan struct{})
	api := client(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 2 {
			close(stale)
		}
		reply(w, `{"ok":true,"result":[{"update_id":1}]}`)
	})
	receiver := lo.NewReceiver(lo.ReceiverConfig{Client: api})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- receiver.Run(ctx) }()
	select {
	case <-stale:
	case <-ctx.Done():
		t.Fatal("poller did not reach the stale batch")
	}
	// Start the observation window after the second request, not at process startup.
	timer := time.NewTimer(150 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("polls=%d; expected initial batch then one stale batch", got)
	}
}

func TestPollingStopsOnPermanentHTTPFailure(t *testing.T) {
	t.Parallel()
	for _, status := range []int{
		http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			api := client(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
				reply(w, "unsupported")
			})
			receiver := lo.NewReceiver(lo.ReceiverConfig{Client: api})
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			err := receiver.Run(ctx)
			var apiErr *lo.APIError
			if !errors.As(err, &apiErr) || apiErr.Code != status || calls.Load() != 1 {
				t.Fatalf("calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestQuotedAttachmentIsExplicitInPrompt(t *testing.T) {
	t.Parallel()
	for _, caption := range []string{"", "diagram caption"} {
		svc := &serviceSpy{}
		receiver := lo.NewReceiver(lo.ReceiverConfig{Service: svc, IsAllowed: func(int64) bool { return true }})
		msg := inbound("explain this")
		msg.Reply = inbound("")
		msg.Reply.Caption = caption
		msg.Reply.Photo = json.RawMessage(`[{}]`)
		receiver.HandleUpdate(t.Context(), lo.Update{Message: msg})
		if len(svc.prompts) != 1 ||
			!strings.Contains(svc.prompts[0], "Quoted attachment is unavailable") || !strings.Contains(svc.prompts[0], caption) {
			t.Fatalf("prompts=%v", svc.prompts)
		}
	}
}

func TestPollingRecoversFromTransientConflict(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var calls atomic.Int32
	api := client(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusConflict)
			reply(w, `{"ok":false,"error_code":409,"description":"previous poll still closing"}`)
			return
		}
		cancel()
		reply(w, `{"ok":true,"result":[]}`)
	})
	err := lo.NewReceiver(lo.ReceiverConfig{Client: api}).Run(ctx)
	if !errors.Is(err, context.Canceled) || calls.Load() != 2 {
		t.Fatalf("polls=%d err=%v", calls.Load(), err)
	}
}
