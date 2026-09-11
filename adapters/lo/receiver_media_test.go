package lo_test

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/duckbugio/flock/adapters/lo"
)

// photoAPI answers the two calls a photo download makes — getFile then the byte fetch —
// and records the notices the bot sends back to the chat.
func photoAPI(t *testing.T, bytes string) (*lo.Client, *[]string) {
	t.Helper()
	var notices []string
	api := client(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			reply(w, `{"ok":true,"result":{"file_id":"ref","file_path":"ref"}}`)
		case strings.Contains(r.URL.Path, "/file/bot"):
			_, _ = w.Write([]byte(bytes))
		default:
			body := make([]byte, 4096)
			n, _ := r.Body.Read(body)
			notices = append(notices, string(body[:n]))
			reply(w, `{"ok":true,"result":{"message_id":1}}`)
		}
	})
	return api, &notices
}

// The point of the whole feature: an image the user sends reaches the agent as a file it
// can open, and the caption still reads as the request.
func TestPhotoReachesTheAgentAsAPath(t *testing.T) {
	t.Parallel()
	svc := &serviceSpy{}
	api, notices := photoAPI(t, "PNGDATA")
	base := t.TempDir()
	receiver := lo.NewReceiver(lo.ReceiverConfig{
		Service: svc, Client: api, Transport: lo.NewTransport(api, false),
		IsAllowed: func(int64) bool { return true },
		Uploads:   lo.NewUploader(api, fakeUploads{dir: base}, 0, nil),
	})

	msg := inbound("")
	msg.Caption = "what is wrong here?"
	msg.Photo = []lo.PhotoSize{
		{FileID: "small", Width: 90, Height: 90},
		{FileID: "ref", Width: 1280, Height: 720},
	}
	receiver.HandleUpdate(t.Context(), lo.Update{Message: msg})

	if len(svc.prompts) != 1 {
		t.Fatalf("prompts=%v, want the photo to start one run", svc.prompts)
	}
	prompt := svc.prompts[0]
	if !strings.Contains(prompt, "what is wrong here?") {
		t.Fatalf("caption lost: %q", prompt)
	}
	saved := savedPathFrom(t, prompt)
	data, err := os.ReadFile(saved) //nolint:gosec // Path comes from the prompt the code built.
	if err != nil || string(data) != "PNGDATA" {
		t.Fatalf("saved file content=%q err=%v", data, err)
	}
	if len(*notices) != 0 {
		t.Fatalf("a working download still sent a notice: %v", *notices)
	}
}

// A photo with no caption is a complete request ("look at this"), so it must not be
// swallowed by the empty-text check that drops chatter.
func TestCaptionlessPhotoStillStartsARun(t *testing.T) {
	t.Parallel()
	svc := &serviceSpy{}
	api, _ := photoAPI(t, "PNG")
	receiver := lo.NewReceiver(lo.ReceiverConfig{
		Service: svc, Client: api, Transport: lo.NewTransport(api, false),
		IsAllowed: func(int64) bool { return true },
		Uploads:   lo.NewUploader(api, fakeUploads{dir: t.TempDir()}, 0, nil),
	})

	msg := inbound("")
	msg.Photo = []lo.PhotoSize{{FileID: "ref", Width: 800, Height: 600}}
	receiver.HandleUpdate(t.Context(), lo.Update{Message: msg})

	if len(svc.prompts) != 1 || !strings.Contains(svc.prompts[0], "attached an image") {
		t.Fatalf("prompts=%v", svc.prompts)
	}
}

// Each unreadable kind gets its own sentence, and none of them starts a run: answering the
// caption alone would be answering a question whose subject never arrived.
func TestUnreadableAttachmentsExplainThemselvesAndStopTheRun(t *testing.T) {
	t.Parallel()
	// The notice TEXT is asserted, not merely its count. Per-kind sentences are what this
	// whole path is for — "I cannot read that" for everything would have been one line — so a
	// test that counts notices would stay green through any mix-up in the table.
	for name, tc := range map[string]struct {
		attach func(*lo.Message)
		says   string
	}{
		"document": {
			func(m *lo.Message) { m.Document = &lo.Attachment{FileID: "d", FileName: "spec.pdf"} },
			"documents", // Names the kind; the sentence deliberately claims nothing about the platform.
		},
		"voice": {func(m *lo.Message) { m.Voice = &lo.Attachment{FileID: "v"} }, "voice"},
		"video": {func(m *lo.Message) { m.Video = &lo.Attachment{FileID: "m"} }, "video"},
		"audio": {func(m *lo.Message) { m.Audio = &lo.Attachment{FileID: "a"} }, "audio"},
		"sticker": {
			func(m *lo.Message) { m.Sticker = &lo.Attachment{FileID: "s"} },
			"Stickers",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc := &serviceSpy{}
			api, notices := photoAPI(t, "")
			receiver := lo.NewReceiver(lo.ReceiverConfig{
				Service: svc, Client: api, Transport: lo.NewTransport(api, false),
				IsAllowed: func(int64) bool { return true },
				Uploads:   lo.NewUploader(api, fakeUploads{dir: t.TempDir()}, 0, nil),
			})
			msg := inbound("review this")
			tc.attach(msg)
			receiver.HandleUpdate(t.Context(), lo.Update{Message: msg})

			if len(svc.prompts) != 0 {
				t.Fatalf("%s started a run without its contents: %v", name, svc.prompts)
			}
			if len(*notices) != 1 {
				t.Fatalf("%s produced %d notices, want exactly one", name, len(*notices))
			}
			if !strings.Contains((*notices)[0], tc.says) {
				t.Fatalf("the %s notice does not name the kind it refused: %s", name, (*notices)[0])
			}
		})
	}
}

