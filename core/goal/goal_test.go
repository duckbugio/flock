//nolint:testpackage // intentionally whitebox to test unexported goal internals
package goal

import (
	"path/filepath"
	"strings"
	"testing"
)

func openTemp(t *testing.T) *FileStore {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "goals.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStoreLifecycle(t *testing.T) {
	s := openTemp(t)
	if _, ok := s.Get("1"); ok {
		t.Fatal("fresh store must have no goal")
	}
	if err := s.Set("1", Goal{Criterion: "all tests green", MaxAttempts: 3}); err != nil {
		t.Fatal(err)
	}
	g, ok := s.Get("1")
	if !ok || g.Criterion != "all tests green" || g.Attempts != 0 {
		t.Fatalf("Get = %+v ok=%v", g, ok)
	}
	g, ok, err := s.Bump("1")
	if err != nil || !ok || g.Attempts != 1 {
		t.Fatalf("Bump = %+v ok=%v err=%v", g, ok, err)
	}
	if _, ok, _ := s.Bump("absent"); ok {
		t.Fatal("Bump on absent chat must report ok=false")
	}
	if err := s.Delete("1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("1"); ok {
		t.Fatal("deleted goal must be gone")
	}
	if err := s.Delete("1"); err != nil {
		t.Fatal("double delete must be a no-op")
	}
}

func TestStorePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goals.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("7", Goal{Criterion: "c", MaxAttempts: 5, Attempts: 2}); err != nil {
		t.Fatal(err)
	}
	re, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	g, ok := re.Get("7")
	if !ok || g.Attempts != 2 || g.MaxAttempts != 5 {
		t.Fatalf("reopened Get = %+v ok=%v", g, ok)
	}
}

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		wantOK   bool
		achieved bool
		unmet    int
	}{
		{
			name:     "bare object",
			text:     `{"achieved": true, "summary": "done", "unmet": []}`,
			wantOK:   true,
			achieved: true,
		},
		{
			name: "fenced with prose",
			text: "Here is my verdict:\n```json\n" +
				`{"achieved": false, "summary": "nope", "unmet": ["AC2 not covered"]}` +
				"\n```\nDone.",
			wantOK:   true,
			achieved: false,
			unmet:    1,
		},
		{
			name: "last object wins",
			text: `{"achieved": true, "summary": "early draft", "unmet": []}
some reasoning...
{"achieved": false, "summary": "final", "unmet": ["x", "y"]}`,
			wantOK:   true,
			achieved: false,
			unmet:    2,
		},
		{
			name:   "unrelated json is not a verdict",
			text:   `{"status": "ok"} no verdict here`,
			wantOK: false,
		},
		{
			name:     "braces inside strings",
			text:     `{"achieved": false, "summary": "code has {braces} and \"quotes\"", "unmet": ["fix {}"]}`,
			wantOK:   true,
			achieved: false,
			unmet:    1,
		},
		{
			name:   "no json at all",
			text:   "I think it's fine.",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, ok := ParseVerdict(tc.text)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (verdict %+v)", ok, tc.wantOK, v)
			}
			if !ok {
				return
			}
			if v.Achieved != tc.achieved {
				t.Fatalf("achieved = %v, want %v", v.Achieved, tc.achieved)
			}
			if len(v.Unmet) != tc.unmet {
				t.Fatalf("unmet = %d, want %d", len(v.Unmet), tc.unmet)
			}
		})
	}
}

func TestPromptsCarryCriterionAndGaps(t *testing.T) {
	p := EvalPrompt("all handlers covered by tests")
	if !strings.Contains(p, "all handlers covered by tests") {
		t.Fatal("EvalPrompt must embed the criterion")
	}
	f := FixupPrompt("crit", Verdict{Summary: "gaps remain", Unmet: []string{"handler X untested"}})
	if !strings.Contains(f, "crit") || !strings.Contains(f, "handler X untested") {
		t.Fatalf("FixupPrompt must carry criterion and unmet points, got:\n%s", f)
	}
}
