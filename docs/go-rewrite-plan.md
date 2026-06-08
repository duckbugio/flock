# flock — Go rewrite plan: Telegram adapter on a transport-agnostic Go core

> **Status:** design / not started. This document is the implementation roadmap for replacing
> the patched Python Telegram bot (`adapters/telegram/`, built on RichardAtCT/claude-code-telegram
> + 15 build-time patches) with a clean Go implementation: a reusable **core** that drives the
> `claude` CLI, and a **Telegram adapter** on top of it.
>
> **How to use this doc:** the rewrite is **self-hosted** — the flock bot implements it by
> running its own pipeline (planner→coder→tester→reviewer→arbiter), **one stage = one PR**. See
> **"Execution model"** below for how to drive it and **"Human-only gates"** for the few points
> that need you. Stages are ordered so each builds on a working previous one.

---

## 0. Context & goal

The current Telegram adapter works but is built by **mutating a third-party Python package at
build time** (15 patches in `adapters/telegram/patches/`). That is fragile (anchors drift on
upstream bumps) and we don't own the architecture — several real warts (see §7) come straight
from the upstream's turn-based design and can't be fully fixed by patching.

We are going multi-platform (Telegram now, Max/others later) and the project's main stack is Go.
The reusable, transport-agnostic part is a **"Claude runner"** — it currently lives fused inside
the Python bot. This rewrite extracts that runner as a clean Go **core**, with Telegram as the
first **adapter** on it. Future platforms become thin adapters on the same core.

**Goal:** feature parity with today's Python adapter (see §6), the warts in §7 fixed, zero
build-time patches, deployable into the existing monorepo + Ansible flow with minimal disruption.

---

## 1. What we're building — and explicitly NOT

**The hard fact:** the Claude **Agent SDK exists only for Python/TypeScript, not Go.** The actual
agent (tools Read/Edit/Bash, subagents, the dev-team) lives in the **`claude` CLI (Node)**.

So this rewrite is **NOT** "reimplement Claude Code in Go" — it is:

> **Go = Telegram transport + per-chat orchestration + lifecycle management of the `claude` CLI
> subprocess**, talking to it over `--output-format stream-json`. The CLI stays the engine; Go
> conducts it.

**In scope (Go):** Telegram I/O, auth/whitelist, per-chat workspaces + concurrency, mention-gate,
edits, voice, the progress UI, session lifecycle, the PR poller, config, packaging.

**Out of scope (stays as-is):** the `claude` CLI itself (Node, shipped in the image); the dev-team
**agents** (`core/agents/*.md`) and **orchestration prompt** (`core/CLAUDE.workspace.md.tmpl`) —
those are file-based and platform-agnostic already; Go just renders them into each workspace.

---

## 2. Architecture

```
                         ┌─────────────────────────────────────────────┐
   Telegram  ◄──────────►│  adapters/telegram (Go binary)              │
                         │  - bot client (receive/send/edit/voice)     │
                         │  - mention-gate, edit handling              │
                         │  - renders core Events → Telegram messages  │
                         └───────────────┬─────────────────────────────┘
                                         │ uses (Go library)
                         ┌───────────────▼─────────────────────────────┐
                         │  core (Go package)                          │
                         │  - Dispatcher: per-chat queue + concurrency │
                         │  - Workspace: /workspace/chat_<id>, renders │
                         │      CLAUDE.md + agents from core/          │
                         │  - Runner: spawns & streams the claude CLI  │
                         │  - Poller: git-host PR comments → Events    │
                         └───────────────┬─────────────────────────────┘
                                         │ exec + stream-json
                         ┌───────────────▼─────────────────────────────┐
                         │  claude CLI (Node, in the image)            │
                         │  reads agents + CLAUDE.md from the workspace│
                         │  does ALL agent work (tools, subagents)     │
                         └─────────────────────────────────────────────┘
```

**Runner model (important decision): one `claude` invocation per user message, babysat to
completion in a per-chat goroutine.**

