// Package atomicfile writes a file in place atomically: it stages the data in a
// temp file in the same directory, fsyncs it, then renames it over the target.
// A crash mid-write therefore never leaves a half-written or corrupt file
// (rename is atomic on the same filesystem).
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// Write atomically replaces the file at path with data. The temp file is created
// in path's directory using tmpPattern (an os.CreateTemp pattern, e.g.
// ".store-*.tmp") so the final rename stays on the same filesystem. The resulting
// file carries os.CreateTemp's default 0600 permission. On any failure before the
// rename the temp file is removed; after a successful rename the temp name no
// longer exists, so the cleanup is a harmless no-op.
func Write(path string, data []byte, tmpPattern string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, tmpPattern)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp file into place: %w", err)
	}
	return nil
}
