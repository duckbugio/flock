package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/duckbugio/flock/adapters/lo"
	"github.com/duckbugio/flock/internal/config"
)

const secretaryOwnerScopeTest = "owner"

func secretaryManualMessage() lo.SecretaryMessage {
	msg := secretaryMessage()
	msg.ID, msg.Context.SourceMessageID, msg.Context.SourceRevision = 13, 13, 2
	msg.From.ID, msg.EventID, msg.Text = 1, "manual", ""
	return msg
}

func handleSecretaryManual(t *testing.T, m *secretaryManager, msg lo.SecretaryMessage, updateID int64) {
	t.Helper()
	if err := m.Handle(t.Context(), lo.Update{ID: updateID, BusinessMessage: &msg}); err != nil {
		t.Fatal(err)
	}
}

func TestSecretaryManualReplyCancelsReviewAndSurvivesRestart(t *testing.T) {
	t.Parallel()
	m, api, runner := secretaryFixture(t, config.SecretaryModeApproval)
	key := admitSecretary(t, m, secretaryMessage())
	m.process(t.Context(), key)
	query := secretaryReviewQuery(m, key, lo.SecretaryActionSend)
	manual := secretaryManualMessage() // Empty text is also a valid media-only takeover.
	handleSecretaryManual(t, m, manual, 100)
	if m.state.Jobs[key].Status != secretaryCancelled || len(m.state.Jobs) != 1 || runner.calls != 1 || api.sends != 0 {
		t.Fatal("manual reply did not cancel only the existing review")
	}
	m.syncReviewNotices(t.Context())
	if len(api.noticeStatuses) != 1 || m.state.Jobs[key].NoticeStatus != secretaryCancelled {
		t.Fatal("cancelled review buttons were not closed")
	}
	reloaded, err := newSecretaryManager(m.cfg, api, m.botID, m.runtime)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.ReviewCallback(t.Context(), query); !strings.Contains(got, "no longer") {
		t.Fatal("old approval remained actionable", got)
	}
	handleSecretaryManual(t, reloaded, manual, 101)
	late := secretaryMessage()
	late.ID, late.Context.SourceMessageID = 99, 99 // IDs do not establish conversation order.
	handleSecretaryManual(t, reloaded, late, 102)
	lateKey := secretaryJobKey(m.botID, late)
	reloaded.process(t.Context(), lateKey)
	if reloaded.state.Jobs[lateKey].Status != secretaryCancelled || runner.calls != 1 || api.sends != 0 {
		t.Fatal("late older revision was generated after restart")
	}
	newer := secretaryMessage()
	newer.ID, newer.Context.SourceMessageID, newer.Context.SourceRevision = 3, 3, 3
	handleSecretaryManual(t, reloaded, newer, 103)
	newKey := secretaryJobKey(m.botID, newer)
	reloaded.process(t.Context(), newKey)
	if runner.calls != 2 || reloaded.state.Jobs[newKey].Status != secretaryAwaiting {
		t.Fatal("newer revision with lower message ID was suppressed")
	}
	handleSecretaryManual(t, reloaded, manual, 104)
	if reloaded.state.Jobs[newKey].Status != secretaryAwaiting {
		t.Fatal("replayed manual event cancelled a newer incoming revision")
	}
}

