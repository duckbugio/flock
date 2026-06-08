# flock — Multi-transport plan: VK + MAX on a transport-neutral core

> **Status:** design / not started. This document is the implementation roadmap for making the Go
> bot speak **more than Telegram**. Today the whole bot is one transport: the orchestration core
> (`core/claude`, `core/dispatch`, `core/session`, …) is already transport-neutral, but the
> *conversation layer* — the `Service` that turns a run into messages, plus all the message
> rendering — lives inside `internal/telegram` and is hardwired to the Telegram API. This plan
> lifts that conversation layer into a reusable **`core/chat`** and turns each platform
> (Telegram, then **VK**, then **MAX**) into a thin **adapter** behind one interface.
>
> **How to use this doc:** like `docs/go-rewrite-plan.md`, the work is **self-hosted** — the
> flock bot builds it by running its own pipeline (planner→coder→tester→reviewer→arbiter),
> **one phase = one PR**. See **"Execution model"** for how to drive it and **"Human-only gates"**
> for the few points that need you. Phases are ordered so each builds on a working previous one:
> **P0 refactor (no behavior change) → P1 VK → P2 MAX.** Do not start P1 before P0 is merged.

---

## 0. Context & goal

The Go rewrite (see `docs/go-rewrite-plan.md`) replaced the patched Python Telegram bot with a
clean Go core + a Telegram adapter. That rewrite already split the genuinely platform-agnostic
machinery into `core/`:

- `core/claude` — spawns/streams the `claude` CLI (transport-neutral).
- `core/dispatch` — per-chat serial queue + global concurrency cap (keyed by `int64` chat id).
- `core/session`, `core/cost`, `core/ratelimit`, `core/poller`, `core/gitsetup`, `core/voice`,
  `core/workspace` — all neutral.

**But the conversation layer is not neutral yet.** Two things are still Telegram-shaped:

1. **`internal/telegram.Service`** (`run.go`) — the heart of the bot: it submits a message to the
   dispatcher, posts a live "Working… (Ns)" progress message, streams events into it, edits on a
   wall-clock tick, replaces it with the final chunked answer, handles Stop, sessions, cost, and
   the outbox sweep. **None of this logic is Telegram-specific** — it only *happens* to call a
   Telegram `chat` interface. It is ~95% reusable across platforms.
2. **`internal/tgui`** (`progress.go`, `chunk.go`) and the gate/media/upload/outbox helpers —
   message framing, the 4096-char chunker, mention-gating, file up/download. Also mostly neutral,
   parameterized by a couple of Telegram constants.

**Goal:** add VK and MAX as first-class transports with **maximum code reuse and zero behavior
change for Telegram**. After P0, a new transport is a single `adapters/<name>` package
implementing one interface + a `cmd/duck-<name>` binary — no fork of the run loop, the progress
UI, the dispatcher, sessions, cost, or the poller.

**Non-goal:** a single binary that speaks all three at once. We ship **one binary per transport**
(`cmd/flock-telegram`, `cmd/duck-vk`, `cmd/duck-max`), each deployed as its own instance with its
own token — exactly as the Telegram instance runs today. This keeps blast radius small and matches
the existing Ansible multi-instance layout.

---

## 1. What we're building — and explicitly NOT

**In scope:**
- **P0:** extract the transport-neutral conversation layer from `internal/telegram` into `core/chat`
  behind a `Transport` interface, with a `Capabilities` descriptor driving graceful degradation.
  Re-point the existing Telegram code at it. **No user-visible change.**
- **P1:** a **VK** adapter (`adapters/vk` + `cmd/duck-vk`) using VK's Bots Long Poll API, behind the
  same `Transport` interface.
- **P2:** a **MAX** adapter (`adapters/max` + `cmd/duck-max`) — **gated** on a maintainer confirming
  MAX actually exposes a usable Bot API (see §6 feasibility table; this is the biggest unknown).

**Out of scope / unchanged:**
- The `claude` CLI engine, the dev-team agents (`core/agents/*.md`), the orchestration template
  (`core/CLAUDE.workspace.md.tmpl`) — platform-agnostic already.
- `core/claude`, `core/dispatch`, `core/session`, `core/cost`, `core/ratelimit`, `core/poller`,
  `core/gitsetup`, `core/voice`, `core/workspace` — neutral; **P0 must not change their behavior**
  (it may widen ONE type — chat/message ids, see §4).
