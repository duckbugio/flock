package lo_test

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/duckbugio/flock/adapters/lo"
)

// photoAPI answers the two calls a photo download makes — getFile then the byte fetch —
// and records the notices the bot sends back to the chat.
// pngBytes is a real PNG signature. The adapter refuses a "photo" whose bytes are not an
// image, so a fixture spelling its content "PNGDATA" would exercise that refusal instead of
// the path it is written for.
const pngBytes = "\x89PNG\r\n\x1a\n"

func photoAPI(t *testing.T, bytes string) (*lo.Client, *[]string) {
	t.Helper()
	var (
		mu      sync.Mutex
		notices []string
	)
	api := client(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			reply(w, `{"ok":true,"result":{"file_id":"ref","file_path":"ref"}}`)
		case strings.Contains(r.URL.Path, "/file/bot"):
			_, _ = w.Write([]byte(bytes))
		default:
			body := make([]byte, 4096)
			n, _ := r.Body.Read(body)
			mu.Lock()
			notices = append(notices, string(body[:n]))
			mu.Unlock()
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
	api, notices := photoAPI(t, pngBytes)
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
	if err != nil || string(data) != pngBytes {
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
	api, _ := photoAPI(t, pngBytes)
	receiver := lo.NewReceiver(lo.ReceiverConfig{
		Service: svc, Client: api, Transport: lo.NewTransport(api, false),
		IsAllowed: func(int64) bool { return true },
		Uploads:   lo.NewUploader(api, fakeUploads{dir: t.TempDir()}, 0, nil),
	})

	msg := inbound("")
	msg.Photo = []lo.PhotoSize{{FileID: "ref", Width: 800, Height: 600}}
	receiver.HandleUpdate(t.Context(), lo.Update{Message: msg})

	if len(svc.prompts) != 1 || !strings.Contains(svc.prompts[0], "sent an image") {
		t.Fatalf("prompts=%v", svc.prompts)
	}
}

// The model is SHOWN the picture, not told where it is. Both other adapters pass the bytes as
// a vision block, and an agent handed only a path spends a tool call opening a file it was
// already sent — or answers the caption without looking.
func TestPhotoReachesTheModelAsAnImage(t *testing.T) {
	t.Parallel()
	svc := &serviceSpy{}
	api, _ := photoAPI(t, pngBytes)
	receiver := lo.NewReceiver(lo.ReceiverConfig{
		Service: svc, Client: api, Transport: lo.NewTransport(api, false),
		IsAllowed: func(int64) bool { return true },
		Uploads:   lo.NewUploader(api, fakeUploads{dir: t.TempDir()}, 0, nil),
	})

	msg := inbound("")
	msg.Photo = []lo.PhotoSize{{FileID: "ref", Width: 800, Height: 600}}
	receiver.HandleUpdate(t.Context(), lo.Update{Message: msg})

	if len(svc.images) != 1 || len(svc.images[0]) != 1 {
		t.Fatalf("images=%v, want the picture itself", svc.images)
	}
	if string(svc.images[0][0].Data) != pngBytes {
		t.Fatalf("the model was shown %q", svc.images[0][0].Data)
	}
}

// A photo array whose every rung lacks a file_id is still WORK by the time it gets here: the
// guard budget is already spent on it. It must end in a sentence — silence drops the message,
// and passing the caption on alone asks the agent about a picture it never received.
func TestPhotoWithNoUsableSizeIsExplained(t *testing.T) {
	t.Parallel()
	for name, photo := range map[string][]lo.PhotoSize{
		"one empty rung": {{Width: 90, Height: 90}},
		"every rung empty": {
			{Width: 90, Height: 90},
			{Width: 1280, Height: 720},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc := &serviceSpy{}
			api, notices := photoAPI(t, pngBytes)
			receiver := lo.NewReceiver(lo.ReceiverConfig{
				Service: svc, Client: api, Transport: lo.NewTransport(api, false),
				IsAllowed: func(int64) bool { return true },
				Uploads:   lo.NewUploader(api, fakeUploads{dir: t.TempDir()}, 0, nil),
			})

			msg := inbound("что тут не так?")
			msg.Photo = photo
			receiver.HandleUpdate(t.Context(), lo.Update{Message: msg})

			if len(svc.prompts) != 0 {
				t.Fatalf("the caption reached the agent without its picture: %v", svc.prompts)
			}
			if len(*notices) != 1 {
				t.Fatalf("notices=%v, want exactly one sentence", *notices)
			}
		})
	}
}

// Each unreadable kind gets its own sentence, and none of them starts a run: answering the
// caption alone would be answering a question whose subject never arrived.
func TestUnreadableAttachmentsExplainThemselvesAndStopTheRun(t *testing.T) {
	t.Parallel()
	// The notice TEXT is asserted, not merely its count. Per-kind sentences are what this
	// whole path is for — "I cannot read that" for everything would have been one line — so a
	// test that counts notices would stay green through any mix-up in the table.
	//
	// Documents are absent: they are read now, and their own tests cover that.
	for name, tc := range map[string]struct {
		attach func(*lo.Message)
		says   string
	}{
		"voice": {func(m *lo.Message) { m.Voice = lo.RawAttachment("v") }, "voice"},
		"video": {func(m *lo.Message) { m.Video = lo.RawAttachment("m") }, "video"},
		"audio": {func(m *lo.Message) { m.Audio = lo.RawAttachment("a") }, "audio"},
		"sticker": {
			func(m *lo.Message) { m.Sticker = lo.RawAttachment("s") },
			"Stickers",
		},
		"animation":  {func(m *lo.Message) { m.Animation = lo.RawAttachment("g") }, "animations"},
		"video note": {func(m *lo.Message) { m.VideoNote = lo.RawAttachment("n") }, "video notes"},
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
	api, notices := photoAPI(t, pngBytes)
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
	api, notices := photoAPI(t, pngBytes)
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
	// The photo prompt is core/chat's, shared with Telegram and VK, and ends the path with
	// ")". The document one is this adapter's and ends it with " —". Reading both keeps the
	// helper usable from either side rather than duplicating it per kind.
	// Both prompts come from core/chat and end the path differently: the photo one closes a
	// parenthesis, the document one ends the line. Reading both keeps one helper usable from
	// either side instead of a copy per kind.
	for _, marker := range []struct{ start, end string }{
		{"saved at ", ")"},
		{"uploaded a file: ", "\n"},
	} {
		idx := strings.Index(prompt, marker.start)
		if idx < 0 {
			continue
		}
		rest := prompt[idx+len(marker.start):]
		if end := strings.Index(rest, marker.end); end >= 0 {
			return rest[:end]
		}
	}
	t.Fatalf("prompt names no saved file: %q", prompt)
	return ""
}

// A document is downloaded exactly like a photo once the platform serves its bytes: the agent
// gets a path to open, and the file keeps the name the user gave it.
func TestDocumentReachesTheAgentAsAPath(t *testing.T) {
	t.Parallel()
	svc := &serviceSpy{}
	api, notices := photoAPI(t, "%PDF-1.7")
	receiver := lo.NewReceiver(lo.ReceiverConfig{
		Service: svc, Client: api, Transport: lo.NewTransport(api, false),
		IsAllowed: func(int64) bool { return true },
		Uploads:   lo.NewUploader(api, fakeUploads{dir: t.TempDir()}, 0, nil),
	})

	msg := inbound("разбери этот файл")
	msg.Document = &lo.Attachment{FileID: "ref", FileName: "spec.pdf", MimeType: "application/pdf"}
	receiver.HandleUpdate(t.Context(), lo.Update{Message: msg})

	if len(svc.prompts) != 1 {
		t.Fatalf("prompts=%v, want the document to start one run", svc.prompts)
	}
	if !strings.Contains(svc.prompts[0], "uploaded a file") {
		t.Fatalf("prompt does not name the attachment: %q", svc.prompts[0])
	}
	saved := savedPathFrom(t, svc.prompts[0])
	if !strings.HasSuffix(saved, "spec.pdf") {
		t.Fatalf("saved as %q, want the name the user gave it", saved)
	}
	data, err := os.ReadFile(saved) //nolint:gosec // Path comes from the prompt the code built.
	if err != nil || string(data) != "%PDF-1.7" {
		t.Fatalf("content=%q err=%v", data, err)
	}
	if len(*notices) != 0 {
		t.Fatalf("a working download still sent a notice: %v", *notices)
	}
}

// A platform that answers the reference but withholds the bytes — every LO that predates
// document downloads — must say so instead of failing silently or blaming the user's file.
func TestDocumentWithoutBytesIsExplained(t *testing.T) {
	t.Parallel()
	svc := &serviceSpy{}
	var notices []string
	api := client(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getFile") {
			// A reference answered WITHOUT a file_path: real reference, no bytes.
			reply(w, `{"ok":true,"result":{"file_id":"ref"}}`)
			return
		}
		body := make([]byte, 4096)
		n, _ := r.Body.Read(body)
		notices = append(notices, string(body[:n]))
		reply(w, `{"ok":true,"result":{"message_id":1}}`)
	})
	receiver := lo.NewReceiver(lo.ReceiverConfig{
		Service: svc, Client: api, Transport: lo.NewTransport(api, false),
		IsAllowed: func(int64) bool { return true },
		Uploads:   lo.NewUploader(api, fakeUploads{dir: t.TempDir()}, 0, nil),
	})

	msg := inbound("посмотри")
	msg.Document = &lo.Attachment{FileID: "ref", FileName: "spec.pdf"}
	receiver.HandleUpdate(t.Context(), lo.Update{Message: msg})

	if len(svc.prompts) != 0 {
		t.Fatalf("a run started without the file: %v", svc.prompts)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "does not hand bots the bytes") {
		t.Fatalf("notices=%v", notices)
	}
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

// The rung with the most BYTES wins, not the one with the most pixels. A rung can be the
// largest on paper and the most compressed in fact, so bytes describe "most detail" better —
// the rule the Telegram adapter already follows. A rung LO has not measured carries no
// file_size, and its pixel area stands in.
func TestLargestPhotoPrefersBytesOverPixels(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		sizes []lo.PhotoSize
		want  string
	}{
		"bytes beat pixels": {
			[]lo.PhotoSize{
				{FileID: "huge-but-crushed", Width: 4000, Height: 3000, FileSize: 20_000},
				{FileID: "smaller-but-rich", Width: 1280, Height: 720, FileSize: 900_000},
			},
			"smaller-but-rich",
		},
		"unmeasured falls back to area": {
			[]lo.PhotoSize{
				{FileID: "thumb", Width: 90, Height: 90},
				{FileID: "full", Width: 1280, Height: 720},
			},
			"full",
		},
		"a rung without an id is skipped": {
			[]lo.PhotoSize{
				{Width: 4000, Height: 3000, FileSize: 900_000},
				{FileID: "usable", Width: 90, Height: 90},
			},
			"usable",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok := lo.LargestPhoto(tc.sizes)
			if !ok || got.FileID != tc.want {
				t.Fatalf("chose %q (ok=%v), want %q", got.FileID, ok, tc.want)
			}
		})
	}
}

