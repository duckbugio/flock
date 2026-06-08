package telegram

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeUploadsResolver returns a fixed uploads dir (created by the test).
type fakeUploadsResolver struct {
	dir string
}

func (f *fakeUploadsResolver) UploadsDir(int64) (string, error) {
	return f.dir, nil
}

// newTestUploader builds an Uploader serving body from a test server, saving into
// a fresh temp uploads dir.
func newTestUploader(t *testing.T, body string, maxBytes int64) (*Uploader, string, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	dir := t.TempDir()
	src := &fakeFileSource{filePath: "documents/file.bin", downloadURL: srv.URL}
	u := NewUploader(src, srv.Client(), &fakeUploadsResolver{dir: dir}, maxBytes, nil)
	return u, dir, srv.Close
}

// TestUploaderSavesFile asserts a normal upload lands in the uploads dir with the
// right bytes and an absolute, contained path.
func TestUploaderSavesFile(t *testing.T) {
	const content = "DOCUMENT-CONTENT"
	u, dir, closeSrv := newTestUploader(t, content, 0)
	defer closeSrv()

	path, err := u.Save(context.Background(), 100, "file-id", "report.pdf")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("saved path %q is not absolute", path)
	}
	if filepath.Dir(path) != dir {
		t.Errorf("saved path %q not in uploads dir %q", path, dir)
	}
	if !strings.HasSuffix(path, "report.pdf") {
		t.Errorf("saved path %q does not preserve the sanitized name", path)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(got) != content {
		t.Errorf("saved bytes = %q, want %q", got, content)
	}
}

// TestUploaderHostileNamesContainedAndUnique runs hostile file names through Save
// and asserts every saved file stays inside the uploads dir (no traversal) and
// two uploads of the same name get distinct paths (collision-safe prefix) — AC3.
func TestUploaderHostileNamesContainedAndUnique(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"dotdot-unix", "../../etc/passwd"},
		{"dotdot-win", `..\..\x`},
		{"absolute", "/abs/secret"},
		{"all-dots", "..."},
		{"single-dot", "."},
		{"empty", ""},
		{"hidden", ".env"},
		{"trailing-slash", "evil/"},
		{"nested", "a/b/c.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, dir, closeSrv := newTestUploader(t, "x", 0)
			defer closeSrv()

			p1, err := u.Save(context.Background(), 1, "fid", tc.input)
			if err != nil {
				t.Fatalf("Save(%q): %v", tc.input, err)
			}
			p2, err := u.Save(context.Background(), 1, "fid", tc.input)
			if err != nil {
				t.Fatalf("second Save(%q): %v", tc.input, err)
			}

			// Containment: the cleaned saved path must sit directly under the uploads
			// dir, never above it.
			for _, p := range []string{p1, p2} {
				if filepath.Dir(p) != dir {
					t.Errorf("hostile name %q escaped uploads dir: saved %q (dir %q)", tc.input, p, dir)
				}
				rel, err := filepath.Rel(dir, p)
				if err != nil || strings.HasPrefix(rel, "..") {
					t.Errorf("hostile name %q produced non-contained path %q (rel %q)", tc.input, p, rel)
				}
			}
			// Uniqueness: same name twice must not collide.
			if p1 == p2 {
				t.Errorf("two uploads of %q collided on path %q", tc.input, p1)
			}
		})
	}
}

// TestUploaderOversizeReturnsNotice asserts a body larger than the cap is
// rejected with ErrUploadTooLarge (AC6: a notice error, not a silent drop) and no
// file is left behind.
func TestUploaderOversizeReturnsNotice(t *testing.T) {
	const maxBytes = 8
	u, dir, closeSrv := newTestUploader(t, strings.Repeat("A", 100), maxBytes)
	defer closeSrv()

	_, err := u.Save(context.Background(), 1, "fid", "big.bin")
	if err == nil {
		t.Fatal("Save of oversize file = nil error, want ErrUploadTooLarge")
	}
	if !strings.Contains(err.Error(), ErrUploadTooLarge.Error()) {
		t.Errorf("error = %v, want ErrUploadTooLarge", err)
	}
	// No partial file should remain.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("oversize upload left %d file(s) behind", len(entries))
	}
}

// TestUploaderDownloadErrorRedactsToken asserts a transport failure yields a
// notice error that does NOT carry the bot token (AC6).
func TestUploaderDownloadErrorRedactsToken(t *testing.T) {
	const token = "123456:SECRET-BOT-TOKEN"
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := closed.URL
	closed.Close()
	downloadURL := addr + "/file/bot" + token + "/documents/file.bin"

	src := &fakeFileSource{filePath: "documents/file.bin", downloadURL: downloadURL}
	u := NewUploader(src, http.DefaultClient, &fakeUploadsResolver{dir: t.TempDir()}, 0, nil)

	_, err := u.Save(context.Background(), 1, "fid", "doc.bin")
	if err == nil {
		t.Fatal("Save on transport error = nil, want error")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("download error leaks the bot token: %q", err.Error())
	}
}

// TestUploaderHTTPErrorReturnsNotice asserts a 404 is surfaced as an error the
// caller turns into a notice (no silent drop).
func TestUploaderHTTPErrorReturnsNotice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	src := &fakeFileSource{filePath: "documents/file.bin", downloadURL: srv.URL}
	u := NewUploader(src, srv.Client(), &fakeUploadsResolver{dir: t.TempDir()}, 0, nil)

	if _, err := u.Save(context.Background(), 1, "fid", "doc.bin"); err == nil {
		t.Fatal("Save on 404 = nil error, want error")
	}
}
