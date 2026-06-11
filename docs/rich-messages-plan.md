# flock — Rich Messages plan: Bot API 10.1 rich rendering & native draft streaming (Telegram), VK-safe

> **Status:** design / not started. This document is the implementation roadmap for adopting the
> **Bot API 10.1 (June 11, 2026) "Rich Messages"** feature set in the Telegram adapter — structured
> block rendering (`sendRichMessage`/`editMessageText` with `rich_message`), native streaming
> (`sendRichMessageDraft`), and a dedicated reasoning block (`RichBlockThinking`) — **without
> touching the VK adapter or any other platform**.
>
> **How to use this doc:** the work is **self-hosted** — the flock bot implements it by running its
> own pipeline (planner→coder→tester→reviewer→arbiter), **one stage = one PR**. Every stage merges
> to `main` on its own **behind `ENABLE_RICH_MESSAGES` (off by default)**; there is no deep stack.
> See **"Execution model"** for how to drive it and **"Human-only gates"** for the few points that
> need you.

---

## 0. Context & goal

flock renders the assistant's output to Telegram through a deliberately small pipe:
`adapters/telegram/html.go`'s `MarkdownToHTML` collapses a Markdown subset into Telegram's tiny
HTML tag set (`b/i/code/pre/a/s/u/blockquote`) — headings degrade to bold, lists to `• `, and
**tables don't exist at all**. The live progress UI (`core/chat/progress.go`) hand-rolls a
"Working… (Ns)" frame: a 5-line ring, budgeted to 3500 source runes, with model *thoughts* and tool
inputs crammed into the same truncated lines. Streaming uses the existing **draft** seam
(`Transport.StreamDraft` → `SendMessageDraft`), with an HTML→plain→edit fallback ladder.

Bot API **10.1** introduces a first-class structured-message system that maps almost exactly onto
what flock already simulates by hand:

- **`sendRichMessage` / `editMessageText(rich_message=…)`** — structured blocks instead of a flat
  HTML string: `RichBlockPreformatted` (code), `RichBlockTable`, `RichBlockList`,
  `RichBlockBlockQuotation`, `RichBlockSectionHeading`, `RichBlockDetails` (collapsible).
- **`sendRichMessageDraft`** — native streaming of a partial message *as it is generated* — the
  exact shape of our progress loop.
- **`RichBlockThinking`** — a dedicated, collapsible reasoning block — a real home for the model's
  thoughts, which today fight tool activity for ring space.

**Goal:** render the assistant's answers and live progress as Bot API 10.1 rich messages **on
Telegram**, gated behind a feature flag (off by default), with a clean fallback to today's
`MarkdownToHTML` path — and **with the VK adapter unchanged (zero-line diff) and its behaviour
byte-identical**.

**Why a staged plan (not a single PR):** the feature spans a new neutral IR, a transport
serializer, two send paths (message + draft), and an uncertain upstream-library situation (§4). It
is too big and too risky for one PR, and it must ship dark (flag off) so each slice merges on its
own. That is exactly the "too large/risky for a single PR → staged, AI-executable plan" case.

---

## 1. What we're building — and explicitly NOT

**In scope (Telegram only):**
- A neutral, transport-agnostic **rich IR** in `core/chat/rich` (blocks + inline runs) plus a
  `Markdown → IR` mapper reusing the *same* subset `MarkdownToHTML` already recognizes.
- A Telegram **IR → Bot API 10.1** serializer + send/edit/draft calls, gated behind a capability
  flag and `ENABLE_RICH_MESSAGES`.
- Native **rich draft streaming** with `RichBlockThinking` for model reasoning.
- A **fallback ladder** at every call site: rich → existing `MarkdownToHTML` HTML → plain text →
  (for drafts) edit — so a rich failure or a flag-off deploy degrades to **today's exact
  behaviour**.

