//nolint:testpackage // intentionally whitebox to test unexported workspace helpers
package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestRenderer builds a Renderer over temp paths: a temp base dir, a temp
// template containing the three placeholders (plus an unrelated one that must
// survive), and a temp agents dir with two markdown files.
func newTestRenderer(t *testing.T) *Renderer {
	t.Helper()
	base := t.TempDir()
	src := t.TempDir()

	tmpl := filepath.Join(src, "CLAUDE.workspace.md.tmpl")
	tmplBody := "cycles=${PRE_PR_CYCLES} review=${PR_REVIEW_CYCLES} enable=${ENABLE_PR_REVIEW} host=${GIT_HOST} keep=${OTHER_VAR}\n"
	if err := os.WriteFile(tmpl, []byte(tmplBody), 0o600); err != nil {
		t.Fatalf("write template: %v", err)
	}

	agents := filepath.Join(src, "agents")
	if err := os.MkdirAll(agents, 0o750); err != nil {
		t.Fatalf("mkdir agents: %v", err)
	}
	for _, name := range []string{"arbiter.md", "coder.md"} {
		if err := os.WriteFile(filepath.Join(agents, name), []byte("# "+name), 0o600); err != nil {
			t.Fatalf("write agent %s: %v", name, err)
		}
	}
	// A non-md file must be ignored.
	if err := os.WriteFile(filepath.Join(agents, "notes.txt"), []byte("ignore"), 0o600); err != nil {
		t.Fatalf("write notes: %v", err)
	}

	return &Renderer{
		BaseDir:        base,
		TemplatePath:   tmpl,
		AgentsDir:      agents,
		PrePRCycles:    "5",
		PrReviewCycles: "10",
		EnablePRReview: "false",
		GitHost:        "github.com",
	}
}

func TestEnsureRendersWorkspace(t *testing.T) {
	r := newTestRenderer(t)

	ws, err := r.Ensure("123")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	wantWS := filepath.Join(r.BaseDir, "chat_123")
	if ws != wantWS {
		t.Fatalf("path = %q, want %q", ws, wantWS)
	}
	if fi, err := os.Stat(ws); err != nil || !fi.IsDir() {
		t.Fatalf("workspace dir missing: err=%v", err)
	}

	// CLAUDE.md substituted the three vars and left the unrelated one intact.
	body, err := os.ReadFile(filepath.Join(ws, "CLAUDE.md")) //nolint:gosec // G304: test reads a controlled temp/workspace path
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}
	got := string(body)
	for _, want := range []string{"cycles=5", "review=10", "enable=false", "host=github.com"} {
		if !strings.Contains(got, want) {
			t.Fatalf("CLAUDE.md missing %q; got %q", want, got)
		}
	}
	for _, raw := range []string{"${PRE_PR_CYCLES}", "${PR_REVIEW_CYCLES}", "${ENABLE_PR_REVIEW}", "${GIT_HOST}"} {
		if strings.Contains(got, raw) {
			t.Fatalf("CLAUDE.md still contains raw placeholder %q", raw)
		}
	}
	// The unrelated placeholder must NOT be substituted (envsubst-with-list parity).
	if !strings.Contains(got, "${OTHER_VAR}") {
		t.Fatalf("unrelated placeholder was substituted; got %q", got)
	}

	// Agents copied (only the .md files).
	agentsDst := filepath.Join(ws, ".claude", "agents")
	for _, name := range []string{"arbiter.md", "coder.md"} {
		if _, err := os.Stat(filepath.Join(agentsDst, name)); err != nil {
			t.Fatalf("agent %s not copied: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(agentsDst, "notes.txt")); !os.IsNotExist(err) {
		t.Fatalf("non-md file should not be copied (err=%v)", err)
	}
}

// TestEnsureCopiesSkills asserts the skills tree (each <name>/SKILL.md) is mirrored
// into the workspace's .claude/skills/, preserving the subdirectory layout.
func TestEnsureCopiesSkills(t *testing.T) {
	r := newTestRenderer(t)
	skills := t.TempDir()
	skillDir := filepath.Join(skills, "verification")
	if err := os.MkdirAll(skillDir, 0o750); err != nil {
		t.Fatalf("mkdir skill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# verify"), 0o600); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	r.SkillsDir = skills

	ws, err := r.Ensure("9")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	dst := filepath.Join(ws, ".claude", "skills", "verification", "SKILL.md")
	body, err := os.ReadFile(dst) //nolint:gosec // G304: controlled temp/workspace path
	if err != nil {
		t.Fatalf("SKILL.md not copied: %v", err)
	}
	if string(body) != "# verify" {
		t.Fatalf("SKILL.md content = %q, want %q", body, "# verify")
	}
}

// TestEnsureSkillsOptional asserts an unset SkillsDir is a no-op: Ensure succeeds
// and writes no .claude/skills/ directory.
func TestEnsureSkillsOptional(t *testing.T) {
	r := newTestRenderer(t) // SkillsDir unset
	ws, err := r.Ensure("8")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".claude", "skills")); !os.IsNotExist(err) {
		t.Fatalf("skills dir should not exist when SkillsDir unset (err=%v)", err)
	}
}

