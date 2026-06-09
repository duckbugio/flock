package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/duckbugio/flock/core/voice"
)

// defaultMaxVoiceBytes caps a voice download to guard against oversized files.
const defaultMaxVoiceBytes int64 = 25 << 20 // 25 MiB

// fileSource resolves and downloads a Telegram file by id. *bot.Bot is adapted
// to it via botFileSource so the download seam unit-tests without a real bot.
type fileSource interface {
	// FileInfo resolves a file id to its storage path (telegram getFile).
	FileInfo(ctx context.Context, fileID string) (filePath string, err error)
	// DownloadURL builds the (token-bearing) download URL for a file path. The
	// URL embeds the bot token, so it must NEVER be logged.
	DownloadURL(filePath string) string
}

// botFileSource adapts a *bot.Bot to fileSource.
type botFileSource struct {
	b *bot.Bot
}

// NewBotFileSource adapts a *bot.Bot to the fileSource used by VoiceTranscriber.
func NewBotFileSource(b *bot.Bot) fileSource {
	return &botFileSource{b: b}
}

// FileInfo resolves fileID to its Telegram storage path via getFile.
func (s *botFileSource) FileInfo(ctx context.Context, fileID string) (string, error) {
	file, err := s.b.GetFile(ctx, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		return "", fmt.Errorf("get file: %w", err)
	}
	return file.FilePath, nil
}

// DownloadURL builds the token-bearing download URL for filePath. Never log it.
func (s *botFileSource) DownloadURL(filePath string) string {
	return s.b.FileDownloadLink(&models.File{FilePath: filePath})
}

// VoiceTranscriber downloads a Telegram voice file and transcribes it. The
// download is capped at maxBytes (via io.LimitReader) to guard against oversized
// files. The token-bearing download URL is never logged.
type VoiceTranscriber struct {
	source      fileSource
	client      *http.Client
	transcriber voice.Transcriber
	maxBytes    int64
	logger      *slog.Logger
}

// NewVoiceTranscriber builds a VoiceTranscriber. A nil client defaults to
// http.DefaultClient, a non-positive maxBytes to defaultMaxVoiceBytes, and a nil
// logger to slog.Default().
func NewVoiceTranscriber(
	source fileSource, client *http.Client, transcriber voice.Transcriber, maxBytes int64, logger *slog.Logger,
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
	return &VoiceTranscriber{
		source:      source,
		client:      client,
		transcriber: transcriber,
		maxBytes:    maxBytes,
		logger:      logger,
	}
}

// Transcribe fetches fileID's bytes (capped at maxBytes) and returns the
// transcript, or an error. The token-bearing download URL is never logged.
func (vt *VoiceTranscriber) Transcribe(ctx context.Context, fileID string) (string, error) {
	filePath, err := vt.source.FileInfo(ctx, fileID)
	if err != nil {
		return "", err
	}

	url := vt.source.DownloadURL(filePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		// url embeds the token — never include it in the error.
		return "", fmt.Errorf("build download request: %w", err)
	}

	resp, err := vt.client.Do(req)
	if err != nil {
		// A transport error from client.Do is a *url.Error whose message embeds
		// the request URL — which carries the bot token. Strip the URL so the
		// token never reaches a log or a user-facing error.
		return "", fmt.Errorf("download voice file: %w", redactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusBadRequest {
		return "", fmt.Errorf("download voice file: status %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, vt.maxBytes)
	return vt.transcriber.Transcribe(ctx, limited, path.Base(filePath))
}

// redactURLError replaces a *url.Error (whose Error() includes the request URL,
// and thus the bot token in a Telegram file-download URL) with its underlying
// cause, which carries no URL. Other errors pass through unchanged.
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}
