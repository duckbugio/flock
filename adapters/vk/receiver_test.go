//nolint:testpackage // intentionally whitebox to test unexported VK receiver internals
package vk

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duckbugio/flock/core/agent"
	"github.com/duckbugio/flock/core/chat"
	"github.com/duckbugio/flock/core/codexauth"
	"github.com/duckbugio/flock/core/codexauth/codexauthtest"
	"github.com/duckbugio/flock/core/codexauth/logintest"
	"github.com/duckbugio/flock/core/goal"
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
	goals       map[string]goal.Goal
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

func (f *fakeService) ArmGoal(chatID chat.ChatID, criterion string) (goal.Goal, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.goals == nil {
		f.goals = map[string]goal.Goal{}
	}
	g := goal.Goal{Criterion: criterion, MaxAttempts: 5}
	f.goals[chatID] = g
	return g, true
}

func (f *fakeService) GoalStatus(chatID chat.ChatID) (goal.Goal, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.goals[chatID]
	return g, ok
}

func (f *fakeService) DisarmGoal(chatID chat.ChatID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.goals[chatID]
	delete(f.goals, chatID)
	return ok
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

// seen snapshots the notices delivered so far. /login is dispatched on its own
// goroutine (it must not block the poll loop), so its tests read through this
// rather than touching the slice.
func (n *fakeNotice) seen() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.texts...)
}

// waitUntilNotices waits for exactly want notices, tolerating the detached send.
func waitUntilNotices(t *testing.T, n *fakeNotice, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && len(n.seen()) < want {
		time.Sleep(2 * time.Millisecond)
	}
	if got := n.seen(); len(got) != want {
		t.Fatalf("got %d notices, want %d: %v", len(got), want, got)
	}
}

// awaitOne waits for exactly ONE notice and fails if a second arrives.
//
// Returning on the FIRST notice would have made every "exactly one" assertion
// mean "at least one": a second message almost never lands before the snapshot is
// read. That matters most for the privacy regressions here — they look for the
// device code in notice [0], so an implementation that also broadcast it to the
// conversation as a second message would keep them green.
func (n *fakeNotice) awaitOne(t *testing.T) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && len(n.seen()) == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	got := n.seen()
	if len(got) != 1 {
		t.Fatalf("got %d notices, want 1: %v", len(got), got)
	}
	// Give a stray second a chance to show up, so "exactly one" means it.
	time.Sleep(50 * time.Millisecond)
	if extra := n.seen(); len(extra) != 1 {
		t.Fatalf("got %d notices, want 1: %v", len(extra), extra)
	}
	return got
}

// newTestReceiver builds a Receiver wired to the fakes, allowing user 42, group
// 123, with the given requireMention.
func newTestReceiver(svc Service, notices NoticeSender, requireMention bool, acked *[]string) *Receiver {
	return newTestReceiverOpts(svc, notices, requireMention, acked, nil)
}

// newTestReceiverWithAuth is newTestReceiver with a Codex sign-in manager wired
// through the config, as production does.
func newTestReceiverWithAuth(svc Service, notices NoticeSender, auth *codexauth.Manager) *Receiver {
	return newTestReceiverOpts(svc, notices, false, nil, auth)
}

