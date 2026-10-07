package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duckbugio/flock/adapters/lo"
	"github.com/duckbugio/flock/internal/config"
)

const (
	reviewNoticeTest = "notice"
)

func secretaryReviewQuery(m *secretaryManager, key, decision string) *lo.CallbackQuery {
	job := m.state.Jobs[key]
	msg := &lo.Message{ID: job.NoticeID, From: &lo.User{ID: m.botID, IsBot: true}}
	msg.Chat.ID, msg.Chat.Type = job.OwnerID, "private"
	return &lo.CallbackQuery{
		ID: "owner-query", From: &lo.User{ID: job.OwnerID}, Message: msg,
		Data: "lo-secretary:" + decision + ":" + job.ReviewToken,
	}
}

func TestSecretaryOwnerReviewRejectsForgedCallbacks(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"owner", "sender-bot", reviewNoticeTest, "group", "bot-author", "token", "decision"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			m, api, _ := secretaryFixture(t, config.SecretaryModeApproval)
			key := admitSecretary(t, m, secretaryMessage())
			m.process(t.Context(), key)
			q := secretaryReviewQuery(m, key, "send")
			switch kind {
			case "owner":
				q.From.ID++
			case "sender-bot":
				q.From.IsBot = true
			case reviewNoticeTest:
				q.Message.ID++
			case "group":
				q.Message.Chat.Type = "group"
			case "bot-author":
				q.Message.From.ID++
			case "token":
				q.Data = "lo-secretary:send:" + strings.Repeat("x", 26)
			case "decision":
				q.Data = strings.Replace(q.Data, ":send:", ":approve:", 1)
			}
			m.ReviewCallback(t.Context(), q)
			m.process(t.Context(), key)
			if api.sends != 0 || m.state.Jobs[key].Status != secretaryAwaiting {
				t.Fatal("forged approval accepted")
			}
		})
	}
}

func TestSecretaryApprovedDeliveryRestartsWithSameAction(t *testing.T) {
	t.Parallel()
	m, api, runner := secretaryFixture(t, config.SecretaryModeApproval)
	key := admitSecretary(t, m, secretaryMessage())
	m.process(t.Context(), key)
	q := secretaryReviewQuery(m, key, "send")
	m.ReviewCallback(t.Context(), q)
	api.err = errors.New("lost response after commit")
	m.process(t.Context(), key)
	first := api.actions[0]
	if m.state.Jobs[key].Status != secretaryApproved {
		t.Fatal("uncertain approved send discarded")
	}
	reloaded, err := newSecretaryManager(m.cfg, api, m.botID, m.runtime)
	if err != nil {
		t.Fatal(err)
	}
	api.err = nil
	reloaded.ReviewCallback(t.Context(), q)
	reloaded.process(t.Context(), key)
	reloaded.ReviewCallback(t.Context(), q)
	reloaded.process(t.Context(), key)
	if api.sends != 2 || runner.calls != 1 || api.actions[1] != first || reloaded.state.Jobs[key].Status != secretaryDone {
		t.Fatal("approval restart regenerated or changed the idempotent send")
	}
	if reloaded.state.Jobs[key].Action.Text != "" {
		t.Fatal("terminal reply text retained")
	}
}

func TestSecretaryDiscardAndExpiredReviewNeverSend(t *testing.T) {
	t.Parallel()
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: secretaryReviewDiscard, true: "expired"}[expired], func(t *testing.T) {
			t.Parallel()
			m, api, _ := secretaryFixture(t, config.SecretaryModeApproval)
			key := admitSecretary(t, m, secretaryMessage())
			m.process(t.Context(), key)
			decision := secretaryReviewDiscard
			if expired {
				job := m.state.Jobs[key]
				job.Message.Date = time.Now().Add(-24 * time.Hour).Unix()
				m.state.Jobs[key] = job
				decision = "send"
			}
			m.ReviewCallback(t.Context(), secretaryReviewQuery(m, key, decision))
			m.process(t.Context(), key)
			if api.sends != 0 || m.state.Jobs[key].Status != secretaryCancelled {
				t.Fatal("cancelled/expired reply sent")
			}
		})
	}
}