- The Telegram adapter's external behavior — P0 is a pure refactor verified by the existing suite
  staying green.

**The honest constraint:** VK's API is documented and stable but its exact rate limits, callback
(button) payload shape, message length cap, and mention syntax must be **verified against the live
API during P1** — the plan marks each as an `UNKNOWN` with a fallback. **MAX is largely
unverifiable from here**; P2 is specced defensively and may ESCALATE at its first task if the API
isn't what we assumed.

---

## 2. Architecture

### Today (one transport, conversation layer fused to Telegram)

```
  Telegram ◄─► internal/telegram (Service + bot.go + tgui + gate/media/upload/outbox)
                      │  imports
                      ▼
               core/{claude,dispatch,session,cost,ratelimit,poller,workspace,voice}
                      │  exec + stream-json
                      ▼
               claude CLI (Node)
```

### After P0 (neutral core/chat + thin adapters)

```
   Telegram ─►┐        VK ─►┐         MAX ─►┐
              │             │              │     each: receive update, send/edit/delete,
   adapters/  │   adapters/ │   adapters/  │     upload/download file, render mention —
   telegram   │      vk     │     max      │     implements ONE interface:
   (Transport)│ (Transport) │  (Transport) │
              ▼             ▼              ▼
        ┌───────────────────────────────────────────────┐
        │  core/chat  — the transport-neutral Service     │
        │   • run loop: progress msg, tick, stream, final │
        │   • Stop / sessions / cost / outbox sweep       │
        │   • Capabilities-driven graceful degradation    │
        └───────────────┬───────────────────────────────┘
                        │  imports (unchanged)
                        ▼
        core/{claude,dispatch,session,cost,ratelimit,poller,workspace,voice}
                        │  exec + stream-json
                        ▼
                 claude CLI (Node)
```

Each `cmd/duck-<transport>/main.go` wires: config → build the transport adapter → build the shared
`core/chat.Service` with that adapter → start receiving. The wiring is ~the same in every binary;
only the adapter construction differs.

### Go module layout (single module at repo root, unchanged)

```
core/
  chat/         # NEW (P0): the transport-neutral Service + Transport/Capabilities + progress UI + chunker
    service.go  #   lifted from internal/telegram/run.go (no behavior change)
    transport.go#   the Transport interface + Capabilities descriptor + ChatID/MessageID types
    progress.go #   lifted from internal/tgui/progress.go
    chunk.go    #   lifted from internal/tgui/chunk.go, MaxRunes parameterized
    gate.go     #   lifted from internal/telegram/gate.go (mention-gate, transport-agnostic core)
    media.go    #   lifted media-routing helpers (image attachment plumbing)
    outbox.go   #   lifted from internal/telegram/outbox.go (already filesystem-only, neutral)
  claude/ dispatch/ session/ cost/ ratelimit/ poller/ gitsetup/ voice/ workspace/   # unchanged
adapters/
  telegram/     # NEW location of the Telegram-SPECIFIC glue (bot client, getFile download, markup)
  vk/           # NEW (P1)
  max/          # NEW (P2)
cmd/
  flock-telegram/ # exists; re-pointed at core/chat + adapters/telegram in P0
  duck-vk/       # NEW (P1)
  duck-max/      # NEW (P2)
internal/
  config/        # extended per transport (TELEGRAM_* stays; add VK_*, MAX_* in P1/P2)
```

> **Decided:** keep the **single Go module at the repo root** (`github.com/duckbugio/flock`).
> `internal/tgui` and the Telegram-neutral parts of `internal/telegram` move UP into `core/chat`;
> the Telegram-SPECIFIC bits (the `*bot.Bot` wrapper in `bot.go`, the getFile download seam in
> `upload.go`, the inline-keyboard markup) move sideways into `adapters/telegram`. `internal/` may
> keep `config` (it is shared wiring, not transport-coupled at its core).

---

## 3. Tech choices (recommended; lock at P0 / P1 start)