// TestUploadsDir asserts the uploads dir resolves to <workspace>/uploads, is
// created, and is NOT nested inside any directory that contains a .git (so
// uploaded files can never enter a repo working tree / commit — AC4).
func TestUploadsDir(t *testing.T) {
	r := newTestRenderer(t)

	dir, err := r.UploadsDir("123")
	if err != nil {
		t.Fatalf("UploadsDir: %v", err)
	}

	wantWS := filepath.Join(r.BaseDir, "chat_123")
	want := filepath.Join(wantWS, "uploads")
	if dir != want {
		t.Fatalf("uploads dir = %q, want %q", dir, want)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("uploads dir missing: err=%v", err)
	}

	// No-commit guarantee: walk from the uploads dir up to BaseDir and assert no
	// ancestor contains a .git entry, so a saved file is outside every repo tree.
	for cur := dir; ; cur = filepath.Dir(cur) {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			t.Fatalf("uploads dir %q is inside a git repo (ancestor %q has .git)", dir, cur)
		}
		if cur == r.BaseDir {
			break
		}
	}
}

// TestUploadsDirIdempotent asserts a second call returns the same path and does
// not error (the dir already exists).
func TestUploadsDirIdempotent(t *testing.T) {
	r := newTestRenderer(t)
	first, err := r.UploadsDir("7")
	if err != nil {
		t.Fatalf("first UploadsDir: %v", err)
	}
	second, err := r.UploadsDir("7")
	if err != nil {
		t.Fatalf("second UploadsDir: %v", err)
	}
	if first != second {
		t.Fatalf("uploads dir changed between calls: %q vs %q", first, second)
	}
}

// TestOutboxDir asserts the outbox dir resolves to <workspace>/outbox, is
// created, and is NOT nested inside any directory that contains a .git (so files
// a run writes there for delivery can never enter a repo working tree — AC5),
// mirroring UploadsDir.
func TestOutboxDir(t *testing.T) {
	r := newTestRenderer(t)

	dir, err := r.OutboxDir("123")
	if err != nil {
		t.Fatalf("OutboxDir: %v", err)
	}

	wantWS := filepath.Join(r.BaseDir, "chat_123")
	want := filepath.Join(wantWS, "outbox")
	if dir != want {
		t.Fatalf("outbox dir = %q, want %q", dir, want)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("outbox dir missing: err=%v", err)
	}

	// No-commit guarantee: walk from the outbox dir up to BaseDir and assert no
	// ancestor contains a .git entry, so a delivered file is outside every repo.
	for cur := dir; ; cur = filepath.Dir(cur) {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			t.Fatalf("outbox dir %q is inside a git repo (ancestor %q has .git)", dir, cur)
		}
		if cur == r.BaseDir {
			break
		}
	}
}

