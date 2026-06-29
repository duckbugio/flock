//nolint:testpackage // intentionally whitebox to test unexported VK receiver internals
package vk

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/duckbugio/flock/core/agent"
	"github.com/duckbugio/flock/core/chat"
	"github.com/duckbugio/flock/core/schedule"
)

// fakeService records the Service calls the receiver makes.
type fakeService struct {
	mu          sync.Mutex
	handleCalls []handleCall
	mediaCalls  []handleCall
	stopRunIDs  []string
	stopChats   []string
	newSessions []string
	starPresses int
}

type handleCall struct {
	chatID, msgID, prompt string
	userID                int64
	images                []agent.ImageInput
}

func (f *fakeService) Handle(_ context.Context, chatID chat.ChatID, userID int64, msgID chat.MessageID, prompt string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handleCalls = append(f.handleCalls, handleCall{chatID: chatID, userID: userID, msgID: msgID, prompt: prompt})
}

func (f *fakeService) HandleMedia(
	_ context.Context, chatID chat.ChatID, userID int64, msgID chat.MessageID, prompt string, images []agent.ImageInput,
) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mediaCalls = append(f.mediaCalls, handleCall{chatID: chatID, userID: userID, msgID: msgID, prompt: prompt, images: images})
}

func (f *fakeService) Stop(runID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopRunIDs = append(f.stopRunIDs, runID)
	return true
}

func (f *fakeService) StopChat(chatID chat.ChatID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopChats = append(f.stopChats, chatID)
	return true
}

func (f *fakeService) NewSession(chatID chat.ChatID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.newSessions = append(f.newSessions, chatID)
	return nil
}

func (f *fakeService) StarPress() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starPresses++
	return "ok", true
}

// fakeNotice records notice calls.
type fakeNotice struct {
	mu    sync.Mutex
	texts []string
}

func (n *fakeNotice) Notify(_ context.Context, _ int64, text string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.texts = append(n.texts, text)
}

// newTestReceiver builds a Receiver wired to the fakes, allowing user 42, group
// 123, with the given requireMention.
func newTestReceiver(svc Service, notices NoticeSender, requireMention bool, acked *[]string) *Receiver {
	return NewReceiver(ReceiverConfig{
		Service:        svc,
		GroupID:        123,
		RequireMention: requireMention,
		IsAllowed:      func(uid int64) bool { return uid == 42 },
		Notices:        notices,
		EventAck: func(_ context.Context, eventID string, _, _ int64) error {
			if acked != nil {
				*acked = append(*acked, eventID)
			}
			return nil
		},
	})
}

// msgNewUpdate builds a message_new update from a messageObject.
func msgNewUpdate(t *testing.T, msg messageObject) update {
	t.Helper()
	obj, err := json.Marshal(messageNewObject{Message: msg})
	if err != nil {
		t.Fatalf("marshal message_new: %v", err)
	}
	return update{Type: "message_new", Object: obj}
}

func TestReceiverMessageNewFromAllowedUser(t *testing.T) {
	svc := &fakeService{}
	r := newTestReceiver(svc, &fakeNotice{}, false, nil)

	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{
		FromID: 42, PeerID: 200, Text: "hello duck", ConversationMessageID: 9,
	}))

	if len(svc.handleCalls) != 1 {
		t.Fatalf("Handle calls = %d, want 1", len(svc.handleCalls))
	}
	got := svc.handleCalls[0]
	if got.chatID != "200" || got.userID != 42 || got.msgID != "9" || got.prompt != "hello duck" {
		t.Errorf("Handle call = %+v, want chat=200 user=42 msg=9 prompt='hello duck'", got)
	}
}

func TestReceiverIgnoresDisallowedAndCommunityMessages(t *testing.T) {
	svc := &fakeService{}
	r := newTestReceiver(svc, &fakeNotice{}, false, nil)

	// Disallowed user.
	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{FromID: 99, PeerID: 200, Text: "hi"}))
	// Community (negative from_id) — ignored.
	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{FromID: -100, PeerID: 200, Text: "echo"}))

	if len(svc.handleCalls) != 0 {
		t.Errorf("Handle calls = %d, want 0 (disallowed + community ignored)", len(svc.handleCalls))
	}
}

