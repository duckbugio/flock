// Package poller inverts the Gitea webhook dependency: inbound webhooks can't
// always reach the bot, but the bot can reach Gitea. A background loop polls the
// bot user's unread Pull notifications and, for each new non-self comment on an
// open duck/<chatid>/... PR, emits a PRComment the adapter routes back into the
// owning chat. It ports adapters/telegram/patches/gitea_poller.py to Go.
package poller

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// PRComment is a new, non-self comment observed on an OPEN team PR whose head
// branch is duck/<chatid>/<slug>. ChatID is the raw branch segment (string,
// may be negative like "-5164159101"); the adapter parses it to route the event.
type PRComment struct {
	ChatID  string
	Repo    string // full name owner/repo
	PRIndex int    // PR number
	Body    string
	Author  string
}

// Config configures the poller.
type Config struct {
	BaseURL   string        // GITEA_API_URL (trailing slash trimmed)
	Token     string        // GIT_TOKEN — sent as "Authorization: token <token>"
	SelfLogin string        // GIT_USER, lowercased — comments by this login are ignored (no self-trigger)
	Interval  time.Duration // poll period; floored to 30s like the Python min(30, ...)
	Client    *http.Client  // injectable for tests; defaults to a 20s-timeout client when nil
	Logger    *slog.Logger  // defaults to slog.Default()
}

// minInterval floors the poll period, mirroring the Python max(30, ...).
const minInterval = 30 * time.Second

// defaultClientTimeout is the request timeout for the default HTTP client used
// when Config.Client is nil.
const defaultClientTimeout = 20 * time.Second

// minRefParts is the minimum number of "/"-separated segments a team branch ref
// must have to carry a chatID: duck/<chatid>[/<slug>].
const minRefParts = 2

// poller holds the resolved runtime state for one Run.
type poller struct {
	base      string
	token     string
	selfLogin string
	client    *http.Client
	log       *slog.Logger
}

// Run polls until ctx is cancelled, emitting a PRComment on out for each new
// non-self comment on an open duck/<chatid>/... PR. It marks handled threads
// read. Never closes out. Logs and continues on transient errors.
func Run(ctx context.Context, cfg Config, out chan<- PRComment) error {
	interval := cfg.Interval
	if interval < minInterval {
		interval = minInterval
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: defaultClientTimeout}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	p := &poller{
		base:      strings.TrimRight(cfg.BaseURL, "/"),
		token:     cfg.Token,
		selfLogin: cfg.SelfLogin,
		client:    client,
		log:       log,
	}
	log.Info("gitea poller started", "base", p.base, "interval", interval)

	for {
		p.pollOnce(ctx, out)
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// notification is one unread Pull notification thread.
type notification struct {
	ID      json.Number `json:"id"`
	Subject struct {
		URL              string `json:"url"`
		LatestCommentURL string `json:"latest_comment_url"` //nolint:tagliatelle // Gitea API uses snake_case.
	} `json:"subject"`
	Repository struct {
		FullName string `json:"full_name"` //nolint:tagliatelle // Gitea API uses snake_case.
	} `json:"repository"`
}

// pull is the relevant subset of a Gitea pull request.
type pull struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Merged bool   `json:"merged"`
	Head   struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Repo struct {
			FullName string `json:"full_name"` //nolint:tagliatelle // Gitea API uses snake_case.
		} `json:"repo"`
	} `json:"base"`
}

