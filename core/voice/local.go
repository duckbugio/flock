package voice

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

// localTranscriber transcribes by invoking a local command. The audio is written
// to a temp file whose path is passed as the command's last argument; the
// command's trimmed stdout is the transcript.
type localTranscriber struct {
	command string
	logger  *slog.Logger
}

// Transcribe writes audio to a temp file, runs the configured command with that
// path as its last argument, and returns the trimmed stdout as the transcript.
// The temp file is removed before returning.
func (t *localTranscriber) Transcribe(ctx context.Context, audio io.Reader, filename string) (string, error) {
	f, err := os.CreateTemp("", "voice-*-"+sanitizeName(filename))
	if err != nil {
		return "", fmt.Errorf("voice: create temp file: %w", err)
	}
	tmpPath := f.Name()
	defer func() {
		if rmErr := os.Remove(tmpPath); rmErr != nil {
			t.logger.Debug("voice: remove temp file", "error", rmErr)
		}
	}()

	if _, err := io.Copy(f, audio); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("voice: write temp audio: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("voice: close temp audio: %w", err)
	}

	//nolint:gosec // G204: command is the configured transcriber binary and a controlled temp path, not raw user input.
	cmd := exec.CommandContext(ctx, t.command, tmpPath)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("voice: local command: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// sanitizeName keeps a temp-file suffix readable while stripping path separators
// from the provided filename hint.
func sanitizeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "audio"
	}
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	return name
}
