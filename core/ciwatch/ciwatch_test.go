//nolint:testpackage // intentionally whitebox to test unexported ciwatch internals
package ciwatch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeCheckout materializes a minimal .git dir (HEAD + config) so Discover can
// parse it without a real git binary.
func fakeCheckout(t *testing.T, base, chat, repo, branch, remote string) {
	t.Helper()
	gitDir := filepath.Join(base, chat, repo, ".git")
	if err := os.MkdirAll(gitDir, 0o750); err != nil {
		t.Fatal(err)
	}
	head := "ref: refs/heads/" + branch + "\n"
	if branch == "" {
		head = "0123456789abcdef0123456789abcdef01234567\n" // detached
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte(head), 0o600); err != nil {
		t.Fatal(err)
	}
	config := "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = " + remote + "\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDiscover(t *testing.T) {
	base := t.TempDir()
	fakeCheckout(t, base, "chat_100", "svc", "duck/100/feature-x", "https://git.example.com/acme/svc.git")
	fakeCheckout(t, base, "chat_100", "lib", "main", "https://git.example.com/acme/lib.git")         // not a duck branch
	fakeCheckout(t, base, "chat_200", "svc2", "duck/200/fix", "git@github.com:acme/svc2.git")        // scp remote
	fakeCheckout(t, base, "chat_300", "bad", "duck/300/x", "not-a-remote-at-all/with/many/segments") // bad remote
	fakeCheckout(t, base, "chat_400", "det", "", "https://git.example.com/acme/det.git")             // detached HEAD
	// Same repo+branch in a second dir must be deduplicated.
	fakeCheckout(t, base, "chat_500", "svc", "duck/100/feature-x", "https://git.example.com/acme/svc.git")
	// Non-chat dirs and plain files are ignored.
	if err := os.MkdirAll(filepath.Join(base, "not_a_chat", "svc"), 0o750); err != nil {
		t.Fatal(err)
	}

	got := Discover(base)
	want := map[string]Checkout{
		"acme/svc#duck/100/feature-x": {ChatID: "100", Repo: "acme/svc", Branch: "duck/100/feature-x"},
		"acme/svc2#duck/200/fix":      {ChatID: "200", Repo: "acme/svc2", Branch: "duck/200/fix"},
	}
	if len(got) != len(want) {
		t.Fatalf("Discover = %+v, want %d checkouts", got, len(want))
	}
	for _, co := range got {
		w, ok := want[co.Repo+"#"+co.Branch]
		if !ok || w != co {
			t.Fatalf("unexpected checkout %+v", co)
		}
	}
}

func TestOwnerRepo(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"https://github.com/acme/svc.git", "acme/svc", true},
		{"https://git.example.com/acme/svc", "acme/svc", true},
		{"http://user@git.example.com/acme/svc.git", "acme/svc", true},
		{"git@github.com:acme/svc.git", "acme/svc", true},
		{"ssh://git@git.example.com/acme/svc.git", "acme/svc", true},
		{"https://git.example.com/acme/group/svc.git", "", false}, // nested groups unsupported
		{"", "", false},
		{"garbage", "", false},
	}
	for _, tc := range cases {
		got, ok := ownerRepo(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ownerRepo(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestStateDedupe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ci.json")
	st, err := OpenState(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Handled("r#b", "sha1#failure") {
		t.Fatal("fresh state must not report handled")
	}
	if err := st.Mark("r#b", "sha1#failure"); err != nil {
		t.Fatal(err)
	}
	if !st.Handled("r#b", "sha1#failure") {
		t.Fatal("marked observation must be handled")
	}
	if st.Handled("r#b", "sha2#failure") {
		t.Fatal("a new sha must not be handled")
	}
	// Survives reopen.
	re, err := OpenState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !re.Handled("r#b", "sha1#failure") {
		t.Fatal("state must persist across reopen")
	}
	// Nil state is a safe no-op.
	var nilState *State
	if nilState.Handled("x", "y") {
		t.Fatal("nil state must report unhandled")
	}
	if err := nilState.Mark("x", "y"); err != nil {
		t.Fatal(err)
	}
}

// fakeHost scripts Status/OpenPR/Merge for loop tests.
type fakeHost struct {
	status Status
	prIdx  int
	prOK   bool
	merged []int
}

func (f *fakeHost) Status(context.Context, string, string) (Status, error) { return f.status, nil }
func (f *fakeHost) OpenPR(context.Context, string, string) (int, bool, error) {
	return f.prIdx, f.prOK, nil
}

func (f *fakeHost) Merge(_ context.Context, _ string, idx int) error {
	f.merged = append(f.merged, idx)
	return nil
}

func TestScanOnceEmitsFailureOncePerSHA(t *testing.T) {
	base := t.TempDir()
	fakeCheckout(t, base, "chat_100", "svc", "duck/100/x", "https://git.example.com/acme/svc.git")
	st, err := OpenState(filepath.Join(t.TempDir(), "ci.json"))
	if err != nil {
		t.Fatal(err)
	}
	host := &fakeHost{status: Status{State: stateFailure, SHA: "abc", Detail: "failing: build"}}
	cfg := Config{BaseDir: base, Host: host, State: st}
	out := make(chan Event, 4)

	scanOnce(context.Background(), cfg, testLogger(), out)
	scanOnce(context.Background(), cfg, testLogger(), out) // same sha — deduped

	select {
	case ev := <-out:
		if ev.Kind != CIFailed || ev.ChatID != "100" || ev.SHA != "abc" {
			t.Fatalf("event = %+v", ev)
		}
	default:
		t.Fatal("expected one failure event")
	}
	select {
	case ev := <-out:
		t.Fatalf("duplicate event emitted: %+v", ev)
	default:
	}

	// A NEW sha fails → a fresh event.
	host.status = Status{State: stateFailure, SHA: "def"}
	scanOnce(context.Background(), cfg, testLogger(), out)
	select {
	case ev := <-out:
		if ev.SHA != "def" {
			t.Fatalf("event = %+v", ev)
		}
	default:
		t.Fatal("expected event for the new sha")
	}
}

func TestScanOnceAutoMerge(t *testing.T) {
	base := t.TempDir()
	fakeCheckout(t, base, "chat_100", "svc", "duck/100/x", "https://git.example.com/acme/svc.git")
	st, err := OpenState(filepath.Join(t.TempDir(), "ci.json"))
	if err != nil {
		t.Fatal(err)
	}
	host := &fakeHost{status: Status{State: stateSuccess, SHA: "abc"}, prIdx: 7, prOK: true}
	out := make(chan Event, 4)

	// AutoMerge off → nothing.
	scanOnce(context.Background(), Config{BaseDir: base, Host: host, State: st}, testLogger(), out)
	if len(host.merged) != 0 {
		t.Fatal("must not merge with AutoMerge off")
	}

	// AutoMerge on → merge once, dedupe the second scan.
	cfg := Config{BaseDir: base, Host: host, State: st, AutoMerge: true}
	scanOnce(context.Background(), cfg, testLogger(), out)
	scanOnce(context.Background(), cfg, testLogger(), out)
	if len(host.merged) != 1 || host.merged[0] != 7 {
		t.Fatalf("merged = %v, want exactly one merge of #7", host.merged)
	}
	ev := <-out
	if ev.Kind != PRMerged || ev.PRIndex != 7 {
		t.Fatalf("event = %+v", ev)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{BaseDir: t.TempDir(), Host: &fakeHost{}, Interval: time.Hour}, make(chan Event))
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run must return ctx error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}