func newTestReceiverOpts(
	svc Service, notices NoticeSender, requireMention bool, acked *[]string, auth *codexauth.Manager,
) *Receiver {
	return NewReceiver(ReceiverConfig{
		CodexAuth:      auth,
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

// TestReceiverFoldsReplyMessage: a reply_message is folded into the text prompt so
// the run sees the quoted original plus the user's new text.
func TestReceiverFoldsReplyMessage(t *testing.T) {
	svc := &fakeService{}
	r := newTestReceiver(svc, &fakeNotice{}, false, nil)

	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{
		FromID: 42, PeerID: 200, Text: "и что с этим", ConversationMessageID: 10,
		ReplyMessage: &messageObject{FromID: 42, Text: "Проверил функционал Git Runners"},
	}))

	if len(svc.handleCalls) != 1 {
		t.Fatalf("Handle calls = %d, want 1", len(svc.handleCalls))
	}
	got := svc.handleCalls[0].prompt
	if !strings.Contains(got, "Проверил функционал Git Runners") {
		t.Errorf("prompt %q missing the quoted reply original", got)
	}
	if !strings.Contains(got, "и что с этим") {
		t.Errorf("prompt %q missing the user's new text", got)
	}
	if !strings.Contains(got, quotedUserLabel) {
		t.Errorf("prompt %q missing the participant author label", got)
	}
}

// TestReceiverFoldsForwardedMessage: with no reply_message, the FIRST fwd_messages
// entry is used as the quoted original. A community source (from_id < 0) is labeled
// as the assistant.
func TestReceiverFoldsForwardedMessage(t *testing.T) {
	svc := &fakeService{}
	r := newTestReceiver(svc, &fakeNotice{}, false, nil)

	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{
		FromID: 42, PeerID: 200, Text: "look at this", ConversationMessageID: 11,
		FwdMessages: []messageObject{
			{FromID: -100, Text: "forwarded original"},
			{FromID: 42, Text: "second one, ignored"},
		},
	}))

	if len(svc.handleCalls) != 1 {
		t.Fatalf("Handle calls = %d, want 1", len(svc.handleCalls))
	}
	got := svc.handleCalls[0].prompt
	if !strings.Contains(got, "forwarded original") || !strings.Contains(got, "look at this") {
		t.Errorf("prompt %q missing the forwarded original or the user text", got)
	}
	if strings.Contains(got, "second one, ignored") {
		t.Errorf("prompt %q folded more than the first forwarded message", got)
	}
	if !strings.Contains(got, quotedAssistantLabel) {
		t.Errorf("prompt %q should label the community-sourced quote as the assistant", got)
	}
}

// TestReceiverMediaOnlyReplyPlaceholder: a reply_message that carries only an
// attachment (no text) yields a generic "[media]" reference rather than being
// silently dropped.
func TestReceiverMediaOnlyReplyPlaceholder(t *testing.T) {
	svc := &fakeService{}
	r := newTestReceiver(svc, &fakeNotice{}, false, nil)

	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{
		FromID: 42, PeerID: 200, Text: "про это", ConversationMessageID: 12,
		ReplyMessage: &messageObject{
			FromID:      42,
			Attachments: []attachment{{Type: "photo", Photo: &photoAttachment{}}},
		},
	}))

	if len(svc.handleCalls) != 1 {
		t.Fatalf("Handle calls = %d, want 1", len(svc.handleCalls))
	}
	got := svc.handleCalls[0].prompt
	if !strings.Contains(got, "[media]") {
		t.Errorf("prompt %q missing the [media] placeholder for a media-only reply", got)
	}
	if !strings.Contains(got, "про это") {
		t.Errorf("prompt %q missing the user's new text", got)
	}
}

// TestReceiverNoReplyPromptUnchanged: a message with neither reply_message nor
// fwd_messages is submitted byte-for-byte unchanged (the QuotedPrompt no-op).
func TestReceiverNoReplyPromptUnchanged(t *testing.T) {
	svc := &fakeService{}
	r := newTestReceiver(svc, &fakeNotice{}, false, nil)

	const userText = "just a normal message"
	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{
		FromID: 42, PeerID: 200, Text: userText, ConversationMessageID: 13,
	}))

	if len(svc.handleCalls) != 1 {
		t.Fatalf("Handle calls = %d, want 1", len(svc.handleCalls))
	}
	if svc.handleCalls[0].prompt != userText {
		t.Errorf("non-reply prompt = %q, want it byte-for-byte unchanged (%q)", svc.handleCalls[0].prompt, userText)
	}
}