**Explicitly NOT in scope:**
- **The VK adapter (`adapters/vk/*`) — not touched.** No new `Transport` methods (the interface is
  unchanged); the only core change is one *additive* `Capabilities` field that VK leaves at its
  zero value. This is the load-bearing invariant of this whole plan (see §3, §6).
- The neutral run loop's contract (`core/chat.Transport`) — **no signature changes**. Rich
  rendering lives **inside** the Telegram adapter (switched by a config bool) plus a shared,
  platform-neutral IR package; the `Service` keeps calling `Send/Edit/StreamDraft` exactly as now.
- Other 10.1 features — **Join Request Queries** (`guard_bot`, `answerChatJoinRequestQuery`) and
  **Poll media** — are out of scope here; they are tracked separately, not in this doc.
- Reworking the chunker, the cost/star UI, or the dev-team prompts.

---

## 2. Architecture

```
   core/chat.Service  (run loop — UNCHANGED: still calls Send/Edit/StreamDraft + asMarkdown)
        │
        ├──────────────► adapters/vk        (UNCHANGED — Capabilities.CanSendRich = false (zero value);
        │                                     never renders rich; behaviour byte-identical to today)
        │
        └──────────────► adapters/telegram   (Capabilities.CanSendRich = cfg.EnableRichMessages)
                              │
                              │  asMarkdown && CanSendRich:
                              ▼
                    rich.FromMarkdown(text) ──► rich.Message (neutral IR)   ← core/chat/rich (NEW, pure)
                              │
                              ▼
                    toInputRichMessage(ir)  ──► Bot API 10.1 sendRichMessage /
                              │                  editMessageText(rich_message) / sendRichMessageDraft
                              │
                       ┌──────┴───────────────── on ANY error / flag off / CanSendRich false ──┐
                       ▼                                                                         ▼
              MarkdownToHTML(text)  ── parse error ──►  plain text   (drafts: ── on error ──► edit fallback)
              (TODAY'S PATH — unchanged)
```

**Key decisions (locked):**
- **The `Transport` interface gains no methods.** Rich is a *rendering* concern internal to the
  Telegram adapter; the seam is the existing `Send/Edit/StreamDraft(asMarkdown=true)`.
- **The neutral IR lives in `core/chat/rich`** (pure, no platform imports) so it is unit-testable
  and reusable, but **VK never imports or calls it** — adding the package cannot affect VK.
- **One additive capability field** `Capabilities.CanSendRich bool`. VK's `Capabilities()` returns a
  struct literal (`adapters/vk/bot.go`), so the new field is `false` with **no VK edit**.
- **Flag-gated, dark by default:** `ENABLE_RICH_MESSAGES` (env, default `false`). Every stage merges
  to `main` with the flag off → no user-visible change until a deliberate flip.

---

## 3. The contract / interfaces (the crux)

### 3.1 Capability (additive — the VK-safety hinge)

`core/chat/transport.go`, extend the existing struct **only by appending a field**:

```go
type Capabilities struct {
	CanSendDocument bool
	MaxMessageRunes int
	// CanSendRich: platform can render Bot API 10.1 rich messages (structured
	// blocks + native rich draft). false → the adapter uses the legacy
	// MarkdownToHTML/plain path. VK leaves this at its zero value (false), so the
	// VK adapter needs no change and behaves exactly as before.
	CanSendRich bool
}
```

- Telegram `Capabilities()` → `CanSendRich: c.enableRich` (wired from config in Stage 0).
- VK `Capabilities()` → **unchanged**; `CanSendRich` is `false` by zero value.

### 3.2 Neutral rich IR — `core/chat/rich` (NEW, pure)

A platform-neutral intermediate representation. **Real signatures to lock in Stage 1** (the exact
block set is finalized against the captured API schema, §4):

