package atomicfile_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/duckbugio/flock/internal/atomicfile"
)

func TestWriteCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")

	if err := atomicfile.Write(path, []byte(`{"a":1}`), ".store-*.tmp"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := os.ReadFile(path) //nolint:gosec // G304: test reads a controlled temp path
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != `{"a":1}` {
		t.Errorf("content = %q, want %q", got, `{"a":1}`)
	}
}

func TestWriteAtomicallyReplacesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")

	if err := atomicfile.Write(path, []byte("first"), ".store-*.tmp"); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if err := atomicfile.Write(path, []byte("second"), ".store-*.tmp"); err != nil {
		t.Fatalf("second Write: %v", err)
	}

	got, err := os.ReadFile(path) //nolint:gosec // G304: test reads a controlled temp path
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("content = %q, want %q", got, "second")
	}

	// No temp files should linger after a successful write.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("dir has %d entries, want 1 (only the target file)", len(entries))
	}
}

func TestWriteResultPerm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")

	if err := atomicfile.Write(path, []byte("data"), ".store-*.tmp"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// os.CreateTemp creates files with mode 0600; the rename preserves it.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 0600", perm)
	}
}

func TestWriteFailsOnMissingDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope", "store.json")
	if err := atomicfile.Write(path, []byte("data"), ".store-*.tmp"); err == nil {
		t.Fatal("Write into a nonexistent directory: want error, got nil")
	}
}
