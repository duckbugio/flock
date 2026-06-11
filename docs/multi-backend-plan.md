# flock — Multi-backend plan: Codex alongside Claude on a provider-neutral runner

> **Status:** design / not started. This document is the implementation roadmap for letting the bot
> drive **more than one AI backend**. Today the bot speaks to exactly one engine: the `claude` CLI,
> wrapped by `core/claude`, which spawns it, streams its `stream-json` stdout as typed Go events, and
> manages its lifecycle. The orchestration around it (`core/chat.Service`, dispatch, sessions, cost,
> the transport adapters) is already engine-agnostic in spirit — it only *happens* to import
> `core/claude` for the runner interface and event types. This plan lifts that contract into a
> reusable **`core/agent`** and turns each engine (Claude, then **Codex**) into a thin **backend**
> behind one interface, selected per instance by an env var.
>
> **How to use this doc:** like `docs/multi-transport-plan.md`, the work is **self-hosted** — the
> flock bot builds it by running its own pipeline (planner → coder ⇄ tester → pre-PR review → PR),
> **one phase = one PR**. See **"Execution model"** for how to drive it and **"Human-only gates"** for
> the few points that need a human. Phases are ordered so each builds on a working previous one:
> **P0 refactor (no behavior change) → P1 Codex backend.** Do not start P1 before P0 is merged.

---

## 0. Context & goal

`core/claude` (`runner.go`, `decode.go`, `tail.go`) is the only thing in the tree that knows an AI
engine exists. It exposes a clean, already-neutral-in-shape contract:

```go
type Runner interface {
    Run(ctx context.Context, prompt string, o Options) (<-chan Event, error)
}
```

…plus the value types every caller speaks in — `Event`, `EventType` (`SystemInit`/`Text`/`ToolUse`/
`ToolResult`/`Result`/`RunError`), `Options`, `RunResult`, `ImageInput`. These types are imported
across the conversation layer and both adapters: a repo grep counts ~120 `claude.<Symbol>` references
(`Event` ×42, `RunResult` ×18, `ToolUse`/`Result` ×11 each, `ImageInput` ×10, `Options`/`Text` ×8,
`SystemInit`/`Runner` ×7, …). **None of those callers care that the engine is Claude** — they care
about the event stream contract. The contract is the seam.

