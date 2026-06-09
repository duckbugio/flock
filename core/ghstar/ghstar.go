// Package ghstar is a minimal GitHub "stars" client used by the post-task star
// nudge: it can check whether the deployment's own account (the GIT_TOKEN owner)
// has starred a repository and, on an explicit user action, star it. It speaks
// only the two endpoints the nudge needs and is styled like core/poller: a small
// Config with an injectable BaseURL + HTTPClient for tests, context-bound
// requests, and explicit per-status handling so callers can log distinctly.
package ghstar

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// DefaultBaseURL is the GitHub REST API root used when Config.BaseURL is empty.
const DefaultBaseURL = "https://api.github.com"

// apiVersion pins the GitHub REST API version, sent on every request per
// GitHub's recommendation.
const apiVersion = "2022-11-28"

// defaultClientTimeout is the request timeout for the default HTTP client used
// when Config.HTTPClient is nil.
const defaultClientTimeout = 15 * time.Second

// ErrUnauthorized is returned by Star when GitHub rejects the token with 401 or
// 403, which almost always means the token lacks the star-write scope (a classic
// token needs public_repo; a fine-grained token needs the "Starring" account
// permission). Callers can errors.Is against it to log that distinctly without
// crashing the run.
var ErrUnauthorized = errors.New("ghstar: token unauthorized (missing star-write scope?)")

// Config configures a Client.
type Config struct {
	// BaseURL is the GitHub REST API root (trailing slash trimmed). Empty uses
	// DefaultBaseURL; tests inject an httptest.Server URL.
	BaseURL string
	// Token is the GitHub PAT sent as "Authorization: Bearer <token>".
	Token string
	// HTTPClient is injectable for tests; nil yields a client with
	// defaultClientTimeout.
	HTTPClient *http.Client
	// Logger defaults to slog.Default() when nil.
	Logger *slog.Logger
}

// Client talks to the GitHub stars endpoints. It is safe for concurrent use (it
// holds only immutable config and an *http.Client).
type Client struct {
	base   string
	token  string
	client *http.Client
	log    *slog.Logger
}

// New builds a Client from cfg, applying the BaseURL/HTTPClient/Logger defaults.
func New(cfg Config) *Client {
	base := cfg.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	base = trimTrailingSlash(base)
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultClientTimeout}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Client{base: base, token: cfg.Token, client: client, log: log}
}

// trimTrailingSlash drops a single trailing slash so the joined path is clean
// regardless of how BaseURL was supplied.
func trimTrailingSlash(s string) string {
	if s != "" && s[len(s)-1] == '/' {
		return s[:len(s)-1]
	}
	return s
}

// IsStarred reports whether the authenticated account has starred owner/repo.
// GitHub answers GET /user/starred/{owner}/{repo} with 204 (starred) or 404 (not
// starred); any other status is surfaced as an error so the caller can log it and
// skip the nudge rather than guess.
func (c *Client) IsStarred(ctx context.Context, owner, repo string) (bool, error) {
	status, err := c.do(ctx, http.MethodGet, owner, repo)
	if err != nil {
		return false, err
	}

	switch status {
	case http.StatusNoContent:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("ghstar: check star: unexpected status %d", status)
	}
}

// Star stars owner/repo for the authenticated account via
// PUT /user/starred/{owner}/{repo}. GitHub returns 204 on success. A 401/403 is
// mapped to ErrUnauthorized (missing scope) so the caller can log it distinctly;
// any other status is a generic error. Star never panics on a nil body.
func (c *Client) Star(ctx context.Context, owner, repo string) error {
	status, err := c.do(ctx, http.MethodPut, owner, repo)
	if err != nil {
		return err
	}

	switch status {
	case http.StatusNoContent:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: status %d", ErrUnauthorized, status)
	default:
		return fmt.Errorf("ghstar: star repo: unexpected status %d", status)
	}
}

// do builds and sends a request to /user/starred/{owner}/{repo} with the shared
// GitHub headers, returning the response status code and draining+closing the
// body (the endpoints return no body the caller needs). The PUT carries an
// explicit zero Content-Length, as the stars endpoint expects an empty body.
func (c *Client) do(ctx context.Context, method, owner, repo string) (int, error) {
	url := c.base + "/user/starred/" + owner + "/" + repo
	req, err := http.NewRequestWithContext(ctx, method, url, http.NoBody)
	if err != nil {
		return 0, fmt.Errorf("ghstar: build request: %w", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	if method == http.MethodPut {
		req.Header.Set("Content-Length", "0")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("ghstar: %s request: %w", method, err)
	}
	// Drain + close so the connection can be reused; the body is unused.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}