| Concern | Choice | Rationale |
|---------|--------|-----------|
| Telegram lib | `github.com/go-telegram/bot` (unchanged) | already in use; P0 doesn't touch it |
| VK lib | `github.com/SevereCloud/vksdk/v3` (Bots Long Poll + API) | the maintained, idiomatic Go VK SDK; covers Long Poll, `messages.send`, `messages.edit`, `docs.*`, keyboards. Alt: hand-rolled `net/http` against `api.vk.com/method/*` if we want zero new deps. |
| MAX lib | **UNKNOWN** — likely hand-rolled `net/http` | no established Go SDK assumed; depends on whether MAX even has a public Bot API (P2 gate). |
| File up/download | per-adapter (Telegram getFile already exists; VK `docs.getMessagesUploadServer`→upload→`docs.save`) | each platform's upload dance differs; the `core/chat` side stays an `io.Reader`/path seam |
| Everything else | unchanged (`os/exec`, `encoding/json`, `caarlos0/env`, `log/slog`, `x/sync/semaphore`, `net/http`) | core is already built on these |

Keep new deps minimal: VK adds at most one SDK; MAX adds none if hand-rolled.

---

## 4. The `Transport` adapter contract (the crux — get this right in P0)

Everything reusable depends on one interface. The neutral `core/chat.Service` must drive any
platform through **only** these operations. The signatures below are the lift of today's `chat`
interface (`run.go` lines 35–48), generalized.

```go
package chat

// ChatID and MessageID are opaque, transport-defined identifiers. Telegram uses
// numeric ids; VK peer ids are numeric too but VK conversation_message_ids and
// MAX ids may be strings. We widen to string so no transport is forced to lie.
// (P0 widens the current int64 chatID / int messageID — see "P0 type widening".)
type ChatID = string
type MessageID = string

// Transport is the ONE interface every platform implements. It is the generalized
// form of today's internal/telegram.chat interface.
type Transport interface {
    // Send posts a new message, optionally carrying a Stop affordance bound to
    // stopRunID (empty = no Stop control). Returns the new message id.
    Send(ctx context.Context, chatID ChatID, text, stopRunID string) (MessageID, error)

    // Edit updates an existing message in place. Adapters whose platform cannot
    // edit (Capabilities.CanEditMessages == false) are never called here — the
    // Service degrades to Send-only first (see graceful degradation).
    Edit(ctx context.Context, chatID ChatID, messageID MessageID, text, stopRunID string) error

    // Delete removes a message; a failure is non-fatal to the caller.
    Delete(ctx context.Context, chatID ChatID, messageID MessageID) error

    // SendDocument uploads a local file to the chat as a document/attachment. The
    // data is streamed (no token-bearing URL constructed). Used by the outbox.
    SendDocument(ctx context.Context, chatID ChatID, name string, data io.Reader) error

    // Capabilities reports what this platform can do, so the Service degrades
    // gracefully instead of erroring on a missing primitive.
    Capabilities() Capabilities
}

// Capabilities lets the Service adapt to platforms weaker than Telegram. Each
// flag has a defined fallback so a "false" never breaks delivery.
type Capabilities struct {
    // CanEditMessages: platform supports editing a sent message. false → the
    // Service does NOT post a live-updating progress message; it posts one
    // "Working…" notice (optional) and then sends the final answer as a new
    // message. (The wall-clock tick edit loop is skipped entirely.)
    CanEditMessages bool

    // CanInlineStop: platform supports an inline button / callback bound to a
    // message. false → no Stop button is rendered; Stop is still reachable via a
    // text command (e.g. "/stop") the adapter maps to Service.Stop.
    CanInlineStop bool

    // CanSendDocument: platform supports file attachments. false → the outbox
    // sweep is skipped and a one-line notice ("N files were produced but this
    // platform can't receive them") is appended instead. (Telegram/VK: true.)
    CanSendDocument bool

    // MaxMessageRunes is the platform's max message length in runes, fed to the
    // chunker (Telegram 4096; VK ~4096; MAX UNKNOWN). 0 → a safe default.
    MaxMessageRunes int
}
```

**File DOWNLOAD (inbound media/voice)** stays a per-adapter concern, not part of `Transport`: today
`internal/telegram/upload.go` owns the Telegram getFile + token-redaction seam. Each adapter keeps
its own download seam and hands `core/chat` either a saved local path or an `io.Reader` + the
already-existing `claude.ImageInput`. The neutral side never sees a platform URL or token.