func TestReceiverMentionGateInGroup(t *testing.T) {
	svc := &fakeService{}
	// peer_id >= 2000000000 is a group conversation; requireMention=true.
	r := newTestReceiver(svc, &fakeNotice{}, true, nil)
	const groupPeer = 2_000_000_001

	// Non-mention group message: ignored.
	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{
		FromID: 42, PeerID: groupPeer, Text: "just chatting", ConversationMessageID: 1,
	}))
	if len(svc.handleCalls) != 0 {
		t.Fatalf("non-mention group message handled (%d calls), want ignored", len(svc.handleCalls))
	}

	// Mention of THIS community: handled, with the mention stripped.
	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{
		FromID: 42, PeerID: groupPeer, Text: "[club123|Duck] build it", ConversationMessageID: 2,
	}))
	if len(svc.handleCalls) != 1 {
		t.Fatalf("mention group message handled %d times, want 1", len(svc.handleCalls))
	}
	if svc.handleCalls[0].prompt != "build it" {
		t.Errorf("mention prompt = %q, want 'build it' (mention stripped)", svc.handleCalls[0].prompt)
	}
}

func TestReceiverStopTextCommand(t *testing.T) {
	svc := &fakeService{}
	r := newTestReceiver(svc, &fakeNotice{}, false, nil)

	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{FromID: 42, PeerID: 200, Text: "/stop"}))
	if len(svc.stopChats) != 1 || svc.stopChats[0] != "200" {
		t.Errorf("StopChat calls = %v, want ['200']", svc.stopChats)
	}
	if len(svc.handleCalls) != 0 {
		t.Errorf("/stop should not start a run, got %d Handle calls", len(svc.handleCalls))
	}
}

// TestReceiverStartTextCommand is the VK /start guard: a bare "/start" replies
// with the welcome notice and does NOT start a run (no Service dispatch).
func TestReceiverStartTextCommand(t *testing.T) {
	svc := &fakeService{}
	notices := &fakeNotice{}
	r := newTestReceiver(svc, notices, false, nil)

	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{FromID: 42, PeerID: 200, Text: "/start"}))

	if len(svc.handleCalls) != 0 {
		t.Errorf("/start should not start a run, got %d Handle calls", len(svc.handleCalls))
	}
	if len(notices.texts) != 1 || notices.texts[0] != welcomeText {
		t.Errorf("notice texts = %v, want one welcome notice", notices.texts)
	}
}

// TestCommandName asserts the VK leading-command parser: "/name args" -> "name"
// (lowercased), non-commands and a lone slash -> "", and "/news" -> "news" (so it
// is NOT confused with the reserved "/new"). VK carries no @botname suffix.
func TestCommandName(t *testing.T) {
	tests := []struct {
		text string
		want string
	}{
		{"/stop", "stop"},
		{"/STOP", "stop"},
		{"  /new  ", "new"},
		{"/new please", "new"},
		{"/loop 5m /foo", "loop"},
		{"/news", "news"},
		{"hello", ""},
		{"", ""},
		{"/", ""},
		{"not /new", ""},
		{"/help\nmore", "help"},
	}
	for _, tc := range tests {
		t.Run(tc.text, func(t *testing.T) {
			if got := commandName(tc.text); got != tc.want {
				t.Fatalf("commandName(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

// TestReceiverHelpTextCommand: a bare "/help" replies with the static HelpText
// notice and does NOT start a run.
func TestReceiverHelpTextCommand(t *testing.T) {
	svc := &fakeService{}
	notices := &fakeNotice{}
	r := newTestReceiver(svc, notices, false, nil)

	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{FromID: 42, PeerID: 200, Text: "/help"}))

	if len(svc.handleCalls) != 0 {
		t.Errorf("/help should not start a run, got %d Handle calls", len(svc.handleCalls))
	}
	if len(notices.texts) != 1 || notices.texts[0] != HelpText {
		t.Errorf("notice texts = %v, want one HelpText notice", notices.texts)
	}
}

// TestReceiverNewSessionCommand: a bare "/new" resets the chat's session
// (NewSession) and confirms with a notice, without starting a run.
func TestReceiverNewSessionCommand(t *testing.T) {
	svc := &fakeService{}
	notices := &fakeNotice{}
	r := newTestReceiver(svc, notices, false, nil)

	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{FromID: 42, PeerID: 200, Text: "/new"}))

	if len(svc.newSessions) != 1 || svc.newSessions[0] != "200" {
		t.Errorf("NewSession calls = %v, want ['200']", svc.newSessions)
	}
	if len(svc.handleCalls) != 0 {
		t.Errorf("/new should not start a run, got %d Handle calls", len(svc.handleCalls))
	}
	if len(notices.texts) != 1 || notices.texts[0] != newSessionText {
		t.Errorf("notice texts = %v, want one fresh-session notice", notices.texts)
	}
}

// TestReceiverNativeCommandPassesThrough: a non-reserved slash command (e.g.
// "/loop 5m /foo") is NOT intercepted — it reaches svc.Handle with its FULL
// original text so Claude Code's own slash-command system runs it. This is the
// core of Part A: only the reserved set is intercepted; everything else flows
// through verbatim.
func TestReceiverNativeCommandPassesThrough(t *testing.T) {
	svc := &fakeService{}
	notices := &fakeNotice{}
	r := newTestReceiver(svc, notices, false, nil)

	const prompt = "/loop 5m /foo"
	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{
		FromID: 42, PeerID: 200, Text: prompt, ConversationMessageID: 7,
	}))

	if len(svc.handleCalls) != 1 {
		t.Fatalf("/loop should reach Handle, got %d Handle calls", len(svc.handleCalls))
	}
	if svc.handleCalls[0].prompt != prompt {
		t.Errorf("Handle prompt = %q, want the full original %q", svc.handleCalls[0].prompt, prompt)
	}
	if len(notices.texts) != 0 {
		t.Errorf("/loop should not send a command notice, got %v", notices.texts)
	}
}

