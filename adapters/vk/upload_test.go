//nolint:testpackage // intentionally whitebox to test unexported VK upload internals
package vk

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

// fakeUploadsResolver returns a fixed uploads dir.
type fakeUploadsResolver struct{ dir string }

func (f *fakeUploadsResolver) UploadsDir(string) (string, error) { return f.dir, nil }

func newTestUploader(t *testing.T, body string, maxBytes int64) (u *Uploader, dir, url string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	dir = t.TempDir()
	u = NewUploader(srv.Client(), &fakeUploadsResolver{dir: dir}, maxBytes, nil)
	return u, dir, srv.URL
}

func TestVKUploaderSavesFile(t *testing.T) {
	const content = "VK-DOC-CONTENT"
	u, dir, url := newTestUploader(t, content, 0)

	path, err := u.Save(context.Background(), 200, url, "report.pdf")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !filepath.IsAbs(path) || filepath.Dir(path) != dir {
		t.Errorf("saved path %q not in uploads dir %q", path, dir)
	}
	if !strings.HasSuffix(path, "report.pdf") {
		t.Errorf("saved path %q does not preserve the sanitized name", path)
	}
	got, err := os.ReadFile(path) //nolint:gosec // G304: test reads a controlled temp path
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(got) != content {
		t.Errorf("saved bytes = %q, want %q", got, content)
	}
}

func TestVKUploaderHostileNameContained(t *testing.T) {
	u, dir, url := newTestUploader(t, "x", 0)
	for _, name := range []string{"../../etc/passwd", `..\..\x`, "/abs/secret", "..."} {
		p, err := u.Save(context.Background(), 1, url, name)
		if err != nil {
			t.Fatalf("Save(%q): %v", name, err)
		}
		if filepath.Dir(p) != dir {
			t.Errorf("hostile name %q escaped uploads dir: %q", name, p)
		}
	}
}

func TestVKUploaderOversizeRejected(t *testing.T) {
	const maxBytes = 8
	u, dir, url := newTestUploader(t, strings.Repeat("A", 100), maxBytes)
	_, err := u.Save(context.Background(), 1, url, "big.bin")
	if err == nil {
		t.Fatal("Save of oversize file = nil, want ErrUploadTooLarge")
	}
	if !strings.Contains(err.Error(), ErrUploadTooLarge.Error()) {
		t.Errorf("error = %v, want ErrUploadTooLarge", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("oversize upload left %d file(s) behind", len(entries))
	}
}

func TestVKUploaderDownloadErrorRedactsURL(t *testing.T) {
	const marker = "ACCESS-MARKER-12345"
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := closed.URL
	closed.Close()
	url := addr + "/doc?key=" + marker

	u := NewUploader(http.DefaultClient, &fakeUploadsResolver{dir: t.TempDir()}, 0, nil)
	_, err := u.Save(context.Background(), 1, url, "doc.bin")
	if err == nil {
		t.Fatal("Save on transport error = nil, want error")
	}
	if strings.Contains(err.Error(), marker) {
		t.Errorf("download error leaks the attachment access key: %q", err.Error())
	}
}