func TestSecretaryManualReplyScopeAndAuthority(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{
		"owner", "connection", "conversation", "peer", "policy", "sender", "bot actor", "bot attribution",
		"revoked", "no receive", "receive only", "newer cached policy", "disallowed owner",
	} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			m, api, runner := secretaryFixture(t, config.SecretaryModeApproval)
			key := admitSecretary(t, m, secretaryMessage())
			m.process(t.Context(), key)
			manual := secretaryManualMessage()
			switch kind {
			case secretaryOwnerScopeTest, "disallowed owner":
				api.connection.User.ID, manual.From.ID = 2, 2
				if kind == secretaryOwnerScopeTest {
					m.cfg.LOAllowedUsers = append(m.cfg.LOAllowedUsers, 2)
				}
			case "connection":
				api.connection.ID = "25eac687-9383-4da1-9644-826693923e44"
				manual.ConnectionID = api.connection.ID
			case "conversation":
				manual.Context.ConversationID++
			case "peer":
				manual.Chat.ID++
				manual.Context.ChatID = manual.Chat.ID
			case "policy":
				api.connection.PolicyVersion++
				manual.Context.PolicyVersion++
			case "sender":
				manual.From.ID = 3
			case "bot actor":
				manual.From.IsBot = true
			case "bot attribution":
				manual.BotID = secretaryTestBot
			case "revoked":
				api.connection.Enabled = false
			case "no receive":
				api.connection.Rights = []string{"send_messages"}
			case "receive only":
				api.connection.Rights = []string{"receive_messages"}
			case "newer cached policy":
				latest := api.connection
				latest.PolicyVersion++
				m.state.Connections[latest.ID] = latest
			}
			handleSecretaryManual(t, m, manual, 100)
			want := secretaryAwaiting
			if kind == "receive only" {
				want = secretaryCancelled
			}
			if m.state.Jobs[key].Status != want || len(m.state.Jobs) != 1 || runner.calls != 1 || api.sends != 0 {
				t.Fatalf("manual event crossed scope or generated work: %s", m.state.Jobs[key].Status)
			}
		})
	}
}

func TestSecretaryManualReplyDuringGenerationAndNotice(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"queued", "running", "notice", "approved"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			m, api, runner := secretaryFixture(t, config.SecretaryModeApproval)
			key := admitSecretary(t, m, secretaryMessage())
			cancel := func() { handleSecretaryManual(t, m, secretaryManualMessage(), 100) }
			switch phase {
			case "queued":
				cancel()
			case "running":
				runner.onRun = func(ctx context.Context) {
					cancel()
					if ctx.Err() == nil {
						t.Fatal("manual takeover did not cancel active generation")
					}
				}
			case "notice":
				api.onNotice = cancel
			case "approved":
				m.process(t.Context(), key)
				m.ReviewCallback(t.Context(), secretaryReviewQuery(m, key, lo.SecretaryActionSend))
				cancel()
			}
			m.process(t.Context(), key)
			if m.state.Jobs[key].Status != secretaryCancelled || api.sends != 0 {
				t.Fatal("manual takeover failed before delegated send")
			}
			if phase == "notice" {
				m.syncReviewNotices(t.Context())
				if m.state.Jobs[key].NoticeID != 99 || m.state.Jobs[key].NoticeStatus != secretaryCancelled {
					t.Fatal("late notice receipt lost or resurrected cancelled review")
				}
			}
		})
	}
}

func TestSecretaryManualReplyPreservesAttemptedDelivery(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"inflight success", "inflight timeout", "retry", "unknown", "done"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			m, api, runner := secretaryFixture(t, config.SecretaryModeApproval)
			key := admitSecretary(t, m, secretaryMessage())
			m.process(t.Context(), key)
			m.ReviewCallback(t.Context(), secretaryReviewQuery(m, key, lo.SecretaryActionSend))
			cancel := func() {
				before := m.state.Jobs[key]
				handleSecretaryManual(t, m, secretaryManualMessage(), 100)
				if !reflect.DeepEqual(m.state.Jobs[key], before) {
					t.Fatal("manual reply rewrote an attempted delivery or receipt")
				}
			}
			if phase != "done" && phase != "inflight success" {
				api.err = errors.New("lost response")
			}
			if strings.HasPrefix(phase, "inflight") {
				api.onSend = cancel
			}
			m.process(t.Context(), key)
			api.onSend = nil
			if phase == "unknown" {
				m.finish(key, secretaryUnknown)
			}
			if !strings.HasPrefix(phase, "inflight") {
				cancel()
			}
			reloaded, err := newSecretaryManager(m.cfg, api, m.botID, m.runtime)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(reloaded.state.Jobs[key], m.state.Jobs[key]) || runner.calls != 1 || api.sends != 1 {
				t.Fatal("restart changed the exact attempted-delivery outcome")
			}
			if phase == "retry" {
				api.err = nil
				reloaded.process(t.Context(), key)
				if api.sends != 2 || api.actions[0] != api.actions[1] || runner.calls != 1 ||
					reloaded.state.Jobs[key].Status != secretaryDone {
					t.Fatal("manual marker changed idempotent retry semantics")
				}
			}
		})
	}
}