// TestReceiverFoldsMediaOnlyForward: a forwarded message that carries only an
// attachment (no text) yields the generic [media] reference, the same as a media-only
// reply — so the forwarded reference is never silently dropped.
func TestReceiverFoldsMediaOnlyForward(t *testing.T) {
	svc := &fakeService{}
	r := newTestReceiver(svc, &fakeNotice{}, false, nil)

	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{
		FromID: 42, PeerID: 200, Text: "смотри", ConversationMessageID: 14,
		FwdMessages: []messageObject{
			{FromID: 42, Attachments: []attachment{{Type: "doc", Doc: &docAttachment{}}}},
		},
	}))

	if len(svc.handleCalls) != 1 {
		t.Fatalf("Handle calls = %d, want 1", len(svc.handleCalls))
	}
	got := svc.handleCalls[0].prompt
	if !strings.Contains(got, "[media]") {
		t.Errorf("prompt %q missing the [media] placeholder for a media-only forward", got)
	}
	if !strings.Contains(got, "смотри") {
		t.Errorf("prompt %q missing the user's new text", got)
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
	if len(notices.texts) != 1 || notices.texts[0] != welcomeText(chat.WithoutLogin) {
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
	if len(notices.texts) != 1 || notices.texts[0] != helpText(chat.WithoutLogin) {
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
	mgr := schedule.NewManager(store, func(string, string, int64) bool { return true }, nil, nil, nil, nil)
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

// unauthorizedCodexReceiver wires a receiver whose Codex backend has never been
// signed in (an empty CODEX_HOME, so no auth.json). The manager goes in through
// ReceiverConfig, the same way production wires it, so this covers NewReceiver's
// own plumbing instead of reaching past it into the private field.
func unauthorizedCodexReceiver(t *testing.T, svc Service, notices NoticeSender) *Receiver {
	t.Helper()
	auth, _ := logintest.Unauthorized(t)
	return newTestReceiverWithAuth(svc, notices, auth)
}

// TestReceiverBlocksRunsWhileCodexUnauthorized: a Codex deploy with no completed
// sign-in must answer with the actionable notice instead of starting a run that
// could only fail inside the CLI.
func TestReceiverBlocksRunsWhileCodexUnauthorized(t *testing.T) {
	svc := &fakeService{}
	notices := &fakeNotice{}
	r := unauthorizedCodexReceiver(t, svc, notices)

	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{FromID: 42, PeerID: 200, Text: "build it"}))

	if len(svc.handleCalls) != 0 {
		t.Errorf("an unauthorized Codex deploy started %d runs, want 0", len(svc.handleCalls))
	}
	if got := notices.awaitOne(t); !strings.Contains(got[0], "/login") {
		t.Errorf("notices = %v, want one notice pointing at /login", got)
	}
}

// TestReceiverLoginCommandStaysReachableWhileUnauthorized is the escape hatch:
// the very command that fixes the unauthorized state must not be blocked by it.
func TestReceiverLoginCommandStaysReachableWhileUnauthorized(t *testing.T) {
	svc := &fakeService{}
	notices := &fakeNotice{}
	r := unauthorizedCodexReceiver(t, svc, notices)

	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{FromID: 42, PeerID: 42, Text: "/login status"}))

	if len(svc.handleCalls) != 0 {
		t.Errorf("/login should not start a run, got %d Handle calls", len(svc.handleCalls))
	}
	if got := notices.awaitOne(t); !strings.Contains(got[0], "NOT authorized") {
		t.Errorf("notices = %v, want the login status reply", got)
	}
}

// TestReceiverLoginWithoutManagerIsInert: a deployment wired without a Codex auth
// manager (the Claude default) answers /login instead of panicking, and never
// blocks a run.
func TestReceiverLoginWithoutManagerIsInert(t *testing.T) {
	svc := &fakeService{}
	notices := &fakeNotice{}
	r := newTestReceiver(svc, notices, false, nil)

	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{FromID: 42, PeerID: 42, Text: "/login"}))
	if got := notices.awaitOne(t); got[0] != codexauth.NoLoginNeededText {
		t.Errorf("notices = %v, want the no-login-needed reply", got)
	}

	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{FromID: 42, PeerID: 200, Text: "build it"}))
	if len(svc.handleCalls) != 1 {
		t.Errorf("Handle calls = %d, want 1 (no manager must never block)", len(svc.handleCalls))
	}
}

