package main

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/duckbugio/flock/core/pending"
)

const anchorCleanupTimeout = 5 * time.Second

type pendingResumer interface {
	ResumePending(chatID string, marker pending.Marker)
}

type anchorDeleter interface {
	Delete(ctx context.Context, chatID, messageID string) error
}

// Resume only markers admitted by this adapter and persisted in its own store.
// Keep each marker until the shared runtime records a clean terminal result.
func resumePending(ctx context.Context, transport anchorDeleter, svc pendingResumer, store pending.Store, logger *slog.Logger) {
	for chatID, queue := range store.All() {
		id, err := strconv.ParseInt(chatID, 10, 64)
		if err != nil || id == 0 {
			logger.Warn("pending: invalid LO chat id; retaining queue", "chat_id", chatID)
			continue
		}
		for _, marker := range queue {
			if ctx.Err() != nil {
				return
			}
			if marker.AnchorMsgID != "" {
				cleanup, cancel := context.WithTimeout(ctx, anchorCleanupTimeout)
				err := transport.Delete(cleanup, chatID, marker.AnchorMsgID)
				cancel()
				if err != nil {
					logger.Debug("delete dangling LO anchor on resume", "chat_id", chatID, "error", err)
				}
			}
			if ctx.Err() != nil {
				return
			}
			svc.ResumePending(chatID, marker)
		}
	}
}
