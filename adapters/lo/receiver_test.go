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
	"github.com/duckbugio/flock/core/agent"
	"github.com/duckbugio/flock/core/goal"
)

const groupChatType = "group"

type serviceSpy struct {
	prompts     []string
	stopped     int
	newSessions int
	// images records what the model was SHOWN, per run. A separate field from prompts because
	// the two can disagree: a photo whose bytes would not load still starts a run, with the
	// path in the prompt and nothing to look at.
	images [][]agent.ImageInput
}

func (s *serviceSpy) Handle(_ context.Context, _ string, _ int64, _, prompt string) {
	s.prompts = append(s.prompts, prompt)
	s.images = append(s.images, nil)
}

func (s *serviceSpy) HandleMedia(
	_ context.Context, _ string, _ int64, _, prompt string, images []agent.ImageInput,
) {
	s.prompts = append(s.prompts, prompt)
	s.images = append(s.images, images)
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
	group.Chat.Type = groupChatType
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
	if len(svc.prompts) != 3 || svc.prompts[1] != "/stop@other" || svc.prompts[2] != "hello" {
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
	media.Document = &lo.Attachment{FileID: "lo-file", FileName: "notes.txt"}
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
		ConflictDelay: 10 * time.Millisecond,
		Service:       svc, Client: api, Transport: lo.NewTransport(api, false), IsAllowed: func(int64) bool { return true },
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
	// A photo IS work and spends the budget; it has its own test. What must not spend it: an
	// unaddressed group message, an empty one, and an attachment this platform will not hand
	// over — the last because it produces one sentence and no work at all, and charging for it
	// would not even save a message, since a guard refusal sends one too.
	empty := inbound("@flock")
	empty.Chat.ID, empty.Chat.Type = -42, groupChatType
	receiver.HandleUpdate(t.Context(), lo.Update{Message: empty})
	receiver.HandleUpdate(t.Context(), lo.Update{Message: inbound("")})
	sticker := inbound("")
	sticker.Sticker = &lo.Attachment{FileID: "s"}
	receiver.HandleUpdate(t.Context(), lo.Update{Message: sticker})
	voice := inbound("послушай")
	voice.Voice = &lo.Attachment{FileID: "v"}
	receiver.HandleUpdate(t.Context(), lo.Update{Message: voice})
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
		msg.Reply.Photo = []lo.PhotoSize{{FileID: "lo-photo", Width: 90, Height: 90}}
		receiver.HandleUpdate(t.Context(), lo.Update{Message: msg})
		if len(svc.prompts) != 1 ||
			!strings.Contains(svc.prompts[0], "Quoted attachment is unavailable") || !strings.Contains(svc.prompts[0], caption) {
			t.Fatalf("prompts=%v", svc.prompts)
		}
	}
}

func TestPollingRecoversFromTransientConflict(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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
	err := lo.NewReceiver(lo.ReceiverConfig{Client: api, ConflictDelay: 10 * time.Millisecond}).Run(ctx)
	if !errors.Is(err, context.Canceled) || calls.Load() != 2 {
		t.Fatalf("polls=%d err=%v", calls.Load(), err)
	}
}

func TestPollingCancellationInterruptsConflictWait(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	responded := make(chan struct{})
	api := client(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		reply(w, `{"ok":false,"error_code":409,"description":"previous poll still closing"}`)
		close(responded)
	})
	done := make(chan error, 1)
	go func() {
		done <- lo.NewReceiver(lo.ReceiverConfig{Client: api}).Run(ctx)
	}()
	<-responded
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt polling")
	}
}

func TestReplyToBotPassesGroupMentionGate(t *testing.T) {
	t.Parallel()
	for _, replyID := range []int64{99, 100, 0} {
		svc := &serviceSpy{}
		r := lo.NewReceiver(lo.ReceiverConfig{
			Service: svc, BotID: 99, Username: "flock",
			RequireMention: true, IsAllowed: func(int64) bool { return true },
		})
		msg := inbound("continue")
		msg.Chat.Type = groupChatType
		msg.Chat.ID = -42
		msg.Reply = &lo.Message{From: &lo.User{ID: replyID}, Text: "question"}
		r.HandleUpdate(t.Context(), lo.Update{Message: msg})
		if (len(svc.prompts) == 1) != (replyID == 99) {
			t.Fatalf("reply author %d: %v", replyID, svc.prompts)
		}
	}
}

func TestProviderCommandTargetIsPreservedInPrivateChat(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"private", groupChatType} {
		svc := &serviceSpy{}
		r := lo.NewReceiver(lo.ReceiverConfig{Service: svc, Username: "flock", IsAllowed: func(int64) bool { return true }})
		msg := inbound("/deploy@staging ship")
		msg.Chat.Type = kind
		r.HandleUpdate(t.Context(), lo.Update{Message: msg})
		if kind == "private" {
			if len(svc.prompts) != 1 || svc.prompts[0] != msg.Text {
				t.Fatal(svc.prompts)
			}
		} else if len(svc.prompts) != 0 {
			t.Fatal(svc.prompts)
		}
	}
}