- Each inbound message → `claude --print --output-format stream-json --input-format stream-json
  --resume <session_id> ...` run in a goroutine that streams events to the adapter until the
  terminal `result` event — however long that takes (minutes is fine).
- Continuity comes from `--resume <session_id>` (we persist the session id per chat), **not** from
  keeping a long-lived process. This is simpler and **fixes the "background work dies" wart**: the
  long task IS the subprocess; Go waits on it to the end and only then is the "turn" done. Nothing
  is backgrounded inside the agent, so nothing is lost.
- (A long-lived streaming-input process per chat is a possible *later* optimization for latency;
  not needed for parity. Do not start there.)

**Proposed Go module layout** (single module, monorepo-friendly):

```
core/                      # package: github.com/duckbugio/flock/core  (Go lib)
  claude/                  #   Runner: exec the CLI, parse stream-json
  workspace/               #   per-chat dirs, render CLAUDE.md + agents
  dispatch/                #   per-chat queue + global concurrency cap
  poller/                  #   git-host PR-comment poller (emits Events)
  go.mod                   #   NOTE: the single go.mod is at the REPO ROOT (decided)
adapters/
  telegram/                # NOTE: today this dir holds the PYTHON adapter.
    ...                    #   The Go adapter will replace it (see Stage 8 migration).
    cmd/flock-telegram/     #   main package (the binary)
    internal/...           #   telegram-specific glue
```

> **Decided:** `core/` keeps its `agents/` + `CLAUDE.workspace.md.tmpl` (data) and **gains Go
> packages under `core/<pkg>/`** beside them — the image bakes the data, the binary imports the
> code. **One Go module at the repo root** (`module github.com/duckbugio/flock`).

---

## 3. Tech choices (recommended; lock in Stage 0)

| Concern        | Choice | Rationale |
|----------------|--------|-----------|
| Telegram lib   | `github.com/go-telegram/bot` | maintained, no heavy deps, context-first, long-poll + webhook. Alt: `github.com/mymmrac/telego`. |
| CLI subprocess | stdlib `os/exec` + `bufio.Scanner` over stdout (JSONL) | no dep needed |
| JSON           | stdlib `encoding/json` (consider `json.Decoder` streaming) | event stream is JSONL |
| Config (env)   | `github.com/caarlos0/env/v11` (env → struct) | reuse existing env var names (§5) |
| Logging        | stdlib `log/slog` (JSON handler) | structured, no dep |
| Concurrency    | stdlib goroutines/channels + `golang.org/x/sync/semaphore` | per-chat goroutine, global cap |
| Voice/HTTP     | stdlib `net/http` | Mistral/OpenAI are simple HTTP POSTs |
| Tests          | stdlib `testing` + a fake-CLI script for the Runner | deterministic |

Keep dependencies minimal — this is a thin orchestrator.

---

## 4. The `claude` CLI contract (the crux — get this right first)

Everything depends on driving the CLI correctly. **Stage 1's first task is to pin the CLI version
and capture REAL event samples** — do not trust this section blindly; verify against the installed
CLI (`claude --help`, and run a sample piping stdout to a file).

**Invocation (headless, streaming):**
```
claude --print \
       --output-format stream-json \
       --input-format stream-json \
       --verbose \
       --model "$CLAUDE_MODEL" \
       --max-turns "$CLAUDE_MAX_TURNS" \
       --permission-mode bypassPermissions \   # sandboxed container, full auto
       [--resume "$SESSION_ID"] \              # omit for a new session
       --add-dir /workspace/chat_<id>           # working dir / allowed dir
# prompt: either as a trailing arg, or written as a stream-json user message to stdin
```
- Auth via env passed to the child: `CLAUDE_CODE_OAUTH_TOKEN` **or** `ANTHROPIC_API_KEY`.
- Run with `cwd = /workspace/chat_<id>` so the agent reads that workspace's `CLAUDE.md` + `.claude/agents/`.
- **Verify the exact flag names/values for the pinned version** (they evolve; e.g. permission-mode
  values, `--print` vs interactive, whether prompt goes as arg or stdin message).

