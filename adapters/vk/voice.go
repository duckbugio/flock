package vk

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"

	"github.com/duckbugio/flock/core/voice"
)

// defaultMaxVoiceBytes caps a voice download to guard against oversized files.
const defaultMaxVoiceBytes int64 = 25 << 20 // 25 MiB

// VoiceTranscriber downloads a VK audio_message file (its self-keyed link_ogg /
// link_mp3 URL) and transcribes it via the provider-neutral core/voice
// Transcriber — the same plumbing the Telegram adapter reuses. The download is
// capped at maxBytes; the (self-keyed) URL is redacted from any error.
type VoiceTranscriber struct {
	client      *http.Client
	transcriber voice.Transcriber
	maxBytes    int64
	logger      *slog.Logger
}

// NewVoiceTranscriber builds a VoiceTranscriber. A nil client defaults to
// http.DefaultClient, a non-positive maxBytes to defaultMaxVoiceBytes, and a nil
// logger to slog.Default().
func NewVoiceTranscriber(
	client *http.Client, transcriber voice.Transcriber, maxBytes int64, logger *slog.Logger,
) *VoiceTranscriber {
	if client == nil {
		client = http.DefaultClient
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxVoiceBytes
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &VoiceTranscriber{client: client, transcriber: transcriber, maxBytes: maxBytes, logger: logger}
}

// Transcribe fetches the audio at url (a VK audio_message link, preferring the
// OGG link) capped at maxBytes and returns the transcript. The URL carries its
// own access key (not the community token) and is redacted from any error.
func (vt *VoiceTranscriber) Transcribe(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build download request: %w", redactURLError(err))
	}

	resp, err := vt.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download voice file: %w", redactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusBadRequest {
		return "", fmt.Errorf("download voice file: status %d", resp.StatusCode)
	}

	// Read one byte past the cap so an oversized file is rejected, not silently
	// truncated to a misleading transcript (mirrors upload.go's writeCapped).
	limited := io.LimitReader(resp.Body, vt.maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return "", fmt.Errorf("download voice file: %w", redactURLError(err))
	}
	if int64(len(data)) > vt.maxBytes {
		return "", ErrUploadTooLarge
	}
	return vt.transcriber.Transcribe(ctx, bytes.NewReader(data), voiceFilename(url))
}

// voiceFilename derives a clean filename hint from a VK audio URL, dropping any
// query string (the self-keyed link carries one) so the transcriber sees a bare
// basename.
func voiceFilename(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Path != "" {
		return path.Base(u.Path)
	}
	return path.Base(rawURL)
}
