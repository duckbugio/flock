package main

import (
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/duckbugio/flock/adapters/lo"
)

// Revision is the server's conversation change sequence, assigned under its
// conversation lock. Message IDs can be allocated before that lock is acquired.
type secretaryManualReply struct {
	OwnerID      int64               `json:"ownerId"`
	ConnectionID string              `json:"connectionId"`
	Context      lo.SecretaryContext `json:"context"`
	ObservedAt   int64               `json:"observedAt"`
}

func (marker secretaryManualReply) key() string {
	return strconv.FormatInt(marker.OwnerID, 10) + ":" + marker.ConnectionID + ":" +
		marker.Context.ConversationID.String() + ":" + marker.Context.ChatID.String() + ":" +
		marker.Context.PolicyVersion.String()
}

func (marker secretaryManualReply) valid() bool {
	return marker.OwnerID > 0 && marker.OwnerID < 1000000000000000 &&
		lo.ValidSecretaryConnectionID(marker.ConnectionID) && marker.Context.Valid() && marker.ObservedAt > 0
}

func isSecretaryManualCandidate(msg *lo.SecretaryMessage) bool {
	// Incoming private messages have the correspondent as both From and Chat.
	return msg != nil && msg.Valid() && msg.BotID == 0 && msg.From.ID != int64(msg.Chat.ID)
}

func (m *secretaryManager) observeManualReplyLocked(msg lo.SecretaryMessage, conn lo.SecretaryConnection) error {
	if !conn.Valid() || conn.ID != msg.ConnectionID || !conn.Enabled || conn.PolicyVersion != msg.Context.PolicyVersion ||
		conn.User.ID != msg.From.ID || !m.cfg.IsLOAllowed(conn.User.ID) || !slices.Contains(conn.Rights, "receive_messages") {
		return nil
	}
	if latest, exists := m.state.Connections[conn.ID]; exists && latest.PolicyVersion >= conn.PolicyVersion &&
		(latest.PolicyVersion != conn.PolicyVersion || latest.User.ID != conn.User.ID || !latest.Enabled ||
			!slices.Contains(latest.Rights, "receive_messages")) {
		return nil
	}
	marker := secretaryManualReply{
		OwnerID: conn.User.ID, ConnectionID: conn.ID, Context: msg.Context, ObservedAt: time.Now().Unix(),
	}
	key := marker.key()
	previous, exists := m.state.ManualReplies[key]
	if !exists && len(m.state.ManualReplies) >= secretaryMaxJobs {
		// Do not acknowledge a takeover that cannot be retained against replay.
		return errors.New("LO secretary manual reply watermark capacity reached")
	}
	if !exists || marker.Context.SourceRevision > previous.Context.SourceRevision {
		m.state.ManualReplies[key] = marker
	}
	for jobKey, job := range m.state.Jobs {
		scope := secretaryManualReply{OwnerID: conn.User.ID, ConnectionID: job.Message.ConnectionID, Context: job.Message.Context}
		if scope.key() == key && m.manuallyAnsweredLocked(conn.User.ID, job) {
			m.invalidateLocked(jobKey, job)
		}
	}
	return nil
}

func (m *secretaryManager) manuallyAnsweredLocked(ownerID int64, job secretaryJob) bool {
	// A send already attempted must keep its exact request, retry and receipt
	// semantics. Manual takeover cannot prove whether that write took effect.
	if job.DeliveryAttempted || secretaryTerminal(job.Status) || (job.OwnerID != 0 && job.OwnerID != ownerID) ||
		job.Message.From.ID != int64(job.Message.Chat.ID) {
		return false
	}
	marker := secretaryManualReply{OwnerID: ownerID, ConnectionID: job.Message.ConnectionID, Context: job.Message.Context}
	previous, exists := m.state.ManualReplies[marker.key()]
	return exists && job.Message.Context.SourceRevision <= previous.Context.SourceRevision
}
