// Package workspace renders a per-chat isolated workspace under a base
// directory: <base>/chat_<id>, containing a CLAUDE.md rendered from the shared
// template plus the dev-team agents under .claude/agents/. It replaces the job
// the Python entrypoint did once for the single shared workspace (see
// adapters/telegram/entrypoint.sh), now run per chat.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Renderer materializes per-chat workspaces. It is config-driven and fully
// injectable: BaseDir, TemplatePath and AgentsDir can point at temp paths in
// tests instead of the real /workspace and baked image data. The Cycles/Enable
// values are substituted into the template, mirroring the three env-style
// placeholders the Python entrypoint substituted.
type Renderer struct {
	// BaseDir is the parent of each chat workspace (APPROVED_DIRECTORY, e.g.
	// /workspace). Ensure creates <BaseDir>/chat_<id>.
	BaseDir string
	// TemplatePath is the CLAUDE.md template to render (core/CLAUDE.workspace.md.tmpl).
	TemplatePath string
	// AgentsDir holds the dev-team agent *.md files to copy into .claude/agents/.
	AgentsDir string

	// PrePRCycles, PrReviewCycles, EnablePRReview and GitHost are substituted into
	// the template for the ${PRE_PR_CYCLES} ${PR_REVIEW_CYCLES} ${ENABLE_PR_REVIEW}
	// ${GIT_HOST} placeholders respectively. Only these placeholders are
	// substituted; any other ${...} in the template is left untouched (matching
	// envsubst with an explicit variable list).
	PrePRCycles    string
	PrReviewCycles string
	EnablePRReview string
	// GitHost names the real git host (github.com / a Gitea host / …) so the prompt
	// states it instead of assuming "Gitea". Empty when git is not configured.
	GitHost string
}

// Ensure creates (or refreshes) the workspace for chatID and returns its path.
// It is idempotent: a CLAUDE.md is always re-rendered fresh (a stale one is
// dropped first, mirroring the entrypoint) and the agents are re-copied, so a
// second call simply rewrites the same files. The returned path is
// <BaseDir>/chat_<id>.
func (r *Renderer) Ensure(chatID int64) (string, error) {
	ws := filepath.Join(r.BaseDir, fmt.Sprintf("chat_%d", chatID))
	agentsDst := filepath.Join(ws, ".claude", "agents")
	if err := os.MkdirAll(agentsDst, 0o755); err != nil {
		return "", fmt.Errorf("create workspace dirs: %w", err)
	}

	if err := r.renderClaudeMD(ws); err != nil {
		return "", err
	}
	if err := r.copyAgents(agentsDst); err != nil {
		return "", err
	}
	return ws, nil
}

// UploadsDir resolves (and creates) the per-chat uploads directory at
// <BaseDir>/chat_<id>/uploads — the SAME workspace base Ensure uses, but a plain
// data dir that is a SIBLING of the cloned repos (which live in subdirectories of
// the chat workspace). User-uploaded files are saved here, so they live OUTSIDE
// every repo working tree and can never end up in a git commit/PR. The directory
// is created with 0o755 and the call is idempotent. The returned path is absolute
// when BaseDir is absolute.
func (r *Renderer) UploadsDir(chatID int64) (string, error) {
	dir := filepath.Join(r.BaseDir, fmt.Sprintf("chat_%d", chatID), "uploads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create uploads dir: %w", err)
	}
	return dir, nil
}

// OutboxDir resolves (and creates) the per-chat outbox directory at
// <BaseDir>/chat_<id>/outbox — the SAME workspace base Ensure uses, mirroring
// UploadsDir. It is a plain data dir that is a SIBLING of the cloned repos
// (which live in subdirectories of the chat workspace), so files a run writes
// here for delivery live OUTSIDE every repo working tree and can never end up in
// a git commit/PR. After a successful send each file is archived under the
// <outbox>/sent subdirectory (created on first archive). The directory is
// created with 0o755 and the call is idempotent. The returned path is absolute
// when BaseDir is absolute.
func (r *Renderer) OutboxDir(chatID int64) (string, error) {
	dir := filepath.Join(r.BaseDir, fmt.Sprintf("chat_%d", chatID), "outbox")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create outbox dir: %w", err)
	}
	return dir, nil
}

// outboxConvention is appended to every rendered CLAUDE.md so the dev-team
// agents know how to hand the user a generated file. It is kept here (not in the
// protected template) so the template file on disk stays byte-identical.
const outboxConvention = `

## Sending files to the user

To deliver a file (a report, an export, a generated artifact) to the user, write
it into the ` + "`outbox/`" + ` directory at the workspace root — a sibling of the
repos that is never committed. After the run finishes, each regular file directly
in ` + "`outbox/`" + ` is sent to the user as a Telegram document and archived
under ` + "`outbox/sent/`" + `. Do not put files you want delivered inside a repo.
`

// renderClaudeMD reads the template, substitutes the three configured
// placeholders, appends the outbox convention, drops any stale CLAUDE.md, and
// writes the fresh file.
func (r *Renderer) renderClaudeMD(ws string) error {
	raw, err := os.ReadFile(r.TemplatePath)
	if err != nil {
		return fmt.Errorf("read template: %w", err)
	}

	// Substitute only the known placeholders; leave every other ${...} intact,
	// exactly like `envsubst '${PRE_PR_CYCLES} ...'` in the entrypoint.
	rendered := strings.NewReplacer(
		"${PRE_PR_CYCLES}", r.PrePRCycles,
		"${PR_REVIEW_CYCLES}", r.PrReviewCycles,
		"${ENABLE_PR_REVIEW}", r.EnablePRReview,
		"${GIT_HOST}", r.GitHost,
	).Replace(string(raw))

	// Append the outbox convention to the RENDERED output (after substitution),
	// keeping the protected template file on disk byte-identical (AC6).
	rendered += outboxConvention

	dst := filepath.Join(ws, "CLAUDE.md")
	// Drop a possibly stale/root-owned stub so the write recreates it fresh,
	// mirroring the entrypoint's `rm -f` before the redirect.
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale CLAUDE.md: %w", err)
	}
	if err := os.WriteFile(dst, []byte(rendered), 0o644); err != nil {
		return fmt.Errorf("write CLAUDE.md: %w", err)
	}
	return nil
}

// copyAgents copies every *.md under AgentsDir into agentsDst, overwriting any
// existing copies so each Ensure refreshes them (matching the entrypoint's
// `cp -f`).
func (r *Renderer) copyAgents(agentsDst string) error {
	entries, err := os.ReadDir(r.AgentsDir)
	if err != nil {
		return fmt.Errorf("read agents dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(r.AgentsDir, e.Name()))
		if err != nil {
			return fmt.Errorf("read agent %s: %w", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(agentsDst, e.Name()), data, 0o644); err != nil {
			return fmt.Errorf("write agent %s: %w", e.Name(), err)
		}
	}
	return nil
}
