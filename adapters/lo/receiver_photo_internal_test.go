package lo

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPhotoRenameFailureKeepsFileWithoutVision(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "photo.jpg")
	data := []byte("\x89PNG\r\n\x1a\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// An existing directory deterministically refuses the rename, including as root.
	if err := os.Mkdir(filepath.Join(dir, "photo.png"), 0o700); err != nil {
		t.Fatal(err)
	}
	receiver := NewReceiver(ReceiverConfig{})
	saved, confirmed, notice := receiver.checkPhotoBytes(path)
	if saved != path || confirmed || notice != "" {
		t.Fatalf("saved=%q confirmed=%v notice=%q, want original path without vision or refusal", saved, confirmed, notice)
	}
	got, err := os.ReadFile(path) //nolint:gosec // Path is a fixture in t.TempDir.
	if err != nil || string(got) != string(data) {
		t.Fatalf("photo must remain intact: bytes=%q err=%v", got, err)
	}
}