func TestReplyContextIdentifiesAssistantAndEmptyMessage(t *testing.T) {
	t.Parallel()
	svc := &serviceSpy{}
	receiver := lo.NewReceiver(lo.ReceiverConfig{Service: svc, BotID: 99, IsAllowed: func(int64) bool { return true }})
	msg := inbound("continue")
	msg.Reply = &lo.Message{From: &lo.User{ID: 99, Username: "flock"}}
	receiver.HandleUpdate(t.Context(), lo.Update{Message: msg})
	if len(svc.prompts) != 1 || !strings.Contains(svc.prompts[0], "the assistant") || !strings.Contains(svc.prompts[0], "[media]") {
		t.Fatalf("prompts=%v", svc.prompts)
	}
}

// pollSpy is serviceSpy's synchronised twin, for the one test that drives Receiver.Run on its
// own goroutine. The shared spy is deliberately lock-free — every other test calls HandleUpdate
// directly — and adding a mutex there would make the simple tests pay for this one.
type pollSpy struct {
	prompts chan string
}

func (p *pollSpy) Handle(_ context.Context, _ string, _ int64, _, prompt string) {
	p.prompts <- prompt
}

func (p *pollSpy) HandleMedia(
	_ context.Context, _ string, _ int64, _, prompt string, _ []agent.ImageInput,
) {
	p.prompts <- prompt
}
func (*pollSpy) StopChat(string) bool                     { return false }
func (*pollSpy) NewSession(string) error                  { return nil }
func (*pollSpy) ArmGoal(string, string) (goal.Goal, bool) { return goal.Goal{}, false }
func (*pollSpy) GoalStatus(string) (goal.Goal, bool)      { return goal.Goal{}, false }
func (*pollSpy) DisarmGoal(string) bool                   { return false }

// One update this adapter cannot decode must not stop it answering everyone else. Since the
// media fields became typed, an unexpected shape in any of them fails that update's Unmarshal;
// if that failed the whole batch, the offset would never advance and the poller would ask for
// the same poisoned batch forever.
func TestUndecodableUpdateDoesNotStopTheBatch(t *testing.T) {
	t.Parallel()
	svc := &pollSpy{prompts: make(chan string, 8)}
	offsets := make(chan int64, 8)
	api := client(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/getUpdates") {
			reply(w, `{"ok":true,"result":{"message_id":1}}`)
			return
		}
		var body struct {
			Offset int64 `json:"offset"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		select {
		case offsets <- body.Offset:
		default:
		}
		if body.Offset > 0 {
			reply(w, `{"ok":true,"result":[]}`)
			return
		}
		const chat = `"chat":{"id":7,"type":"private"},"from":{"id":7}`
		// The middle update models `document` as a string where this adapter expects an
		// object, and the LAST one spells its own update_id as a string — the case that used
		// to be skipped WITHOUT advancing the offset, so a poisoned tail re-fetched forever.
		reply(w, `{"ok":true,"result":[
			{"update_id":10,"message":{"message_id":1,"text":"first",`+chat+`}},
			{"update_id":11,"message":{"message_id":2,"text":"poison",`+chat+`,"document":"not-an-object"}},
			{"update_id":12,"message":{"message_id":3,"text":"third",`+chat+`}},
			{"update_id":"13","message":{"message_id":4,"text":"tail",`+chat+`}}
		]}`)
	})
	receiver := lo.NewReceiver(lo.ReceiverConfig{
		Service: svc, Client: api, Transport: lo.NewTransport(api, false),
		IsAllowed: func(int64) bool { return true },
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- receiver.Run(ctx) }()
	defer func() { cancel(); <-done }()

	// Every readable message must arrive — including the TAIL, whose only broken field is its
	// own id: the body beside it is whole, so answering it costs nothing and losing it would
	// mean an id spelling could silence the bot for every message in every batch. Before the
	// fix none of them arrived: the batch failed to decode as a whole, the offset never moved,
	// and the poller re-asked for it forever.
	var got []string
	for range 3 {
		select {
		case prompt := <-svc.prompts:
			got = append(got, prompt)
		case <-time.After(4 * time.Second):
			t.Fatalf("the batch stalled; prompts so far: %v", got)
		}
	}
	if !strings.Contains(got[0], "first") || !strings.Contains(got[1], "third") ||
		!strings.Contains(got[2], "tail") {
		t.Fatalf("prompts=%v, want every readable message in order", got)
	}
	// The poisoned one must NOT arrive: its body is what failed to decode.
	select {
	case extra := <-svc.prompts:
		t.Fatalf("prompt %q arrived, want the unreadable body dropped", extra)
	case <-time.After(200 * time.Millisecond):
	}
	// The offset must have moved PAST the poisoned update, not stopped at it.
	select {
	case <-offsets: // the first poll, from zero
		select {
		case next := <-offsets:
			if next <= 13 {
				t.Fatalf("second poll asked from offset %d, want past the poisoned tail", next)
			}
		case <-time.After(4 * time.Second):
			t.Fatal("no second poll: the offset never advanced")
		}
	default:
		t.Fatal("the poller never asked for updates")
	}
}
