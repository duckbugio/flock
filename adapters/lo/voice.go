package lo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/duckbugio/flock/core/voice"
)

// VoiceInput is the optional receiver seam for speech recognition.
type VoiceInput interface {
	TranscribeRecording(ctx context.Context, fileID, filename string) (string, error)
}

// VoiceTranscriber resolves LO's signed reference and transcribes bounded audio.
// The download URL remains inside Client, including its credential redaction.
type VoiceTranscriber struct {
	source      fileSource
	transcriber voice.Transcriber
	maxBytes    int64
}

// NewVoiceTranscriber uses the shared upload cap when maxBytes is nonpositive.
func NewVoiceTranscriber(source fileSource, transcriber voice.Transcriber, maxBytes int64) *VoiceTranscriber {
	if maxBytes <= 0 {
		maxBytes = defaultMaxUploadBytes
	}
	return &VoiceTranscriber{source: source, transcriber: transcriber, maxBytes: maxBytes}
}

const voiceTimeout = time.Minute

// Transcribe reads the complete bounded recording before calling the configured provider.
func (v *VoiceTranscriber) Transcribe(ctx context.Context, fileID string) (string, error) {
	return v.TranscribeRecording(ctx, fileID, "voice.ogg")
}

// TranscribeRecording preserves the audio container name supplied by the receiver.
func (v *VoiceTranscriber) TranscribeRecording(ctx context.Context, fileID, filename string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, voiceTimeout)
	defer cancel()
	file, err := v.source.GetFile(ctx, fileID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(file.FilePath) == "" {
		return "", ErrNoBytes
	}
	if file.FileSize > v.maxBytes {
		return "", ErrUploadTooLarge
	}
	body, err := v.source.Download(ctx, file.FilePath)
	if err != nil {
		return "", err
	}
	defer func() { _ = body.Close() }()
	// Detect overflow before invoking a paid provider. A LimitReader alone silently
	// truncates audio and would turn an incomplete recording into a partial request.
	audio, err := io.ReadAll(io.LimitReader(body, v.maxBytes+1))
	if err != nil {
		return "", fmt.Errorf("read voice: %w", err)
	}
	if int64(len(audio)) > v.maxBytes {
		return "", ErrUploadTooLarge
	}
	if len(audio) == 0 {
		return "", errors.New("empty voice recording")
	}
	return v.transcriber.Transcribe(ctx, bytes.NewReader(audio), filename)
}

// voiceReference keeps unknown voice fields forward-compatible, but requires a real ID.
func voiceReference(msg *Message) string {
	var attachment Attachment
	raw := msg.Voice
	if !present(raw) {
		raw = msg.Audio
	}
	if json.Unmarshal(raw, &attachment) != nil {
		return ""
	}
	return strings.TrimSpace(attachment.FileID)
}

func (r *Receiver) handleVoice(ctx context.Context, msg *Message, chatID string, replyToBot bool, fileID string) {
	kind := "voice message"
	if !present(msg.Voice) {
		kind = "audio recording"
	}
	transcript, err := r.cfg.Voice.TranscribeRecording(ctx, fileID, recordingFilename(msg))
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		r.cfg.Logger.Warn("lo: voice transcription failed", "error", err)
		note := "Sorry, I couldn't transcribe that " + kind + ". Please send the request as text."
		if errors.Is(err, ErrNoBytes) {
			note = "This LO cannot provide the bytes of that " + kind + ". Please send the request as text."
		}
		if errors.Is(err, ErrUploadTooLarge) {
			note = "That " + kind + " is too large. Please send a shorter recording."
		}
		r.notify(ctx, chatID, note)
		return
	}
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		r.notify(ctx, chatID, "Sorry, I couldn't make out that "+kind+". Please send the request as text.")
		return
	}
	// Serialize the handoff with /stop so a late provider response cannot start a new run.
	r.voiceMu.Lock()
	defer r.voiceMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	if caption := strings.TrimSpace(msg.Caption); caption != "" {
		transcript = caption + "\n\nTranscript:\n" + transcript
	}
	r.cfg.Service.Handle(ctx, chatID, msg.From.ID, strconv.FormatInt(msg.ID, 10), replyPrompt(msg, replyToBot, transcript))
}