// TestReceiverNewsNotTreatedAsNew: "/news" must not be confused with the reserved
// "/new" — exact-name matching means commandName("/news") = "news", which is not
// reserved, so it passes through to Handle (and never resets the session).
func TestReceiverNewsNotTreatedAsNew(t *testing.T) {
	svc := &fakeService{}
	r := newTestReceiver(svc, &fakeNotice{}, false, nil)

	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{
		FromID: 42, PeerID: 200, Text: "/news today", ConversationMessageID: 8,
	}))

	if len(svc.newSessions) != 0 {
		t.Errorf("/news must NOT reset the session, got %v", svc.newSessions)
	}
	if len(svc.handleCalls) != 1 || svc.handleCalls[0].prompt != "/news today" {
		t.Errorf("/news should pass through to Handle with full text, got %+v", svc.handleCalls)
	}
}

// TestReceiverScheduleDisabled: with no scheduler wired (ENABLE_SCHEDULER off),
// "/schedule" replies with the disabled notice and is NOT forwarded to Claude.
func TestReceiverScheduleDisabled(t *testing.T) {
	svc := &fakeService{}
	notices := &fakeNotice{}
	r := newTestReceiver(svc, notices, false, nil) // no scheduler

	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{
		FromID: 42, PeerID: 200, Text: "/schedule list", ConversationMessageID: 5,
	}))

	if len(svc.handleCalls) != 0 {
		t.Errorf("/schedule must not reach Claude when disabled, got %d Handle calls", len(svc.handleCalls))
	}
	if len(notices.texts) != 1 || notices.texts[0] != scheduleDisabledText {
		t.Errorf("notice texts = %v, want one disabled notice", notices.texts)
	}
}

// TestReceiverScheduleEnabledReachesDispatch: with a scheduler wired, "/schedule
// add ..." reaches Manager.Dispatch (the job is persisted) and the reply is sent
// as a notice — never forwarded to Claude.
func TestReceiverScheduleEnabledReachesDispatch(t *testing.T) {
	svc := &fakeService{}
	notices := &fakeNotice{}
	store, err := schedule.Open(filepath.Join(t.TempDir(), "schedules.json"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	mgr := schedule.NewManager(store, func(string, string, int64) bool { return true }, nil, nil, nil)
	r := NewReceiver(ReceiverConfig{
		Service:   svc,
		GroupID:   123,
		IsAllowed: func(uid int64) bool { return uid == 42 },
		Notices:   notices,
		Scheduler: mgr,
	})

	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{
		FromID: 42, PeerID: 200, Text: "/schedule add job 0 9 * * 1 do the thing", ConversationMessageID: 6,
	}))

	if len(svc.handleCalls) != 0 {
		t.Errorf("/schedule must not reach Claude when enabled, got %d Handle calls", len(svc.handleCalls))
	}
	if len(notices.texts) != 1 || !strings.Contains(notices.texts[0], "Added job #") {
		t.Errorf("notice texts = %v, want one 'Added job #...' confirmation", notices.texts)
	}
	if got := store.List("200"); len(got) != 1 || got[0].Prompt != "do the thing" {
		t.Errorf("store after add = %+v, want one job with prompt 'do the thing' for chat 200", got)
	}
}

