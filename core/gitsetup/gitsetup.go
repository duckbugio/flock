// Package gitsetup performs the bot's startup git configuration: identity,
// default branch, and — when a host + token are configured — an inline
// credential helper that reads the username/token from the live process
// environment at call time, so no secret ever lands in a git config file.
//
// It replaces the git portion of the Python adapter's entrypoint.sh, improving
// on the external "gitea" credential helper by wiring an inline shell helper
// instead.
package gitsetup

import (
	"context"
	"fmt"
	"os/exec"
)

// credentialHelperValue is the inline credential-helper command stored in the
// git config. The leading "!" tells git to run it via the shell; git appends
// the operation (e.g. "get"), so $1 is the operation and $GIT_USER/$GIT_TOKEN
// expand from the live environment at call time — the token is never persisted.
//
//nolint:gosec // G101: identifier name, not a credential value.
const credentialHelperValue = `!f() { test "$1" = get && echo "username=$GIT_USER" && echo "password=$GIT_TOKEN"; }; f`

// Config is the git startup configuration derived from the environment.
type Config struct {
	Host        string // GIT_HOST (e.g. "git.example.com"); when empty, the credential helper is skipped
	Scheme      string // GIT_SCHEME, default "https"
	AuthorName  string // GIT_AUTHOR_NAME, default "AI Team"
	AuthorEmail string // GIT_AUTHOR_EMAIL, default "ai@example.com"
	HasToken    bool   // GIT_TOKEN present — only then wire the credential helper
}

// Apply writes the global git config (identity, default branch, and — when a host
// + token are configured — the inline credential helper) by shelling out to
// `git config --global`. Best-effort startup wiring: returns the first error but
// the caller treats failures as non-fatal (logs a warning), matching the Python
// entrypoint which never crash-loops on git setup.
func Apply(ctx context.Context, cfg Config) error {
	scheme := cfg.Scheme
	if scheme == "" {
		scheme = "https"
	}
	authorName := cfg.AuthorName
	if authorName == "" {
		authorName = "AI Team"
	}
	authorEmail := cfg.AuthorEmail
	if authorEmail == "" {
		authorEmail = "ai@example.com"
	}

	settings := [][2]string{
		{"user.name", authorName},
		{"user.email", authorEmail},
		{"init.defaultBranch", "main"},
	}
	// Only wire the credential helper when both a host and a token are present,
	// mirroring the Python entrypoint's guard.
	if cfg.Host != "" && cfg.HasToken {
		key := fmt.Sprintf("credential.%s://%s.helper", scheme, cfg.Host)
		settings = append(settings, [2]string{key, credentialHelperValue})
	}

	for _, kv := range settings {
		if err := setGlobal(ctx, kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// setGlobal runs `git config --global <key> <value>`, inheriting the ambient
// environment (so tests can override HOME to redirect the global config).
func setGlobal(ctx context.Context, key, value string) error {
	//nolint:gosec // G204: args are internal git config keys/values, not raw user input.
	cmd := exec.CommandContext(ctx, "git", "config", "--global", key, value)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git config --global %s: %w: %s", key, err, out)
	}
	return nil
}
