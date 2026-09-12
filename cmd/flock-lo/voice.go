package main

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/duckbugio/flock/adapters/lo"
	"github.com/duckbugio/flock/core/voice"
	"github.com/duckbugio/flock/internal/config"
)

func buildVoice(cfg config.Config, api *lo.Client, logger *slog.Logger) lo.VoiceInput {
	if !cfg.EnableVoiceMessages {
		return nil
	}
	transcriber, err := voice.New(voice.Config{
		Provider: cfg.VoiceProvider, MistralAPIKey: cfg.MistralAPIKey, OpenAIAPIKey: cfg.OpenAIAPIKey,
		MistralModel: cfg.VoiceMistralModel, OpenAIModel: cfg.VoiceOpenAIModel, LocalCommand: cfg.VoiceLocalCommand,
		HTTPClient: &http.Client{Timeout: time.Minute}, Logger: logger,
	})
	if err != nil {
		logger.Warn("LO voice transcription disabled", "error", err)
		return nil
	}
	logger.Info("LO voice transcription enabled", "provider", cfg.VoiceProvider)
	return lo.NewVoiceTranscriber(api, transcriber, cfg.MaxUploadBytes)
}