**stream-json events to parse** (line-delimited JSON on stdout; shapes to confirm in Stage 1):
- `{"type":"system","subtype":"init","session_id":"...","tools":[...],...}` → capture `session_id`.
- `{"type":"assistant","message":{"content":[{"type":"text","text":"..."}|{"type":"tool_use","name":"Bash","input":{...}}]}}`
  → text deltas/blocks drive the reply; `tool_use` blocks drive the progress indicator ("🔧 Bash").
- `{"type":"user","message":{"content":[{"type":"tool_result",...}]}}` → tool finished (optional UI).
- `{"type":"result","subtype":"success"|"error_max_turns"|...,"result":"<final text>","session_id":"...","num_turns":N,"total_cost_usd":X,"duration_ms":...}`
  → terminal event: final answer + session id + cost/turns.

**Session continuity:** persist `session_id` per chat (sqlite or a file in the chat workspace);
pass `--resume <session_id>` on the next message. A timeout/cancel must **keep** the stored
session id (do NOT discard it) — that is the §7 timeout fix, for free in this model.

**Cost/turns:** read from the `result` event; enforce caps in Go (compare to `CLAUDE_MAX_COST_*`).

---

## 5. Config (env) — reuse the existing names for deploy parity

The Ansible role (`adapters/telegram/deploy/`) renders an `.env`. **Keep the same variable names**
so the deploy/`env.j2` barely changes. Map env → a Go `Config` struct (caarlos0/env):

- Required: `TELEGRAM_BOT_TOKEN`, `TELEGRAM_BOT_USERNAME`, `ALLOWED_USERS` (csv int64),
  `CLAUDE_CODE_OAUTH_TOKEN` **or** `ANTHROPIC_API_KEY`.
- Claude: `CLAUDE_MODEL`, `CLAUDE_MAX_TURNS`, `CLAUDE_TIMEOUT_SECONDS`, `CLAUDE_MAX_COST_PER_USER`,
  `CLAUDE_MAX_COST_PER_REQUEST`, `APPROVED_DIRECTORY` (=/workspace).
- Concurrency: `MAX_CONCURRENT_CHAT_RUNS`.
- Team loop: `PRE_PR_CYCLES`, `PR_REVIEW_CYCLES` (rendered into CLAUDE.md, as today).
- Behavior flags: `REQUIRE_GROUP_MENTION` (false=answer all group msgs; true=mention-only) and
  `ENABLE_PR_REVIEW` (false=build+open PR then stop; true=poller + on-PR Phase-2 review). The Go
  adapter must honor both: gate the group mention-handler and the poller/Phase-2 on them.
- Git: `GIT_HOST`, `GIT_SCHEME`, `GIT_USER`, `GIT_TOKEN`, `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`,
  `GITEA_API_URL`, `GITEA_POLL_INTERVAL`.
- Voice: `ENABLE_VOICE_MESSAGES`, `VOICE_PROVIDER`, `MISTRAL_API_KEY`/`OPENAI_API_KEY`.
- Misc: `LOG_LEVEL`, `RATE_LIMIT_REQUESTS`, `RATE_LIMIT_WINDOW`, `SESSION_TIMEOUT_HOURS`.

---

## 6. Feature-parity checklist (vs the current Python adapter)

- [ ] Long-poll receive; send replies; **live "Working… (Ns)" progress** with tool activity + Stop.
- [ ] Auth whitelist (`ALLOWED_USERS`); friendly "not allowed" + welcome on first allowed contact.
- [ ] **Per-chat workspace** `/workspace/chat_<id>` (1:1 and group); render `CLAUDE.md` + agents in it.
- [ ] **Parallel chats** (goroutine per chat) capped by `MAX_CONCURRENT_CHAT_RUNS`; **serial within a chat**.
- [ ] **Group mention-gate:** respond only when `@<bot>` mentioned or the message replies to the bot.
- [ ] **Edited messages:** an edit supersedes the in-flight/queued run for that message (deletes ignored).
- [ ] **Voice:** transcribe (`mistral` | `openai` | `local`) → treat transcript as the message.
- [ ] **Session per chat** with resume; survives restarts (persist session_id).
- [ ] **Git:** credential helper from `GIT_USER`/`GIT_TOKEN`; git identity; clean clone URLs.
- [ ] **PR poller:** poll `GITEA_API_URL/notifications` for new PR comments on `duck/<chatid>/…`
      branches → route back to the originating chat (parse chat id from branch).