**Mention-gate** generalizes too: `core/chat/gate.go` holds the transport-agnostic decision ("did
this group message address the bot?"); the adapter supplies the platform's mention representation
(Telegram `@username` entity; VK `[club<id>|...]` mention; MAX UNKNOWN). The adapter parses its own
entities and tells the gate "addressed / not addressed + stripped text".

### P0 type widening (do this once, in P0, mechanically)

Today `core/dispatch`, `core/session`, `core/cost`, `core/ratelimit`, and `Service` key everything
on `int64` chat id and `int` message id. VK peer ids fit `int64`, but VK
`conversation_message_id`s and MAX ids may not. **P0 widens the chat/message id types to `string`
(or an alias) across the conversation layer** so no transport is forced into a lossy cast. This is
the single riskiest mechanical change in P0 — it touches ~the dispatcher key, the session-store
key, the cost/rate-limit key, and every `chatID int64` signature in the run loop. It is pure
type-churn with **no behavior change**, fully covered by the existing tests (which must be updated
to the new types and stay green). If the team judges the blast radius too large, the fallback is to
keep `int64` chat ids (VK/MAX peer ids are numeric) and widen **only** `MessageID` to string — the
planner should pick one at P0 kickoff and the arbiter records the decision in `.team/memory.md`.

---

## 5. Config (env) — one prefix per transport

Each binary reads only its own transport's vars plus the shared core vars. Keep the existing
`TELEGRAM_*` names untouched (deploy parity). Mirror them for new transports:

- **Shared (all binaries):** `CLAUDE_CODE_OAUTH_TOKEN`/`ANTHROPIC_API_KEY`, `CLAUDE_MODEL`,
  `CLAUDE_MAX_TURNS`, `CLAUDE_TIMEOUT_SECONDS`, `CLAUDE_MAX_COST_PER_USER`, `APPROVED_DIRECTORY`,
  `MAX_CONCURRENT_CHAT_RUNS`, `PRE_PR_CYCLES`, `PR_REVIEW_CYCLES`, `ENABLE_PR_REVIEW`,
  `REQUIRE_GROUP_MENTION`, all `GIT_*`/`GITEA_*`, `ENABLE_VOICE_MESSAGES`/`VOICE_PROVIDER`/keys,
  `MAX_UPLOAD_BYTES`, `MAX_OUTBOX_BYTES`, `MAX_OUTBOX_FILES`, `LOG_LEVEL`, `RATE_LIMIT_*`,
  `SESSION_TIMEOUT_HOURS`.
- **Telegram (`cmd/flock-telegram`):** `TELEGRAM_BOT_TOKEN`, `TELEGRAM_BOT_USERNAME`, `ALLOWED_USERS`.
- **VK (`cmd/duck-vk`, P1):** `VK_BOT_TOKEN` (community access token), `VK_GROUP_ID`,
  `VK_ALLOWED_USERS` (csv int). (`REQUIRE_GROUP_MENTION` reused.)
- **MAX (`cmd/duck-max`, P2):** `MAX_BOT_TOKEN`, `MAX_ALLOWED_USERS`, plus whatever the API needs —
  **UNKNOWN until the API is confirmed.**

> **Decided:** `internal/config` gains a per-transport sub-struct, but the **shared** core config is
> parsed once and reused. `ALLOWED_USERS` semantics (whitelist; friendly refusal) are identical on
> every transport.

---

## 6. Per-platform feasibility table (verify the UNKNOWNs before/at each phase)

| Capability | Telegram (today, known) | VK (P1 — verify at kickoff) | MAX (P2 — mostly unknown) |
|------------|-------------------------|------------------------------|----------------------------|
| Inbound updates | Long Poll (`go-telegram/bot`) | **Bots Long Poll** (`vksdk`) — known to exist | **UNKNOWN**: does a Bot API + long-poll/webhook exist? |
| Send message | ✅ | ✅ `messages.send` | UNKNOWN |
| **Edit message** (live progress) | ✅ | ⚠️ `messages.edit` exists but **edit window / rate limits UNKNOWN** → fallback: `CanEditMessages=false`, send-final-only | UNKNOWN → assume false |
| **Inline Stop button** | ✅ callback | ⚠️ VK **keyboards + callback** exist but **payload shape UNKNOWN** → fallback: `CanInlineStop=false`, `/stop` text command | UNKNOWN → assume false |
| **File attachment** (outbox / uploads) | ✅ getFile / multipart | ⚠️ `docs.getMessagesUploadServer`→upload→`docs.save` (multi-step) — known but fiddly | UNKNOWN |
| Message length cap | 4096 runes | ⚠️ ~4096 — **verify**, feeds `MaxMessageRunes` | UNKNOWN |
| Group mention syntax | `@username` entity | ⚠️ `[club<id>|name]` — **verify parse** | UNKNOWN |
| Voice | getFile + transcribe | docs/audio download + transcribe (provider-neutral core reused) | UNKNOWN |

**Rule:** every ⚠️ is the **first task** of its phase — the adapter author hits the live API (with a
throwaway test-community/bot token, a Human-only gate) and **records the real answer in
`.team/memory.md`** before building on the assumption. Every ⚠️ has a `Capabilities` fallback so the
adapter ships degraded rather than blocked. **MAX (P2) ESCALATEs at task 1 if no usable Bot API
exists** — we do not guess an API into existence.

---

## Execution model — the bot builds its own transports (self-hosting)

Same model as `docs/go-rewrite-plan.md`: the flock bot implements each phase by running its own
pipeline (`planner → coder ⇄ tester → pre-PR review → PR → reviewer ⇄ coder → arbiter`),
**one phase = one PR**, base = the previous phase's branch (stacked). Scope is pre-approved by THIS
doc, so the team skips "confirm scope & wait" and builds straight away.

**Drive it:** in the chat — *"Implement P0 from `docs/multi-transport-plan.md` (read §1–§6 +
Execution model + Human-only gates first)."* Then *"Implement P1."*, then (only if the MAX gate is
green) *"Implement P2."*

