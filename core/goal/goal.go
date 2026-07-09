// Package goal implements the /goal evaluator loop: the user states a "done"
// criterion once, and after every clean run an INDEPENDENT evaluator — a fresh
// Claude session with no shared context, so it cannot inherit the working
// session's blind spots — judges the workspace against that criterion. Not met →
// the verdict's unmet points are injected back as a fix-up prompt; met → the
// goal is cleared and the user notified. The attempt counter bounds the loop.
//
// This package holds the durable goal store (one active goal per chat, mirroring
// core/session.FileStore: single JSON file, atomic rewrite, mutex) plus the
// evaluator prompt builder and the verdict parser. Actually RUNNING the
// evaluator (spawning the CLI) lives in core/chat, which owns the Runner.
package goal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/duckbugio/flock/internal/atomicfile"
)

// dirPerm is the owner-only permission for bot-created directories.
const dirPerm os.FileMode = 0o750

// Goal is one chat's active objective. Attempts counts completed evaluation
// rounds (bumped when a verdict is parsed, not when a run merely finishes), so
// MaxAttempts bounds the evaluate→fix loop regardless of how many messages the
// user sends while the goal is armed.
type Goal struct {
	Criterion   string `json:"criterion"`
	MaxAttempts int    `json:"maxAttempts"`
	Attempts    int    `json:"attempts"`
	CreatedAt   int64  `json:"createdAt"` // Unix ms
}

// Store persists at most one active goal per chat. A nil *FileStore behind this
// interface is NOT expected — core/chat treats a nil Store interface as "feature
// disabled" instead.
type Store interface {
	// Get returns the chat's active goal, ok=false when none is armed.
	Get(chatID string) (Goal, bool)
	// Set arms (or replaces) the chat's goal and persists the change.
	Set(chatID string, g Goal) error
	// Bump increments the chat's attempt counter and persists it, returning the
	// updated goal. ok=false when no goal is armed (nothing was bumped).
	Bump(chatID string) (Goal, bool, error)
	// Delete disarms the chat's goal. Deleting an absent goal is a no-op.
	Delete(chatID string) error
}

// FileStore is a JSON-file-backed Store, shaped like core/session.FileStore.
type FileStore struct {
	path string

	mu    sync.Mutex
	goals map[string]Goal
}

