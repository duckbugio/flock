---
name: verification-before-completion
description: Before telling the user a coding task is done, actually verify it — build, run the relevant tests/linter, and confirm the change does what was asked with no regressions. Use whenever you are about to claim a change is finished, fixed, or working.
---

# Verify before you claim it's done

Saying "done" without checking is the most common way an agent ships a broken
change. Before you report a coding task as finished, fixed, or working, VERIFY
it — do not assume.

## Checklist (do it, don't narrate it)

1. **Build / compile** what you changed. If it doesn't build, it isn't done.
2. **Run the relevant tests** with the project's own command (a `Taskfile`/
   `Makefile` target, `go test`, `npm test`, …). Run the narrowest command that
   covers your change first, then the fuller suite if it's cheap. If the area you
   changed has no test and the change is non-trivial, add one.
3. **Run the linter/formatter** the project uses and fix what it flags.
4. **Re-read the request** and confirm EVERY part is addressed — not just the
   easy part. Check the edge cases and error paths you touched.
5. **Look for regressions** — did the change break a caller, a contract, a test,
   or a neighbouring feature?

## Reporting

- Report what you actually ran and its result ("`go test ./...` green, `task
  lint` 0 issues"), never "should work".
- If something failed, or you could not verify a part, SAY SO plainly — never
  paper over a failure or a skipped step.
- If a test fails, fix the code (or the test, if the test was wrong) before
  claiming done — don't hand back a red build.

A task is "done" only when you have evidence it works, not when the code looks
right.