```go
package rich

// Inline is a run of styled text (bold/italic/code/strikethrough/link), mirroring
// the subset MarkdownToHTML already recognizes. Maps to RichText* on Telegram.
type Inline struct { Spans []Span }
type Span struct { Text, URL string; Bold, Italic, Code, Strike bool }

// Block is one structural element. A small closed set covering what flock emits.
type Block interface{ isBlock() }

type Paragraph struct { Text Inline }                 // → RichBlockParagraph
type Heading   struct { Level int; Text Inline }      // → RichBlockSectionHeading
type Code      struct { Lang, Text string }           // → RichBlockPreformatted
type List      struct { Ordered bool; Items []Inline }// → RichBlockList / RichBlockListItem
type Quote     struct { Text Inline }                 // → RichBlockBlockQuotation
type Details   struct { Summary string; Body []Block }// → RichBlockDetails (collapsible)
type Thinking  struct { Text string }                 // → RichBlockThinking
type Table     struct { Header []string; Rows [][]string } // → RichBlockTable / RichBlockTableCell

type Message struct { Blocks []Block }

// FromMarkdown parses the SAME Markdown subset MarkdownToHTML handles into the IR.
// It is total and best-effort: anything it can't classify becomes a Paragraph, so
// the serializer always has something valid to emit (mirrors MarkdownToHTML's
// "degrade to escaped literal" guarantee).
func FromMarkdown(md string) Message
```

### 3.3 Telegram serializer + send paths (adapter-internal)

`adapters/telegram/rich.go` (NEW):

```go
// toInputRichMessage maps the neutral IR to the Bot API 10.1 InputRichMessage
// payload (lib type if go-telegram/bot exposes it, else the raw-HTTP shim type, §4).
func toInputRichMessage(m rich.Message) <InputRichMessage>

// sendRich / editRich / streamRichDraft wrap sendRichMessage,
// editMessageText(rich_message=…) and sendRichMessageDraft respectively.
```

The existing `Send`/`Edit`/`StreamDraft` in `adapters/telegram/bot.go` gain a **leading branch**:
when `c.enableRich && asMarkdown && text != ""`, render via the rich path; on **any** error fall
through to the current `MarkdownToHTML` block (which itself already falls back to plain text). The
draft path keeps its extra rung (rich-draft → HTML-draft → plain-draft → flip `draftStreaming` off
→ edit). **No existing line of the legacy path is deleted** — it becomes the fallback.

---

## 4. Feasibility — every UNKNOWN has a fallback