func TestSecretaryApprovalCannotSendAfterConsentRevocation(t *testing.T) {
	t.Parallel()
	m, api, _ := secretaryFixture(t, config.SecretaryModeApproval)
	key := admitSecretary(t, m, secretaryMessage())
	m.process(t.Context(), key)
	m.ReviewCallback(t.Context(), secretaryReviewQuery(m, key, "send"))
	api.connection.Enabled = false
	m.process(t.Context(), key)
	if api.sends != 0 || m.state.Jobs[key].Status != secretaryCancelled {
		t.Fatal("revoked approved reply sent")
	}
}

func TestSecretaryLegacyPreparedReviewIsNotAutoApproved(t *testing.T) {
	t.Parallel()
	m, api, runner := secretaryFixture(t, config.SecretaryModeApproval)
	key := admitSecretary(t, m, secretaryMessage())
	m.process(t.Context(), key)
	job := m.state.Jobs[key]
	job.Status, job.ReviewToken, job.NoticeID, job.Action.Reason = secretaryPrepared, "", 0, "manual_review"
	m.state.Jobs[key], m.state.Version = job, 1
	data, err := json.Marshal(m.state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded, err := newSecretaryManager(m.cfg, api, m.botID, m.runtime)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.process(t.Context(), key)
	if api.sends != 0 || runner.calls != 1 || reloaded.state.Jobs[key].Status != secretaryCancelled {
		t.Fatal("legacy native review became a bot-owned send")
	}
}

func TestSecretaryApprovalPersistenceFailureNeverSends(t *testing.T) {
	t.Parallel()
	m, api, _ := secretaryFixture(t, config.SecretaryModeApproval)
	key := admitSecretary(t, m, secretaryMessage())
	m.process(t.Context(), key)
	q := secretaryReviewQuery(m, key, "send")
	m.path = filepath.Join(t.TempDir(), "missing", "state.json")
	toast := m.ReviewCallback(t.Context(), q)
	m.process(t.Context(), key)
	if api.sends != 0 || m.err() == nil || !strings.Contains(toast, "could not be saved") {
		t.Fatal("approval persistence failed open")
	}
}

func TestSecretaryReviewRestartAndConcurrentCallbacks(t *testing.T) {
	t.Parallel()
	m, api, runner := secretaryFixture(t, config.SecretaryModeApproval)
	key := admitSecretary(t, m, secretaryMessage())
	m.process(t.Context(), key)
	q := secretaryReviewQuery(m, key, "send")
	reloaded, err := newSecretaryManager(m.cfg, api, m.botID, m.runtime)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.process(t.Context(), key)
	if api.sends != 0 || runner.calls != 1 || api.drafts != 1 {
		t.Fatal("restart implicitly approved or re-generated preview")
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { reloaded.ReviewCallback(t.Context(), q) })
	}
	wg.Wait()
	reloaded.process(t.Context(), key)
	if api.sends != 1 || reloaded.state.Jobs[key].Status != secretaryDone {
		t.Fatal("concurrent approval sent twice")
	}
}

func TestSecretaryLostSendOutcomeSurvivesRevocationAndRestart(t *testing.T) {
	t.Parallel()
	for _, invalidation := range []string{"revoke", "expiry", "edited", "mode"} {
		t.Run(invalidation, func(t *testing.T) {
			t.Parallel()
			m, api, _ := secretaryFixture(t, config.SecretaryModeApproval)
			key := admitSecretary(t, m, secretaryMessage())
			m.process(t.Context(), key)
			q := secretaryReviewQuery(m, key, "send")
			m.ReviewCallback(t.Context(), q)
			api.err = errors.New("response lost after possible commit")
			m.process(t.Context(), key)
			request := m.state.Jobs[key].Action.RequestID
			api.err = nil
			switch invalidation {
			case "revoke":
				api.connection.Enabled = false
				m.process(t.Context(), key)
			case "expiry":
				job := m.state.Jobs[key]
				job.Message.Date = time.Now().Add(-24 * time.Hour).Unix()
				m.state.Jobs[key] = job
				m.mu.Lock()
				m.pruneLocked()
				err := m.saveLocked()
				m.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
			case "edited":
				message := secretaryMessage()
				if err := m.Handle(t.Context(), lo.Update{ID: 13, EditedBusinessMessage: &message}); err != nil {
					t.Fatal(err)
				}
			case "mode":
				m.cfg.SecretaryMode = config.SecretaryModeAuto
			}
			reloaded, err := newSecretaryManager(m.cfg, api, m.botID, m.runtime)
			if err != nil {
				t.Fatal(err)
			}
			reloaded.process(t.Context(), key)
			job := reloaded.state.Jobs[key]
			if job.Status != secretaryUnknown || job.DeliveryRequestID != request || job.Action.Text != "" || api.sends != 1 {
				t.Fatal("uncertain delivery became cancelled or was replayed after invalidation")
			}
			if invalidation != "mode" && !strings.Contains(reloaded.ReviewCallback(t.Context(), q), "unknown") {
				t.Fatal("callback falsely reports failed delivery")
			}
		})
	}
}