func TestSecretaryManualStateMigrationBoundsAndValidation(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"v2", "bad key", "bad owner", "capacity"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			m, api, _ := secretaryFixture(t, config.SecretaryModeApproval)
			manual := secretaryManualMessage()
			handleSecretaryManual(t, m, manual, 100)
			switch kind {
			case "v2":
				m.state.Version, m.state.ManualReplies = 2, nil
			case "bad key":
				for _, marker := range m.state.ManualReplies {
					m.state.ManualReplies["wrong"] = marker
				}
			case "bad owner":
				for key, marker := range m.state.ManualReplies {
					marker.OwnerID = 0
					m.state.ManualReplies[key] = marker
				}
			case "capacity":
				for id := range secretaryMaxJobs {
					marker := secretaryManualReply{
						OwnerID: 1, ConnectionID: manual.ConnectionID, Context: manual.Context,
						ObservedAt: time.Now().Unix(),
					}
					marker.Context.ConversationID = lo.SecretaryNumber(id + 1)
					m.state.ManualReplies[marker.key()] = marker
				}
				manual.Context.ConversationID = secretaryMaxJobs + 1
				if err := m.Handle(t.Context(), lo.Update{ID: 101, BusinessMessage: &manual}); err == nil || m.state.UpdateFloor != 100 {
					t.Fatal("unretained manual takeover was acknowledged")
				}
				return
			}
			if err := m.saveLocked(); err != nil {
				t.Fatal(err)
			}
			reloaded, err := newSecretaryManager(m.cfg, api, m.botID, m.runtime)
			if kind != "v2" {
				if err == nil {
					t.Fatal("invalid manual watermark state loaded")
				}
				return
			}
			if err != nil || reloaded.state.Version != 3 || reloaded.state.ManualReplies == nil {
				t.Fatal("v2 migration failed", err)
			}
			data, err := os.ReadFile(m.path)
			if err != nil {
				t.Fatal(err)
			}
			var disk secretaryState
			if err := json.Unmarshal(data, &disk); err != nil || disk.Version != 3 {
				t.Fatal("migration was not durable", err)
			}
		})
	}
}

func TestSecretaryManualDuplicateDoesNotLookupRevokedConnection(t *testing.T) {
	t.Parallel()
	m, api, _ := secretaryFixture(t, config.SecretaryModeApproval)
	manual := secretaryManualMessage()
	handleSecretaryManual(t, m, manual, 100)
	api.onLookup = func() { t.Fatal("acknowledged replay queried a possibly deleted connection") }
	handleSecretaryManual(t, m, manual, 100)
}

func TestSecretaryManualPersistenceFailureNeverAcknowledges(t *testing.T) {
	t.Parallel()
	m, api, _ := secretaryFixture(t, config.SecretaryModeApproval)
	key := admitSecretary(t, m, secretaryMessage())
	m.process(t.Context(), key)
	// A full state must fail closed rather than acknowledging an unretained fence.
	m.state.Invalid[strings.Repeat("x", secretaryMaxStateBytes)] = time.Now().Unix()
	manual := secretaryManualMessage()
	if err := m.Handle(t.Context(), lo.Update{ID: 100, BusinessMessage: &manual}); err == nil || m.err() == nil {
		t.Fatal("state byte overflow acknowledged manual takeover")
	}
	reloaded, err := newSecretaryManager(m.cfg, api, m.botID, m.runtime)
	if err != nil || reloaded.state.UpdateFloor != 12 || len(reloaded.state.ManualReplies) != 0 {
		t.Fatal("failed write changed durable polling watermark", err)
	}
	handleSecretaryManual(t, reloaded, manual, 100)
	if reloaded.state.Jobs[key].Status != secretaryCancelled {
		t.Fatal("unacknowledged event did not apply after restart")
	}
}

func TestSecretaryManualStateRejectsMissingV3Watermarks(t *testing.T) {
	t.Parallel()
	m, api, _ := secretaryFixture(t, config.SecretaryModeApproval)
	data, err := os.ReadFile(m.path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "manualReplies")
	data, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newSecretaryManager(m.cfg, api, m.botID, m.runtime); err == nil {
		t.Fatal("v3 state without its mandatory replay fence was accepted")
	}
}
