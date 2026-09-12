package lo_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/duckbugio/flock/adapters/lo"
)

// fakeSource stands in for the Bot API: it decides what getFile answers and what bytes the
// download yields, which is the whole contract the uploader depends on.
type fakeSource struct {
	file      lo.File
	fileErr   error
	body      string
	bodyErr   error
	gotID     string
	gotPath   string
	downloads int
}

// photoBytes is the body the fake platform serves, asserted on both sides of a save.
const photoBytes = "PNGDATA"

func (f *fakeSource) GetFile(_ context.Context, fileID string) (lo.File, error) {
	f.gotID = fileID
	return f.file, f.fileErr
}

func (f *fakeSource) Download(_ context.Context, filePath string) (io.ReadCloser, error) {
	f.gotPath = filePath
	f.downloads++
	if f.bodyErr != nil {
		return nil, f.bodyErr
	}
	return io.NopCloser(strings.NewReader(f.body)), nil
}

type fakeUploads struct{ dir string }

func (f fakeUploads) UploadsDir(chatID string) (string, error) {
	dir := filepath.Join(f.dir, "chat_"+chatID, "uploads")
	return dir, os.MkdirAll(dir, 0o700)
}

func TestUploaderSavesPhotoBytes(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	src := &fakeSource{file: lo.File{FileID: "ref", FilePath: "ref"}, body: photoBytes}
	up := lo.NewUploader(src, fakeUploads{dir: base}, 0, nil)

	saved, err := up.Save(t.Context(), "42", "ref", "photo_7.jpg")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if src.gotID != "ref" || src.gotPath != "ref" {
		t.Fatalf("resolved %q and downloaded %q", src.gotID, src.gotPath)
	}
	if !strings.HasPrefix(saved, filepath.Join(base, "chat_42", "uploads")) {
		t.Fatalf("saved outside the chat uploads dir: %s", saved)
	}
	data, err := os.ReadFile(saved) //nolint:gosec // Path is produced by the code under test.
	if err != nil || string(data) != photoBytes {
		t.Fatalf("content=%q err=%v", data, err)
	}
	info, err := os.Stat(saved)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v err=%v", info.Mode().Perm(), err)
	}
}

// An answered reference with no path is LO saying "bytes not served". It must be its own
// error, because the user-facing sentence for it differs from a failed download.
func TestUploaderReportsUnservedBytes(t *testing.T) {
	t.Parallel()
	src := &fakeSource{file: lo.File{FileID: "ref"}}
	up := lo.NewUploader(src, fakeUploads{dir: t.TempDir()}, 0, nil)

	_, err := up.Save(t.Context(), "1", "ref", "clip.mp4")
	if !errors.Is(err, lo.ErrNoBytes) {
		t.Fatalf("err=%v, want ErrNoBytes", err)
	}
	if src.downloads != 0 {
		t.Fatal("downloaded bytes the platform said it would not serve")
	}
}

func TestUploaderRefusesOversizeBeforeAndDuringDownload(t *testing.T) {
	t.Parallel()
	t.Run("declared size", func(t *testing.T) {
		t.Parallel()
		src := &fakeSource{file: lo.File{FilePath: "ref", FileSize: 100}, body: "x"}
		up := lo.NewUploader(src, fakeUploads{dir: t.TempDir()}, 10, nil)
		if _, err := up.Save(t.Context(), "1", "ref", "big.jpg"); !errors.Is(err, lo.ErrUploadTooLarge) {
			t.Fatalf("err=%v, want ErrUploadTooLarge", err)
		}
		if src.downloads != 0 {
			t.Fatal("a file already known to be too large was still downloaded")
		}
	})
	t.Run("undeclared size", func(t *testing.T) {
		t.Parallel()
		// file_size zero means "unknown" on this platform, so the cap must also hold on the
		// stream itself — otherwise an unmeasured file is an unbounded write.
		src := &fakeSource{file: lo.File{FilePath: "ref"}, body: strings.Repeat("x", 64)}
		up := lo.NewUploader(src, fakeUploads{dir: t.TempDir()}, 10, nil)
		if _, err := up.Save(t.Context(), "1", "ref", "big.jpg"); !errors.Is(err, lo.ErrUploadTooLarge) {
			t.Fatalf("err=%v, want ErrUploadTooLarge", err)
		}
	})
}

// The file name comes from the chat and is hostile input: it must not be able to place a
// file anywhere but the chat's own uploads directory.
func TestUploaderContainsHostileFileNames(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	for _, name := range []string{"../../escape.sh", "/etc/passwd", "..", ".ssh/authorized_keys"} {
		src := &fakeSource{file: lo.File{FilePath: "ref"}, body: "data"}
		up := lo.NewUploader(src, fakeUploads{dir: base}, 0, nil)
		saved, err := up.Save(t.Context(), "9", "ref", name)
		if err != nil {
			continue // A rejected name is an equally good outcome.
		}
		uploads := filepath.Join(base, "chat_9", "uploads")
		if filepath.Dir(saved) != uploads {
			t.Fatalf("name %q escaped to %s", name, saved)
		}
	}
}
