// Package voice transcribes spoken audio (e.g. Telegram OGG/Opus voice messages)
// into text via a configurable provider. It is transport-agnostic: callers hand
// it an io.Reader of audio and receive a transcript, so the Telegram glue and the
// providers stay decoupled. Providers are selected by Config.Provider — a hosted
// HTTP API (mistral, openai) or a local command.
package voice

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// Transcriber converts spoken audio into text.
type Transcriber interface {
	// Transcribe reads audio (e.g. Telegram OGG/Opus) and returns the transcript.
	// filename is a hint for the multipart upload / provider content negotiation.
	Transcribe(ctx context.Context, audio io.Reader, filename string) (string, error)
}

// Config selects and configures the transcription provider.
type Config struct {
	Provider      string       // "mistral" | "openai" | "local"
	MistralAPIKey string       // provider=mistral
	OpenAIAPIKey  string       // provider=openai
	MistralModel  string       // default "voxtral-mini-latest"
	OpenAIModel   string       // default "whisper-1"
	LocalCommand  string       // provider=local: a binary that reads an audio file path as its last arg and prints the transcript to stdout
	HTTPClient    *http.Client // injectable for tests; default 60s timeout
	Logger        *slog.Logger // optional; defaults to slog.Default()
}

// Default provider models and the hosted transcription endpoints.
const (
	defaultMistralModel = "voxtral-mini-latest"
	defaultOpenAIModel  = "whisper-1"

	mistralEndpoint = "https://api.mistral.ai/v1/audio/transcriptions"
	openAIEndpoint  = "https://api.openai.com/v1/audio/transcriptions"
)

// New builds the Transcriber for cfg.Provider, or returns an error when the
// provider is unknown or its required key/command is missing. Provider matching
// is case-insensitive.
func New(cfg Config) (Transcriber, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "mistral":
		if cfg.MistralAPIKey == "" {
			return nil, &configError{provider: "mistral", missing: "MISTRAL_API_KEY"}
		}
		model := cfg.MistralModel
		if model == "" {
			model = defaultMistralModel
		}
		return &httpMultipartTranscriber{
			endpoint: mistralEndpoint,
			model:    model,
			bearer:   cfg.MistralAPIKey,
			client:   httpClient(cfg.HTTPClient),
		}, nil
	case "openai":
		if cfg.OpenAIAPIKey == "" {
			return nil, &configError{provider: "openai", missing: "OPENAI_API_KEY"}
		}
		model := cfg.OpenAIModel
		if model == "" {
			model = defaultOpenAIModel
		}
		return &httpMultipartTranscriber{
			endpoint: openAIEndpoint,
			model:    model,
			bearer:   cfg.OpenAIAPIKey,
			client:   httpClient(cfg.HTTPClient),
		}, nil
	case "local":
		if strings.TrimSpace(cfg.LocalCommand) == "" {
			return nil, &configError{provider: "local", missing: "VOICE_LOCAL_COMMAND"}
		}
		return &localTranscriber{command: cfg.LocalCommand, logger: logger}, nil
	default:
		return nil, &unknownProviderError{provider: cfg.Provider}
	}
}
