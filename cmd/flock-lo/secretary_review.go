package main

import (
	"context"
	"crypto/subtle"
	"strings"
	"time"

	"github.com/duckbugio/flock/adapters/lo"
	"github.com/duckbugio/flock/internal/config"
)

// ReviewCallback records owner consent before the pump attempts a delegated send.
// Callback handling stays outside agent admission and never runs tools.
const secretaryReviewDiscard = "discard"

func (m *secretaryManager) ReviewCallback(_ context.Context, q *lo.CallbackQuery) string {
	const unavailable = "Reply is no longer available."
	const saveFailed = "Reply could not be saved. Please try again later."
	if !m.reviewCallbackAllowed(q) {
		return unavailable
	}
	parts := strings.Split(q.Data, ":")
	if len(parts) != 3 || parts[0] != "lo-secretary" ||
		(parts[1] != "send" && parts[1] != secretaryReviewDiscard) || len(parts[2]) != 26 {
		return unavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failure != nil {
		return saveFailed
	}
	for key, job := range m.state.Jobs {
		if subtle.ConstantTimeCompare([]byte(job.ReviewToken), []byte(parts[2])) != 1 ||
			job.Mode != config.SecretaryModeApproval || job.OwnerID != q.From.ID || job.NoticeID != q.Message.ID {
			continue
		}
		switch job.Status {
		case secretaryUnknown:
			return "Delivery outcome is unknown. Check the target chat."
		case secretaryDone:
			return "Reply sent."
		case secretaryCancelled:
			return unavailable
		case secretaryApproved:
			return "Reply approved; delivery is pending."
		case secretaryAwaiting:
		default:
			return unavailable
		}
		if job.Message.Date <= time.Now().Add(-24*time.Hour).Unix() {
			m.invalidateLocked(key, job)
			if m.saveLocked() != nil {
				return saveFailed
			}
			return "Reply expired."
		}
		if parts[1] == secretaryReviewDiscard {
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
		if parts[1] == secretaryReviewDiscard {
			return "Reply discarded."
		}
		return "Reply approved; delivery is pending."
	}
	return unavailable
}

func (m *secretaryManager) reviewCallbackAllowed(q *lo.CallbackQuery) bool {
	if q == nil || q.From == nil || q.From.IsBot || !m.cfg.IsLOAllowed(q.From.ID) || q.Message == nil ||
		q.Message.From == nil || !q.Message.From.IsBot || q.Message.From.ID != m.botID ||
		q.Message.Chat.Type != "private" || q.Message.Chat.ID != q.From.ID || q.Message.ID <= 0 {
		return false
	}
	return true
}
