// Package teambranch owns the team-branch naming contract: every branch the
// dev team pushes is named duck/<chatid>[/<slug>], and the <chatid> segment is
// what routes host-side events (PR comments, CI state) back to the owning
// chat. The convention is load-bearing in several independent places — the
// Gitea notification poller, the CI watch, and the workspace prompt that tells
// the agent how to name branches — so the parser lives here once instead of
// drifting per consumer (the pre-extraction copies already disagreed on
// whether an empty chat id was acceptable).
package teambranch

import "strings"

// Prefix marks team branches; refs without it are not the team's.
const Prefix = "duck/"

// minSegments is the fewest "/"-separated segments a team branch carries:
// duck/<chatid>. A trailing slug is optional.
const minSegments = 2

// ChatID extracts the owning chat id from a team branch ref. ok=false when ref
// is not a team branch: no duck/ prefix, too few segments, or an EMPTY chat id
// (a "duck//slug" ref routes nowhere and must be skipped, not emitted).
func ChatID(ref string) (string, bool) {
	if !strings.HasPrefix(ref, Prefix) {
		return "", false
	}
	parts := strings.Split(ref, "/")
	if len(parts) < minSegments || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}
