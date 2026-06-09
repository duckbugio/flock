//nolint:testpackage // intentionally whitebox to test unexported VK receiver internals
package vk

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/duckbugio/flock/core/chat"
	"github.com/duckbugio/flock/core/claude"
)

// fakeService records the Service calls the receiver makes.
type fakeService struct {
	mu          sync.Mutex
	handleCalls []handleCall
	mediaCalls  []handleCall
	stopRunIDs  []string
	stopChats   []string
	starPresses int
}

type handleCall struct {
	chatID, msgID, prompt string
	userID                int64
	images                []claude.ImageInput
}

func (f *fakeService) Handle(_ context.Context, chatID chat.ChatID, userID int64, msgID chat.MessageID, prompt string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handleCalls = append(f.handleCalls, handleCall{chatID: chatID, userID: userID, msgID: msgID, prompt: prompt})
}

func (f *fakeService) HandleMedia(
	_ context.Context, chatID chat.ChatID, userID int64, msgID chat.MessageID, prompt string, images []claude.ImageInput,
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
