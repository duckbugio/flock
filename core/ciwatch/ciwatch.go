// Package ciwatch closes the CI feedback loop without a human relay: a
// background loop discovers every duck/<chatid>/... branch checked out in the
// per-chat workspaces, polls the git host for that branch's CI state, and — on
// a red build — emits an event the adapter injects into the owning chat as a
// fix-up prompt ("CI failed, here are the failing checks — fix and push").
// Optionally (ENABLE_AUTO_MERGE) it also merges a branch's open PR once CI is
// green, removing the last human touchpoint for deployments that want it.
//
// Like core/poller it POLLS (the bot can reach the host; inbound webhooks often
// can't reach the bot) and routes events by the chat id embedded in the branch
// name. Handled failures are deduplicated per repo+branch+SHA in a small
// durable store so one red build injects exactly one fix-up, across restarts.
package ciwatch

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/duckbugio/flock/core/teambranch"
)

// minInterval floors the poll period, mirroring core/poller.
const minInterval = 30 * time.Second

// ownerRepoParts is the exact "/"-separated segments of an owner/repo name.
const ownerRepoParts = 2

// EventKind discriminates watch events.
type EventKind int

const (
	// CIFailed is emitted once per (repo, branch, SHA) when CI turns red.
	CIFailed EventKind = iota
	// PRMerged is emitted after auto-merge lands a green PR.
	PRMerged
	// CIGreen is emitted once per (repo, branch, SHA) when CI completes
	// successfully and auto-merge did not consume the event. It exists to WAKE
	// the owning chat: nothing else notifies an agent that promised to report a
	// CI verdict — the poller only relays new PR comments, so without this a
	// green build ends every conversation until the user pings.
	CIGreen
)

// Event is one actionable CI observation, routed by ChatID.
type Event struct {
	Kind    EventKind
	ChatID  string
	Repo    string // owner/repo
	Branch  string
	SHA     string
	Detail  string // short failing-checks summary (CIFailed)
	PRIndex int    // PRMerged
}

// Status is a host-neutral CI summary for one branch head.
type Status struct {
	// State is one of "success", "failure", "pending", "none" (no CI configured).
	State  string
	SHA    string
	Detail string
}

// Host abstracts the git host API (Gitea or GitHub — see hosts.go).
type Host interface {
	// Status resolves branch's head and summarizes its CI state.
	Status(ctx context.Context, repo, branch string) (Status, error)
	// OpenPR finds the open PR whose head is branch; ok=false when none.
	OpenPR(ctx context.Context, repo, branch string) (index int, ok bool, err error)
	// Merge merges the PR (the host's default merge style).
	Merge(ctx context.Context, repo string, index int) error
}

// Config configures the watch loop.
type Config struct {
	// BaseDir is the workspace root scanned for chat_<id>/<repo> checkouts
	// (APPROVED_DIRECTORY).
	BaseDir   string
	Host      Host
	Interval  time.Duration // poll period; floored to 30s
	AutoMerge bool
	State     *State // dedupe store; nil disables dedupe (every scan re-emits)
	Logger    *slog.Logger
}

