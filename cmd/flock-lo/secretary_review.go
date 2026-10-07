package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/duckbugio/flock/adapters/lo"
	"github.com/duckbugio/flock/internal/config"
)

const (
	secretaryReviewSenderRunes    = 80
	secretaryReviewIncomingRunes  = 500
	secretaryReviewReceiptTimeout = 5 * time.Second
)

// ReviewCallback records owner consent before the pump attempts a delegated send.
// Callback handling stays outside agent admission and never runs tools.
func (m *secretaryManager) ReviewCallback(_ context.Context, q *lo.CallbackQuery) string {
	const unavailable = "Reply is no longer available."
	const saveFailed = "Reply could not be saved. Please try again later."
	if !m.reviewCallbackAllowed(q) {
		return unavailable
	}
	parts := strings.Split(strings.TrimPrefix(q.Data, lo.SecretaryCallbackPrefix), ":")
	if !strings.HasPrefix(q.Data, lo.SecretaryCallbackPrefix) || len(parts) != 2 ||
		(parts[0] != lo.SecretaryActionSend && parts[0] != lo.SecretaryActionDiscard) || !lo.ValidSecretaryReviewToken(parts[1]) {
		return unavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failure != nil {
		return saveFailed
	}
	for key, job := range m.state.Jobs {
		if subtle.ConstantTimeCompare([]byte(job.ReviewToken), []byte(parts[1])) != 1 ||
			job.Mode != config.SecretaryModeApproval || job.OwnerID != q.From.ID {
			continue
		}
		if job.Status == secretaryPrepared && job.NoticeID == 0 {
			return "Review is being saved. Please try again."
		}
		if job.NoticeID != q.Message.ID {
			continue
		}
		switch job.Status {
		case secretaryUnknown:
			return "Delivery outcome is unknown. Check the target chat."
		case secretaryDone:
			return "Reply sent."
		case secretaryRejected:
			return "LO rejected the reply; it was not sent."
		case secretaryCancelled:
			return unavailable
		case secretaryApproved:
			return "Reply approved; delivery is pending."
		case secretaryAwaiting:
		default:
			return unavailable
		}
		if job.Message.Date <= time.Now().Add(-secretaryApprovalLifetime).Unix() {
			m.invalidateLocked(key, job)
			if m.saveLocked() != nil {
				return saveFailed
			}
			return "Reply expired."
		}
		if parts[0] == lo.SecretaryActionDiscard {
			job.Status = secretaryCancelled
			m.state.Jobs[key] = compactSecretaryJob(job)
		} else {
			job.Status, job.NextAttemptAt, job.Attempts = secretaryApproved, 0, 0
			m.state.Jobs[key] = job
		}
		if m.saveLocked() != nil {
			return saveFailed
		}
		select {
		case m.wake <- struct{}{}:
		default:
		}
		if parts[0] == lo.SecretaryActionDiscard {
			return "Reply discarded."
		}
		return "Reply approved; delivery is pending."
	}
	return unavailable
}

func secretaryReviewContext(message lo.SecretaryMessage, incoming string) string {
	sender := fmt.Sprintf("LO #%d", message.From.ID)
	if name := []rune(strings.TrimSpace(message.From.Username)); len(name) > 0 {
		sender = "@" + string(name[:min(len(name), secretaryReviewSenderRunes)]) + " (" + sender + ")"
	}
	text := []rune(strings.TrimSpace(incoming))
	if len(text) > secretaryReviewIncomingRunes {
		text = append(text[:secretaryReviewIncomingRunes], '…')
	}
	if len(text) == 0 {
		text = []rune("Incoming message content is unavailable.")
	}
	return fmt.Sprintf("Secretary · Chat %s · Incoming #%s\nFrom: %s\n\nIncoming:\n%s",
		message.Chat.ID.String(), message.ID.String(), sender, string(text))
}

// Context is attempted once and its confirmed message ID is durable before
// publishing controls. A lost context response cannot authorize a blind preview.
// Preview attempts are durable and bounded because ordinary send has no dedupe.
func (m *secretaryManager) publishReviewNotice(ctx context.Context, key string, job secretaryJob) {
	if job.ContextNoticeID == 0 {
		var ok bool
		job, ok = m.publishReviewContext(ctx, key, job)
		if !ok {
			return
		}
	}
	if ctx.Err() != nil || !m.live(key) {
		return
	}
	if job.NoticeAttempts >= secretaryMaxNoticeAttempts {
		m.finish(key, secretaryCancelled)
		return
	}
	job.NoticeAttempts++
	if !m.store(key, job) {
		return
	}
	id, err := m.api.SendSecretaryReviewNotice(ctx, job.OwnerID, m.botID, int64(job.Message.Chat.ID),
		job.Action.Text, job.ReviewToken)
	if err == nil && id > 0 {
		job.NoticeID, job.Status = id, secretaryAwaiting
		m.store(key, job)
		return
	}
	var apiErr *lo.APIError
	if job.NoticeAttempts >= secretaryMaxNoticeAttempts ||
		(errors.As(err, &apiErr) && apiErr.Code >= 400 && apiErr.Code < 500 && apiErr.Code != 429) {
		m.finish(key, secretaryCancelled)
	} else {
		m.retry(key, err)
	}
	slog.Warn("LO secretary review preview failed", "update_id", job.UpdateID, "attempt", job.NoticeAttempts)
}

func (m *secretaryManager) publishReviewContext(ctx context.Context, key string, job secretaryJob) (secretaryJob, bool) {
	if job.ContextAttempted {
		m.finish(key, secretaryCancelled)
		return job, false
	}
	job.ContextAttempted = true
	if !m.store(key, job) {
		return job, false
	}
	incoming := job.ReviewContext
	if incoming == "" {
		incoming = secretaryReviewContext(job.Message, strings.TrimSpace(job.Message.Text+"\n"+job.Message.Caption))
	}
	id, err := m.api.SendSecretaryReviewContext(ctx, job.OwnerID, m.botID, incoming)
	if err != nil || id <= 0 {
		m.finish(key, secretaryCancelled)
		slog.Warn("LO secretary review context unconfirmed; reply cancelled", "update_id", job.UpdateID)
		return job, false
	}
	job.ContextNoticeID = id
	return job, m.store(key, job)
}

// Only the pump edits receipts, avoiding out-of-order writes from concurrent
// completion and consent callbacks. A failed edit never retries delegated send.
func (m *secretaryManager) syncReviewNotices(ctx context.Context) {
	const batchLimit = 16
	ctx, cancel := context.WithTimeout(ctx, secretaryReviewReceiptTimeout)
	defer cancel()
	m.mu.Lock()
	jobs := make(map[string]secretaryJob)
	if m.failure == nil {
		for key, job := range m.state.Jobs {
			// An invalidated send can still return a known success. Do not publish
			// unknown before it settles: a timed-out edit may take effect later.
			if job.Status == secretaryUnknown && m.active[key] != nil {
				continue
			}
			if job.NoticeID > 0 && secretaryTerminal(job.Status) && job.NoticeStatus != job.Status &&
				job.NoticeRetryAt <= time.Now().Unix() {
				jobs[key] = job
				if len(jobs) == batchLimit {
					break
				}
			}
		}
	}
	m.mu.Unlock()
	for key, job := range jobs {
		if ctx.Err() != nil {
			return
		}
		status := "Reply discarded or no longer valid."
		switch job.Status {
		case secretaryDone:
			status = "Reply sent to chat " + job.Message.Chat.ID.String() + "."
		case secretaryRejected:
			status = "LO rejected the reply; it was not sent."
		case secretaryUnknown:
			status = "Delivery outcome is unknown. Check chat " + job.Message.Chat.ID.String() + "."
		}
		err := m.api.CloseSecretaryReviewNotice(ctx, job.OwnerID, m.botID, job.NoticeID, status)
		if err != nil {
			slog.Warn("update LO secretary review receipt", "error", err)
		}
		m.mu.Lock()
		current := m.state.Jobs[key]
		if current.NoticeID == job.NoticeID && current.Status == job.Status && m.failure == nil {
			if err == nil {
				current.NoticeStatus, current.NoticeRetryAt = job.Status, 0
			} else {
				current.NoticeRetryAt = time.Now().Add(secretaryMaxRetryInterval).Unix()
			}
			m.state.Jobs[key] = current
			if saveErr := m.saveLocked(); saveErr != nil {
				slog.Error("persist LO secretary review receipt", "error", saveErr)
			}
		}
		m.mu.Unlock()
	}
}

func (m *secretaryManager) reviewCallbackAllowed(q *lo.CallbackQuery) bool {
	if q == nil || q.From == nil || q.From.IsBot || !m.cfg.IsLOAllowed(q.From.ID) || q.Message == nil ||
		q.Message.From == nil || !q.Message.From.IsBot || q.Message.From.ID != m.botID ||
		q.Message.Chat.Type != "private" || q.Message.Chat.ID != q.From.ID || q.Message.ID <= 0 {
		return false
	}
	return true
}