- [ ] Cost/turn caps; rate limiting; structured logs.

---

## 7. Warts to fix (that patching couldn't)

1. **"Background work dies on turn end."** Upstream disconnects the client after each message, so a
   backgrounded job is killed. → **Fixed by the Runner model (§2):** the long task is the babysat
   subprocess; Go streams progress until it actually finishes. No "turn" ends early.
2. **Frozen "Working… (Ns)" counter** during a long silent tool call. → Drive the progress message
   from a **wall-clock ticker** in Go (independent of stream events), so it always advances; a
   frozen counter then genuinely means a hang.
3. **Session discarded on timeout.** → On timeout/cancel, **keep** the persisted `session_id`; the
   next message `--resume`s it. Timeout is a deadline on delivery, not a reason to forget.
4. **Per-chat concurrency via a bolted-on queue.** → Native: one goroutine + buffered channel per
   chat (serial), a global `semaphore` (parallel cap). Edits supersede via a per-chat "current
   message id" check.

---

## Execution model — the bot builds its own successor (self-hosting, max autonomy)

The rewrite is implemented **by the flock bot itself**, running the pipeline this repo ships
(`core/CLAUDE.workspace.md.tmpl` + `core/agents/*.md`). You don't hand stages to a generic
assistant — you hand them to the **dev team**. **One stage = one feature = one PR**, through the
full cycle `planner → coder ⇄ tester → pre-PR review → PR → reviewer ⇄ coder → arbiter`:

1. **planner** — formalizes the stage's pre-written **Goal/Scope/Acceptance** into a spec (AC IDs +
   REUSE map). **Scope is already approved by this doc, so the usual "confirm scope & wait" step is
   skipped** — the team builds straight away.
2. **coder** — implements on `duck/<chatid>/go-stage-<N>-<slug>`.
3. **tester** — runs the stage's machine-checkable Acceptance incl. regression; loops `coder⇄tester`
   up to `PRE_PR_CYCLES` until green.
4. **pre-PR reviewer** — correctness/security **+ the 4 design lenses** (reuse/simplify/efficiency/
   altitude) on the local diff.
5. **PR** — opened; **reviewer ⇄ coder** loop up to `PR_REVIEW_CYCLES` (line-anchored inline comments).
6. **arbiter** — APPROVE (ready to merge) or ESCALATE (needs you); records learnings to
   `./.team/memory.md` so later stages compound.

**Drive it:** in the chat — *"Implement Stage N from `docs/go-rewrite-plan.md` (read §1–§7 +
Execution model + Human-only gates first)."* The team runs the whole cycle and stops at a PR for
you to merge, or at a Human-only gate. Then *"Stage N+1."*

**Acceptance is machine-checkable** so the **tester/arbiter verify it without you** — every stage is
expressed as commands the team runs itself: `go build/vet ./...`, `golangci-lint run`,
`go test ./... -race`, the fake-CLI Runner test, `ansible-playbook --syntax-check`,
`docker build --check`. Only **live Telegram/Claude behavior** (Stages 2/4/6/7/8) needs your glance.

**Go toolchain:** the team builds/tests Go via the **dind sidecar it already has** —
`docker run --rm -v "$PWD":/w -w /w golang:1.x go build ./...` — so **no change to the current bot
image** is needed to self-host the rewrite. (Or add Go to the adapter image; dind is simpler.)

**Decisions are pre-baked — the bot does NOT stop to ask.** Resolved: one Go module at the repo
root; Go packages under `core/<pkg>/` beside the data; image stays
`ghcr.io/duckbugio/flock-telegram`; runner = one-shot-per-message (§2). On a genuinely undecided
fork the bot **ESCALATEs** instead of guessing — the only pause outside a Human-only gate.