**Acceptance is machine-checkable** where possible so the tester/arbiter verify without you:
`gofmt -l`, `go build ./...`, `go vet ./...`, `golangci-lint run`, `go test ./... -race -count=1`
via the **dind sidecar** (`docker run --rm -v "$PWD":/w -w /w golang:1.23 …` from inside the repo
dir). **P0's headline acceptance is "the entire existing suite passes unchanged"** — a pure refactor
is provable by a green regression run. **Live VK/MAX behavior** (P1/P2) needs your glance against a
test community/bot — the team tells you exactly what to expect.

**Decisions are pre-baked** (single module at repo root; one binary per transport; `core/chat`
houses the neutral Service; Capabilities-driven degradation; id widening per §4). On a genuinely
undecided fork (e.g. VK edit semantics turn out unusable in a way the fallback doesn't cover, or MAX
has no API) the team **ESCALATEs** instead of guessing.

## Human-only gates (the minimal set the bot cannot do alone)

1. **Test tokens** — a throwaway **VK community token** (+ test community) for P1 and a **MAX bot
   token** for P2. The bot can't mint these.
2. **Confirm the MAX Bot API exists and its primitives** (send/edit/long-poll/files/keyboards)
   **before P2** — this is the P2 go/no-go. If you can't confirm it, P2 stays parked; P0+P1 still
   ship value.
3. **Merge each phase's PR** — arbiter approves, the human merges (stacked order below).
4. **Live behavior checks** — "message the VK/MAX bot, see streamed progress (or its degraded
   form) then the final answer" — you eyeball; the bot says what to expect.

## Stacked merge order

P0 → P1 → P2 (each base = the previous). **Merge P0 first** (it's the shared contract every adapter
depends on), then P1, then P2. The arbiter restates this on each PR.

---

## 7. Phased implementation plan

Each phase: **Goal → Scope → Key packages/files → Interfaces → Acceptance.** Implement in order.

### P0 — Lift the conversation layer into `core/chat` (refactor, NO behavior change)

- **Goal:** the transport-neutral Service + progress UI + chunker + mention-gate + outbox live in
  `core/chat` behind the `Transport`/`Capabilities` contract; the Telegram-specific glue lives in
  `adapters/telegram`; `cmd/flock-telegram` is re-wired onto them. **Telegram behavior is byte-for-byte
  unchanged.**
- **Complexity:** **risky** (broad mechanical move + a type widening), but **low-uncertainty** — it's
  a refactor with a strong oracle (the existing suite).
