package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/duckbugio/flock/core/claude"
	"github.com/duckbugio/flock/core/dispatch"
	"github.com/duckbugio/flock/internal/config"
	"github.com/duckbugio/flock/internal/telegram"
)

// recordingRunner records the prompts and image counts it was asked to run and
// emits a minimal successful stream so the Service finishes cleanly.
type recordingRunner struct {
	mu      sync.Mutex
	prompts []string
	images  []int
}

func (r *recordingRunner) Run(ctx context.Context, prompt string, o claude.Options) (<-chan claude.Event, error) {
	r.mu.Lock()
	r.prompts = append(r.prompts, prompt)
	r.images = append(r.images, len(o.Images))
	r.mu.Unlock()
	out := make(chan claude.Event)
	go func() {
		defer close(out)
		select {
		case out <- claude.Event{Type: claude.Result, Result: &claude.RunResult{Text: "done"}}:
		case <-ctx.Done():
		}
	}()
	return out, nil
}

func (r *recordingRunner) seen() ([]string, []int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.prompts...), append([]int(nil), r.images...)
}

// fakeUploads resolves a per-chat uploads dir under a temp root.
type fakeUploads struct{ root string }

func (f *fakeUploads) UploadsDir(chatID int64) (string, error) {
	dir := filepath.Join(f.root, "chat_"+strconv.FormatInt(chatID, 10), "uploads")
	return dir, os.MkdirAll(dir, 0o750)
}

// fakeWorkspace returns a fixed workdir.
type fakeWorkspace struct{ dir string }

func (f *fakeWorkspace) Ensure(int64) (string, error) { return f.dir, nil }

// mediaTestHarness wires a real Service + Uploader against a fake Telegram API
// server (counting getFile + file downloads) so the routing in handleMessage can
// be exercised black-box.
type mediaTestHarness struct {
	b          *bot.Bot
	svc        *telegram.Service
	up         *telegram.Uploader
	runner     *recordingRunner
	disp       *dispatch.Dispatcher
	downloads  *int32
	fileBytes  string
	srvCleanup func()
}

func newMediaHarness(t *testing.T, maxUpload int64) *mediaTestHarness {
	t.Helper()
	var downloads int32
	const content = "FILECONTENT-1234567890"

	mux := http.NewServeMux()
	// getFile: return a file_path the download step will fetch.
	mux.HandleFunc("/bot123:ABC/getFile", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"result": map[string]any{"file_id": "fid", "file_path": "documents/file.bin"},
		})
	})
	// file download: count the hit and serve the bytes.
	mux.HandleFunc("/file/bot123:ABC/", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&downloads, 1)
		_, _ = io.WriteString(w, content)
	})
	// sendMessage / editMessageText: progress + final delivery (always ok).
	okResult := func(w http.ResponseWriter, _ *http.Request) {
		if err := json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": 1, "chat": map[string]any{"id": 1}, "date": 0},
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}
	mux.HandleFunc("/bot123:ABC/sendMessage", okResult)
	mux.HandleFunc("/bot123:ABC/editMessageText", okResult)
	srv := httptest.NewServer(mux)

	b, err := bot.New("123:ABC", bot.WithSkipGetMe(), bot.WithServerURL(srv.URL))
	if err != nil {
		srv.Close()
		t.Fatalf("bot.New: %v", err)
	}

	logger := slog.New(slog.DiscardHandler)
	uploadsRoot := t.TempDir()
	up := telegram.NewUploader(telegram.NewBotFileSource(b), srv.Client(), &fakeUploads{root: uploadsRoot}, maxUpload, logger)

	runner := &recordingRunner{}
	disp := dispatch.New(2)
	svc := telegram.New(telegram.Config{
		Runner:     runner,
		Chat:       telegram.NewBotChat(b),
		Dispatcher: disp,
		Workspace:  &fakeWorkspace{dir: t.TempDir()},
		Logger:     logger,
	})

	return &mediaTestHarness{
		b: b, svc: svc, up: up, runner: runner, disp: disp,
		downloads: &downloads, fileBytes: content,
		srvCleanup: func() { disp.Close(); srv.Close() },
	}
}

func (h *mediaTestHarness) downloadCount() int32 { return atomic.LoadInt32(h.downloads) }

