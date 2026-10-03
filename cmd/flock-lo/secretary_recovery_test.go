package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/duckbugio/flock/adapters/lo"
	"github.com/duckbugio/flock/core/cost"
	"github.com/duckbugio/flock/core/ratelimit"
	"github.com/duckbugio/flock/internal/config"
)

func secretaryJobFixture(id int, status string) (string, secretaryJob) {
	msg := secretaryMessage()
	msg.ID = lo.SecretaryNumber(id)
	msg.Context.SourceMessageID = msg.ID
	job := secretaryJob{
		UpdateID: int64(id), Message: msg, Mode: config.SecretaryModeAuto,
		Status: status, CreatedAt: time.Now().Unix(),
	}
	return secretaryJobKey(secretaryTestBot, msg), job
}

func TestSecretaryCapacityAcknowledgesWithoutLosingLiveWork(t *testing.T) {
	t.Parallel()
	for _, status := range []string{secretaryDone, secretaryQueued} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			manager, api, _ := secretaryFixture(t, config.SecretaryModeAuto)
			for id := 1; id <= secretaryMaxJobs; id++ {
				key, job := secretaryJobFixture(id, status)
				if id == 1 {
					job.CreatedAt--
				}
				manager.state.Jobs[key] = job
			}
			manager.state.UpdateFloor = secretaryMaxJobs
			key, next := secretaryJobFixture(secretaryMaxJobs+1, secretaryQueued)
			if err := manager.Handle(t.Context(), lo.Update{ID: next.UpdateID, BusinessMessage: &next.Message}); err != nil {
				t.Fatal("capacity poisoned poll acknowledgement", err)
			}
			_, admitted := manager.state.Jobs[key]
			if admitted != (status == secretaryDone) || len(manager.state.Jobs) != secretaryMaxJobs || manager.err() != nil {
				t.Fatal("capacity lost live work or stopped the bot")
			}
			reload, err := newSecretaryManager(manager.cfg, api, manager.botID, manager.runtime)
			if err != nil {
				t.Fatal(err)
			}
			old := secretaryMessage()
			old.ID, old.Context.SourceMessageID = 1, 1
			if err := reload.Handle(t.Context(), lo.Update{ID: 1, BusinessMessage: &old}); err != nil {
				t.Fatal(err)
			}
			oldKey := secretaryJobKey(manager.botID, old)
			if status == secretaryDone {
				if _, exists := reload.state.Jobs[oldKey]; exists {
					t.Fatal("evicted terminal run was replayed after restart")
				}
			}
			if reload.state.UpdateFloor != next.UpdateID {
				t.Fatal("admission watermark was not durable")
			}
		})
	}
}

func TestSecretaryByteCapacityIsNonfatal(t *testing.T) {
	t.Parallel()
	manager, _, _ := secretaryFixture(t, config.SecretaryModeAuto)
	for id := 1; id <= secretaryMaxConnections; id++ {
		key, job := secretaryJobFixture(id, secretaryQueued)
		manager.state.Jobs[key] = job
	}
	_, next := secretaryJobFixture(secretaryMaxConnections+1, secretaryQueued)
	if err := manager.Handle(t.Context(), lo.Update{ID: next.UpdateID, BusinessMessage: &next.Message}); err != nil {
		t.Fatal(err)
	}
	if len(manager.state.Jobs) != secretaryMaxConnections || manager.err() != nil || manager.state.UpdateFloor != next.UpdateID {
		t.Fatal("byte pressure became a fatal state write or admitted unreserved reply")
	}
}