// TestReceiverLoginRefusedInConversation: the reply goes to the PEER, so in a
// community conversation the verification link and one-time code would be
// readable by every participant — and whoever acts on the code first binds THEIR
// ChatGPT account to the bot, sending every later run through it.
func TestReceiverLoginRefusedInConversation(t *testing.T) {
	svc := &fakeService{}
	notices := &fakeNotice{}
	r := unauthorizedCodexReceiver(t, svc, notices)

	// 2000000000+ is VK's conversation (chat) peer range.
	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{FromID: 42, PeerID: 2000000001, Text: "/login"}))

	got := notices.awaitOne(t)
	if !strings.Contains(got[0], "direct message") {
		t.Fatalf("notices = %v, want the refusal pointing at a direct message", got)
	}
	if strings.Contains(got[0], "http") {
		t.Error("the refusal leaked a link into the conversation")
	}
}

// TestReceiverLoginStatusInConversationHidesAPendingCode is the regression for
// the hole an argument-based guard left: /login status ALSO re-shows a pending
// one-time code, so refusing only the code-starting arguments let it into the
// conversation anyway — where anyone, allow-listed or not, could bind their own
// ChatGPT account to the bot.
func TestReceiverLoginStatusInConversationHidesAPendingCode(t *testing.T) {
	svc := &fakeService{}
	notices := &fakeNotice{}
	r := newTestReceiverWithAuth(svc, notices, logintest.PendingLogin(t))

	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{
		FromID: 42, PeerID: 2000000001, Text: "/login status",
	}))

	got := notices.awaitOne(t)
	if strings.Contains(got[0], codexauthtest.DeviceCode) || strings.Contains(got[0], "http") {
		t.Errorf("the pending code or link reached a conversation: %q", got[0])
	}
	if !strings.Contains(got[0], "in progress") {
		t.Errorf("status = %q, want it to still report the pending sign-in", got[0])
	}
}

// TestReceiverLoginStatusInDirectMessageShowsTheCode: the same command in a 1:1
// chat must still re-show the code — that is what makes a lost message
// recoverable.
func TestReceiverLoginStatusInDirectMessageShowsTheCode(t *testing.T) {
	svc := &fakeService{}
	notices := &fakeNotice{}
	r := newTestReceiverWithAuth(svc, notices, logintest.PendingLogin(t))

	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{FromID: 42, PeerID: 42, Text: "/login status"}))

	if got := notices.awaitOne(t); !strings.Contains(got[0], codexauthtest.DeviceCode) {
		t.Errorf("notices = %v, want the pending code re-shown in a direct message", got)
	}
}

// TestReceiverLoginRejectsDisallowedSender: /login (and especially /login force)
// changes the provider credentials for the WHOLE process, so it must be as
// allow-list gated as any other command. onMessageNew returns before dispatching
// any reserved command for a disallowed sender; this pins that.
func TestReceiverLoginRejectsDisallowedSender(t *testing.T) {
	svc := &fakeService{}
	notices := &fakeNotice{}
	r := unauthorizedCodexReceiver(t, svc, notices)

	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{FromID: 999, PeerID: 999, Text: "/login"}))

	if len(notices.texts) != 0 {
		t.Errorf("a disallowed sender got %v, want silence", notices.texts)
	}
}

// TestHelpListsLoginOnlyWhereItApplies mirrors the Telegram adapter: the /login
// line appears only where an interactive sign-in exists, so the two adapters (and
// Telegram's command menu) cannot disagree.
func TestHelpListsLoginOnlyWhereItApplies(t *testing.T) {
	if !strings.Contains(helpText(chat.WithLogin), "/login") {
		t.Error("help omits /login where the sign-in is real")
	}
	if strings.Contains(helpText(chat.WithoutLogin), "/login") {
		t.Error("help advertises /login where there is no sign-in")
	}
	if strings.Contains(welcomeText(chat.WithoutLogin), "/login") {
		t.Error("welcome advertises /login where there is no sign-in")
	}
}

// TestPrivacyFollowsTheDirectMessageInvariant: VK states 1:1 directly — in a
// direct message the peer IS the sender. A range check on the peer id would call
// community peers private too, which is far too loose for the flag that decides
// who may see a code that takes over the bot's account.
func TestPrivacyFollowsTheDirectMessageInvariant(t *testing.T) {
	auth := logintest.PendingLogin(t)

	tests := []struct {
		name      string
		fromID    int64
		peerID    int64
		wantsCode bool
	}{
		{"direct message", 42, 42, true},
		{"conversation", 42, 2000000001, false},
		{"community peer", 42, -1500, false},
		{"peer that is not the sender", 42, 200, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			notices := &fakeNotice{}
			r := newTestReceiverWithAuth(&fakeService{}, notices, auth)

			r.dispatch(context.Background(), msgNewUpdate(t, messageObject{
				FromID: tt.fromID, PeerID: tt.peerID, Text: "/login status",
			}))

			shown := notices.awaitOne(t)
			got := strings.Contains(shown[0], codexauthtest.DeviceCode)
			if got != tt.wantsCode {
				t.Errorf("code shown = %v, want %v (reply: %q)", got, tt.wantsCode, notices.awaitOne(t)[0])
			}
		})
	}
}