// The saved name is corrected to what the BYTES are. It is not decoration: core/chat derives
// the vision block's media type from the saved path, so a PNG kept under the guessed .jpg would
// be declared to the model as a JPEG — a claim about the bytes made by a name this adapter
// invented.
func TestSavedPhotoIsNamedByItsActualBytes(t *testing.T) {
	t.Parallel()
	// The first bytes of a real PNG; http.DetectContentType reads exactly this prefix.
	png := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 32)
	for name, tc := range map[string]struct{ body, wantExt string }{
		"a png":  {png, ".png"},
		"a gif":  {"GIF89a" + strings.Repeat("\x00", 32), ".gif"},
		"a jpeg": {"\xff\xd8\xff" + strings.Repeat("\x00", 32), ".jpg"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc := &serviceSpy{}
			api, _ := photoAPI(t, tc.body)
			receiver := lo.NewReceiver(lo.ReceiverConfig{
				Service: svc, Client: api, Transport: lo.NewTransport(api, false),
				IsAllowed: func(int64) bool { return true },
				Uploads:   lo.NewUploader(api, fakeUploads{dir: t.TempDir()}, 0, nil),
			})

			msg := inbound("")
			msg.Photo = []lo.PhotoSize{{FileID: "ref", Width: 800, Height: 600}}
			receiver.HandleUpdate(t.Context(), lo.Update{Message: msg})

			if len(svc.prompts) != 1 {
				t.Fatalf("prompts=%v, want one run", svc.prompts)
			}
			saved := savedPathFrom(t, svc.prompts[0])
			if !strings.HasSuffix(saved, tc.wantExt) {
				t.Fatalf("saved as %q, want %q for %s", saved, tc.wantExt, name)
			}
			if _, err := os.Stat(saved); err != nil {
				t.Fatalf("the renamed file is not where the prompt says: %v", err)
			}
		})
	}
}