func TestSecretaryConnectionCachePreservesActiveOwners(t *testing.T) {
	t.Parallel()
	manager, api, _ := secretaryFixture(t, config.SecretaryModeAuto)
	key := admitSecretary(t, manager, secretaryMessage())
	for id := 0; id < secretaryMaxConnections; id++ {
		conn := api.connection
		conn.ID = fmt.Sprintf("%08x-9383-4da1-9644-826693923e44", id+1)
		conn.Date = int64(id + 2)
		manager.state.Connections[conn.ID] = conn
	}
	delete(manager.state.Connections, "00000001-9383-4da1-9644-826693923e44")
	manager.state.Connections[api.connection.ID] = api.connection
	conn := api.connection
	conn.ID = "ffffffff-9383-4da1-9644-826693923e44"
	if err := manager.Handle(t.Context(), lo.Update{BusinessConnection: &conn}); err != nil {
		t.Fatal(err)
	}
	if _, exists := manager.state.Connections[api.connection.ID]; !exists {
		t.Fatal("active owner consent evicted")
	}
	if _, exists := manager.state.Connections[conn.ID]; !exists {
		t.Fatal("inactive cache entry was not reclaimed")
	}
	conn.User.ID = 2
	if err := manager.Handle(t.Context(), lo.Update{BusinessConnection: &conn}); err != nil {
		t.Fatal(err)
	}
	if _, exists := manager.state.Connections[conn.ID]; exists {
		t.Fatal("disallowed owner consumes cache capacity")
	}
	if manager.state.Jobs[key].Status != secretaryQueued || manager.err() != nil {
		t.Fatal("unrelated live job changed")
	}
}

func TestSecretaryInvalidationCapacityStillCancelsSource(t *testing.T) {
	t.Parallel()
	manager, _, _ := secretaryFixture(t, config.SecretaryModeAuto)
	msg := secretaryMessage()
	key := admitSecretary(t, manager, msg)
	for id := 100; id < secretaryMaxJobs+100; id++ {
		manager.state.Invalid[secretarySourceKey(msg.ConnectionID, msg.Context, lo.SecretaryNumber(id))] = time.Now().Unix()
	}
	if err := manager.Handle(t.Context(), lo.Update{ID: 13, EditedBusinessMessage: &msg}); err != nil {
		t.Fatal(err)
	}
	if manager.state.Jobs[key].Status != secretaryCancelled || len(manager.state.Invalid) != secretaryMaxJobs ||
		manager.err() != nil {
		t.Fatal("full invalidation cache lost cancellation or stopped polling")
	}
	if _, exists := manager.state.Invalid[secretarySourceKey(msg.ConnectionID, msg.Context, msg.ID)]; !exists {
		t.Fatal("new invalidation was not retained")
	}
}

func TestSecretarySubmitPrunesExpiredStateWithoutNewUpdates(t *testing.T) {
	t.Parallel()
	manager, _, runner := secretaryFixture(t, config.SecretaryModeAuto)
	key := admitSecretary(t, manager, secretaryMessage())
	job := manager.state.Jobs[key]
	job.CreatedAt = time.Now().Add(-secretaryLifetime - time.Second).Unix()
	manager.state.Jobs[key] = job
	manager.state.Invalid["expired"] = job.CreatedAt
	manager.submit()
	data, err := os.ReadFile(manager.path)
	if err != nil {
		t.Fatal(err)
	}
	var saved secretaryState
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Jobs) != 0 || len(saved.Invalid) != 0 || len(manager.scheduled) != 0 || runner.calls != 0 {
		t.Fatal("expired work was resubmitted or not durably pruned")
	}
}

func TestSecretaryRetryBackoffIsDurableAndPreservesAction(t *testing.T) {
	t.Parallel()
	manager, api, runner := secretaryFixture(t, config.SecretaryModeAuto)
	api.err = errors.New("uncertain write")
	key := admitSecretary(t, manager, secretaryMessage())
	manager.process(t.Context(), key)
	action := api.actions[0]
	first := manager.state.Jobs[key]
	if first.Attempts != 1 || first.NextAttemptAt < time.Now().Add(14*time.Second).Unix() {
		t.Fatal("initial retry was not delayed")
	}
	reload, err := newSecretaryManager(manager.cfg, api, manager.botID, manager.runtime)
	if err != nil {
		t.Fatal(err)
	}
	if reload.state.Jobs[key].NextAttemptAt != first.NextAttemptAt {
		t.Fatal("retry delay lost after restart")
	}
	reload.submit()
	if len(reload.scheduled) != 0 {
		t.Fatal("future retry submitted prematurely")
	}
	for attempt := 2; attempt <= 8; attempt++ {
		reload.retry(key, api.err)
		job := reload.state.Jobs[key]
		delay := min(secretaryRetryInterval*time.Duration(1<<uint(attempt-1)), secretaryMaxRetryInterval)
		if job.Attempts != attempt || job.NextAttemptAt < time.Now().Add(delay-time.Second).Unix() || job.Action != action {
			t.Fatal("retry backoff or persisted body changed")
		}
	}
	reload.retry(key, &lo.APIError{Code: 429, Delay: 10 * time.Minute})
	if reload.state.Jobs[key].NextAttemptAt < time.Now().Add(10*time.Minute-time.Second).Unix() {
		t.Fatal("server retry_after ignored")
	}
	api.err = nil
	reload.process(t.Context(), key)
	if runner.calls != 1 || api.actions[1] != action || reload.state.Jobs[key].Status != secretaryDone {
		t.Fatal("delayed retry reran tools or changed idempotent write")
	}
}