// A download that fails must tell the user. The old behaviour — silence — left them
// watching a delivered file that the bot never mentioned again.
func TestFailedPhotoDownloadNotifiesInsteadOfSilence(t *testing.T) {
	t.Parallel()
	svc := &serviceSpy{}
	api, notices := photoAPI(t, "PNG")
	src := &fakeSource{fileErr: errors.New("getFile exploded")}
	receiver := lo.NewReceiver(lo.ReceiverConfig{
		Service: svc, Client: api, Transport: lo.NewTransport(api, false),
		IsAllowed: func(int64) bool { return true },
		Uploads:   lo.NewUploader(src, fakeUploads{dir: t.TempDir()}, 0, nil),
	})

	msg := inbound("look")
	msg.Photo = []lo.PhotoSize{{FileID: "ref", Width: 10, Height: 10}}
	receiver.HandleUpdate(t.Context(), lo.Update{Message: msg})

	if len(svc.prompts) != 0 || len(*notices) != 1 {
		t.Fatalf("prompts=%v notices=%v", svc.prompts, *notices)
	}
}

// Without an uploads directory there is nowhere to put the bytes; say so rather than
// pretending the image was read.
func TestPhotoWithoutUploaderIsRefusedExplicitly(t *testing.T) {
	t.Parallel()
	svc := &serviceSpy{}
	api, notices := photoAPI(t, "PNG")
	receiver := lo.NewReceiver(lo.ReceiverConfig{
		Service: svc, Client: api, Transport: lo.NewTransport(api, false),
		IsAllowed: func(int64) bool { return true },
	})

	msg := inbound("look")
	msg.Photo = []lo.PhotoSize{{FileID: "ref", Width: 10, Height: 10}}
	receiver.HandleUpdate(t.Context(), lo.Update{Message: msg})

	if len(svc.prompts) != 0 || len(*notices) != 1 {
		t.Fatalf("prompts=%v notices=%v", svc.prompts, *notices)
	}
}

func savedPathFrom(t *testing.T, prompt string) string {
	t.Helper()
	const marker = "saved at "
	idx := strings.Index(prompt, marker)
	if idx < 0 {
		t.Fatalf("prompt names no saved file: %q", prompt)
	}
	rest := prompt[idx+len(marker):]
	end := strings.Index(rest, " —")
	if end < 0 {
		t.Fatalf("prompt path is unterminated: %q", prompt)
	}
	return rest[:end]
}

// A rate limit or a spent cost cap is paid with a refusal, not with a download: doing the
// network call and the disk write first means a capped user still costs bandwidth and storage
// on every message they send.
func TestGuardsRunBeforeTheDownload(t *testing.T) {
	t.Parallel()
	svc := &serviceSpy{}
	var fetched int
	api := client(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/file/bot") || strings.HasSuffix(r.URL.Path, "/getFile") {
			fetched++
		}
		reply(w, `{"ok":true,"result":{"message_id":1}}`)
	})
	receiver := lo.NewReceiver(lo.ReceiverConfig{
		Service: svc, Client: api, Transport: lo.NewTransport(api, false),
		IsAllowed: func(int64) bool { return true },
		Guards:    func(int64) (bool, string) { return false, "Daily cap reached." },
		Uploads:   lo.NewUploader(api, fakeUploads{dir: t.TempDir()}, 0, nil),
	})

	msg := inbound("")
	msg.Photo = []lo.PhotoSize{{FileID: "ref", Width: 800, Height: 600}}
	receiver.HandleUpdate(t.Context(), lo.Update{Message: msg})

	if len(svc.prompts) != 0 {
		t.Fatalf("a capped user still started a run: %v", svc.prompts)
	}
	if fetched != 0 {
		t.Fatalf("the refusal still cost %d download calls", fetched)
	}
}
