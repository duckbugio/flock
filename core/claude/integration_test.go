//go:build integration

//nolint:testpackage // intentionally whitebox to test unexported runner internals
package claude

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestIntegration_RealCLI drives the real `claude` CLI on a trivial prompt and
// asserts it reaches a terminal Result. It is gated behind the `integration`
// build tag so the normal `go test ./...` (which has no auth) never runs it.
//
// Run it explicitly with auth in the environment:
//
//	go test -tags integration -run TestIntegration ./core/claude/...
func TestIntegration_RealCLI(t *testing.T) {
	bin := os.Getenv("CLAUDE_BIN")
	if bin == "" {
		bin = "claude"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	r := New(bin)
	ch, err := r.Run(ctx, "say hi", Options{
		Workdir:  wd,
		MaxTurns: 1,
		Env:      os.Environ(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var result *RunResult
	var runErr error
	for e := range ch {
		switch e.Type {
		case Result:
			result = e.Result
		case RunError:
			runErr = e.Err
		default:
			// Other event types are not asserted by this test.
		}
	}

	if runErr != nil {
		t.Fatalf("run error: %v", runErr)
	}
	if result == nil {
		t.Fatal("no terminal Result event")
	}
	t.Logf("result: subtype=%q is_error=%v turns=%d cost=%v session=%s text=%q",
		result.Subtype, result.IsError, result.NumTurns, result.CostUSD, result.SessionID, result.Text)
}