## Human-only gates (the minimal set the bot cannot do alone)

Everything else is autonomous. A human is needed only here:

1. **Secrets/tokens** — a **throwaway test bot** token + Claude auth + git token for dev. The bot
   can't mint these.
2. **Stage 1 CLI spike** — you confirm the captured stream-json `testdata` looks right (it's the
   foundation the parser locks onto).
3. **Merge each stage's PR** — per the repo's own rule, arbiter approves, **the human merges**.
   (Relax to auto-merge if you want full hands-off; default = you click.)
4. **Live behavior checks** — "message the bot, see X in Telegram" acceptance (Stages 2/4/6/7); you
   eyeball — the bot tells you exactly what to expect.
5. **Prod cutover & make-public** (Stage 8) — pointing prod at the new image / package visibility,
   authorized explicitly as with any deploy.

Outside these five, the team plans → codes → tests → reviews → arbiter-approves every stage itself,
opening a PR you can read.

---

## 8. Staged implementation plan

Each stage: **Goal → Scope → Key packages/files → Interfaces → Acceptance.** Implement in order.

### Stage 0 — Scaffold & decisions
- **Goal:** a building Go project that connects to Telegram and replies "pong".
- **Scope:** `go.mod` at the **repo root** (`module github.com/duckbugio/flock` — decided);
  pick + pin deps (§3); `cmd/flock-telegram/main.go`;
  `slog` logging; `caarlos0/env` `Config`; a `Makefile`/`Taskfile` target to build+lint (golangci-lint).
- **Acceptance:** `go build ./...` clean; `golangci-lint run` clean; bot started with a token
  replies "pong" to `/ping` from an allowed user. CI builds the Go binary.

### Stage 1 — `core/claude` Runner (the foundation)
- **Goal:** run a prompt through the `claude` CLI and return a typed event stream.
- **First task (do before coding):** pin the CLI version; run a sample with `--output-format
  stream-json` and **save real event JSON** to `core/claude/testdata/`. Lock the schema to that.
- **Scope:** spawn the CLI (`os/exec`, set `cwd`, env, args per §4); scan stdout JSONL; decode into
  `Event`s; capture `session_id`; emit terminal `RunResult`; handle stderr + non-zero exit;
  context cancellation kills the process group.
- **Interfaces:**
  ```go
  package claude
  type EventType int
  const ( SystemInit EventType = iota; Text; ToolUse; ToolResult; Result; RunError )
  type Event struct {
      Type EventType; Text, Tool string; ToolInput json.RawMessage
      SessionID string; Result *RunResult; Err error
  }
  type RunResult struct { Text, SessionID string; NumTurns int; CostUSD float64; DurationMS int64; IsError bool; Subtype string }
  type Options struct { SessionID, Workdir, Model string; MaxTurns int; Env []string /* auth, etc. */ }
  type Runner interface { Run(ctx context.Context, prompt string, o Options) (<-chan Event, error) }
  func New(claudeBin string) Runner
  ```
- **Acceptance:** a unit test using a **fake `claude` script** (emits canned JSONL) asserts the
  event sequence + parsed `RunResult`; an integration test (build-tagged) runs the real CLI on
  "say hi" and gets a `Result`. Cancelling the context kills the child within ~1s.

### Stage 2 — Telegram MVP (single chat, serial)
- **Goal:** message the bot (DM, allowed user) → Claude answers, with a live progress message.
- **Scope:** receive text; auth check; create/resolve the chat workspace (hardcode one for now);
  call `Runner.Run`; render the event stream to Telegram: one editable "Working… (Ns)" message
  (ticked by a wall-clock `time.Ticker`, §7.2) showing the last few tool/text lines + a Stop
  inline button; on `Result`, replace with the final text (chunk if >4096 chars); Stop cancels the
  context.
- **Acceptance:** a real DM produces a streamed-progress reply then the final answer; the counter
  advances even during a long `Bash`; Stop aborts.

