package lo_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/duckbugio/flock/adapters/lo"
)

func TestAudioVideoDownloadsReachAgentWithCaption(t *testing.T) {
	for _, kind := range []string{"audio", "video"} {
		t.Run(kind, func(t *testing.T) {
			svc := &serviceSpy{}
			api, notices := photoAPI(t, "media bytes")
			base := t.TempDir()
			r := lo.NewReceiver(lo.ReceiverConfig{
				Service: svc, Client: api, Transport: lo.NewTransport(api, false),
				IsAllowed: func(int64) bool { return true },
				Uploads:   lo.NewUploader(api, fakeUploads{dir: base}, 0, nil),
			})
			msg := inbound("")
			msg.Caption = "inspect this recording"
			mime, ext := "audio/mpeg", ".mp3"
			if kind == "video" {
				mime, ext = "video/mp4", ".mp4"
			}
			raw, err := json.Marshal(lo.Attachment{FileID: "ref", MimeType: mime})
			if err != nil {
				t.Fatal(err)
			}
			if kind == "audio" {
				msg.Audio = raw
			} else {
				msg.Video = raw
			}
			r.HandleUpdate(t.Context(), lo.Update{Message: msg})
			if len(svc.prompts) != 1 || len(*notices) != 0 || !strings.Contains(svc.prompts[0], msg.Caption) {
				t.Fatalf("media did not reach agent: prompts=%v notices=%v", svc.prompts, *notices)
			}
			saved := savedPathFrom(t, svc.prompts[0])
			data, err := os.ReadFile(saved) //nolint:gosec // Path produced by the uploader under the test workspace.
			if err != nil || string(data) != "media bytes" || filepath.Ext(saved) != ext {
				t.Fatalf("saved media=%q data=%q err=%v", saved, data, err)
			}
		})
	}
}

func TestAudioVideoGuardAndMissingBytesNeverStartRun(t *testing.T) {
	for _, denied := range []bool{true, false} {
		resolves, downloads, notices := 0, 0, 0
		svc := &serviceSpy{}
		api := client(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/getFile"):
				resolves++
				reply(w, `{"ok":true,"result":{"file_id":"ref"}}`)
			case strings.Contains(r.URL.Path, "/file/bot"):
				downloads++
			default:
				notices++
				reply(w, `{"ok":true,"result":{"message_id":1}}`)
			}
		})
		r := lo.NewReceiver(lo.ReceiverConfig{
			Service: svc, Client: api, Transport: lo.NewTransport(api, false),
			IsAllowed: func(int64) bool { return true },
			Guards:    func(int64) (bool, string) { return !denied, "Limit reached" },
			Uploads:   lo.NewUploader(api, fakeUploads{dir: t.TempDir()}, 0, nil),
		})
		msg := inbound("review this")
		msg.Video = lo.RawAttachment("ref")
		r.HandleUpdate(t.Context(), lo.Update{Message: msg})
		wantResolves := 1
		if denied {
			wantResolves = 0
		}
		if resolves != wantResolves || downloads != 0 || notices != 1 || len(svc.prompts) != 0 {
			t.Fatalf("denied=%v resolves=%d downloads=%d notices=%d prompts=%v", denied, resolves, downloads, notices, svc.prompts)
		}
	}
}