**Goal:** add **Codex** (OpenAI's `codex` CLI) as a second backend with **maximum reuse and zero
behavior change for Claude**. After P0, a new backend is a single `core/<name>` package implementing
one interface; the binary picks its backend from an env var. The conversation layer, dispatcher,
sessions, cost gate, transports, and progress UI are untouched.

**Non-goal:** running two backends at once in one process, or per-chat/per-message backend switching.
We select **one backend per running instance** via env — exactly as we run one transport per binary.
That keeps blast radius small and matches the existing deploy model (an instance = one container, one
token, one config). Per-chat routing is a possible later extension; it is explicitly out of scope here.

---

## 1. What we're building — and explicitly NOT

**In scope:**
- **P0:** extract the engine-neutral contract from `core/claude` into **`core/agent`** (the `Runner`
  interface + `Event`/`EventType`/`Options`/`RunResult`/`ImageInput`). Re-point every caller at
  `core/agent`. `core/claude` keeps the Claude-specific implementation (arg building, `stream-json`
  decode, lifecycle) and now implements `agent.Runner`. Add a small `agent.New(cfg)` factory that
  selects a backend by env (only `claude` exists after P0). **No user-visible change.**
- **P1:** a **Codex** backend (`core/codex`) implementing `agent.Runner` over `codex exec --json`,
  behind the same interface, selected by `AGENT_BACKEND=codex`.

**Out of scope:** running Claude and Codex simultaneously in one process; per-chat/per-message backend
choice; changing the transport layer (Telegram/VK) or the conversation layer; adding non-CLI backends
(HTTP SDKs). Those can layer on later — the seam does not preclude them.

---

## 2. Architecture

### Today (one backend, contract fused to Claude)

```
transports ─┐
            ├─> core/chat.Service ──> core/claude.Runner ──spawns──> `claude` CLI
adapters ───┘        (imports claude.Event/Options/RunResult/ImageInput)
```

`core/chat.Service`, both adapters, and both `cmd/*/main.go` import `core/claude` purely for the
runner interface and event/value types. `cmd/*/main.go` constructs it with `claude.New(cfg.ClaudeBin)`
and feeds the child its auth via `CLAUDE_CODE_OAUTH_TOKEN` in the per-run env.

### After P0 (neutral core/agent + backend implementations)

```
transports ─┐
            ├─> core/chat.Service ──> core/agent.Runner  (interface + Event/Options/RunResult/ImageInput)
adapters ───┘                              ▲
                                           ├── core/claude   (implements agent.Runner; `claude` CLI)
                                           └── core/codex     (implements agent.Runner; `codex` CLI)  [P1]

cmd/*/main.go: runner, err := agent.New(agentCfg)   // agentCfg built from internal/config; selects claude|codex
```

The conversation layer speaks only `core/agent`. Each backend is a leaf package that knows how to
spawn its CLI, translate its stream into `agent.Event`, and map session/resume semantics. The factory
is the only place that knows which backends exist. `agentCfg` is the neutral `agent.Config` (§4) that
each `cmd/*/main.go` builds from `internal/config` — so `core/agent` stays free of any `internal/config`
import (the dependency points app → core, never the reverse).

### Go module layout (single module at repo root, unchanged)

```
core/
  agent/        NEW — Runner interface, Event/EventType, Options, RunResult, ImageInput, New() factory
  claude/       Claude backend: runner.go (buildArgs), decode.go (stream-json), tail.go — implements agent.Runner
  codex/        NEW (P1) — Codex backend: runner.go (codex exec args), decode.go (JSONL), tail reuse — implements agent.Runner
  chat/ dispatch/ session/ cost/ …   unchanged (now import core/agent)
internal/config/   gains AGENT_BACKEND + CODEX_* (see §5)
cmd/flock-telegram, cmd/duck-vk   build agent.Config from cfg, call agent.New(...) instead of claude.New(...)
```

`core/claude` and `core/codex` may share small helpers (the `tail` stderr buffer, a process-group
kill, the scanner buffer sizing). P0 can lift those into `core/agent` (or an internal helper) only if
the share is clean; otherwise leave `tail` where it is and let `core/codex` reuse it by import. Decide
at P0 — do not force a shared base.

---

## 3. Tech choices (recommended; lock at P0 / P1 start)

| Concern | Choice | Rationale |
|---------|--------|-----------|
| Neutral package | `core/agent` with interface `Runner` (name kept) | smallest move; `claude.Event`→`agent.Event` is pure type-churn the suite covers |
| Backend selection | env `AGENT_BACKEND` (`claude`\|`codex`, default `claude`), via `agent.New(agent.Config)` factory fed a neutral config built by `cmd/*` from `internal/config` | orthogonal to transport; one backend per instance; no new binaries; `core/agent` stays free of `internal/config` |
| Claude backend | `core/claude` unchanged internally, now implements `agent.Runner` | zero behavior change |
| Codex backend | `core/codex` over `codex exec --json` (JSONL events) | the maintained OpenAI CLI; non-interactive exec + JSONL + resume are documented |
| Codex final answer | parse the `item`/`agent_message` event; OR `-o <file>` (`--output-last-message`) as a fallback | robust to JSONL schema drift |
| Codex session/resume | session id from `thread.started.thread_id`; resume via the `codex exec resume <id>` **subcommand** | differs from Claude's `--resume <id>` flag — handled per-backend in buildArgs |
| Codex auth | `OPENAI_API_KEY` (or `CODEX_API_KEY`) in the per-run env | mirrors how Claude gets `CLAUDE_CODE_OAUTH_TOKEN` |
| Everything else | unchanged (`os/exec`, process-group kill, `bufio.Scanner`, `encoding/json`) | the Claude runner's lifecycle pattern is backend-agnostic and is reused verbatim |

Keep new deps minimal: **no new Go modules** — Codex is another `os/exec` + JSON-decode backend, like
Claude.

---

## 4. The `agent.Runner` contract (the crux — get this right in P0)

The neutral contract is the **lift of today's `core/claude` types**, moved to `core/agent` with no
shape change. Both backends implement it; every caller depends only on it.

```go
package agent

// EventType discriminates the kinds of Event a backend emits on a run's channel.
type EventType int
const (
    SystemInit EventType = iota // once per run; SessionID set (Claude system/init; Codex thread.started)
    Text                        // an assistant text chunk
    ToolUse                     // a tool/command invocation; Tool + ToolInput set
    ToolResult                  // a tool finished
    Result                      // terminal success; Result set
    RunError                    // process failed without a result; Err set
)

// Event is one typed item from a run's stream. Only fields relevant to Type are set.
type Event struct {
    Type      EventType
    Text      string
    Tool      string
    ToolInput json.RawMessage
    SessionID string
    Result    *RunResult
    Err       error
}

// RunResult is the terminal result of a run.
type RunResult struct {
    Text       string
    SessionID  string
    CostUSD    float64 // Claude: from result envelope. Codex: best-effort from token usage, else 0 (see §6).
    IsError    bool
    // NumTurns/DurationMS/Subtype remain for Claude; a backend that lacks them leaves them zero.
    // Only CostUSD is read by the neutral layer (the cost gate, core/chat.Service); the other three
    // are populated by core/claude's decode but read nowhere outside it, so Codex may leave them zero
    // with no behavior change.
    NumTurns   int
    DurationMS int64
    Subtype    string
}

// ImageInput is one inbound image attachment for a run.
type ImageInput struct {
    MediaType string
    Data      []byte
}

// Options configures a single run. Backends use the subset they support.
type Options struct {
    SessionID string      // resume an existing session (Claude --resume; Codex `exec resume <id>`)
    Workdir   string      // working/allowed dir (Claude --add-dir + cwd; Codex --cd + --add-dir)
    Model     string      // model id (Claude --model; Codex --model)
    MaxTurns  int         // Claude --max-turns; Codex has no equivalent → ignored
    Env       []string    // child environment (auth etc.)
    Images    []ImageInput // vision; Codex support is an UNKNOWN (see §6) → may be ignored by that backend
}

// Runner spawns a backend for one prompt and streams typed events. The channel closes
// when the run terminates (result, error, or ctx cancellation). The error return is
// non-nil only when the process could not be started.
type Runner interface {
    Run(ctx context.Context, prompt string, o Options) (<-chan Event, error)
}

// Config is the NEUTRAL construction-time input to the factory. It carries only
// backend-selection + per-backend primitives (CLI path, model, turn cap) — never
// the app's own config type. `cmd/*` maps internal/config → agent.Config; this
// keeps the dependency arrow pointing app → core only. core/agent MUST NOT import
// internal/config (today no core/* package does — preserve that). Per-run auth and
// env stay in Options.Env, so adding a backend never widens this struct's surface.
type Config struct {
    Backend  string // AGENT_BACKEND: "claude" (default) | "codex"
    Bin      string // backend CLI path (CLAUDE_BIN / CODEX_BIN)
    Model    string // model id (CLAUDE_MODEL / CODEX_MODEL)
    MaxTurns int    // turn cap; backends without one ignore it (Codex)
}

// New selects and constructs the backend named by cfg.Backend. Unknown name → error.
// It is the single place that knows which backends exist; adding one = one case here
// + one leaf package, with no change to callers or to this signature.
func New(cfg Config) (Runner, error)
```

> **Layering rule (enforced in P0):** the factory takes the neutral `agent.Config`
> above, constructed by each `cmd/*/main.go` from `internal/config`. `core/agent`
> and the backend leaves never import `internal/config` — the dependency stays
> app → core, exactly as `claude.New(cfg.ClaudeBin)` works today. This is what keeps
> the seam scalable to N backends without a config import cycle.

**Event mapping (the per-backend translation each `decode` owns):**

| Neutral event | Claude `stream-json` | Codex `--json` JSONL |
|---------------|----------------------|----------------------|
| `SystemInit` (SessionID) | `system`/`init` envelope | `thread.started` (`thread_id`) |
| `Text` | assistant `text` content block | `item.*` / `agent_message` |
| `ToolUse` | assistant `tool_use` block | `item.*` `command_execution` / tool call |
| `ToolResult` | `user`/`tool_result` envelope | `item.completed` for a tool item |
| `Result` (RunResult) | `result` envelope (`total_cost_usd`, `num_turns`, …) | `turn.completed` (`usage.{input,output}_tokens`) + final `agent_message` |
| `RunError` | non-zero exit / no result | `turn.failed` / `error` event / non-zero exit |

The lifecycle machinery in `core/claude/runner.go` — process-group spawn, ctx-cancellation kill
(SIGTERM→grace→SIGKILL), stderr tail for error enrichment, the 16 MiB scanner buffer, one-shot
per-message invocation — is **backend-agnostic and reused** by `core/codex`. What differs per
backend is **`buildArgs`, `decode`, and prompt/input delivery**: Claude's image path swaps the
trailing-prompt arg for a `stream-json` user message on stdin (`userMessageEnvelope`/`stdinMode`),
which is Claude-specific; Codex delivers the prompt as a trailing arg (or `codex exec -` on stdin)
and, per §6, ignores `Options.Images` until vision is verified. So the reusable core is the
process lifecycle; the per-backend surface is args + decode + how the prompt enters the child.

---

## 5. Config (env)

Shared, read by every binary regardless of transport:

- `AGENT_BACKEND` — `claude` (default) | `codex`. Selects the backend.
- Claude (unchanged): `CLAUDE_BIN` (default `claude`), `CLAUDE_MODEL`, `CLAUDE_MAX_TURNS`,
  `CLAUDE_CODE_OAUTH_TOKEN` / `ANTHROPIC_API_KEY`, `CLAUDE_TIMEOUT_SECONDS`.
- Codex (P1, new): `CODEX_BIN` (default `codex`), `CODEX_MODEL`, `OPENAI_API_KEY` (or `CODEX_API_KEY`),
  `CODEX_SANDBOX` (default the bot's autonomous mode — see §6), optional `CODEX_TIMEOUT_SECONDS`.
- Backend-neutral: `CLAUDE_MAX_COST_PER_USER` gates the cost cap today. Keep the env name for deploy
  parity but treat it as the neutral per-user cap; Codex feeds it a best-effort cost (§6). A
  `MAX_COST_PER_USER` alias may be added; do not rename the existing var.

> **Decided:** backend is one env var per instance; `internal/config` parses the active backend's vars
> and validates them per binary (mirror the existing `ValidateTelegram` / `ValidateVK` pattern with a
> `ValidateBackend`). `CLAUDE_MAX_TURNS` stays Claude-only; Codex ignores it.

> **Timeout is NOT part of `agent.Config`.** The per-run delivery deadline already lives in the neutral
> conversation layer — `core/chat.Service.Timeout` (set once at startup from `cfg.ClaudeTimeout()` in
> each `cmd/*/main.go`, applied as a `context.WithTimeout` around a run). It is backend-agnostic today.
> So `CODEX_TIMEOUT_SECONDS` is just an alternate source for that same `Service.Timeout` value, selected
> by the active backend in `cmd/*` — it does **not** thread through the factory or the runner. Keep
> `CLAUDE_TIMEOUT_SECONDS` as-is; P1 only adds the cmd-level "pick the active backend's timeout var".

---

## 6. Codex feasibility table (verify the UNKNOWNs at P1 kickoff)

The `codex exec` surface below is from the OpenAI Codex CLI docs; **verify each ⚠️ against the actual
installed `codex` version with a real key** at the first task of P1 (a Human-only gate — the key), and
record the real answers in `.team/memory.md`. Every ⚠️ has a fallback so the backend ships degraded
rather than blocked.

| Capability | Claude (today, known) | Codex (P1 — verify) |
|------------|-----------------------|----------------------|
| Non-interactive run | `claude --print --output-format stream-json --verbose` | `codex exec "<prompt>"` (final msg to stdout) ✓ |
| Streaming events | `stream-json` stdout | `codex exec --json` (a.k.a. `--experimental-json`) → JSONL: `thread.started`, `turn.started/completed/failed`, `item.started/completed`, `error` ✓ — ⚠️ confirm exact field names/subtypes |
| Final answer | `result` envelope `.text` | `agent_message` item, or `-o/--output-last-message <file>` ⚠️ pick the robust source |
| Session id + resume | `--resume <id>`; id from init | `thread.started.thread_id`; resume via **subcommand** `codex exec resume <id> "<prompt>"` ⚠️ confirm |
| Model select | `--model` | `--model, -m` ✓ |
| Working dir | `--add-dir <dir>` + cwd | `--cd, -C <dir>` + `--add-dir <dir>` ✓ |
| Autonomous (no prompts) | `--permission-mode bypassPermissions` | `--dangerously-bypass-approvals-and-sandbox` (`--yolo`) OR `--sandbox danger-full-access --ask-for-approval never`; plus `--skip-git-repo-check` (cwd may not be a git repo) ⚠️ confirm the right combo for the container |
| Auth (headless) | `CLAUDE_CODE_OAUTH_TOKEN` env | `OPENAI_API_KEY` / `CODEX_API_KEY` env (vs `codex login`) ⚠️ confirm which works headless in the image |
| Vision / inbound images | stdin `stream-json` user message w/ base64 image blocks | ⚠️ **UNKNOWN** for `codex exec` → **fallback: Codex backend ignores `Options.Images`** and answers text-only (a one-line notice that images were dropped); revisit if exec supports image input |
| Cost accounting | `result.total_cost_usd` (USD) | `turn.completed.usage.{input,output}_tokens` (tokens, **no USD**) → **fallback: compute best-effort USD from a configured per-token price, else leave `CostUSD=0`** (cost cap effectively off for Codex until priced) ⚠️ |
| Prompt delivery | trailing arg / stdin stream-json | trailing arg; `codex exec -` reads prompt from stdin ✓ |

**Rule:** every ⚠️ is resolved at P1's first task against the live CLI, recorded in `.team/memory.md`,
and wired to a fallback so the Codex backend ships degraded rather than blocked. If a core primitive
turns out missing in a way no fallback covers, **ESCALATE** rather than guess.

---

## Execution model — the bot builds its own backends (self-hosting)

Same model as `docs/multi-transport-plan.md`: the bot implements each phase by running its own
pipeline (`planner → coder ⇄ tester → pre-PR review → PR`), **one phase = one PR**, scope pre-approved
by THIS doc so the team skips "confirm scope & wait."

**Drive it:** *"Implement P0 from `docs/multi-backend-plan.md`."* then *"Implement P1."*

**Acceptance is machine-checkable** via the `dev-tools` image: `gofmt -l .`, `go build ./...`,
`go vet ./...`, `golangci-lint run`, `go test -race ./...`. **P0's headline acceptance is "the entire
existing suite passes unchanged"** — a pure refactor proven by a green regression run. **Codex's live
behavior** (P1) needs a glance against a real key — the team says exactly what to expect.

**Decisions are pre-baked** (neutral `core/agent`; env-selected backend; one backend per instance;
Claude unchanged; Codex over `codex exec --json`). On a genuinely undecided fork (e.g. the Codex JSONL
schema differs from docs in a way the fallback doesn't cover, or headless auth is impossible) the team
**ESCALATEs** instead of guessing.

## Human-only gates (the minimal set the bot cannot do alone)

1. **A Codex API key** (`OPENAI_API_KEY`) + a `codex` binary in the build image for P1's live
   verification. The bot can't mint these.
2. **Confirm the Codex `exec --json` surface** (event schema, resume subcommand, headless auth,
   autonomous sandbox flags) at P1 task 1 — this is the P1 go/no-go for the assumed mapping.
3. **Merge each phase's PR** — the team leaves it green; a human merges.
4. **Live behavior check** — "set `AGENT_BACKEND=codex`, message the bot, see streamed progress then a
   final answer" — a human eyeballs; the bot says what to expect.

## Stacked merge order

P0 → P1 (each base = the previous merged). **Merge P0 first** (the shared contract Codex depends on),
then P1.

---

## 7. Phased implementation plan

Each phase: **Goal → Scope → Key files → Interfaces → Acceptance.** Implement in order.

### P0 — Lift the runner contract into `core/agent` (refactor, NO behavior change)

- **Goal:** the engine-neutral `Runner` interface + event/value types live in `core/agent`; every
  caller imports `core/agent`; `core/claude` implements it; a factory selects the backend. **Claude
  behavior is byte-for-byte unchanged** (it is still the only backend).
- **Complexity:** **risky** but low-uncertainty — a broad mechanical move (~120 `claude.X`→`agent.X`
  references) with a strong oracle (the existing suite).
- **Scope:**
  - Create `core/agent/agent.go`: move `Runner`, `Event`, `EventType` (+ the six constants),
    `Options`, `RunResult`, `ImageInput` out of `core/claude` into package `agent` (§4).
  - `core/claude`: keep `runner`/`buildArgs`/`decode`/`tail`/`userMessageEnvelope`; change it to
    emit `agent.Event` / accept `agent.Options` / return `agent.RunResult`. `claude.New(bin)` returns
    an `agent.Runner`. Decide whether the reusable lifecycle helpers (`tail`, `killGroup`, scanner
    sizing) move to `core/agent` for P1 reuse or stay in `core/claude` (import-reuse) — pick the
    lower-coupling option; do not force a shared base.
  - Add `core/agent/factory.go`: the neutral `Config` struct (§4) + `New(cfg Config) (Runner, error)`
    switching on `cfg.Backend` (only `claude` wired now; unknown → error). `core/agent` must NOT
    import `internal/config` (no `core/*` package does today — keep it that way).
  - Re-point all importers (`core/chat/{service,media,progress}.go`, the VK adapter's receiver — the
    only adapter that references `claude.*` today, via `[]claude.ImageInput`; telegram has none —
    both `cmd/*/main.go`, and all the `_test.go` that reference `claude.*`) to `core/agent`. The set
    is compiler/grep-driven, not this list — re-point whatever the build flags.
  - `cmd/*/main.go`: build `agent.Config` from `internal/config` (backend/bin/model/turns), replace
    `claude.New(cfg.ClaudeBin)` with `runner, err := agent.New(agentCfg)`, and handle the new error
    return (fatal at startup, like the other constructor failures).
  - `internal/config`: add `AGENT_BACKEND` (default `claude`) + a `ValidateBackend`.
- **Interfaces:** `core/agent.Runner` + the value types (§4); the neutral `agent.Config` +
  `agent.New(Config) (Runner, error)`.
- **Acceptance:**
  - `gofmt -l .` empty; `go build ./...`, `go vet ./...`, `golangci-lint run` clean.
  - **`go test -race ./...` passes with the existing tests essentially unchanged** (only import paths
    + `claude.X`→`agent.X` retypes) — the proof of no behavior change.
  - Protected files (`core/CLAUDE.workspace.md.tmpl`, `core/agents/**`, deploy paths) untouched.
  - `cmd/flock-telegram` + `cmd/duck-vk` still build and behave exactly as before with
    `AGENT_BACKEND` unset/`claude`.

### P1 — Codex backend (`core/codex`)

- **Goal:** with `AGENT_BACKEND=codex`, a message runs through `codex exec --json` and renders the
  same live progress + final answer + Stop + sessions + cost gate, through the **unchanged**
  `core/chat.Service` and the **unchanged** transports.
- **Complexity:** **standard** (the run loop, lifecycle pattern, and contract already exist; the new
  work is one `buildArgs` + one `decode` + the factory wiring).
- **First task (do before coding):** with a real `OPENAI_API_KEY` and a `codex` binary, **verify the
  §6 ⚠️ rows** (JSONL event schema, resume subcommand, headless auth, autonomous sandbox flags, image
  support, cost source). **Record the answers in `.team/memory.md`** and set the backend's fallbacks.
- **Scope:**
  - `core/codex/runner.go`: implement `agent.Runner` reusing the Claude lifecycle pattern (process
    group, ctx-kill, stderr tail, scanner). `buildArgs`: `exec --json -m <model> --cd <workdir>
    --add-dir <workdir> --skip-git-repo-check` + the autonomous sandbox/approval flags + resume as the
    `exec resume <id>` subcommand when `Options.SessionID` is set. Auth via `Options.Env`
    (`OPENAI_API_KEY`).
  - `core/codex/decode.go`: map JSONL (`thread.started`→`SystemInit`, `agent_message`→`Text`,
    `command_execution`/tool items→`ToolUse`/`ToolResult`, `turn.completed`→`Result` with best-effort
    cost, `turn.failed`/`error`→`RunError`).
  - `core/agent/factory.go`: wire `codex` → `codex.New(cfg)`.
  - `internal/config`: add the `CODEX_*` vars (§5) + extend `ValidateBackend` for codex.
  - Honor the §6 fallbacks: images ignored if unsupported (notice), cost best-effort/0.
- **Interfaces:** none new in `core/agent` (that's the point — P0 defined them). `core/codex`
  satisfies `agent.Runner`.
- **Acceptance:**
  - Build/vet/lint/test green. `core/codex` has unit tests with a **fake `codex` CLI** (a shell script
    that emits canned JSONL, mirroring `core/claude/testdata/fake-claude.sh`) → asserted `agent.Event`
    sequence + `RunResult`, incl. a resume invocation and an error path.
  - **Live (Human-only gate):** `AGENT_BACKEND=codex` + a real key → a real message yields streamed
    progress then a final answer; a follow-up message resumes the session; an oversize answer chunks;
    the cost gate behaves per the chosen cost fallback.
  - Claude untouched (its suite still green; `AGENT_BACKEND=claude` byte-identical to before).

---

## 8. Testing strategy

- **P0:** the existing suite is the oracle — it must pass with only import/type edits. Add a tiny
  `core/agent` test for `New()` backend selection (claude resolves; unknown errors).
- **P1:** mirror `core/claude`'s test pattern — a `core/codex/testdata/fake-codex.sh` emitting canned
  `--json` JSONL drives `core/codex` unit tests that assert the decoded `agent.Event` stream, the
  terminal `RunResult`, the resume-subcommand argv, and the error/`turn.failed` path. No network in
  unit tests (the fake CLI stands in for `codex`). Live verification is the Human-only gate.

## 9. Deploy / migration

- One backend per instance: set `AGENT_BACKEND` + the chosen backend's vars in that instance's env.
  Default unset = `claude` = today's behavior, so **existing deployments are unaffected**.
- The runtime image must contain the chosen backend's CLI. Claude image ships the `claude` CLI today;
  a Codex deployment needs the `codex` CLI in the image (a Dockerfile build-arg/stage selecting which
  CLI to install, or a separate image variant — decide at P1 deploy task).
- Auth: provide `OPENAI_API_KEY` (Codex) or `CLAUDE_CODE_OAUTH_TOKEN` (Claude) to the instance.

## 10. Risks & open questions

- **P0 type-move churn (~120 refs)** across the conversation layer + adapters + cmds — pure
  `claude.X`→`agent.X`; the suite is the oracle. Mitigate by doing the move mechanically and leaning on
  the green regression run. (Mirrors the P0 id-widening of the transport plan.)
- **Codex JSONL schema drift** — docs vs the installed binary may differ; the §6 verify-at-kickoff gate
  + the `-o` final-message fallback contain this.
- **Cost accounting gap** — Codex reports tokens, not USD; the per-user cost cap is a Claude-era
  feature. Fallback: best-effort USD from a configured price, else `CostUSD=0` (cap off for Codex).
  Flag to the human at P1 — if a hard Codex cost cap is required, that's a follow-up (price table).
- **Vision gap** — `codex exec` image support is unverified; fallback is text-only on Codex. If inbound
  images matter for a Codex deployment, that's a follow-up.
- **Headless auth** — confirm `OPENAI_API_KEY` works without an interactive `codex login` in the
  container; if only `codex login` works, the deploy task must persist its credential file (ESCALATE if
  neither is viable headless).
- **Lifecycle reuse vs duplication** — `core/codex` should reuse the Claude runner's process-group /
  cancellation / tail pattern. If P0 didn't lift those into `core/agent`, P1 must decide between
  import-reuse and a small shared helper; avoid copy-paste of the kill/tail logic.