// TestMediaGateRejectionZeroWork is AC5: a captioned media message that the
// mention-gate rejects (group chat, mention required, no @mention) must perform
// ZERO downloads and ZERO submits.
func TestMediaGateRejectionZeroWork(t *testing.T) {
	h := newMediaHarness(t, 0)
	defer h.srvCleanup()

	cfg := config.Config{
		AllowedUsers:        []int64{10},
		RequireGroupMention: true,
	}
	msg := &models.Message{
		ID:       1,
		From:     &models.User{ID: 10},
		Chat:     models.Chat{ID: 555, Type: models.ChatTypeSupergroup},
		Caption:  "please read this",
		Document: &models.Document{FileID: "fid", FileName: "doc.pdf"},
	}

	deps := messageDeps{cfg: cfg, service: h.svc, up: h.up, guards: telegram.GuardConfig{}}
	handleMessage(context.Background(), deps, h.b, msg, false)

	// Give any erroneous async work a brief chance to run, then assert nothing did.
	time.Sleep(50 * time.Millisecond)
	if n := h.downloadCount(); n != 0 {
		t.Errorf("gate-rejected media triggered %d download(s), want 0", n)
	}
	if prompts, _ := h.runner.seen(); len(prompts) != 0 {
		t.Errorf("gate-rejected media submitted %d run(s), want 0: %v", len(prompts), prompts)
	}
}

// TestDocumentRoutingSubmitsPromptWithPath is AC1: an accepted document upload is
// downloaded once and submitted with a prompt referencing the saved path.
func TestDocumentRoutingSubmitsPromptWithPath(t *testing.T) {
	h := newMediaHarness(t, 0)
	defer h.srvCleanup()

	cfg := config.Config{AllowedUsers: []int64{10}}
	msg := &models.Message{
		ID:       1,
		From:     &models.User{ID: 10},
		Chat:     models.Chat{ID: 777, Type: models.ChatTypePrivate},
		Caption:  "summarize",
		Document: &models.Document{FileID: "fid", FileName: "report.pdf"},
	}

	deps := messageDeps{cfg: cfg, service: h.svc, up: h.up, guards: telegram.GuardConfig{}}
	handleMessage(context.Background(), deps, h.b, msg, false)

	waitFor(t, func() bool {
		prompts, _ := h.runner.seen()
		return len(prompts) == 1
	})
	prompts, imgs := h.runner.seen()
	if !strings.Contains(prompts[0], "uploads") || !strings.Contains(prompts[0], "report.pdf") {
		t.Errorf("document prompt %q missing the saved uploads path", prompts[0])
	}
	if !strings.Contains(prompts[0], "summarize") {
		t.Errorf("document prompt %q missing the caption", prompts[0])
	}
	if imgs[0] != 0 {
		t.Errorf("document run carried %d image(s), want 0 (path-only)", imgs[0])
	}
	if h.downloadCount() != 1 {
		t.Errorf("document download count = %d, want 1", h.downloadCount())
	}
}

// TestPhotoRoutingAttachesImageAndPath is AC2: an accepted photo upload is saved
// AND attached as a vision image block, with the prompt referencing the saved
// path; the largest photo size is chosen.
func TestPhotoRoutingAttachesImageAndPath(t *testing.T) {
	h := newMediaHarness(t, 0)
	defer h.srvCleanup()

	cfg := config.Config{AllowedUsers: []int64{10}}
	msg := &models.Message{
		ID:      1,
		From:    &models.User{ID: 10},
		Chat:    models.Chat{ID: 888, Type: models.ChatTypePrivate},
		Caption: "what is this?",
		Photo: []models.PhotoSize{
			{FileID: "small", Width: 90, Height: 90, FileSize: 1000},
			{FileID: "large", Width: 1280, Height: 1280, FileSize: 50000},
		},
	}

	deps := messageDeps{cfg: cfg, service: h.svc, up: h.up, guards: telegram.GuardConfig{}}
	handleMessage(context.Background(), deps, h.b, msg, false)

	waitFor(t, func() bool {
		prompts, _ := h.runner.seen()
		return len(prompts) == 1
	})
	prompts, imgs := h.runner.seen()
	if !strings.Contains(prompts[0], "uploads") {
		t.Errorf("photo prompt %q missing the saved uploads path", prompts[0])
	}
	if !strings.Contains(prompts[0], "what is this?") {
		t.Errorf("photo prompt %q missing the caption", prompts[0])
	}
	if imgs[0] != 1 {
		t.Errorf("photo run carried %d image(s), want 1 (vision attached)", imgs[0])
	}
	if h.downloadCount() != 1 {
		t.Errorf("photo download count = %d, want 1", h.downloadCount())
	}
}

// TestLargestPhotoSelection unit-checks the size picker independently.
func TestLargestPhotoSelection(t *testing.T) {
	sizes := []models.PhotoSize{
		{FileID: "a", FileSize: 10},
		{FileID: "b", FileSize: 500},
		{FileID: "c", FileSize: 0, Width: 100, Height: 100}, // 10000 via dims
	}
	got := largestPhoto(sizes)
	if got == nil || got.FileID != "c" {
		t.Fatalf("largestPhoto picked %+v, want the dims-scored 'c'", got)
	}
	if largestPhoto(nil) != nil {
		t.Error("largestPhoto(nil) should be nil")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}
