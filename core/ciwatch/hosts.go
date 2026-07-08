package ciwatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// defaultClientTimeout is the per-request budget of the default HTTP client.
const defaultClientTimeout = 20 * time.Second

// maxDetailChecks caps how many failing check names are listed in an event's
// Detail (the fix-up prompt needs the gist, not the full matrix).
const maxDetailChecks = 5

// errBodySnippet is how much of a non-2xx response body is folded into the error.
const errBodySnippet = 256

// Host-neutral Status.State values (Gitea/GitHub raw states map onto these).
const (
	stateSuccess = "success"
	stateFailure = "failure"
	statePending = "pending"
	stateNone    = "none"
	// stateError is Gitea's/legacy-GitHub's "error" raw state, folded into failure.
	stateError = "error"
)

// NewGitea returns a Host backed by a Gitea API base URL (…/api/v1), using the
// same token header as core/poller. client may be nil.
func NewGitea(baseURL, token string, client *http.Client) Host {
	return &giteaHost{api: api{base: strings.TrimRight(baseURL, "/"), token: token, scheme: "token", client: client}}
}

// NewGitHub returns a Host backed by the GitHub REST API. client may be nil.
func NewGitHub(token string, client *http.Client) Host {
	return &githubHost{api: api{base: "https://api.github.com", token: token, scheme: "Bearer", client: client}}
}

// api is the tiny shared JSON-over-HTTP helper for both hosts.
type api struct {
	base   string
	token  string
	scheme string // Authorization scheme: "token" (Gitea) / "Bearer" (GitHub)
	client *http.Client
}

func (a *api) httpClient() *http.Client {
	if a.client != nil {
		return a.client
	}
	return &http.Client{Timeout: defaultClientTimeout}
}

// do performs one JSON request; v may be nil to discard the body. A non-2xx
// status is an error carrying the status and a body snippet.
func (a *api) do(ctx context.Context, method, path string, body, v any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, reader)
	if err != nil {
		return err
	}
	if a.token != "" {
		req.Header.Set("Authorization", a.scheme+" "+a.token)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusMultipleChoices {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, errBodySnippet))
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(snippet))
	}
	if v == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// headSHA resolves a branch's head commit via the slash-safe query form
// (…/commits?sha=<branch>&limit=1 — a branch name with slashes cannot be a path
// segment). Both hosts serve this shape.
func (a *api) headSHA(ctx context.Context, repo, branch string) (string, error) {
	var commits []struct {
		SHA string `json:"sha"`
	}
	path := "/repos/" + repo + "/commits?limit=1&per_page=1&sha=" + url.QueryEscape(branch)
	if err := a.do(ctx, http.MethodGet, path, nil, &commits); err != nil {
		return "", err
	}
	if len(commits) == 0 || commits[0].SHA == "" {
		return "", fmt.Errorf("branch %s not found in %s", branch, repo)
	}
	return commits[0].SHA, nil
}

// ---------------------------------------------------------------------------
// Gitea

type giteaHost struct{ api api }

// giteaCombined is the subset of Gitea's combined commit status.
type giteaCombined struct {
	State    string `json:"state"` // pending | success | error | failure | warning
	Statuses []struct {
		Status      string `json:"status"`
		Context     string `json:"context"`
		Description string `json:"description"`
	} `json:"statuses"`
}

func (g *giteaHost) Status(ctx context.Context, repo, branch string) (Status, error) {
	sha, err := g.api.headSHA(ctx, repo, branch)
	if err != nil {
		return Status{}, err
	}
	var combined giteaCombined
	if err := g.api.do(ctx, http.MethodGet, "/repos/"+repo+"/commits/"+sha+"/status", nil, &combined); err != nil {
		return Status{}, err
	}
	if len(combined.Statuses) == 0 {
		return Status{State: stateNone, SHA: sha}, nil
	}
	st := Status{SHA: sha}
	var failing []string
	for _, s := range combined.Statuses {
		if s.Status == stateFailure || s.Status == stateError {
			failing = append(failing, s.Context)
		}
	}
	switch combined.State {
	case stateSuccess:
		st.State = stateSuccess
	case stateFailure, stateError:
		st.State = stateFailure
		st.Detail = detail(failing)
	default:
		st.State = statePending
	}
	return st, nil
}

// giteaPull is the subset of a Gitea PR needed to find/merge by head branch.
type giteaPull struct {
	Number int  `json:"number"`
	Merged bool `json:"merged"`
	Head   struct {
		Ref string `json:"ref"`
	} `json:"head"`
}