// Bytes that are NOT an image must not reach the model as one. The sniff already knows — a
// storage error page served with 200, or an empty body — and keeping the guessed .jpg would
// make the very claim the renaming above exists to prevent, one step further along.
func TestPhotoBytesThatAreNotAnImageAreRefused(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"an error page":  "<!DOCTYPE html><html><body>500</body></html>",
		"nothing at all": "",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc := &serviceSpy{}
			api, notices := photoAPI(t, body)
			receiver := lo.NewReceiver(lo.ReceiverConfig{
				Service: svc, Client: api, Transport: lo.NewTransport(api, false),
				IsAllowed: func(int64) bool { return true },
				Uploads:   lo.NewUploader(api, fakeUploads{dir: t.TempDir()}, 0, nil),
			})

			msg := inbound("")
			msg.Photo = []lo.PhotoSize{{FileID: "ref", Width: 800, Height: 600}}
			receiver.HandleUpdate(t.Context(), lo.Update{Message: msg})

			if len(svc.prompts) != 0 {
				t.Fatalf("prompts=%v, want no run for bytes that are not an image", svc.prompts)
			}
			if len(*notices) != 1 {
				t.Fatalf("notices=%v, want the user told their image did not arrive", *notices)
			}
		})
	}
}

// A picture in a format the vision block cannot carry is refused BY NAME rather than renamed:
// core/chat answers image/jpeg for every extension it does not know, so keeping a BMP would
// declare it a JPEG to the model — the exact false claim the renaming exists to prevent.
func TestPhotoInAFormatTheModelCannotSeeIsRefusedByName(t *testing.T) {
	t.Parallel()
	svc := &serviceSpy{}
	// A BMP header: "BM" plus a size field. DetectContentType reads exactly this prefix.
	api, notices := photoAPI(t, "BM"+strings.Repeat("\x00", 32))
	receiver := lo.NewReceiver(lo.ReceiverConfig{
		Service: svc, Client: api, Transport: lo.NewTransport(api, false),
		IsAllowed: func(int64) bool { return true },
		Uploads:   lo.NewUploader(api, fakeUploads{dir: t.TempDir()}, 0, nil),
	})

	msg := inbound("")
	msg.Photo = []lo.PhotoSize{{FileID: "ref", Width: 800, Height: 600}}
	receiver.HandleUpdate(t.Context(), lo.Update{Message: msg})

	if len(svc.prompts) != 0 {
		t.Fatalf("prompts=%v, want no run for a format the model cannot be shown", svc.prompts)
	}
	if len(*notices) != 1 || !strings.Contains((*notices)[0], "image/bmp") {
		t.Fatalf("notices=%v, want one naming the format", *notices)
	}
}

