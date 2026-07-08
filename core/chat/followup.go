package chat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/duckbugio/flock/core/followup"
)

// followupDirName is the workspace-root directory the follow-up convention
// sweeps (a sibling of the repos, like uploads/ and outbox/).
const followupDirName = "followup"

// sweepFollowups collects the follow-up files a run wrote into
// <workdir>/followup/ and schedules each in the durable store: the filename
// stem is the delay ("15m.md" → 15 minutes), the content is the prompt to run
// then. Every consumed file is deleted (scheduled or rejected — leaving a bad
// file would re-reject it after every subsequent run); each outcome is
// surfaced to the chat with a short notice. Returns how many follow-ups were
// scheduled. A nil store (feature disabled) or an absent directory is a no-op.
func (s *Service) sweepFollowups(ctx context.Context, chatID ChatID, workdir string) int {
	if s.postrun.Followups == nil {
		return 0
	}
	dir := filepath.Join(workdir, followupDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0 // absent dir — the run scheduled nothing
	}
	scheduled := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		path := filepath.Join(dir, name)
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		delay, ok := followup.ParseDelay(stem)
		data, readErr := os.ReadFile(path) //nolint:gosec // path is a join of internal workspace dirs.
		prompt := strings.TrimSpace(string(data))
		switch {
		case !ok:
			s.notify(ctx, chatID, fmt.Sprintf(
				"⚠️ followup/%s ignored — the filename must be a delay like 15m.md or 2h.md.", name))
		case readErr != nil || prompt == "":
			s.notify(ctx, chatID, fmt.Sprintf(
				"⚠️ followup/%s ignored — the file content must be the prompt to run.", name))
		default:
			due := s.nowFunc().Add(delay)
			if _, err := s.postrun.Followups.Add(chatID, prompt, due); err != nil {
				s.log.Warn("schedule follow-up", "chat_id", chatID, "error", err)
				s.notify(ctx, chatID, "⚠️ followup/"+name+" not scheduled: "+err.Error())
			} else {
				scheduled++
				s.notify(ctx, chatID, fmt.Sprintf(
					"⏰ Follow-up scheduled in %s (%s).", delay, due.Format("15:04 MST")))
			}
		}
		if err := os.Remove(path); err != nil {
			s.log.Warn("remove follow-up file", "chat_id", chatID, "path", path, "error", err)
		}
	}
	return scheduled
}

// FollowupPrompt renders the injected prompt for a fired follow-up.
func FollowupPrompt(it followup.Item) string {
	return "Scheduled follow-up fired — you asked to come back to this now. Do the work in the " +
		"foreground and report the result to the user in their language.\n\n" + it.Prompt
}

// promiseNudgeMarker is the first line of the corrective prompt injected when
// a run's final answer promised to come back on its own. Its presence in a
// run's OWN prompt suppresses the detector for that run, so a nudge can never
// chain into a nudge loop.
const promiseNudgeMarker = "Self-follow-up check."

// promiseRe matches first-person "I'll come back on my own" promises in a
// final answer — the exact phrases that strand users, in the two languages the
// bot's audiences use. Deliberately conservative: asking the USER to follow up
// ("let me know", "напиши, когда") must not match.
var promiseRe = regexp.MustCompile(`(?i)` +
	`i(?:['’]|\x60)?ll (?:report|follow up|check back|update you|keep (?:you posted|an eye|watching)|let you know|get back to you)` +
	`|i will (?:report|follow up|update you|let you know|get back to you)` +
	`|report back (?:once|when|after|as soon as)` +
	`|standing by` +
	`|доложу` +
	`|отпишусь` +
	`|вернусь с результат` +
	`|сообщу,? (?:когда|как только)` +
	`|дам знать,? (?:когда|как только)` +
	`|напишу,? (?:когда|как только)` +
	`|буду следить`)

// maybeNudgePromise fires ONE corrective run when a cleanly finished run's
// final answer promised to come back on its own while scheduling nothing that
// actually would (no follow-up file, and this run is not itself the nudge).
// Prompt rules alone demonstrably fail here — the model rationalizes "the
// poller will wake me" — so the safety net is mechanical: the nudge makes the
// run either do the work now, schedule a real follow-up, or retract the
// promise. Bounded to one step by promiseNudgeMarker and budget-gated by
// InjectAuto.
func (s *Service) maybeNudgePromise(ctx context.Context, chatID ChatID, runPrompt, finalText string) {
	if s.postrun.Followups == nil || !s.postrun.PromiseNudge {
		return
	}
	if strings.Contains(runPrompt, promiseNudgeMarker) {
		return // the nudge itself may not chain
	}
	matched := promiseRe.FindString(finalText)
	if matched == "" {
		return
	}
	s.log.Info("final answer promised an unprompted return; nudging", "chat_id", chatID, "matched", matched)
	s.InjectAuto(ctx, chatID, fmt.Sprintf(
		promiseNudgeMarker+"\n\n"+
			"Your previous reply promised to come back on your own (matched: %q). Nothing wakes you "+
			"automatically — that promise strands the user. Do ONE of the following NOW:\n"+
			"1. Complete the promised work in the FOREGROUND and report the real result.\n"+
			"2. If it genuinely must wait, schedule a real follow-up: write followup/<delay>.md "+
			"(e.g. followup/15m.md) whose content is the prompt to run then, and tell the user when "+
			"you will be back.\n"+
			"3. Send a short correction telling the user you will NOT return on your own and what "+
			"they should do instead.\n"+
			"Reply in the user's language.", matched))
}