### Stage 3 — Workspaces, isolation & concurrency
- **Goal:** per-chat isolated workspaces; parallel chats; serial within a chat.
- **Scope:**
  - `core/workspace`: `Ensure(chatID) (path string, err error)` → `/workspace/chat_<id>`; render
    `CLAUDE.md` from `core/CLAUDE.workspace.md.tmpl` (env subst `PRE_PR_CYCLES`,`PR_REVIEW_CYCLES`)
    and copy `core/agents/*.md` → `<ws>/.claude/agents/` (replaces the Python entrypoint's job).
  - `core/dispatch`: `Submit(chatID string, job func(ctx) )`; one goroutine + buffered chan per
    chat (serial); a global `semaphore.Weighted(MAX_CONCURRENT_CHAT_RUNS)` gating job starts;
    `Supersede(chatID, msgID)` and `Cancel(chatID)` hooks for Stage 4/Stop.
- **Interfaces:**
  ```go
  package dispatch
  type Dispatcher struct { /* ... */ }
  func New(maxConcurrent int) *Dispatcher
  func (d *Dispatcher) Submit(chatID string, run func(ctx context.Context)) // serial per chat, capped globally
  func (d *Dispatcher) Cancel(chatID string)                                // stop current run in chat
  ```
- **Acceptance:** two chats run concurrently (observable interleaved progress); a second message in
  the *same* chat waits for the first; workspaces don't see each other's files; `CLAUDE.md` +
  agents present in each workspace.

### Stage 4 — Groups, mention-gate, edits
- **Goal:** correct group behavior + message edits.
- **Scope:** detect group vs DM; in groups, only act when the message **@mentions** the bot's
  username or **replies to** the bot (parse entities); strip the mention before sending to Claude;
  on an **edited** message, if a run for that message id is in-flight/queued, supersede it
  (cancel + re-submit with the new text); ignore deletions.
- **Acceptance:** in a group the bot ignores non-mention chatter and answers on mention/reply;
  editing a just-sent message reruns with the new text; DMs unaffected.

### Stage 5 — Session lifecycle done right
- **Goal:** durable sessions + the timeout/background fixes (§7.1, §7.3).
- **Scope:** persist `chatID → session_id` (sqlite via `modernc.org/sqlite` (pure-Go, no cgo), or a
  JSON file per workspace); pass `--resume` on subsequent messages; apply `CLAUDE_TIMEOUT_SECONDS`
  as a context deadline that **cancels delivery but preserves the stored session_id**; on the next
  message, resume seamlessly; survive process restart (reload the map).
- **Acceptance:** kill+restart the bot mid-conversation → next message still has context; a run that
  exceeds the timeout doesn't wipe the session (next message resumes); a multi-minute task delivers
  its result without the user re-poking.

### Stage 6 — Git + dev-team + PR poller
- **Goal:** the team can clone/branch/PR, and PR comments come back to the right chat.
- **Scope:** write the git credential helper (emit `GIT_USER`/`GIT_TOKEN`) + global git config
  (identity, `credential.<host>.helper`) at startup (replaces the Python entrypoint); `core/poller`
  polls `GITEA_API_URL/notifications` (unread, subject-type Pull) every `GITEA_POLL_INTERVAL`; for
  `duck/<chatid>/<slug>` branches with new non-self comments on **open** PRs, emit an Event the
  adapter routes to chat `<chatid>` (parse the id from the branch). Reuse the logic in the current
  `patches/gitea_poller.py` as the spec.
- **Interfaces:**
  ```go
  package poller
  type PRComment struct { ChatID, Repo string; PRIndex int; Body, Author string }
  func Run(ctx context.Context, cfg Config, out chan<- PRComment) // ticks; dedups; only open PRs, non-self
  ```
- **Acceptance:** "build feature X" opens a PR per repo on `duck/<chatid>/…`; a human comment on the
  PR is picked up within one poll interval and addressed, with the reply routed to the right chat.

### Stage 7 — Voice
- **Goal:** voice messages handled like text.
- **Scope:** download the voice file (Telegram getFile); transcribe via provider
  (`mistral` Voxtral / `openai` Whisper HTTP, or `local` whisper.cpp if shipped); feed the
  transcript into the normal pipeline; (re)use `ffmpeg` in the image if format conversion is needed.
- **Acceptance:** a voice note produces a transcript and runs as a command; provider switch via
  `VOICE_PROVIDER`.

### Stage 8 — Packaging, deploy & migration
- **Goal:** a deployable image that fits the monorepo + Ansible flow, and a safe cutover.
- **Scope:**
  - **Dockerfile** (multi-stage): build the Go binary (scratch/distroless final **plus** Node +
    the `claude` CLI + `git`/`ripgrep`/`ffmpeg` it shells out to). Bake `core/agents` +
    `CLAUDE.workspace.md.tmpl`. Non-root `claude` user, same volume layout
    (`claude_home`, `bot_data`, `workspace`) so **existing volumes are reused**.
  - **CI:** the existing `publish-telegram.yml` builds this image — **same name**
    `ghcr.io/duckbugio/flock-telegram` (cut over in place; the Python image stays pullable for
    rollback). Update the Dockerfile path/stages; keep the paths filter.
  - **Ansible:** the role already pulls `bot_image` + renders env — mostly unchanged; verify env
    var parity (§5). Keep the multi-instance `inventories/<name>/` layout.
  - **Migration/cutover:** run the Go image against a **throwaway test bot** first (new token) for a
    full parity pass; then point `gitea-lo-duck`'s `bot_image` at it. Sessions differ in format from
    the Python SDK — accept a **fresh session start** at cutover (document it), or write a one-off
    importer if needed.
- **Acceptance:** `docker build` works; deployed via the existing playbook; a full §6 smoke test
  passes against a test instance before touching prod.

### Stage 9 — Hardening (optional, ongoing)
- Rate limiting (`RATE_LIMIT_*`), cost-cap enforcement from `result.total_cost_usd`, graceful
  shutdown (drain in-flight, persist sessions), `/help`/`/new`/`/stop` commands, metrics/telemetry,
  broader tests (table-driven Runner parsing, dispatcher race tests with `-race`).

---

## 9. Testing strategy

- **Runner:** a **fake `claude`** shell script in `testdata/` that emits recorded JSONL → fully
  deterministic parsing tests; one build-tagged integration test against the real CLI.
- **Dispatcher:** `-race` tests asserting serial-per-chat + global cap + supersede/cancel.
- **Adapter:** a fake Telegram server (or the lib's test hooks) for update→reply flows.
- **End-to-end:** a scripted parity checklist (§6) run against a test bot before any cutover.

---

## 10. Deploy / migration summary

- Same image-pull deploy + same named volumes → **no data migration** for workspaces.
- Sessions reset at cutover (Python SDK session files ≠ CLI `--resume` ids) — document, or import.
- Cut over **one instance** (`gitea-lo-duck`) after a green test-bot pass; the Python image stays
  pullable for instant rollback (`bot_image` back to the old tag).

---

## 11. Risks & open questions

- **CLI schema drift** — pin the `claude` version; re-capture `testdata` on any bump. (Biggest risk.)
- **CLI flag/behavior gaps** — confirm headless permission mode, prompt-via-stdin vs arg, resume
  semantics, and how subagents/`CLAUDE.md` are picked up in `--print` mode (Stage 1 spike).
- **Long-running cost** — a babysat subprocess holds a goroutine + a slot for the whole task;
  ensure the concurrency cap accounts for that (it does — slot held until `Result`).
- **Voice `local`** — only if whisper.cpp is shipped; otherwise mistral/openai only.
- **Module layout** — decided: single module at the repo root (see Execution model). Don't churn it.
- **Auth in CI/headless** — the CLI needs a valid token at runtime (not build); same as today.

---

*Drive it via the bot: "Implement Stage N from docs/go-rewrite-plan.md (read §1–§7 + Execution
model + Human-only gates)." It runs the full pipeline and stops at a PR or a Human-only gate.*
