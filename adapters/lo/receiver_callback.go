package lo

import (
	"context"
	"strconv"

	"github.com/duckbugio/flock/core/chat"
)

const callbackUnavailable = "Not available."

// CallbackService exposes user-triggered control actions independently of model admission.
type CallbackService interface {
	Stop(runID string) bool
	StarPress() (toast string, ok bool)
}

func (r *Receiver) handleCallback(ctx context.Context, query *CallbackQuery) {
	if query.ID == "" || r.cfg.Client == nil {
		return
	}
	toast := r.callbackToast(ctx, query)
	if err := r.cfg.Client.AnswerCallbackQuery(ctx, query.ID, toast); err != nil {
		r.cfg.Logger.Warn("lo: answer callback failed", "error", err)
	}
}

func (r *Receiver) callbackToast(ctx context.Context, query *CallbackQuery) string {
	if !r.callbackAllowed(query) {
		return callbackUnavailable
	}
	chatID := strconv.FormatInt(query.Message.Chat.ID, 10)
	if query.Data == "star:confirm" {
		// The core bounds the account operation independently of the polling request.

		toast, ok := r.cfg.Callbacks.StarPress()
		if ok {
			r.confirmStar(ctx, chatID, strconv.FormatInt(query.Message.ID, 10))
		}
		return toast
	}
	runID := r.cfg.Transport.stopButtonRun(chatID, query.Data)
	if runID == "" {
		return callbackUnavailable
	}
	if r.stopCallbackRun(chatID, runID) {
		return "Stopping…"
	}
	return "Run already finished."
}

func (r *Receiver) confirmStar(ctx context.Context, chatID, messageID string) {
	if err := r.cfg.Transport.Edit(ctx, chatID, messageID, chat.StarDoneText(), "", false); err != nil {
		r.cfg.Logger.Warn("lo: confirm star message failed", "error", err)
		return
	}
}

func (r *Receiver) callbackAllowed(query *CallbackQuery) bool {
	if r.cfg.Callbacks == nil || r.cfg.Transport == nil || !r.cfg.Transport.keyboards || r.cfg.BotID <= 0 ||
		query.From == nil || query.From.ID <= 0 || query.From.IsBot || !r.cfg.IsAllowed(query.From.ID) {
		return false
	}
	msg := query.Message
	if msg == nil || msg.ID <= 0 || msg.From == nil || !msg.From.IsBot || msg.From.ID != r.cfg.BotID {
		return false
	}
	switch msg.Chat.Type {
	case privateChatType:
		return msg.Chat.ID == query.From.ID
	case groupChatType, supergroupChatType:
		return msg.Chat.ID < 0
	default:
		return false
	}
}

// stopCallbackRun excludes late transcription handoff while cancelling the active run.
// A stale run token must not purge voice work queued for a newer run.
func (r *Receiver) stopCallbackRun(chatID, runID string) bool {
	r.voiceMu.Lock()
	defer r.voiceMu.Unlock()
	if !r.cfg.Callbacks.Stop(runID) {
		return false
	}
	if r.voiceJobs != nil {
		r.cancelVoiceLocked(chatID)
	}
	return true
}
