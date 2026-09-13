package lo

import (
	"path/filepath"
	"testing"

	"github.com/duckbugio/flock/core/dispatch"
	"github.com/duckbugio/flock/core/pending"
)

func TestStopCallbackOnlyClearsVoiceForAnActiveRun(t *testing.T) {
	t.Parallel()
	for _, finished := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "finished"}[finished], func(t *testing.T) {
			t.Parallel()
			store, err := pending.Open(filepath.Join(t.TempDir(), "voice.json"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Enqueue("77", pending.Marker{Prompt: "voice"}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Enqueue("88", pending.Marker{Prompt: "other chat"}); err != nil {
				t.Fatal(err)
			}
			receiver := NewReceiver(ReceiverConfig{Callbacks: &callbackSpy{finished: finished}, VoiceStore: store})
			receiver.voiceJobs = dispatch.New(1)
			defer receiver.voiceJobs.Close()
			stopped := receiver.stopCallbackRun("77", "1")
			if stopped == finished {
				t.Fatalf("stopped=%v finished=%v", stopped, finished)
			}
			remaining := len(store.All()["77"])
			if (remaining > 0) != finished {
				t.Fatalf("stale/current voice state wrong: %d", remaining)
			}
			if len(store.All()["88"]) != 1 {
				t.Fatal("another chat's voice queue was cleared")
			}
		})
	}
}