// TestHelpLineComesFromTheCanonicalSet: both adapters render the /login help
// line, so it has one home beside the canonical command set. Two literals would
// drift silently — the adapters would still share the applicability predicate but
// not the words.
func TestHelpLineComesFromTheCanonicalSet(t *testing.T) {
	if !strings.Contains(helpText(chat.WithLogin), chat.LoginHelpLine) {
		t.Error("the VK help does not render the canonical /login line")
	}
}

// TestSilentMessagesGetNoUnauthorizedNotice: a sticker or an empty message is
// dropped silently when the deployment is healthy, so an unauthorized one must
// not answer it either — that spends rate limit telling the user about work they
// never asked for.
func TestSilentMessagesGetNoUnauthorizedNotice(t *testing.T) {
	svc := &fakeService{}
	notices := &fakeNotice{}
	r := unauthorizedCodexReceiver(t, svc, notices)

	// No text, no voice, no attachment the receiver would save.
	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{FromID: 42, PeerID: 42, Text: "   "}))

	if got := notices.seen(); len(got) != 0 {
		t.Errorf("an empty message drew %v, want silence", got)
	}
	if len(svc.handleCalls) != 0 {
		t.Errorf("an empty message started %d runs, want 0", len(svc.handleCalls))
	}
}

// TestRealMessagesStillGetTheUnauthorizedNotice is the other half: a message that
// WOULD have run must still be answered.
func TestRealMessagesStillGetTheUnauthorizedNotice(t *testing.T) {
	svc := &fakeService{}
	notices := &fakeNotice{}
	r := unauthorizedCodexReceiver(t, svc, notices)

	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{FromID: 42, PeerID: 42, Text: "build it"}))

	if got := notices.awaitOne(t); !strings.Contains(got[0], "/login") {
		t.Errorf("notice = %q, want it to point at /login", got[0])
	}
}

// TestLoginDoesNotBlockThePollLoop: this receiver processes updates serially, and
// /login can block for seconds (Cancel waits out the login goroutine; each reply
// is a bounded transport call). Handling it inline would stall every chat, so it
// runs detached — dispatch must return without waiting for the reply.
func TestLoginDoesNotBlockThePollLoop(t *testing.T) {
	svc := &fakeService{}
	notices := &fakeNotice{}
	r := newTestReceiverWithAuth(svc, notices, logintest.PendingLogin(t))

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.dispatch(context.Background(), msgNewUpdate(t, messageObject{FromID: 42, PeerID: 42, Text: "/login cancel"}))
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch blocked on /login; the poll loop would stall with it")
	}
	notices.awaitOne(t)
}

// TestClassifyDrivesBothGateAndDispatch pins the single decision: whatever the
// receiver would submit is exactly what the unauthorized gate answers about, and
// the attachment travels WITH that decision. Two independent condition lists
// would agree only until the next attachment type is added to the dispatch switch.
func TestClassifyDrivesBothGateAndDispatch(t *testing.T) {
	r := newTestReceiver(&fakeService{}, &fakeNotice{}, false, nil)
	r.uploads = nil
	r.voice = nil

	tests := []struct {
		name string
		text string
		msg  messageObject
		want workKind
	}{
		{"plain text", "build it", messageObject{}, workText},
		{"nothing at all", "", messageObject{}, workNone},
		{"attachment with no uploader", "", messageObject{Attachments: []attachment{{Type: "doc"}}}, workNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.classify(tt.text, tt.msg); got.kind != tt.want {
				t.Errorf("classify kind = %d, want %d", got.kind, tt.want)
			}
		})
	}
}