// comment is the relevant subset of a Gitea issue/PR comment.
type comment struct {
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

// pollOnce fetches unread Pull notifications and handles each thread. Transport
// or non-2xx errors are logged and swallowed so the loop keeps running.
func (p *poller) pollOnce(ctx context.Context, out chan<- PRComment) {
	url := p.base + "/notifications?status-types=unread&subject-type=Pull&limit=50"
	var threads []notification
	status, err := p.getJSON(ctx, url, &threads)
	if err != nil {
		p.log.Warn("gitea notifications poll failed", "error", err)
		return
	}
	if status >= http.StatusBadRequest {
		p.log.Warn("gitea notifications poll failed", "status", status)
		return
	}
	for i := range threads {
		if err := p.handleThread(ctx, &threads[i], out); err != nil {
			p.log.Warn("failed handling notification thread", "thread_id", threads[i].ID.String(), "error", err)
		}
	}
}

// handleThread resolves one notification to a PR, decides whether it is an
// actionable comment on an open team branch, emits it, and marks the thread
// read. A transient PR fetch error leaves the thread unread for the next cycle.
func (p *poller) handleThread(ctx context.Context, t *notification, out chan<- PRComment) error {
	threadID := t.ID.String()
	if t.Subject.URL == "" {
		p.markRead(ctx, threadID)
		return nil
	}

	// Gitea points the subject at the issue URL; the PR (with head.ref) is /pulls/N.
	prURL := strings.Replace(t.Subject.URL, "/issues/", "/pulls/", 1)
	var pr pull
	status, err := p.getJSON(ctx, prURL, &pr)
	if err != nil {
		return err
	}
	if status >= http.StatusBadRequest {
		// Transient — leave unread, retry next cycle.
		return nil
	}
	// Only act on OPEN PRs — the human merges; nothing to fix on a merged/closed PR.
	if pr.State != "open" || pr.Merged {
		p.markRead(ctx, threadID)
		return nil
	}
	ref := pr.Head.Ref
	if !strings.HasPrefix(ref, "duck/") {
		p.markRead(ctx, threadID) // not a team branch — clear it
		return nil
	}

	var c comment
	if t.Subject.LatestCommentURL != "" {
		cStatus, cErr := p.getJSON(ctx, t.Subject.LatestCommentURL, &c)
		if cErr != nil {
			return cErr // transient — leave unread, retry next cycle (matches the Python spec)
		}
		if cStatus >= http.StatusBadRequest {
			c = comment{} // server-side error on the comment endpoint: proceed with empty comment, like Python
		}
	}

	// Never react to our own comments (avoid self-trigger loops).
	author := strings.ToLower(c.User.Login)
	if author != "" && p.selfLogin != "" && author == p.selfLogin {
		p.markRead(ctx, threadID)
		return nil
	}

	repo := pr.Base.Repo.FullName
	if repo == "" {
		repo = t.Repository.FullName
	}

	// Parse chatID from duck/<chatid>/<slug>.
	parts := strings.Split(ref, "/")
	if len(parts) < minRefParts {
		p.markRead(ctx, threadID)
		return nil
	}
	chatID := parts[1]

	select {
	case out <- PRComment{
		ChatID:  chatID,
		Repo:    repo,
		PRIndex: pr.Number,
		Body:    c.Body,
		Author:  author,
	}:
	case <-ctx.Done():
		return ctx.Err()
	}

	p.log.Info("gitea poller emitted PR comment", "pr", pr.Number, "branch", ref)
	p.markRead(ctx, threadID)
	return nil
}

// markRead marks a notification thread read; failures are logged, never fatal.
func (p *poller) markRead(ctx context.Context, threadID string) {
	if threadID == "" || threadID == "0" {
		return
	}
	url := p.base + "/notifications/threads/" + threadID + "?to-status=read"
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, nil)
	if err != nil {
		p.log.Warn("failed to build mark-read request", "thread_id", threadID, "error", err)
		return
	}
	p.setHeaders(req)
	resp, err := p.client.Do(req)
	if err != nil {
		p.log.Warn("failed to mark notification read", "thread_id", threadID, "error", err)
		return
	}
	_ = resp.Body.Close()
}

// getJSON performs a GET and decodes a <400 response into v. It returns the HTTP
// status code so the caller can apply the Python loop's status branching. A
// non-nil error means the request itself failed (transport/decoding).
func (p *poller) getJSON(ctx context.Context, url string, v any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	p.setHeaders(req)
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusBadRequest {
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return resp.StatusCode, err
	}
	return resp.StatusCode, nil
}

// setHeaders applies the Gitea auth + accept headers to a request.
func (p *poller) setHeaders(req *http.Request) {
	if p.token != "" {
		req.Header.Set("Authorization", "token "+p.token)
	}
	req.Header.Set("Accept", "application/json")
}
