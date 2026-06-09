//nolint:testpackage // intentionally whitebox to test unexported poller internals
package poller

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// slogDiscard returns a logger that drops all output, keeping test output clean.
func slogDiscard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// fakeGitea serves canned notifications, a pull, a comment, and records PATCH
// (mark-read) calls. The notification it serves is configurable per test.
type fakeGitea struct {
	mu            sync.Mutex
	server        *httptest.Server
	pull          pull
	comment       comment
	patched       []string // thread ids marked read
	commentOK     bool     // whether latest_comment_url is served
	commentHijack bool     // when true, hijack and close the comment connection (forces a transport error)
}

func newFakeGitea(t *testing.T, pr pull, c comment) *fakeGitea {
	t.Helper()
	f := &fakeGitea{pull: pr, comment: c, commentOK: true}
	mux := http.NewServeMux()

	mux.HandleFunc("/notifications", func(w http.ResponseWriter, _ *http.Request) {
		base := strings.TrimRight(f.server.URL, "/")
		threads := []map[string]any{{
			"id": "42",
			"subject": map[string]any{
				"url":                base + "/repos/owner/repo/issues/7",
				"latest_comment_url": base + "/repos/owner/repo/issues/comments/100",
			},
			"repository": map[string]any{"full_name": "owner/repo"},
		}}
		writeJSON(w, threads)
	})

	mux.HandleFunc("/repos/owner/repo/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, f.pull)
	})

	mux.HandleFunc("/repos/owner/repo/issues/comments/100", func(w http.ResponseWriter, _ *http.Request) {
		if f.commentHijack {
			// Hijack and immediately close the connection so the client sees a
			// transport error (EOF) rather than an HTTP status — simulating the
			// flaky network this poller exists to tolerate.
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		}
		if !f.commentOK {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeJSON(w, f.comment)
	})

	mux.HandleFunc("/notifications/threads/42", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		f.mu.Lock()
		f.patched = append(f.patched, "42")
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeGitea) markedRead() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.patched...)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		panic(err)
	}
}

// selfLogin is the bot's own Gitea login used across the poller tests.
const selfLogin = "duckbot"

// newPoller builds a poller pointed at the fake server.
func newPoller(f *fakeGitea) *poller {
	return &poller{
		base:      strings.TrimRight(f.server.URL, "/"),
		token:     "tok",
		selfLogin: selfLogin,
		client:    f.server.Client(),
		log:       slogDiscard(),
	}
}

func openPull(ref string) pull {
	var p pull
	p.Number = 7
	p.State = "open"
	p.Head.Ref = ref
	p.Base.Repo.FullName = "owner/repo"
	return p
}

func TestPollOnceEmitsComment(t *testing.T) {
	f := newFakeGitea(t,
		openPull("duck/-5164159101/go-stage-6"),
		comment{Body: "please fix", User: struct {
			Login string `json:"login"`
		}{Login: "Reviewer"}},
	)
	p := newPoller(f)

	out := make(chan PRComment, 1)
	ctx := context.Background()
	p.pollOnce(ctx, out)

	select {
	case c := <-out:
		if c.ChatID != "-5164159101" {
			t.Errorf("ChatID = %q, want -5164159101", c.ChatID)
		}
		if c.Repo != "owner/repo" {
			t.Errorf("Repo = %q, want owner/repo", c.Repo)
		}
		if c.PRIndex != 7 {
			t.Errorf("PRIndex = %d, want 7", c.PRIndex)
		}
		if c.Body != "please fix" {
			t.Errorf("Body = %q, want %q", c.Body, "please fix")
		}
		if c.Author != "reviewer" {
			t.Errorf("Author = %q, want reviewer (lowercased)", c.Author)
		}
	default:
		t.Fatal("expected a PRComment, got none")
	}

	if got := f.markedRead(); len(got) != 1 || got[0] != "42" {
		t.Errorf("markedRead = %v, want [42]", got)
	}
}