// Run polls until ctx is cancelled, emitting an Event on out for each fresh CI
// failure (and each auto-merge). Never closes out; logs and continues on
// transient errors.
func Run(ctx context.Context, cfg Config, out chan<- Event) error {
	interval := cfg.Interval
	if interval < minInterval {
		interval = minInterval
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	log.Info("ci watch started", "base", cfg.BaseDir, "interval", interval, "auto_merge", cfg.AutoMerge)
	for {
		scanOnce(ctx, cfg, log, out)
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// scanOnce walks the current checkouts and reconciles each against the host,
// then prunes dedupe state whose branch is no longer checked out anywhere so
// the store tracks live branches instead of growing toward the wipe backstop.
func scanOnce(ctx context.Context, cfg Config, log *slog.Logger, out chan<- Event) {
	checkouts := Discover(cfg.BaseDir)
	live := make(map[string]struct{}, len(checkouts))
	for _, co := range checkouts {
		live[co.Repo+"#"+co.Branch] = struct{}{}
		if ctx.Err() != nil {
			return
		}
		st, err := cfg.Host.Status(ctx, co.Repo, co.Branch)
		if err != nil {
			log.Warn("ci watch: status failed", "repo", co.Repo, "branch", co.Branch, "error", err)
			continue
		}
		switch st.State {
		case stateFailure:
			handleFailure(ctx, cfg, log, out, co, st)
		case stateSuccess:
			handleSuccess(ctx, cfg, log, out, co, st)
		default:
			// pending / none — nothing to do this cycle.
		}
	}
	if err := cfg.State.Prune(live); err != nil {
		log.Warn("ci watch: prune state", "error", err)
	}
}

// handleFailure emits one fix-up event per fresh (repo, branch, SHA) failure.
func handleFailure(ctx context.Context, cfg Config, log *slog.Logger, out chan<- Event, co Checkout, st Status) {
	key := co.Repo + "#" + co.Branch
	mark := st.SHA + "#failure"
	if cfg.State.Handled(key, mark) {
		return
	}
	ev := Event{
		Kind: CIFailed, ChatID: co.ChatID, Repo: co.Repo, Branch: co.Branch, SHA: st.SHA, Detail: st.Detail,
	}
	if !emit(ctx, out, ev) {
		return
	}
	log.Info("ci watch: failure relayed", "repo", co.Repo, "branch", co.Branch, "sha", st.SHA)
	if err := cfg.State.Mark(key, mark); err != nil {
		log.Warn("ci watch: persist state", "error", err)
	}
}

// handleSuccess reacts to a green build exactly once per (repo, branch, SHA):
// with auto-merge on it merges the branch's open PR (emitting PRMerged); in
// every other case it emits CIGreen so the owning chat WAKES UP — an agent
// that told its user "I'll report when CI finishes" has no other wake-up
// signal for a green build.
func handleSuccess(ctx context.Context, cfg Config, log *slog.Logger, out chan<- Event, co Checkout, st Status) {
	key := co.Repo + "#" + co.Branch
	greenMark := st.SHA + "#green"
	mergedMark := st.SHA + "#merged"
	if cfg.State.Handled(key, greenMark) || cfg.State.Handled(key, mergedMark) {
		return
	}
	if cfg.AutoMerge {
		if consumed := tryAutoMerge(ctx, cfg, log, out, co, st, key, mergedMark); consumed {
			return
		}
		// No open PR: the merge path has nothing to do — still announce the green
		// build below exactly once.
	}
	ev := Event{Kind: CIGreen, ChatID: co.ChatID, Repo: co.Repo, Branch: co.Branch, SHA: st.SHA}
	if !emit(ctx, out, ev) {
		return
	}
	log.Info("ci watch: green relayed", "repo", co.Repo, "branch", co.Branch, "sha", st.SHA)
	if err := cfg.State.Mark(key, greenMark); err != nil {
		log.Warn("ci watch: persist state", "error", err)
	}
}

// tryAutoMerge merges the branch's open PR, reporting whether the green event
// was consumed (merged, or a transient lookup/merge failure to retry next
// cycle). false means "no PR" — the caller falls through to the CIGreen path.
func tryAutoMerge(
	ctx context.Context, cfg Config, log *slog.Logger, out chan<- Event, co Checkout, st Status, key, mark string,
) bool {
	idx, ok, err := cfg.Host.OpenPR(ctx, co.Repo, co.Branch)
	if err != nil {
		log.Warn("ci watch: open-pr lookup failed", "repo", co.Repo, "branch", co.Branch, "error", err)
		return true // transient: retry the whole green handling next cycle
	}
	if !ok {
		return false
	}
	if err := cfg.Host.Merge(ctx, co.Repo, idx); err != nil {
		// Not mergeable yet (conflicts, required reviews) — retried next cycle.
		log.Warn("ci watch: merge failed", "repo", co.Repo, "pr", idx, "error", err)
		return true
	}
	ev := Event{Kind: PRMerged, ChatID: co.ChatID, Repo: co.Repo, Branch: co.Branch, SHA: st.SHA, PRIndex: idx}
	if !emit(ctx, out, ev) {
		return true
	}
	log.Info("ci watch: auto-merged", "repo", co.Repo, "pr", idx)
	if err := cfg.State.Mark(key, mark); err != nil {
		log.Warn("ci watch: persist state", "error", err)
	}
	return true
}

// emit sends ev on out unless ctx is cancelled.
func emit(ctx context.Context, out chan<- Event, ev Event) bool {
	select {
	case out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// Checkout is one duck/* branch checked out in some chat's workspace.
type Checkout struct {
	ChatID string
	Repo   string // owner/repo derived from the origin remote
	Branch string
}

// Discover scans base for chat_<id>/<repo> git checkouts currently on a
// duck/<chatid>/... branch and returns one Checkout per (repo, branch),
// deduplicated. It reads .git/HEAD and .git/config directly (no git exec), so a
// scan is a handful of small file reads. The chat id is taken from the BRANCH
// (authoritative for routing, mirroring core/poller), not the directory name.
func Discover(base string) []Checkout {
	chats, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	var out []Checkout
	for _, chatDir := range chats {
		if !chatDir.IsDir() || !strings.HasPrefix(chatDir.Name(), "chat_") {
			continue
		}
		repos, err := os.ReadDir(filepath.Join(base, chatDir.Name()))
		if err != nil {
			continue
		}
		for _, repoDir := range repos {
			if !repoDir.IsDir() {
				continue
			}
			gitDir := filepath.Join(base, chatDir.Name(), repoDir.Name(), ".git")
			co, ok := checkoutFromGitDir(gitDir)
			if !ok {
				continue
			}
			key := co.Repo + "#" + co.Branch
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, co)
		}
	}
	return out
}

// checkoutFromGitDir builds a Checkout from one .git directory, requiring a
// routable team branch (see core/teambranch) and a parsable origin remote.
func checkoutFromGitDir(gitDir string) (Checkout, bool) {
	branch := currentBranch(gitDir)
	chatID, ok := teambranch.ChatID(branch)
	if !ok {
		return Checkout{}, false
	}
	repo, ok := ownerRepo(originURL(gitDir))
	if !ok {
		return Checkout{}, false
	}
	return Checkout{ChatID: chatID, Repo: repo, Branch: branch}, true
}

// currentBranch reads .git/HEAD and returns the checked-out branch name, or ""
// for a detached HEAD / unreadable file / worktree-style .git file.
func currentBranch(gitDir string) string {
	data, err := os.ReadFile(filepath.Join(gitDir, "HEAD")) //nolint:gosec // internal workspace path
	if err != nil {
		return ""
	}
	const refPrefix = "ref: refs/heads/"
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, refPrefix) {
		return ""
	}
	return strings.TrimPrefix(line, refPrefix)
}

// originURL scans .git/config for the [remote "origin"] url line.
func originURL(gitDir string) string {
	data, err := os.ReadFile(filepath.Join(gitDir, "config")) //nolint:gosec // internal workspace path
	if err != nil {
		return ""
	}
	inOrigin := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "[") {
			inOrigin = line == `[remote "origin"]`
			continue
		}
		if !inOrigin {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "url"); ok {
			if v, ok := strings.CutPrefix(strings.TrimSpace(rest), "="); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

// ownerRepo extracts "owner/repo" from an https/ssh/scp-style remote URL.
func ownerRepo(url string) (string, bool) {
	if url == "" {
		return "", false
	}
	s := strings.TrimSuffix(strings.TrimSpace(url), ".git")
	// scp-style: git@host:owner/repo
	if at := strings.Index(s, "@"); at >= 0 && !strings.Contains(s, "://") {
		if colon := strings.Index(s[at:], ":"); colon >= 0 {
			s = s[at+colon+1:]
			return validOwnerRepo(s)
		}
	}
	// URL-style: scheme://[user@]host/owner/repo
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
		if slash := strings.IndexByte(s, '/'); slash >= 0 {
			return validOwnerRepo(s[slash+1:])
		}
		return "", false
	}
	return validOwnerRepo(s)
}

// validOwnerRepo accepts exactly "owner/repo" with non-empty segments.
func validOwnerRepo(s string) (string, bool) {
	parts := strings.Split(strings.Trim(s, "/"), "/")
	if len(parts) != ownerRepoParts || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return parts[0] + "/" + parts[1], true
}
