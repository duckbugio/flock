//nolint:testpackage // Tests the optional progress path with the existing run-loop harness.
package chat

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/duckbugio/flock/core/agent"
)

type draftChat struct {
	*fakeChat
	drafts          []string
	draftErr        error
	maxRunes        int
	cleared         []string
	clearErr        error
	clearBeforeSend bool
	clearCanceled   bool
}

func (c *draftChat) Capabilities() Capabilities {
	limit := c.maxRunes
	if limit == 0 {
		limit = 2048
	}
	return Capabilities{CanSendDraft: true, MaxMessageRunes: limit}
}

func (c *draftChat) SendDraft(_ context.Context, _ ChatID, runID, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drafts = append(c.drafts, runID+":"+text)
	return c.draftErr
}

func (c *draftChat) ClearDraft(ctx context.Context, _ ChatID, runID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cleared = append(c.cleared, runID)
	c.clearBeforeSend = len(c.sent) == 0
	c.clearCanceled = ctx.Err() != nil
	return c.clearErr
}

func TestDraftProgressPersistsOnlyTheTerminalAnswer(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "draft", true: "fallback"}[fail], func(t *testing.T) {
			fc := &draftChat{fakeChat: newFakeChat()}
			if fail {
				fc.draftErr = errors.New("not implemented")
			}
			runner := &fakeRunner{events: []agent.Event{{Type: agent.Result, Result: &agent.RunResult{Text: finalAnswer}}}}
			svc, dispatcher := newTestService(t, runner, fc)
			defer dispatcher.Close()
			svc.Handle(t.Context(), "100", 100, "1", "hello")
			waitUntil(t, func() bool { fc.mu.Lock(); defer fc.mu.Unlock(); return fc.texts["1"] == finalAnswer })
			fc.mu.Lock()
			defer fc.mu.Unlock()
			if len(fc.drafts) == 0 {
				t.Fatal("draft was not attempted")
			}
			if !fail && (fc.edits != 0 || fc.sent[0] != finalAnswer) {
				t.Fatalf("draft wrote a persistent anchor: sent=%v edits=%d", fc.sent, fc.edits)
			}
			if !fail && (len(fc.cleared) != 1 || !fc.clearBeforeSend || fc.clearCanceled) {
				t.Fatal("draft was not cleared before final delivery")
			}
			if fail && len(fc.cleared) != 0 {
				t.Fatal("cleared a draft that never started")
			}
			if fail && fc.sent[0] != anchorText {
				t.Fatal("missing persistent fallback")
			}
		})
	}
}

func TestDraftProgressUsesTransportBudgetAndStableRun(t *testing.T) {
	const budget = 32
	fc := &draftChat{fakeChat: newFakeChat(), maxRunes: budget}
	gate := make(chan struct{})
	runner := &fakeRunner{gate: gate, events: []agent.Event{
		{Type: agent.Text, Text: strings.Repeat("😀", 200)},
		{Type: agent.Result, Result: &agent.RunResult{Text: finalAnswer}},
	}}
	svc, dispatcher := newTestService(t, runner, fc)
	defer dispatcher.Close()
	defer close(gate)
	svc.Handle(t.Context(), "100", 100, "1", "hello")
	waitUntil(t, func() bool { fc.mu.Lock(); defer fc.mu.Unlock(); return len(fc.drafts) >= 2 })
	fc.mu.Lock()
	defer fc.mu.Unlock()
	firstID, _, _ := strings.Cut(fc.drafts[0], ":")
	for _, draft := range fc.drafts {
		id, text, _ := strings.Cut(draft, ":")
		if id != firstID || utf8.RuneCountInString(text) > budget {
			t.Fatalf("invalid draft: %q", draft)
		}
	}
	if len(fc.sent) != 0 || fc.edits != 0 {
		t.Fatal("progress created a persistent message")
	}
}

func TestDraftClearFailureDoesNotBlockFinalAnswer(t *testing.T) {
	fc := &draftChat{fakeChat: newFakeChat(), clearErr: errors.New("cleanup unavailable")}
	runner := &fakeRunner{events: []agent.Event{{Type: agent.Result, Result: &agent.RunResult{Text: finalAnswer}}}}
	svc, dispatcher := newTestService(t, runner, fc)
	defer dispatcher.Close()
	svc.Handle(t.Context(), "100", 100, "1", "hello")
	waitUntil(t, func() bool { fc.mu.Lock(); defer fc.mu.Unlock(); return fc.texts["1"] == finalAnswer })
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.cleared) != 1 {
		t.Fatal("cleanup was not attempted")
	}
}

func TestDraftClearRunsAfterCancellation(t *testing.T) {
	fc := &draftChat{fakeChat: newFakeChat()}
	runner := &fakeRunner{gate: make(chan struct{})}
	svc, dispatcher := newTestService(t, runner, fc)
	defer dispatcher.Close()
	svc.Handle(t.Context(), "100", 100, "1", "hello")
	waitUntil(t, func() bool { fc.mu.Lock(); defer fc.mu.Unlock(); return len(fc.drafts) > 0 })
	svc.StopChat("100")
	waitUntil(t, func() bool { fc.mu.Lock(); defer fc.mu.Unlock(); return len(fc.sent) > 0 })
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.cleared) != 1 || fc.clearCanceled || !fc.clearBeforeSend {
		t.Fatal("cancelled run leaked its draft")
	}
}
