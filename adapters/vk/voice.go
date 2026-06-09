package vk

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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

	limited := io.LimitReader(resp.Body, vt.maxBytes)
	return vt.transcriber.Transcribe(ctx, limited, path.Base(url))
}
