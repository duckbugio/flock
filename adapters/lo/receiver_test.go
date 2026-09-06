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
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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