| Concern | Status | Fallback (pre-decided) |
|---|---|---|
| Does `github.com/go-telegram/bot` (pinned `v1.21.0`) expose the 10.1 rich types (`InputRichMessage`, `RichBlock*`, `SendRichMessage*`)? | **UNKNOWN** — the API shipped the same day; the lib almost certainly lags. | **Hand-rolled raw-HTTP shim** in the Telegram adapter calling `sendRichMessage`/`sendRichMessageDraft`/`editMessageText` directly (precedent: the **entire VK adapter is hand-rolled `net/http`**, and `StreamDraft` already tolerates a transport that rejects its params). Swap the shim for lib types in a later cleanup PR once the lib bumps. |
| Exact JSON schema of `InputRichMessage` / `RichBlock*` / `RichText*`. | **UNKNOWN** | **Stage 1 captures a golden sample** (from the live Bot API + the 10.1 docs) into `core/chat/rich/testdata/` and locks the serializer to it — same "capture real samples first, then lock the schema" discipline as the Go-rewrite Runner. If the schema cannot be obtained/verified → **ESCALATE** (don't guess a wire format). |
| Does `sendRichMessageDraft` reuse the existing `draftID` streaming model (so it slots into the current `StreamDraft` seam)? | **UNKNOWN** | Keep the **current `SendMessageDraft` path intact**; add the rich draft *on top* only once confirmed. If it's a different model, rich draft falls back to the existing HTML draft. |
| 4096-rune cap semantics for rich messages (do blocks count toward the limit differently?). | **UNKNOWN** | Keep the **existing source-rune chunker** (`frameBudgetMax` 3500, `MaxMessageRunes` 4096) unchanged and conservative; revisit only if a real overflow is observed. |
| Rich-draft rate-limit behaviour vs. the rate-limit-free normal draft. | **UNKNOWN** | The existing plain-draft fallback + `draftStreaming` off → edit ladder already handles a draft transport that misbehaves; reuse it verbatim. |

**Net:** the only hard dependency is the wire schema (Stage 1). Everything else has a working
degradation that lands on **today's behaviour**.

---

## 5. Config

`ENABLE_RICH_MESSAGES` (env, `bool`, default `false`) → a new field on the adapter's `Config`
struct (caarlos0/env, same place as `ENABLE_PR_REVIEW`/`REQUIRE_GROUP_MENTION`), threaded into the
Telegram `botChat` and surfaced via `Capabilities().CanSendRich`. Add it to `.env.example`
(commented, default off) and to the Ansible `env.j2` parity list. **No other env changes.**

---

## 6. The VK-safety invariant (non-negotiable, every stage asserts it)

Every stage's Acceptance includes, mechanically:

1. **`git diff --stat origin/main -- adapters/vk` is empty** — the VK adapter is not edited.
2. **`go test ./adapters/vk/...` stays green** — VK behaviour unchanged.
3. **A unit test asserts `vk.Capabilities().CanSendRich == false`.**
4. **With `ENABLE_RICH_MESSAGES` unset/false, the Telegram golden tests** (`html_test.go` and the
   progress/draft tests) are **byte-identical to `main`** — the flag-off path is the legacy path.

If any of these four fails, the stage is red regardless of the rich feature working.

---

## Execution model — the bot builds it (self-hosting, flag-dark)

Implemented **by the flock bot itself**, running this repo's pipeline
(`core/CLAUDE.workspace.md.tmpl` + `core/agents/*.md`). **One stage = one PR**, full cycle
`planner → coder ⇄ tester → pre-PR review → PR → reviewer ⇄ coder → arbiter`. **Scope is approved
by this doc**, so the team skips the "confirm scope & wait" step and builds straight away.

- Stages are **independent and flag-gated** — each merges to `main` on its own with
  `ENABLE_RICH_MESSAGES` off. **Not a dependent stack.** (Stage N builds on N−1's merged code, but
  nothing partial is ever user-visible, so out-of-order review/merge is safe.)
- **Acceptance is machine-checkable** — the tester/arbiter verify without you: `task build`,
  `task lint`, `task test` (the repo's Taskfile runner, mirroring CI), plus the per-stage assertions
  above and the §6 invariant.
- **Decisions are pre-baked** (§2–§4): no new `Transport` methods; IR in `core/chat/rich`; one
  additive `Capabilities` field; flag default off; raw-HTTP shim if the lib lags. On a genuinely
  undecided fork (e.g. the wire schema can't be verified) the team **ESCALATEs** instead of guessing.

**Drive it:** in the chat — *"Implement Stage N from `docs/rich-messages-plan.md` (read §1–§6 +
Execution model + Human-only gates first)."* The team runs the full cycle and stops at a PR or a
Human-only gate. Then *"Stage N+1."*

## Human-only gates (the minimal set the bot cannot do alone)

1. **Confirm the captured rich-message API schema/testdata** (Stage 1) looks right — it's the
   foundation the serializer locks onto.
2. **Eyeball the rich render in a real Telegram client** (Stages 2 & 3) — "message the bot with the
   flag on, see real blocks / a collapsible thinking section." The bot tells you exactly what to
   expect; you glance.
3. **Merge each stage's PR** — arbiter approves, the human merges (repo rule).
4. **Flip `ENABLE_RICH_MESSAGES=true` in prod** — a deliberate deploy decision; default stays off.

Outside these four, the team plans → codes → tests → reviews → arbiter-approves every stage itself.

---

## 7. Staged implementation plan

Each stage: **Goal → Scope → Key files → Interfaces → Acceptance.** Implement in order. Every stage
also carries the **§6 VK-safety invariant** as part of Acceptance.

### Stage 0 — Flag + capability seam (no rendering yet)
- **Goal:** the plumbing exists and is provably inert — `ENABLE_RICH_MESSAGES` and
  `Capabilities.CanSendRich` are wired end-to-end, but nothing renders rich yet.
- **Scope:** append `CanSendRich bool` to `chat.Capabilities` (§3.1); add `EnableRichMessages` to the
  Telegram adapter `Config` (env `ENABLE_RICH_MESSAGES`, default false); thread it into `botChat`
  and return it from `Capabilities()`; VK `Capabilities()` **untouched**. Add the env to
  `.env.example` (commented) + Ansible `env.j2`. **Probe** whether `go-telegram/bot` exposes the
  10.1 rich types and **record the finding** (lib vs. raw-HTTP shim) in the PR description for
  Stage 2.
- **Interfaces:** §3.1 only.
- **Acceptance:** `task build` + `task lint` + `task test` green. New unit test:
  `telegram.Capabilities().CanSendRich` follows the config bool; `vk.Capabilities().CanSendRich ==
  false`. **§6 invariant holds** (VK diff empty; flag-off golden tests identical to `main`).

### Stage 1 — Neutral rich IR + `FromMarkdown` (`core/chat/rich`, pure)
- **Goal:** a tested, platform-neutral IR and a `Markdown → IR` mapper; **no adapter wiring**.
- **First task (do before coding):** capture a **golden `InputRichMessage` sample** from the live
  Bot API + 10.1 docs into `core/chat/rich/testdata/`; finalize the block set against it. If the
  schema can't be verified → **ESCALATE**.
- **Scope:** the `rich` package per §3.2; `FromMarkdown` reuses the exact subset `MarkdownToHTML`
  recognizes (fenced/inline code, `**`/`__` bold, ATX headings, `[t](u)` links, `-/*/+` bullets,
  blockquotes), classifying each into a block; unknown input → `Paragraph` (total, best-effort).
- **Interfaces:** §3.2.
- **Acceptance:** table-driven tests map representative Markdown (a code fence, a heading, a bullet
  list, a blockquote, a link-in-paragraph, a table) → expected IR; a fuzz/round-trip check that
  `FromMarkdown` never panics and always yields ≥1 block for non-empty input. **No file under
  `adapters/` changes.** §6 invariant holds trivially (no adapter touched).

### Stage 2 — Telegram rich `Send`/`Edit` behind the flag
- **Goal:** with the flag on, answers send as rich blocks; with it off, byte-identical to today.
- **Scope:** `adapters/telegram/rich.go` — `toInputRichMessage(rich.Message)` + `sendRich`/`editRich`
  using the lib types **or** the raw-HTTP shim (per Stage 0's finding). Add the leading rich branch
  to `Send`/`Edit` (§3.3) with the **rich → `MarkdownToHTML` → plain** fallback ladder; **delete no
  existing line** of the legacy path. Gated by `c.enableRich && asMarkdown`.
- **Interfaces:** §3.3.
- **Acceptance:** unit test with a fake Bot API server: flag **on** → a `sendRichMessage`/rich
  `editMessageText` call with the expected payload; **forced rich error** → falls back to the
  current HTML `SendMessage` (asserted); flag **off** → the exact legacy call as on `main`. `task
  build/lint/test` green. **§6 invariant holds.**

### Stage 3 — Native rich draft streaming + `RichBlockThinking`
- **Goal:** the live progress frame streams as a native rich draft, with model reasoning in a
  dedicated collapsible thinking block.
- **Scope:** extend `StreamDraft` (Telegram) to render the progress frame via the IR and call
  `sendRichMessageDraft`; route model *thoughts* into a `rich.Thinking` block and tool activity into
  paragraphs/preformatted, instead of cramming both into the 5-line ring. Keep the
  fallback ladder: **rich-draft → HTML-draft → plain-draft → `draftStreaming` off → edit**. The
  progress assembly in `core/chat/progress.go` may grow an *optional* IR builder **used only by the
  Telegram adapter**; VK keeps consuming the existing string `Frame()`.
- **Interfaces:** reuse `Transport.StreamDraft` (unchanged signature); internal
  `streamRichDraft(...)`.
- **Acceptance:** unit test: flag **on** → `sendRichMessageDraft` with a thinking block when the
  event stream carries reasoning; **forced draft error** → falls through to the HTML draft then to
  edit (asserted, mirroring the existing fallback test); flag **off** → the current draft path
  unchanged. **VK `StreamDraft` still returns `errDraftUnsupported`** and the Service still falls
  back to edit (existing VK test stays green). **§6 invariant holds.**

### Stage 4 — Structured dev-team artifacts (optional, polish)
- **Goal:** make the dev-team output genuinely richer where blocks earn their keep.
- **Scope (opt-in, all flag-gated):** render per-AC checklists and cost summaries as
  `rich.Table`; fold long review logs / diffs under `rich.Details` (collapsible); keep a plain
  fallback. No new primitives — pure use of the Stage 1 IR.
- **Acceptance:** snapshot tests for the table/details IR; flag-off path unchanged; §6 invariant
  holds. This stage is **optional/ongoing** — ship Stages 0–3 first.

---

## 8. Testing strategy

- **IR (`core/chat/rich`):** table-driven `FromMarkdown` tests + a no-panic/≥1-block round-trip
  guard; golden `InputRichMessage` serialization locked to captured `testdata/`.
- **Telegram adapter:** a fake Bot API server asserting the rich call payloads **and** the full
  fallback ladder (force a rich error → assert the legacy HTML call); flag-off golden tests proving
  byte-identity with `main`.
- **VK regression:** `go test ./adapters/vk/...` unchanged + the `CanSendRich == false` assertion —
  the mechanical proof VK is untouched.
- **Whole suite:** `task test` (mirrors CI) green at every stage; `task lint` clean.

---

## 9. Deploy / migration

- Default **off** (`ENABLE_RICH_MESSAGES` unset → `false`): deploying any stage changes nothing
  user-visible. Same image, same volumes, no data migration.
- Cut over by flipping the env on **one instance** first (the Ansible role already renders env; add
  the one var), eyeball a real chat (Human-only gate 2), then roll out. Instant rollback: flip the
  var back off — the legacy `MarkdownToHTML` path is still the live fallback in the binary.
- If Stage 2 shipped on the **raw-HTTP shim**, a later cleanup PR swaps it for `go-telegram/bot`
  lib types once the lib exposes 10.1 — behind the same flag, no behaviour change.

---

## 10. Risks & open questions

- **Wire-schema drift / lib lag (biggest risk).** The 10.1 rich types may not be in
  `go-telegram/bot` yet; the raw-HTTP shim is the mitigation, and Stage 1 locks the schema to
  captured `testdata`. Re-capture on any Bot API bump.
- **Rich draft semantics** may differ from the current draft model — Stage 3 keeps the existing
  draft path as the fallback rung, so a mismatch degrades, never breaks.
- **Over-rendering.** Not every message benefits from blocks; keep `FromMarkdown` conservative and
  let the design-lens review (reuse/simplify/altitude) catch dead abstraction before each PR.
- **VK divergence.** The single guard against accidental VK breakage is the §6 invariant — it is
  mechanical and runs every stage. Do not relax it.

---

*Drive it via the bot: "Implement Stage N from docs/rich-messages-plan.md (read §1–§6 + Execution
model + Human-only gates)." It runs the full pipeline and stops at a PR or a Human-only gate.*