- **Scope:**
  - Move `internal/telegram/run.go` → `core/chat/service.go`; rename the package; keep the run loop,
    Stop, session, cost, and outbox-sweep logic **identical**.
  - Define `core/chat/transport.go`: the `Transport` interface + `Capabilities` + the `ChatID`/
    `MessageID` types (§4). Generalize the current `chat` interface onto it.
  - Move `internal/tgui/{progress.go,chunk.go}` → `core/chat/`; **parameterize the 4096 limit** as
    `Capabilities.MaxMessageRunes` (default 4096 so Telegram is unchanged).
  - Move `internal/telegram/{gate.go,media.go,outbox.go}` → `core/chat/` (these are already
    transport-neutral; `outbox.go` is pure filesystem). Keep the Telegram entity-parsing half of the
    gate in `adapters/telegram`.
  - Move the Telegram-SPECIFIC glue — `bot.go` (the `*bot.Bot` wrapper, now implementing `Transport`
    with `Capabilities{CanEditMessages:true, CanInlineStop:true, CanSendDocument:true,
    MaxMessageRunes:4096}`), `upload.go` (getFile download + token redaction), inline-markup — into
    `adapters/telegram`.
  - **Widen the chat/message id types** per §4 across `core/dispatch`, `core/session`, `core/cost`,
    `core/ratelimit`, and the Service (or the reduced fallback: only `MessageID`→string). Update all
    tests to the new types.
  - Re-wire `cmd/flock-telegram/main.go` to build `adapters/telegram` → `core/chat.New(...)`.
- **Interfaces:** `core/chat.Transport` + `Capabilities` (§4); `core/chat.New(Config) *Service` with
  the same fields as today's `telegram.New` but taking a `Transport` instead of the `chat` interface.
- **Acceptance:**
  - `gofmt -l .` empty; `go build ./...`, `go vet ./...`, `golangci-lint run` all clean.
  - **`go test ./... -race -count=1` passes with the existing tests essentially unchanged** (only
    import paths + the widened id types updated) — this is the proof of "no behavior change".
  - **Protected files stay byte-identical:** `core/CLAUDE.workspace.md.tmpl`, `core/agents/**`, and
    every protected deploy/adapter path are untouched.
  - `cmd/flock-telegram` still builds and (live check) behaves exactly as before: streamed progress,
    Stop, sessions, uploads, outbox.

### P1 — VK adapter (`adapters/vk` + `cmd/duck-vk`)

- **Goal:** message a VK community bot (allowed user) → Claude answers, with progress (live-edited if
  VK editing is usable, else send-final), Stop (button if usable, else `/stop`), inbound files/voice,
  and the outbox — all through the **unchanged** `core/chat.Service`.
- **Complexity:** **standard** (the hard part — the run loop — is already built and tested in P0).
- **First task (do before coding):** with a throwaway VK community token, **verify the §6 ⚠️ rows**:
  Long Poll update shape, `messages.edit` window/rate limit, keyboard/callback payload shape, message
  length cap, `[club<id>|...]` mention parsing, the `docs.*` upload dance. **Record the real answers
  in `.team/memory.md`** and set the adapter's `Capabilities` accordingly.
