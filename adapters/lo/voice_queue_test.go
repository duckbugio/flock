package lo_test

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/duckbugio/flock/adapters/lo"
	"github.com/duckbugio/flock/core/pending"
)

type queuedService struct {
	serviceSpy
	runs  atomic.Int32
	stops atomic.Int32
}

func (s *queuedService) Handle(context.Context, string, int64, string, string) { s.runs.Add(1) }
func (s *queuedService) StopChat(string) bool                                  { s.stops.Add(1); return false }

type heldVoice struct {
	started       chan struct{}
	release       chan struct{}
	respectCancel bool
}

func (v *heldVoice) Transcribe(ctx context.Context, _ string) (string, error) {
	select {
	case v.started <- struct{}{}:
	default:
	}
	if v.respectCancel {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-v.release:
		}
	} else {
		<-v.release
	}
	return "late transcript", nil
}

type observedStore struct {
	pending.Store
	removed chan struct{}
}

func (s *observedStore) Remove(chatID, id string) error {
	err := s.Store.Remove(chatID, id)
	select {
	case s.removed <- struct{}{}:
	default:
	}
	return err
}

func awaitVoiceSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal("voice operation blocked polling or failed to complete")
	}
}

func voiceQueueReceiver(t *testing.T, speech lo.VoiceInput, store pending.Store, svc *queuedService) (*lo.Receiver, func()) {
	t.Helper()
	polled := make(chan struct{}, 1)
	api := client(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bot"+testToken+"/getUpdates" {
			select {
			case polled <- struct{}{}:
			default:
			}
			reply(w, `{"ok":true,"result":[]}`)
			return
		}
		reply(w, `{"ok":true,"result":{"message_id":1}}`)
	})
	receiver := lo.NewReceiver(lo.ReceiverConfig{
		Client: api, Transport: lo.NewTransport(api, false),
		Service: svc, Voice: speech, VoiceStore: store, VoiceConcurrency: 1, IsAllowed: func(int64) bool { return true },
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = receiver.Run(ctx) }()
	awaitVoiceSignal(t, polled)
	stop := func() { cancel(); awaitVoiceSignal(t, done) }
	t.Cleanup(stop)
	return receiver, stop
}

func TestQueuedVoiceDoesNotDelayStopAndLateTranscriptCannotStartRun(t *testing.T) {
	store, err := pending.Open(filepath.Join(t.TempDir(), "voice.json"))
	if err != nil {
		t.Fatal(err)
	}
	observed := &observedStore{Store: store, removed: make(chan struct{}, 1)}
	speech := &heldVoice{started: make(chan struct{}, 1), release: make(chan struct{})}
	svc := &queuedService{}
	receiver, _ := voiceQueueReceiver(t, speech, observed, svc)
	msg := inbound("")
	msg.Voice = lo.RawAttachment("voice-ref")
	admitted := make(chan struct{})
	go func() { receiver.HandleUpdate(t.Context(), lo.Update{Message: msg}); close(admitted) }()
	awaitVoiceSignal(t, admitted)
	awaitVoiceSignal(t, speech.started)
	if len(store.All()["77"]) != 1 {
		t.Fatal("voice was acknowledged before durable queue write")
	}
	receiver.HandleUpdate(t.Context(), lo.Update{Message: inbound("/stop")})
	if svc.stops.Load() != 1 || len(store.All()["77"]) != 0 {
		t.Fatal("stop did not cancel and clear pending voice")
	}
	close(speech.release)
	awaitVoiceSignal(t, observed.removed)
	if svc.runs.Load() != 0 {
		t.Fatal("late transcript started a stopped run")
	}
}

func TestInterruptedVoiceIsReplayedFromDurableReference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voice.json")
	store, err := pending.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	msg := inbound("")
	msg.Voice = lo.RawAttachment("voice-ref")
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enqueue("77", pending.Marker{Prompt: string(raw)}); err != nil {
		t.Fatal(err)
	}
	speech := &heldVoice{started: make(chan struct{}, 1), release: make(chan struct{}), respectCancel: true}
	svc := &queuedService{}
	_, stop := voiceQueueReceiver(t, speech, store, svc)
	awaitVoiceSignal(t, speech.started)
	stop()
	restored, err := pending.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.All()["77"]) != 1 {
		t.Fatal("shutdown discarded interrupted transcription")
	}
	observed := &observedStore{Store: restored, removed: make(chan struct{}, 1)}
	resumed := &heldVoice{started: make(chan struct{}, 1), release: make(chan struct{})}
	close(resumed.release)
	_, _ = voiceQueueReceiver(t, resumed, observed, svc)
	awaitVoiceSignal(t, observed.removed)
	if svc.runs.Load() != 1 || len(restored.All()["77"]) != 0 {
		t.Fatal("restart did not finish exactly the pending reference")
	}
}

func TestVoiceQueueCapacityDoesNotBlockPolling(t *testing.T) {
	store, err := pending.Open(filepath.Join(t.TempDir(), "voice.json"))
	if err != nil {
		t.Fatal(err)
	}
	speech := &heldVoice{started: make(chan struct{}, 1), release: make(chan struct{}), respectCancel: true}
	svc := &queuedService{}
	receiver, _ := voiceQueueReceiver(t, speech, store, svc)
	admitted := make(chan struct{})
	go func() {
		for range 40 {
			msg := inbound("")
			msg.Voice = lo.RawAttachment("voice-ref")
			receiver.HandleUpdate(t.Context(), lo.Update{Message: msg})
		}
		close(admitted)
	}()
	awaitVoiceSignal(t, admitted)
	if got := len(store.All()["77"]); got != 32 {
		t.Fatalf("durable queue size = %d, want 32", got)
	}
	receiver.HandleUpdate(t.Context(), lo.Update{Message: inbound("/stop")})
	if len(store.All()["77"]) != 0 || svc.stops.Load() != 1 || svc.runs.Load() != 0 {
		t.Fatal("full voice queue prevented Stop")
	}
}