func TestReceiverMessageEventStopAndAck(t *testing.T) {
	svc := &fakeService{}
	var acked []string
	r := newTestReceiver(svc, &fakeNotice{}, false, &acked)

	ev := messageEventObject{
		UserID:  42,
		PeerID:  200,
		EventID: "evt-1",
		Payload: json.RawMessage(stopPayload("run-77")),
	}
	obj, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	r.dispatch(context.Background(), update{Type: "message_event", Object: obj})

	if len(svc.stopRunIDs) != 1 || svc.stopRunIDs[0] != "run-77" {
		t.Errorf("Stop calls = %v, want ['run-77']", svc.stopRunIDs)
	}
	if len(acked) != 1 || acked[0] != "evt-1" {
		t.Errorf("acked events = %v, want ['evt-1'] (sendMessageEventAnswer)", acked)
	}
}

func TestReceiverMessageEventStarPress(t *testing.T) {
	svc := &fakeService{}
	var acked []string
	r := newTestReceiver(svc, &fakeNotice{}, false, &acked)

	obj, err := json.Marshal(messageEventObject{
		UserID: 42, PeerID: 200, EventID: "evt-2", Payload: json.RawMessage(starPayloadJSON()),
	})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	r.dispatch(context.Background(), update{Type: "message_event", Object: obj})

	if svc.starPresses != 1 {
		t.Errorf("StarPress calls = %d, want 1", svc.starPresses)
	}
	if len(acked) != 1 {
		t.Errorf("star press not acked: %v", acked)
	}
}

// TestRunLoopProcessesUpdatesAndHandlesFailed drives the full Run loop with a
// scripted poll func: first an outdated-ts failed code (1), then a real update,
// then it cancels the context to stop the loop.
func TestRunLoopProcessesUpdatesAndHandlesFailed(t *testing.T) {
	svc := &fakeService{}
	r := newTestReceiver(svc, &fakeNotice{}, false, nil)

	ctx, cancel := context.WithCancel(context.Background())
	msg := msgNewUpdate(t, messageObject{FromID: 42, PeerID: 200, Text: "go", ConversationMessageID: 3})
	lp := &fakeLongPoller{
		server: longPollServer{Key: "k", Server: "srv", TS: "1"},
		responses: []longPollResponse{
			{Failed: failedOutdatedTS, TS: json.Number("5")},
			{TS: json.Number("6"), Updates: []update{msg}},
		},
		onExhausted: cancel, // stop the loop once the scripted responses are delivered
	}

	err := r.Run(ctx, lp)
	if !isContextErr(err) {
		t.Fatalf("Run returned %v, want context cancellation", err)
	}
	if len(svc.handleCalls) != 1 || svc.handleCalls[0].prompt != "go" {
		t.Errorf("Run did not deliver the scripted update: %+v", svc.handleCalls)
	}
}

// fakeLongPoller returns a fixed server descriptor and replays scripted poll
// responses; once exhausted it calls onExhausted (to cancel the loop) and returns
// an empty response.
type fakeLongPoller struct {
	server      longPollServer
	responses   []longPollResponse
	onExhausted func()
	step        int
}

func (f *fakeLongPoller) getLongPollServer(_ context.Context, _ int64) (longPollServer, error) {
	return f.server, nil
}

func (f *fakeLongPoller) poll(_ context.Context, _ longPollServer) (longPollResponse, error) {
	if f.step < len(f.responses) {
		resp := f.responses[f.step]
		f.step++
		return resp, nil
	}
	if f.onExhausted != nil {
		f.onExhausted()
	}
	return longPollResponse{}, nil
}

func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