// This sequential test restores the process logger before parallel tests resume.
func TestSecretaryGuardDenialsAreObservableAndRateWorkCanResume(t *testing.T) {
	for _, kind := range []string{"rate", "cost"} {
		t.Run(kind, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			manager, api, runner := secretaryFixture(t, config.SecretaryModeAuto)
			if kind == "rate" {
				manager.limit = ratelimit.New(1, time.Minute)
				manager.limit.Allow(1, time.Now())
			} else {
				costs, err := cost.Open(t.TempDir() + "/costs.json")
				if err != nil {
					t.Fatal(err)
				}
				if err := costs.Add(1, 2); err != nil {
					t.Fatal(err)
				}
				manager.runtime.costs, manager.cfg.ClaudeMaxCostPerUser = costs, 1
			}
			key := admitSecretary(t, manager, secretaryMessage())
			manager.process(t.Context(), key)
			for _, field := range []string{`"update_id":12`, `"owner_id":1`, `"reason":`} {
				if !strings.Contains(logs.String(), field) {
					t.Fatal("guard denial lacks structured identity/reason", logs.String())
				}
			}
			if runner.calls != 0 || len(api.actions) != 0 {
				t.Fatal("denied job reached paid generation/delivery")
			}
			if kind == "rate" {
				if manager.state.Jobs[key].Status != secretaryQueued {
					t.Fatal("rate-limited message permanently lost")
				}
				manager.limit = ratelimit.New(1, time.Minute)
				manager.process(t.Context(), key)
				if runner.calls != 1 || api.sends != 1 {
					t.Fatal("rate-limited queued message could not resume")
				}
			} else if manager.state.Jobs[key].Status != secretaryCancelled {
				t.Fatal("cost-capped job was not cancelled")
			}
		})
	}
}

func TestSecretaryNewPolicyDoesNotTrustStaleConsentCache(t *testing.T) {
	t.Parallel()
	manager, api, runner := secretaryFixture(t, config.SecretaryModeAuto)
	stale := api.connection
	stale.Enabled = false
	manager.state.Connections[stale.ID] = stale
	api.connection.PolicyVersion++
	msg := secretaryMessage()
	msg.Context.PolicyVersion = api.connection.PolicyVersion
	key := admitSecretary(t, manager, msg)
	// An older connection update must not invalidate a newer source generation.
	if err := manager.Handle(t.Context(), lo.Update{ID: 13, BusinessConnection: &stale}); err != nil {
		t.Fatal(err)
	}
	manager.process(t.Context(), key)
	if runner.calls != 1 || api.sends != 1 || manager.state.Jobs[key].Status != secretaryDone {
		t.Fatal("older cached consent blocked a server-authorized newer source")
	}
}

func TestSecretaryNewerRevocationStillWinsAgainstFreshLookup(t *testing.T) {
	t.Parallel()
	manager, api, runner := secretaryFixture(t, config.SecretaryModeAuto)
	key := admitSecretary(t, manager, secretaryMessage())
	api.onLookup = func() {
		revoked := api.connection
		revoked.PolicyVersion++
		revoked.Enabled = false
		if err := manager.Handle(t.Context(), lo.Update{ID: 13, BusinessConnection: &revoked}); err != nil {
			t.Error(err)
		}
	}
	manager.process(t.Context(), key)
	if runner.calls != 0 || api.sends != 0 || manager.state.Jobs[key].Status != secretaryCancelled {
		t.Fatal("stale in-flight lookup overrode a newer revocation")
	}
}