// Open loads (or creates) a FileStore at path. A missing file yields an empty
// store. The parent directory is created if needed.
func Open(path string) (*FileStore, error) {
	if path == "" {
		return nil, errors.New("goal: store path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, fmt.Errorf("goal: create store dir: %w", err)
	}
	s := &FileStore{path: path, goals: map[string]Goal{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load reads and decodes the backing file. A non-existent or empty file is not
// an error (a fresh store).
func (s *FileStore) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("goal: read store: %w", err)
	}
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, &s.goals); err != nil {
		return fmt.Errorf("goal: parse store %s: %w", s.path, err)
	}
	return nil
}

// Get returns the chat's active goal.
func (s *FileStore) Get(chatID string) (Goal, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.goals[chatID]
	return g, ok
}

// Set arms (or replaces) the chat's goal and rewrites the file atomically.
func (s *FileStore) Set(chatID string, g Goal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.goals[chatID] = g
	return s.persistLocked()
}

// Bump increments the chat's attempt counter, persists, and returns the updated
// goal. A chat with no armed goal reports ok=false and persists nothing.
func (s *FileStore) Bump(chatID string) (Goal, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.goals[chatID]
	if !ok {
		return Goal{}, false, nil
	}
	g.Attempts++
	s.goals[chatID] = g
	return g, true, s.persistLocked()
}

// Delete disarms the chat's goal and rewrites the file. Deleting an absent goal
// is a no-op that persists nothing.
func (s *FileStore) Delete(chatID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.goals[chatID]; !ok {
		return nil
	}
	delete(s.goals, chatID)
	return s.persistLocked()
}

// persistLocked writes the current map atomically. The caller must hold s.mu.
func (s *FileStore) persistLocked() error {
	data, err := json.Marshal(s.goals)
	if err != nil {
		return fmt.Errorf("goal: encode store: %w", err)
	}
	if err := atomicfile.Write(s.path, data, ".goals-*.tmp"); err != nil {
		return fmt.Errorf("goal: %w", err)
	}
	return nil
}

// Verdict is the evaluator's structured judgment. Achieved is authoritative;
// Summary is a one-paragraph justification; Unmet lists the concrete gaps the
// fix-up prompt relays back to the working session.
type Verdict struct {
	Achieved bool     `json:"achieved"`
	Summary  string   `json:"summary"`
	Unmet    []string `json:"unmet"`
}

// EvalPrompt builds the evaluator's instruction. The evaluator runs as a FRESH
// session in the same workspace (bypass permissions, like every bot run), so the
// prompt must (a) scope it to read-only inspection plus running checks, and
// (b) pin the output to a single JSON object the caller can parse.
func EvalPrompt(criterion string) string {
	return `You are an INDEPENDENT acceptance evaluator. A development session in this
workspace claims to be progressing toward the goal below. Judge, strictly and
adversarially, whether the goal is FULLY met RIGHT NOW.

GOAL (the user's own acceptance criterion):
` + criterion + `

Rules:
- Inspect the workspace yourself: read code, diffs (git log/diff on the duck/*
  branches), and run the repos' own checks (task/make/npm test, linters) when
  they bear on the goal. Evidence over claims: a session's report that "tests
  pass" counts for nothing — re-run them.
- Do NOT modify, commit, or push anything. You are a judge, not a fixer.
- Unverifiable ≠ met. If you cannot demonstrate a criterion holds, it is unmet.
- Your ENTIRE final reply must be a single JSON object, no prose around it:
  {"achieved": true|false, "summary": "<one short paragraph of justification>",
   "unmet": ["<concrete unmet point>", ...]}
  "unmet" must be empty when achieved is true, and non-empty when false.`
}

// FixupPrompt builds the prompt injected back into the WORKING session when the
// evaluator judged the goal unmet. It is an engineering artifact (professional
// English, no duck flavor) and carries the verdict's concrete gaps.
func FixupPrompt(criterion string, v Verdict) string {
	var b strings.Builder
	_, _ = b.WriteString("An independent evaluation judged the goal NOT yet achieved.\n\nGoal:\n")
	_, _ = b.WriteString(criterion)
	_, _ = b.WriteString("\n\nEvaluator's summary:\n")
	_, _ = b.WriteString(v.Summary)
	if len(v.Unmet) > 0 {
		_, _ = b.WriteString("\n\nUnmet points:\n")
		for _, u := range v.Unmet {
			_, _ = b.WriteString("- ")
			_, _ = b.WriteString(u)
			_, _ = b.WriteString("\n")
		}
	}
	_, _ = b.WriteString("\nAddress every unmet point, then finish as usual. The goal will be re-evaluated.")
	return b.String()
}

// ParseVerdict extracts the evaluator's JSON verdict from its final text. The
// model is told to reply with ONLY the JSON object, but wrappers happen (a code
// fence, a stray closing sentence), so the parser scans balanced {...} spans and
// unmarshals candidates from the LAST one backwards, accepting the first that
// carries an "achieved" key. ok=false means no verdict could be parsed — the
// caller treats the round as inconclusive rather than looping on garbage.
func ParseVerdict(text string) (Verdict, bool) {
	spans := jsonSpans(text)
	for i := len(spans) - 1; i >= 0; i-- {
		// Require the "achieved" key to be present (not merely defaulted) so an
		// unrelated JSON object in the reply can't masquerade as a verdict.
		var probe map[string]json.RawMessage
		if err := json.Unmarshal([]byte(spans[i]), &probe); err != nil {
			continue
		}
		if _, has := probe["achieved"]; !has {
			continue
		}
		var v Verdict
		if err := json.Unmarshal([]byte(spans[i]), &v); err != nil {
			continue
		}
		return v, true
	}
	return Verdict{}, false
}

// jsonSpans returns every top-level balanced {...} span in text, tracking JSON
// string literals so braces inside strings don't unbalance the scan.
func jsonSpans(text string) []string {
	var (
		spans    []string
		depth    int
		start    int
		inString bool
		escaped  bool
	)
	for i := 0; i < len(text); i++ {
		c := text[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			if depth > 0 {
				inString = true
			}
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth == 0 {
				continue
			}
			depth--
			if depth == 0 {
				spans = append(spans, text[start:i+1])
			}
		}
	}
	return spans
}
