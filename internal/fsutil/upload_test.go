package fsutil_test

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/duckbugio/flock/internal/fsutil"
)

func TestSanitizeUploadName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "report.pdf", "report.pdf"},
		{"unix traversal", "../../etc/passwd", "passwd"},
		{"windows traversal", `..\..\secret.txt`, "secret.txt"},
		{"leading dots dotfile", ".env", "env"},
		{"dot dot only", "..", "upload"},
		{"empty", "", "upload"},
		{"whitespace", "  spaced.txt  ", "spaced.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fsutil.SanitizeUploadName(tc.in); got != tc.want {
				t.Errorf("SanitizeUploadName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestUniquePrefixDistinctAndContainsCounter(t *testing.T) {
	var seq atomic.Uint64
	a := fsutil.UniquePrefix(&seq)
	b := fsutil.UniquePrefix(&seq)
	if a == b {
		t.Fatalf("UniquePrefix returned identical prefixes: %q", a)
	}
	if !strings.HasSuffix(a, "-") || !strings.HasSuffix(b, "-") {
		t.Errorf("prefixes should end with '-': %q, %q", a, b)
	}
}

func TestDestPathContainsAndSanitizes(t *testing.T) {
	dir := t.TempDir()
	var seq atomic.Uint64

	dest, err := fsutil.DestPath(dir, "../../etc/passwd", &seq)
	if err != nil {
		t.Fatalf("DestPath: %v", err)
	}
	if filepath.Dir(dest) != dir {
		t.Errorf("dest %q not contained in %q", dest, dir)
	}
	if !strings.HasSuffix(dest, "passwd") {
		t.Errorf("dest %q should preserve the sanitized base name", dest)
	}
}

func TestWriteCappedWritesUnderCap(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")

	got, err := fsutil.WriteCapped(dest, strings.NewReader("hello"), 1024, 0o600)
	if err != nil {
		t.Fatalf("WriteCapped: %v", err)
	}
	if got != dest {
		t.Errorf("returned path = %q, want %q", got, dest)
	}
	data, err := os.ReadFile(dest) //nolint:gosec // G304: test reads a controlled temp path
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("content = %q, want %q", data, "hello")
	}
	if info, _ := os.Stat(dest); info.Mode().Perm() != 0o600 {
		t.Errorf("perm = %o, want 0600", info.Mode().Perm())
	}
}

func TestWriteCappedRejectsOversize(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")

	_, err := fsutil.WriteCapped(dest, strings.NewReader("0123456789"), 4, 0o600)
	if !errors.Is(err, fsutil.ErrUploadTooLarge) {
		t.Fatalf("WriteCapped oversize = %v, want ErrUploadTooLarge", err)
	}
}

func TestRedactURLError(t *testing.T) {
	cause := errors.New("connection refused")
	ue := &url.Error{Op: "Get", URL: "https://api.example/method?token=SECRET", Err: cause}

	got := fsutil.RedactURLError(ue)
	if !errors.Is(got, cause) {
		t.Errorf("RedactURLError unwrap = %v, want %v", got, cause)
	}
	if strings.Contains(got.Error(), "SECRET") {
		t.Errorf("redacted error still leaks URL: %v", got)
	}

	plain := errors.New("plain")
	if !errors.Is(fsutil.RedactURLError(plain), plain) {
		t.Error("RedactURLError should pass non-url errors through unchanged")
	}
}
