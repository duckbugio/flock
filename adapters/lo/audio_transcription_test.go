package lo_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/duckbugio/flock/adapters/lo"
)

func TestAudioTranscriptionPreservesContainerAndCaption(t *testing.T) {
	for _, tc := range []struct {
		mime, name, want string
	}{
		{"audio/mpeg", "", "audio.mp3"},
		{"audio/mp4", "wrong.mp3", "audio.m4a"},
		{"audio/ogg", "", "audio.ogg"},
		{"audio/wav", "", "audio.wav"},
		{"audio/flac", "", "audio.flac"},
		{"audio/webm", "", "audio.webm"},
		{"", "../../recording.M4A", "audio.m4a"},
		{"", "", "audio.mp3"},
	} {
		t.Run(tc.want+tc.mime, func(t *testing.T) {
			api, notices := photoAPI(t, "recording bytes")
			speech, svc := &speechSpy{}, &serviceSpy{}
			r := lo.NewReceiver(lo.ReceiverConfig{
				Service: svc, Client: api, Transport: lo.NewTransport(api, false),
				IsAllowed: func(int64) bool { return true },
				Voice:     lo.NewVoiceTranscriber(api, speech, 100),
			})
			msg := inbound("")
			msg.Caption = "Summarize the action items"
			raw, err := json.Marshal(lo.Attachment{FileID: "audio-ref", FileName: tc.name, MimeType: tc.mime})
			if err != nil {
				t.Fatal(err)
			}
			msg.Audio = raw
			r.HandleUpdate(t.Context(), lo.Update{Message: msg})
			if speech.name != tc.want || speech.bytes != "recording bytes" || speech.calls != 1 || len(svc.prompts) != 1 ||
				len(*notices) != 0 || !strings.Contains(svc.prompts[0], msg.Caption) ||
				!strings.Contains(svc.prompts[0], "review the changes") {
				t.Fatalf("audio context lost: speech=%+v prompts=%v notices=%v", speech, svc.prompts, *notices)
			}
		})
	}
}