- **Scope:**
  - `adapters/vk`: a `Transport` implementation over `vksdk` (or `net/http`): `Send`/`Edit`/`Delete`
    (`messages.send`/`messages.edit`/`messages.delete`), `SendDocument` (`docs.getMessagesUploadServer`
    →PUT→`docs.save`→attach), and a `Capabilities` reflecting the verified facts. An inbound
    Long-Poll receive loop mapping VK updates → `core/chat.Service.Handle/HandleEdit`. A VK download
    seam (mirror `upload.go`'s token-redaction discipline) for inbound docs/photos/voice. A VK
    mention parser feeding `core/chat/gate.go`.
  - `cmd/duck-vk/main.go`: wire config (`VK_*`) → `adapters/vk` → `core/chat.New` → receive.
  - `internal/config`: add the VK sub-struct (§5).
- **Interfaces:** none new in `core/chat` (that's the whole point — P0 already defined them). The VK
  adapter satisfies `core/chat.Transport`.
- **Acceptance:**
  - Build/vet/lint/test green (`go test ./... -race`); VK adapter has unit tests with a **fake VK
    API** (canned Long-Poll JSON → asserted `Service` calls), mirroring the Telegram fakes.
  - **Live (Human-only gate):** a real VK DM to the test community produces a progress reply (live or
    degraded per `Capabilities`) then the final answer; Stop works (button or `/stop`); an oversize
    answer is chunked at the verified cap; a produced file arrives via the outbox; a non-mention group
    message is ignored, a mention is answered.
  - Telegram untouched (its suite still green; its binary unchanged).

### P2 — MAX adapter (`adapters/max` + `cmd/duck-max`) — GATED

- **Goal:** the same, for MAX — **only if** a usable MAX Bot API is confirmed (Human-only gate #2).
- **Complexity:** **risky** — driven entirely by an external unknown.
- **First task (the gate):** confirm MAX exposes send/edit/receive(long-poll or webhook)/files/
  keyboards. **If not → ESCALATE and stop**; P0+P1 already delivered. If yes, **record the API
  surface in `.team/memory.md`** and proceed exactly like P1.
- **Scope (assuming the gate passes):** `adapters/max` (`Transport` impl over the MAX API, with
  honest `Capabilities` — likely `CanEditMessages`/`CanInlineStop` false at first → send-final +
  `/stop`), an inbound receive loop, a download seam, a mention parser; `cmd/duck-max/main.go`;
  `MAX_*` config.
- **Interfaces:** none new (satisfies `core/chat.Transport`).
- **Acceptance:** build/vet/lint/test green; fake-MAX-API unit tests; **live (Human-only gate)** a MAX
  DM yields an answer (degraded UI acceptable); Telegram + VK untouched.

---

## 8. Testing strategy

- **P0 is verified by the existing suite.** The whole point of a no-behavior-change refactor is that
  `go test ./... -race` stays green with only import-path/type edits. Add no new behavior, so add no
  new tests beyond what the move requires. A green regression run **is** the acceptance.
- **Per-adapter unit tests** mirror the Telegram fakes: a **fake platform API** (canned inbound
  updates → asserted `Service` calls; asserted outbound `messages.send`/`edit`/upload payloads),
  plus a `Capabilities`-degradation test (with `CanEditMessages:false`, the Service sends one final
  message and never calls `Edit`).
- **`core/chat` Service tests** (lifted from `run_test.go`) gain a **capabilities matrix** case:
  same run driven by a full-capability fake vs a degraded fake produces the expected two shapes.
- **No live API in CI** — VK/MAX live checks are Human-only gates, scripted as a short checklist the
  maintainer runs against a test community/bot.

---

## 9. Deploy / migration

- **One image + binary per transport**, each its own Ansible instance with its own token — exactly
  the existing Telegram pattern. `cmd/duck-vk`/`cmd/duck-max` get their own Dockerfile target /
  published image (`ghcr.io/duckbugio/flock-vk`, `…-max`) mirroring `…-telegram`.
- **P0 ships no deploy change** — `cmd/flock-telegram` is the same binary/image, same env, same
  volumes. (Verify the published Telegram image still builds after the refactor.)
- VK/MAX instances reuse the same `core/agents` + `CLAUDE.workspace.md.tmpl` bake and the same volume
  layout; only the transport token + receive loop differ. Sessions/workspaces are per-chat and
  transport-local — no cross-transport migration.

---

## 10. Risks & open questions

- **P0 id-widening blast radius** (biggest P0 risk) — touches dispatcher/session/cost/ratelimit keys.
  Mitigation: the existing suite is the oracle; if too broad, fall back to widening only `MessageID`
  (§4). Arbiter records the choice.
- **VK edit/rate/keyboard/length/mention unknowns** (biggest P1 risk) — each is a §6 ⚠️ resolved by a
  live spike at P1 task 1, each with a `Capabilities` fallback so the adapter ships degraded, never
  blocked.
- **MAX may have no usable Bot API** (P2 go/no-go) — P2 is gated; it ESCALATEs at task 1 rather than
  inventing an API. P0+P1 stand on their own.
- **Capabilities sprawl** — keep the descriptor SMALL (the four flags in §4). New flags only when a
  real platform forces one; resist speculative capability knobs (altitude lens).
- **Refactor scope creep in P0** — P0 must be a pure lift, not a redesign. Any "while we're here"
  improvement is a separate later PR. The reviewer's design lenses guard this.
- **Don't fork the run loop.** If an adapter author is tempted to copy `service.go`, that's a signal
  the `Transport`/`Capabilities` seam is missing something — fix the seam in `core/chat`, don't fork.

---

*Drive it via the bot: "Implement P0 from docs/multi-transport-plan.md (read §1–§6 + Execution
model + Human-only gates)." It runs the full pipeline and stops at a PR or a Human-only gate. Then
"Implement P1", then — only if the MAX gate is green — "Implement P2."*