func TestSecretaryAttemptIsDurableBeforeNetworkSend(t *testing.T) {
	t.Parallel()
	m, api, _ := secretaryFixture(t, config.SecretaryModeApproval)
	key := admitSecretary(t, m, secretaryMessage())
	m.process(t.Context(), key)
	m.ReviewCallback(t.Context(), secretaryReviewQuery(m, key, "send"))
	api.onSend = func() {
		data, err := os.ReadFile(m.path)
		if err != nil {
			t.Fatal(err)
		}
		var disk secretaryState
		if err := json.Unmarshal(data, &disk); err != nil {
			t.Fatal(err)
		}
		job := disk.Jobs[key]
		if job.Status != secretaryApproved || !job.DeliveryAttempted || job.DeliveryRequestID != job.Action.RequestID {
			t.Fatal("network preceded durable approval/attempt")
		}
	}
	m.process(t.Context(), key)
	if api.sends != 1 {
		t.Fatal("no delivery")
	}
}

func TestSecretaryAttemptPersistenceFailureNeverSends(t *testing.T) {
	t.Parallel()
	m, api, _ := secretaryFixture(t, config.SecretaryModeApproval)
	key := admitSecretary(t, m, secretaryMessage())
	m.process(t.Context(), key)
	m.ReviewCallback(t.Context(), secretaryReviewQuery(m, key, "send"))
	m.path = filepath.Join(t.TempDir(), "missing", "state.json")
	m.process(t.Context(), key)
	if api.sends != 0 || m.err() == nil {
		t.Fatal("attempt persistence failed open")
	}
}

func TestSecretaryInvalidationDuringNetworkWrite(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{reviewNoticeTest, "send-timeout", "send-success"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			m, api, _ := secretaryFixture(t, config.SecretaryModeApproval)
			key := admitSecretary(t, m, secretaryMessage())
			invalidate := func() {
				source := secretaryMessage()
				if err := m.Handle(t.Context(), lo.Update{ID: 13, EditedBusinessMessage: &source}); err != nil {
					t.Fatal(err)
				}
			}
			want := secretaryCancelled
			if kind == reviewNoticeTest {
				api.onNotice = invalidate
				m.process(t.Context(), key)
			} else {
				m.process(t.Context(), key)
				m.ReviewCallback(t.Context(), secretaryReviewQuery(m, key, "send"))
				api.onSend = invalidate
				if kind == "send-timeout" {
					api.err = errors.New("lost outcome")
					want = secretaryUnknown
				} else {
					want = secretaryDone
				}
				m.process(t.Context(), key)
			}
			if m.state.Jobs[key].Status != want {
				t.Fatalf("late network result resurrected decision: %s", m.state.Jobs[key].Status)
			}
			m.process(t.Context(), key)
			if kind == reviewNoticeTest && api.sends != 0 {
				t.Fatal("invalidated preview sent")
			}
			if kind != reviewNoticeTest && api.sends != 1 {
				t.Fatal("invalidated delivery retried")
			}
		})
	}
}

func TestSecretaryCompletedReplySurvivesModeChange(t *testing.T) {
	t.Parallel()
	m, api, _ := secretaryFixture(t, config.SecretaryModeApproval)
	key := admitSecretary(t, m, secretaryMessage())
	m.process(t.Context(), key)
	q := secretaryReviewQuery(m, key, "send")
	m.ReviewCallback(t.Context(), q)
	m.process(t.Context(), key)
	m.cfg.SecretaryMode = config.SecretaryModeAuto
	reloaded, err := newSecretaryManager(m.cfg, api, m.botID, m.runtime)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.state.Jobs[key].Status != secretaryDone || reloaded.ReviewCallback(t.Context(), q) != "Reply sent." {
		t.Fatal("completed receipt changed with mode")
	}
}