// stubUploader satisfies the uploader seam; classify only checks it is present.
type stubUploader struct{}

func (stubUploader) Save(_ context.Context, _ int64, _, _ string) (string, error) { return "", nil }
func (stubUploader) MaxBytes() int64                                              { return 1 << 20 }

// TestClassifyCarriesTheAttachment: every handler dereferences its argument
// unconditionally, so the decision must hand over the attachment it found rather
// than leave the dispatch to look again.
func TestClassifyCarriesTheAttachment(t *testing.T) {
	r := newTestReceiver(&fakeService{}, &fakeNotice{}, false, nil)
	r.uploads = stubUploader{}

	got := r.classify("", messageObject{Attachments: []attachment{{
		Type: "doc", Doc: &docAttachment{URL: "https://vk.example/doc", Title: "notes.txt"},
	}}})
	if got.kind != workDoc {
		t.Fatalf("kind = %d, want workDoc", got.kind)
	}
	if got.doc == nil {
		t.Fatal("classify returned workDoc with no document; the handler would panic")
	}
}

// TestUnauthorizedNoticeIsSentOncePerPeer: without the rate limiter throttling
// it — the gate now runs before the guards, so a refused message costs the user
// nothing — the restraint has to live here. A busy conversation would otherwise
// get a refusal for every message.
func TestUnauthorizedNoticeIsSentOncePerPeer(t *testing.T) {
	svc := &fakeService{}
	notices := &fakeNotice{}
	r := unauthorizedCodexReceiver(t, svc, notices)

	for range 5 {
		r.dispatch(context.Background(), msgNewUpdate(t, messageObject{FromID: 42, PeerID: 42, Text: "build it"}))
	}

	if got := notices.awaitOne(t); !strings.Contains(got[0], "/login") {
		t.Errorf("notice = %q, want it to point at /login", got[0])
	}
	if len(svc.handleCalls) != 0 {
		t.Errorf("started %d runs while unauthorized, want 0", len(svc.handleCalls))
	}
}

// TestUnauthorizedNoticeReturnsAfterAuthorization: told once, but told again the
// next time the deployment lapses — otherwise a returning outage is silent.
func TestUnauthorizedNoticeReturnsAfterAuthorization(t *testing.T) {
	svc := &fakeService{}
	notices := &fakeNotice{}
	auth, home := logintest.Unauthorized(t)
	r := newTestReceiverWithAuth(svc, notices, auth)

	send := func() {
		r.dispatch(context.Background(), msgNewUpdate(t, messageObject{FromID: 42, PeerID: 42, Text: "build it"}))
	}
	send()
	notices.awaitOne(t)

	// Authorized: the message runs, and the "already told them" memory resets.
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
	send()
	if len(svc.handleCalls) != 1 {
		t.Fatalf("Handle calls = %d, want 1 once authorized", len(svc.handleCalls))
	}

	// Lapsed again: announced afresh.
	if err := os.Remove(filepath.Join(home, "auth.json")); err != nil {
		t.Fatalf("remove auth.json: %v", err)
	}
	send()
	waitUntilNotices(t, notices, 2)
}

// TestUserIsAnsweredEvenAfterABackgroundRefusal is the scenario this split
// exists for: a container comes up with a lost auth.json, the restart replay
// refuses and explains to the chat, and the person who then writes must STILL be
// answered. Sharing one registry between the two channels met them with silence,
// in the one feature whose whole job is telling a human that /login is needed.
func TestUserIsAnsweredEvenAfterABackgroundRefusal(t *testing.T) {
	svc := &fakeService{}
	notices := &fakeNotice{}
	auth, _ := logintest.Unauthorized(t)
	r := newTestReceiverWithAuth(svc, notices, auth)

	// A background submission is refused first and takes its own notice.
	if background := auth.NoticeFor("42"); background == "" {
		t.Fatal("the background channel said nothing while unauthorized")
	}

	r.dispatch(context.Background(), msgNewUpdate(t, messageObject{FromID: 42, PeerID: 42, Text: "build it"}))

	if got := notices.awaitOne(t); !strings.Contains(got[0], "/login") {
		t.Errorf("notices = %v, want the user answered despite the background refusal", got)
	}
}
