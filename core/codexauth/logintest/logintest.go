// Package logintest builds a codexauth.Manager with a device login already in
// flight, for tests that need to assert what each kind of destination is allowed
// to see while a one-time code exists.
//
// It is separate from codexauthtest for one reason: this package imports
// codexauth, and codexauth's OWN tests import codexauthtest for the captured CLI
// banner. Putting this helper there would close that loop into an import cycle.
// So codexauthtest holds what everyone needs (the fixtures) and stays import-free
// of codexauth; this package holds what only the adapters need.
package logintest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/duckbugio/flock/core/codexauth"
	"github.com/duckbugio/flock/core/codexauth/codexauthtest"
)

// noticeBuffer holds the notices a pending login delivers before the code, so a
// slow reader cannot block the manager's broadcast.
const noticeBuffer = 8

// Unauthorized returns a Manager for a deployment that has never been signed in,
// and the scratch CODEX_HOME it watches, so a test can complete the sign-in by
// planting auth.json there.
//
// Bin points at a path that does not exist ON PURPOSE. Every caller previously
// set that by hand, and forgetting it in one future case would run the REAL
// `codex login --device-auth` on a developer's machine and leave it polling for
// the whole DeviceCodeTTL. Making it the default removes the chance to forget.
//
// (Here rather than in codexauthtest because this returns a codexauth type, and
// codexauthtest must not import codexauth — see the package doc.)
func Unauthorized(t *testing.T) (*codexauth.Manager, string) {
	t.Helper()
	home := t.TempDir()
	return codexauth.NewManager(codexauth.Config{
		Backend:     codexauth.BackendCodex,
		AuthMode:    codexauth.AuthSubscription,
		RequireAuth: true,
		Home:        home,
		Bin:         filepath.Join(home, "no-such-codex"),
	}), home
}

// PendingLogin returns a Manager whose device login is under way and whose
// one-time code has already been issued, with a scratch CODEX_HOME holding no
// credential. The login is cancelled when the test ends.
func PendingLogin(t *testing.T) *codexauth.Manager {
	t.Helper()
	m := codexauth.NewManager(codexauth.Config{
		Backend:     codexauth.BackendCodex,
		AuthMode:    codexauth.AuthSubscription,
		RequireAuth: true,
		Home:        t.TempDir(),
		Bin:         codexauthtest.WriteCLI(t, codexauthtest.Polling),
		Env:         []string{"PATH=" + os.Getenv("PATH")},
	})
	t.Cleanup(func() { m.Cancel() })

	issued := make(chan string, noticeBuffer)
	m.Dispatch(context.Background(), "", codexauth.Subscriber{
		ID: "dm", Private: true, Notify: func(text string) { issued <- text },
	})
	codexauthtest.AwaitCode(t, issued)
	return m
}