// TestOutboxDirIdempotent asserts a second call returns the same path and does
// not error (the dir already exists).
func TestOutboxDirIdempotent(t *testing.T) {
	r := newTestRenderer(t)
	first, err := r.OutboxDir("7")
	if err != nil {
		t.Fatalf("first OutboxDir: %v", err)
	}
	second, err := r.OutboxDir("7")
	if err != nil {
		t.Fatalf("second OutboxDir: %v", err)
	}
	if first != second {
		t.Fatalf("outbox dir changed between calls: %q vs %q", first, second)
	}
}

// TestRenderedClaudeMDHasOutboxSection covers AC6: the RENDERED CLAUDE.md gains
// the outbox-convention section, while the template FILE on disk stays
// byte-identical (it has no such text).
func TestRenderedClaudeMDHasOutboxSection(t *testing.T) {
	r := newTestRenderer(t)

	// Capture the template file's exact bytes BEFORE rendering.
	before, err := os.ReadFile(r.TemplatePath)
	if err != nil {
		t.Fatalf("read template before: %v", err)
	}

	ws, err := r.Ensure("123")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(ws, "CLAUDE.md")) //nolint:gosec // G304: test reads a controlled temp/workspace path
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		"## Sending files to the user", "outbox/", "outbox/sent/",
		// A fragment of the outbox-delivery sentence: a file dropped in outbox/ is
		// sent to the user as a Telegram document.
		"sent to the user as a Telegram document",
		"## Taking webpage screenshots", "shot <url> <out.png>", "--full",
		// The auth'd-page limitation must be documented (initData).
		"initData",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered CLAUDE.md missing %q; got:\n%s", want, got)
		}
	}

	// The template FILE on disk must be unchanged (byte-identical) and must NOT
	// itself contain the appended sections — the append happens only to the
	// rendered output.
	after, err := os.ReadFile(r.TemplatePath)
	if err != nil {
		t.Fatalf("read template after: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("template file changed during render (must be byte-identical)")
	}
	if strings.Contains(string(after), "## Sending files to the user") {
		t.Fatalf("template file must not contain the outbox section")
	}
	if strings.Contains(string(after), "## Taking webpage screenshots") {
		t.Fatalf("template file must not contain the screenshot section")
	}
}

func TestEnsureIdempotent(t *testing.T) {
	r := newTestRenderer(t)

	if _, err := r.Ensure("7"); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	ws, err := r.Ensure("7")
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}

	// CLAUDE.md is still present and correct after the second call.
	body, err := os.ReadFile(filepath.Join(ws, "CLAUDE.md")) //nolint:gosec // G304: test reads a controlled temp/workspace path
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}
	if !strings.Contains(string(body), "cycles=5") {
		t.Fatalf("CLAUDE.md content lost after second Ensure: %q", string(body))
	}
}

func TestAutoApproveScopeSubstitution(t *testing.T) {
	r := newTestRenderer(t)
	tmpl := filepath.Join(t.TempDir(), "tmpl.md")
	if err := os.WriteFile(tmpl, []byte("auto=${AUTO_APPROVE_SCOPE}\n"), 0o600); err != nil {
		t.Fatalf("write template: %v", err)
	}
	r.TemplatePath = tmpl

	// Unset renders the fail-safe "off".
	ws, err := r.Ensure("900")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(ws, "CLAUDE.md")) //nolint:gosec // test path under t.TempDir
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}
	if !strings.Contains(string(data), "auto=off") {
		t.Fatalf("unset AutoApproveScope must render off, got: %s", data)
	}

	// A configured level renders verbatim.
	r.AutoApproveScope = "trivial"
	if _, err := r.Ensure("900"); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	data, err = os.ReadFile(filepath.Join(ws, "CLAUDE.md")) //nolint:gosec // test path under t.TempDir
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}
	if !strings.Contains(string(data), "auto=trivial") {
		t.Fatalf("AutoApproveScope must substitute, got: %s", data)
	}
}