func TestPollOnceSkipsSelfComment(t *testing.T) {
	f := newFakeGitea(t,
		openPull("duck/-5164159101/go-stage-6"),
		comment{Body: "my own note", User: struct {
			Login string `json:"login"`
		}{Login: "DuckBot"}},
	)
	p := newPoller(f)

	out := make(chan PRComment, 1)
	p.pollOnce(context.Background(), out)

	select {
	case c := <-out:
		t.Fatalf("expected no emit for self comment, got %+v", c)
	default:
	}
	if got := f.markedRead(); len(got) != 1 {
		t.Errorf("self comment should still be marked read, markedRead = %v", got)
	}
}

func TestPollOnceSkipsNonDuckBranch(t *testing.T) {
	f := newFakeGitea(t,
		openPull("feature/other"),
		comment{Body: "hi", User: struct {
			Login string `json:"login"`
		}{Login: "Reviewer"}},
	)
	p := newPoller(f)

	out := make(chan PRComment, 1)
	p.pollOnce(context.Background(), out)

	select {
	case c := <-out:
		t.Fatalf("expected no emit for non-duck branch, got %+v", c)
	default:
	}
	if got := f.markedRead(); len(got) != 1 {
		t.Errorf("non-duck branch should be marked read, markedRead = %v", got)
	}
}

func TestPollOnceSkipsClosedPR(t *testing.T) {
	pr := openPull("duck/-5164159101/go-stage-6")
	pr.State = "closed"
	f := newFakeGitea(t, pr,
		comment{Body: "hi", User: struct {
			Login string `json:"login"`
		}{Login: "Reviewer"}},
	)
	p := newPoller(f)

	out := make(chan PRComment, 1)
	p.pollOnce(context.Background(), out)

	select {
	case c := <-out:
		t.Fatalf("expected no emit for closed PR, got %+v", c)
	default:
	}
	if got := f.markedRead(); len(got) != 1 {
		t.Errorf("closed PR should be marked read, markedRead = %v", got)
	}
}

func TestPollOnceSkipsMergedPR(t *testing.T) {
	pr := openPull("duck/-5164159101/go-stage-6")
	pr.Merged = true
	f := newFakeGitea(t, pr,
		comment{Body: "hi", User: struct {
			Login string `json:"login"`
		}{Login: "Reviewer"}},
	)
	p := newPoller(f)

	out := make(chan PRComment, 1)
	p.pollOnce(context.Background(), out)

	select {
	case c := <-out:
		t.Fatalf("expected no emit for merged PR, got %+v", c)
	default:
	}
	if got := f.markedRead(); len(got) != 1 {
		t.Errorf("merged PR should be marked read, markedRead = %v", got)
	}
}

// TestPollOnceCommentTransportErrorLeavesUnread proves that a transport error on
// the latest-comment fetch (the flaky-network case this poller exists to
// tolerate) does NOT emit a PRComment and does NOT mark the thread read, so the
// reviewer's comment is retried next cycle rather than lost.
func TestPollOnceCommentTransportErrorLeavesUnread(t *testing.T) {
	f := newFakeGitea(t,
		openPull("duck/-5164159101/go-stage-6"),
		comment{Body: "please fix", User: struct {
			Login string `json:"login"`
		}{Login: "Reviewer"}},
	)
	f.commentHijack = true
	p := newPoller(f)

	out := make(chan PRComment, 1)
	p.pollOnce(context.Background(), out)

	select {
	case c := <-out:
		t.Fatalf("expected no emit on comment transport error, got %+v", c)
	default:
	}
	if got := f.markedRead(); len(got) != 0 {
		t.Errorf("thread must stay unread on transport error, markedRead = %v", got)
	}
}

func TestRunCancels(t *testing.T) {
	f := newFakeGitea(t,
		openPull("duck/-5164159101/go-stage-6"),
		comment{Body: "fix", User: struct {
			Login string `json:"login"`
		}{Login: "Reviewer"}},
	)
	out := make(chan PRComment, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			BaseURL:   f.server.URL,
			Token:     "tok",
			SelfLogin: "duckbot",
			Interval:  time.Hour, // first cycle runs immediately, then blocks
			Client:    f.server.Client(),
			Logger:    slogDiscard(),
		}, out)
	}()

	select {
	case c := <-out:
		if c.ChatID != "-5164159101" {
			t.Errorf("ChatID = %q, want -5164159101", c.ChatID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for emit")
	}

	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Errorf("Run() returned nil, want context error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
