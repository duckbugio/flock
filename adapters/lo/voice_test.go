package lo_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/duckbugio/flock/adapters/lo"
)

type speechSpy struct {
	bytes string
	calls int
}

func (s *speechSpy) Transcribe(_ context.Context, audio io.Reader, _ string) (string, error) {
	raw, err := io.ReadAll(audio)
	s.bytes = string(raw)
	s.calls++
	return "review the changes", err
}

func TestVoiceDownloadAndTranscription(t *testing.T) {
	t.Parallel()
	api, _ := photoAPI(t, "OggSvoice")
	speech := &speechSpy{}
	v := lo.NewVoiceTranscriber(api, speech, 100)
	text, err := v.Transcribe(t.Context(), "voice-ref")
	if err != nil || text != "review the changes" || speech.bytes != "OggSvoice" {
		t.Fatalf("text=%q bytes=%q err=%v", text, speech.bytes, err)
	}
}

func TestOversizeVoiceNeverCallsProvider(t *testing.T) {
	t.Parallel()
	api, _ := photoAPI(t, "OggSvoice")
	speech := &speechSpy{}
	_, err := lo.NewVoiceTranscriber(api, speech, 4).Transcribe(t.Context(), "voice-ref")
	if !errors.Is(err, lo.ErrUploadTooLarge) || speech.calls != 0 {
		t.Fatalf("err=%v calls=%d", err, speech.calls)
	}
}

type voiceInputSpy struct {
	calls int
	text  string
	err   error
}

func (v *voiceInputSpy) Transcribe(context.Context, string) (string, error) {
	v.calls++
	return v.text, v.err
}

func TestVoiceAdmissionAndFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                string
		allowed, guard      bool
		transcript          string
		err                 error
		wantCalls, wantRuns int
	}{
		{"success", true, true, "review changes", nil, 1, 1},
		{"denied user", false, true, "review changes", nil, 0, 0},
		{"cost cap", true, false, "review changes", nil, 0, 0},
		{"empty transcript", true, true, " ", nil, 1, 0},
		{"download failure", true, true, "", lo.ErrNoBytes, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api, _ := photoAPI(t, "")
			svc := &serviceSpy{}
			speech := &voiceInputSpy{text: tc.transcript, err: tc.err}
			receiver := lo.NewReceiver(lo.ReceiverConfig{
				Service: svc, Client: api, Transport: lo.NewTransport(api, false),
				IsAllowed: func(int64) bool { return tc.allowed },
				Guards:    func(int64) (bool, string) { return tc.guard, "budget spent" }, Voice: speech,
			})
			msg := inbound("")
			msg.Voice = lo.RawAttachment("voice-ref")
			receiver.HandleUpdate(t.Context(), lo.Update{Message: msg})
			if speech.calls != tc.wantCalls || len(svc.prompts) != tc.wantRuns {
				t.Fatalf("calls=%d prompts=%v", speech.calls, svc.prompts)
			}
			if tc.wantRuns > 0 && !strings.Contains(svc.prompts[0], tc.transcript) {
				t.Fatalf("transcript lost: %v", svc.prompts)
			}
		})
	}
}