func (g *giteaHost) OpenPR(ctx context.Context, repo, branch string) (int, bool, error) {
	var pulls []giteaPull
	if err := g.api.do(ctx, http.MethodGet, "/repos/"+repo+"/pulls?state=open&limit=50", nil, &pulls); err != nil {
		return 0, false, err
	}
	for _, p := range pulls {
		if p.Head.Ref == branch && !p.Merged {
			return p.Number, true, nil
		}
	}
	return 0, false, nil
}

func (g *giteaHost) Merge(ctx context.Context, repo string, index int) error {
	body := map[string]string{"Do": "merge"}
	return g.api.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/pulls/%d/merge", repo, index), body, nil)
}

// ---------------------------------------------------------------------------
// GitHub

type githubHost struct{ api api }

// githubCheckRuns is the subset of the check-runs listing (GitHub Actions and
// most modern CI report here, not on the legacy statuses API).
type githubCheckRuns struct {
	TotalCount int `json:"total_count"` //nolint:tagliatelle // GitHub API uses snake_case.
	CheckRuns  []struct {
		Name   string `json:"name"`
		Status string `json:"status"` // queued | in_progress | completed
		// Conclusion: success | failure | neutral | cancelled | timed_out |
		// action_required | skipped | stale | null.
		Conclusion string `json:"conclusion"`
	} `json:"check_runs"` //nolint:tagliatelle // GitHub API uses snake_case.
}

func (g *githubHost) Status(ctx context.Context, repo, branch string) (Status, error) {
	sha, err := g.api.headSHA(ctx, repo, branch)
	if err != nil {
		return Status{}, err
	}
	var runs githubCheckRuns
	if err := g.api.do(ctx, http.MethodGet, "/repos/"+repo+"/commits/"+sha+"/check-runs", nil, &runs); err != nil {
		return Status{}, err
	}
	if runs.TotalCount == 0 {
		// No check runs — fall back to the legacy combined status (external CI).
		return g.legacyStatus(ctx, repo, sha)
	}
	st := Status{SHA: sha}
	var failing []string
	pending := false
	for _, r := range runs.CheckRuns {
		if r.Status != "completed" {
			pending = true
			continue
		}
		switch r.Conclusion {
		case stateFailure, "timed_out", "action_required":
			failing = append(failing, r.Name)
		default:
			// success / neutral / skipped / cancelled / stale — not a failure signal.
		}
	}
	switch {
	case len(failing) > 0:
		// Failures are terminal even while other runs are still going: react early.
		st.State = stateFailure
		st.Detail = detail(failing)
	case pending:
		st.State = statePending
	default:
		st.State = stateSuccess
	}
	return st, nil
}

// legacyStatus reads GitHub's combined commit status (pre-check-runs CI).
func (g *githubHost) legacyStatus(ctx context.Context, repo, sha string) (Status, error) {
	var combined struct {
		State    string `json:"state"` // failure | pending | success
		Statuses []struct {
			State   string `json:"state"`
			Context string `json:"context"`
		} `json:"statuses"`
	}
	if err := g.api.do(ctx, http.MethodGet, "/repos/"+repo+"/commits/"+sha+"/status", nil, &combined); err != nil {
		return Status{}, err
	}
	if len(combined.Statuses) == 0 {
		return Status{State: stateNone, SHA: sha}, nil
	}
	st := Status{SHA: sha}
	switch combined.State {
	case stateSuccess:
		st.State = stateSuccess
	case stateFailure, stateError:
		st.State = stateFailure
		var failing []string
		for _, s := range combined.Statuses {
			if s.State == stateFailure || s.State == stateError {
				failing = append(failing, s.Context)
			}
		}
		st.Detail = detail(failing)
	default:
		st.State = statePending
	}
	return st, nil
}

func (g *githubHost) OpenPR(ctx context.Context, repo, branch string) (int, bool, error) {
	owner := repo[:strings.IndexByte(repo, '/')]
	var pulls []struct {
		Number int `json:"number"`
	}
	path := "/repos/" + repo + "/pulls?state=open&head=" + url.QueryEscape(owner+":"+branch)
	if err := g.api.do(ctx, http.MethodGet, path, nil, &pulls); err != nil {
		return 0, false, err
	}
	if len(pulls) == 0 {
		return 0, false, nil
	}
	return pulls[0].Number, true, nil
}

func (g *githubHost) Merge(ctx context.Context, repo string, index int) error {
	return g.api.do(ctx, http.MethodPut, fmt.Sprintf("/repos/%s/pulls/%d/merge", repo, index), map[string]string{}, nil)
}

// detail renders a short failing-checks summary, capped.
func detail(failing []string) string {
	if len(failing) == 0 {
		return ""
	}
	if len(failing) > maxDetailChecks {
		failing = append(failing[:maxDetailChecks:maxDetailChecks], "…")
	}
	return "failing: " + strings.Join(failing, ", ")
}