// Bytes the sniffer does not RECOGNISE are unknown, not disproven. Deleting a picture because
// this adapter could not identify it would lose a file that may be perfectly readable — the
// opposite of the refusals above, which act on what the bytes are known to be.
func TestUnrecognisedPhotoBytesAreKept(t *testing.T) {
	t.Parallel()
	svc := &serviceSpy{}
	// DetectContentType answers application/octet-stream for a binary prefix it knows nothing
	// about — here a HEIC-shaped header.
	api, _ := photoAPI(t, "\x00\x00\x00\x18ftypheic"+strings.Repeat("\x00", 32))
	receiver := lo.NewReceiver(lo.ReceiverConfig{
		Service: svc, Client: api, Transport: lo.NewTransport(api, false),
		IsAllowed: func(int64) bool { return true },
		Uploads:   lo.NewUploader(api, fakeUploads{dir: t.TempDir()}, 0, nil),
	})

	msg := inbound("")
	msg.Photo = []lo.PhotoSize{{FileID: "ref", Width: 800, Height: 600}}
	receiver.HandleUpdate(t.Context(), lo.Update{Message: msg})

	if len(svc.prompts) != 1 {
		t.Fatalf("prompts=%v, want the file kept and the run started", svc.prompts)
	}
	if _, err := os.Stat(savedPathFrom(t, svc.prompts[0])); err != nil {
		t.Fatalf("the file the prompt names is gone: %v", err)
	}
	// The PATH travels, the vision block does not: core/chat derives the block's media type
	// from the saved name, and nothing here identified these bytes — claiming image/jpeg on
	// the strength of a name this adapter invented is the lie the whole sniff exists to stop.
	for i, block := range svc.images {
		if len(block) != 0 {
			t.Fatalf("run %d carried %d images, want none for bytes nobody identified", i, len(block))
		}
	}
}

// Bot API fills `document` ALONGSIDE `animation` for a GIF, so the arm that answers documents
// has to come last: otherwise someone who sent an animation is told about documents.
func TestAnimationIsAnsweredAsAnAnimationNotAsADocument(t *testing.T) {
	t.Parallel()
	svc := &serviceSpy{}
	api, notices := photoAPI(t, "")
	receiver := lo.NewReceiver(lo.ReceiverConfig{
		Service: svc, Client: api, Transport: lo.NewTransport(api, false),
		IsAllowed: func(int64) bool { return true },
		Uploads:   lo.NewUploader(api, fakeUploads{dir: t.TempDir()}, 0, nil),
	})

	msg := inbound("что тут")
	msg.Animation = lo.RawAttachment("g")
	msg.Document = &lo.Attachment{FileID: "g", FileName: "loop.gif", MimeType: "video/mp4"}
	receiver.HandleUpdate(t.Context(), lo.Update{Message: msg})

	if len(svc.prompts) != 0 {
		t.Fatalf("prompts=%v, want no run", svc.prompts)
	}
	if len(*notices) != 1 || !strings.Contains((*notices)[0], "animations") {
		t.Fatalf("notices=%v, want the animation sentence, not the document one", *notices)
	}
}
