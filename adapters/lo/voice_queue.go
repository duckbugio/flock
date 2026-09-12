package lo

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/duckbugio/flock/core/dispatch"
	"github.com/duckbugio/flock/core/pending"
)

// This total cap is below the dispatcher's per-chat capacity, so enqueue never
// blocks the polling loop waiting for a transcription slot.
const maxQueuedVoices = 32

func (r *Receiver) queueVoice(ctx context.Context, msg *Message, chatID string, replyToBot bool, fileID string) {
	raw, err := json.Marshal(msg)
	if err != nil {
		r.cfg.Logger.Error("encode pending voice", "error", err)
		return
	}
	r.voiceMu.Lock()
	total := 0
	for _, queue := range r.cfg.VoiceStore.All() {
		total += len(queue)
	}
	if total >= maxQueuedVoices {
		r.voiceMu.Unlock()
		r.notify(ctx, chatID, "Voice processing is busy. Please retry in a moment or send text.")
		return
	}
	marker := pending.Marker{Prompt: string(raw), StartedAt: time.Now().UnixMilli()}
	marker.ID, err = r.cfg.VoiceStore.Enqueue(chatID, marker)
	if err != nil {
		r.voiceMu.Unlock()
		r.cfg.Logger.Error("persist pending voice", "error", err)
		r.notify(ctx, chatID, "Could not queue that voice message. Please retry or send text.")
		return
	}
	r.submitVoice(ctx, msg, chatID, replyToBot, fileID, marker)
	r.voiceMu.Unlock()
}

func (r *Receiver) submitVoice(
	ctx context.Context, msg *Message, chatID string, replyToBot bool, fileID string, marker pending.Marker,
) {
	// The dispatcher owns the job lifetime and shutdown cause; the poll context
	// only controls admission. A cancelled admission remains durable for restart.
	if ctx.Err() != nil {
		return
	}
	//nolint:contextcheck // Dispatcher owns cancellation and ErrShutdown, which controls durable replay.
	r.voiceJobs.Submit(chatID, func(ctx context.Context) {
		r.handleVoice(ctx, msg, chatID, replyToBot, fileID)
		// Interrupted preparation is replayed after restart. On success the shared
		// service has already persisted its own pending agent run before this removal.
		// A crash between stores may replay work; this is intentionally at-least-once.
		if errors.Is(context.Cause(ctx), dispatch.ErrShutdown) {
			return
		}
		if err := r.cfg.VoiceStore.Remove(chatID, marker.ID); err != nil {
			r.cfg.Logger.Error("remove completed voice preparation", "error", err)
		}
	})
}

func (r *Receiver) resumeVoice(ctx context.Context) {
	total := 0
	for chatID, queue := range r.cfg.VoiceStore.All() {
		for _, marker := range queue {
			if ctx.Err() != nil {
				return
			}
			var msg Message
			err := json.Unmarshal([]byte(marker.Prompt), &msg)
			_, replyToBot, allowed := r.admits(&msg)
			fileID := voiceReference(&msg)
			if err != nil || !allowed || strconv.FormatInt(msg.Chat.ID, 10) != chatID || fileID == "" {
				r.cfg.Logger.Warn("pending voice is invalid or sender is no longer allowed; retaining record", "chat_id", chatID)
				continue
			}
			if total >= maxQueuedVoices {
				r.cfg.Logger.Warn("pending voice queue exceeds capacity; remaining records retained")
				return
			}
			total++
			r.submitVoice(ctx, &msg, chatID, replyToBot, fileID, marker)
		}
	}
}

func (r *Receiver) cancelVoice(chatID string) bool {
	if r.voiceJobs == nil {
		return false
	}
	r.voiceMu.Lock()
	defer r.voiceMu.Unlock()
	active := len(r.cfg.VoiceStore.All()[chatID]) > 0
	r.voiceJobs.Cancel(chatID)
	if err := r.cfg.VoiceStore.Clear(chatID); err != nil {
		r.cfg.Logger.Error("clear cancelled voice queue", "error", err)
	}
	return active
}
