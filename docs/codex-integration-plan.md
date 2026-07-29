> Status: technical specification for adding Codex as an alternative local
> coding backend next to Claude. The primary target is subscription-based Codex
> usage; API billing is supported only as an explicit opt-in mode.

# Codex Backend Integration Plan

## How to Use This Doc

Use this document as the implementation contract for adding a Codex backend to
Flock. Each stage is intended to land as a separate PR that preserves the
existing Claude behavior. The default deployment must keep using Claude until an
operator explicitly selects Codex.

The most important rule: subscription auth and API billing auth must be
mutually exclusive at runtime. The bot must never silently fall back from a
subscription/Codex Local identity to an API key.

## Goal

Add Codex as a second local coding engine that can be driven from the existing
Telegram/VK chat flow, using the same per-chat workspace, dispatcher, progress
message, stop/cancel path, session persistence, and deploy model already used by
`core/claude`.

The integration must support two operator-selected authentication modes:

1. Subscription mode: use Codex Local with ChatGPT/Codex account authentication
   so runs consume the user's included Codex access/limits rather than OpenAI
   Platform API billing.
2. Billing mode: use an OpenAI Platform API key only when the operator
   explicitly enables API billing.

## Background

Claude is currently integrated as a one-shot CLI child process per chat message:
the bot spawns `claude`, parses stream-json events, persists the returned
session id, and resumes the next message with `--resume`.

Codex has a close equivalent for a first implementation: `codex exec --json`.
Official Codex docs describe `codex exec` as the non-interactive scripting mode;
with `--json`, stdout becomes a JSON Lines stream with events such as
`thread.started`, `turn.started`, `item.*`, `turn.completed`, `turn.failed`, and
`error`. Codex also supports resuming non-interactive sessions with
`codex exec resume <SESSION_ID>`.

## Source Facts to Preserve

- Codex CLI supports ChatGPT sign-in for subscription access and API-key sign-in
  for usage-based access.
- ChatGPT sign-in credentials are cached under `CODEX_HOME` by default.
- `CODEX_API_KEY` is supported for `codex exec` and represents API-key billing.
- `CODEX_ACCESS_TOKEN` can provide a ChatGPT/Codex access token for trusted
  automation; Codex access tokens are documented for ChatGPT Business and
  Enterprise workspaces.
- `codex exec` defaults to read-only sandboxing; editing workflows must set an
  explicit sandbox, either with CLI flags where supported or config overrides
  such as `-c 'sandbox_mode="workspace-write"'`.
- `codex exec --json` is the stable machine-readable path for scripting.
- Codex requires a git repository by default; `--skip-git-repo-check` exists but
  should be used only where the environment is deliberately safe.

## In Scope

- A new transport-neutral Codex runner package, `core/codex`.
- A backend selector so deployments can choose `claude` or `codex`.
- Docker/runtime changes that install Codex CLI and persist `CODEX_HOME`.
- Subscription auth setup for headless/container deploys.
- API billing auth as an explicit, guarded fallback.
- JSONL event parsing into the existing chat progress/result model.
- Tests with a fake `codex` binary and captured JSONL fixtures.
- Documentation and `.env.example` updates.

## Out of Scope

- Codex Cloud tasks.
- Replacing the chat adapters.
- Replacing the existing Claude backend.
- Building directly on `codex app-server` in the first PR series.
- Sharing one Codex identity across untrusted public users.
- Any hidden fallback from Codex subscription mode to API billing.

## Architecture

```text
Telegram/VK
   |
   v
core/chat.Service
   |
   +--> core/claude.Runner -> claude --print --output-format stream-json
   |
   +--> core/codex.Runner  -> codex exec --json
```

The first implementation should mirror `core/claude`: one Codex CLI process per
message. Later, if we need finer control, the runner can move behind the same
interface to `codex app-server` or the Codex SDK.

## Backend Selection

Add a backend selector:

```env
AI_BACKEND=claude # claude | codex
```

Rules:

- Empty or unknown value defaults to `claude` for backward compatibility, but
  unknown values must log a startup warning.
- `AI_BACKEND=codex` constructs `core/codex.Runner` and maps Codex options into
  the existing `chat.Service`.
- Claude-specific env continues to work unchanged.
- Codex-specific env is ignored unless `AI_BACKEND=codex`, except validation may
  warn about conflicting secrets.

## Codex Runtime Config

Add config fields:

```env
CODEX_BIN=codex
CODEX_MODEL=gpt-5.5
CODEX_HOME=/home/claude/.codex
CODEX_AUTH_MODE=subscription # subscription | billing
CODEX_SANDBOX=workspace-write
CODEX_APPROVAL_POLICY=never
CODEX_SKIP_GIT_REPO_CHECK=false
CODEX_EXTRA_ARGS=
```

### Subscription Mode

`CODEX_AUTH_MODE=subscription` is the default for Codex because the product goal
is to use a Codex/ChatGPT subscription instead of Platform API billing.

Accepted credential sources:

1. Persisted login in the `CODEX_HOME` volume, normally
   `/home/claude/.codex/auth.json`.
2. `CODEX_ACCESS_TOKEN` for trusted automation, when available for the
   workspace. If set, the startup path may run:

   ```bash
   printf '%s' "$CODEX_ACCESS_TOKEN" | codex login --with-access-token
   ```

3. In-chat device login: an allow-listed user sends `/login` and the bot relays
   the verification link and one-time code (`core/codexauth`). This is the
   primary path — it needs no shell on the host.
4. Manual one-time setup with device auth, for operators who do have a shell:

   ```bash
   docker exec -it <bot-container> codex login --device-auth
   ```

5. Manual copy of a local `auth.json` into the persistent `CODEX_HOME` volume.

Validation rules:

- If `CODEX_AUTH_MODE=subscription` and `CODEX_API_KEY` is non-empty, startup
  must fail with a clear error. This prevents accidental API billing.
- If no persisted auth is present and no `CODEX_ACCESS_TOKEN` is supplied, the
  bot starts in a "needs login" state rather than failing: runs are blocked with
  a notice pointing at `/login`, which is the command that clears the state.
  Failing startup here would be a deadlock — the login lives inside the bot.
- `CODEX_ACCESS_TOKEN` must be treated as a secret and must never be copied into
  the per-chat workspace.

Recommended default:

```env
CODEX_AUTH_MODE=subscription
CODEX_REQUIRE_AUTH=true
CODEX_API_KEY=
```

### Billing Mode

`CODEX_AUTH_MODE=billing` uses OpenAI Platform billing. This mode exists for
operators who explicitly want usage-based API-key operation.

Required env:

```env
CODEX_AUTH_MODE=billing
CODEX_API_KEY=sk-...
CODEX_BILLING_ACK=true
```

Validation rules:

- If `CODEX_AUTH_MODE=billing` and `CODEX_API_KEY` is empty, startup fails.
- If `CODEX_AUTH_MODE=billing` and `CODEX_BILLING_ACK` is not exactly `true`,
  startup fails.
- `CODEX_API_KEY` must be injected only into the Codex child process env, not
  into broad process environments used by repository-controlled scripts.
- The startup log must say that API billing is enabled.

## Docker Runtime

Update Telegram and VK images:

- Install Codex CLI during image build.
- Install `bubblewrap` on Linux for Codex sandbox reliability.
- Create `/home/claude/.codex` owned by uid 1000.
- Set `CODEX_HOME=/home/claude/.codex`.
- Add a named volume:

  ```yaml
  - codex_home:/home/claude/.codex
  ```

- Keep `claude_home` unchanged.
- Do not mount `codex_home` under `/workspace`.

The install path should use the official Codex installer in non-interactive
mode unless the project later pins a standalone package in a reproducible way:

```dockerfile
ENV CODEX_NON_INTERACTIVE=1 \
    CODEX_INSTALL_DIR=/usr/local/bin
RUN curl -fsSL https://chatgpt.com/codex/install.sh | sh
```

If this install path proves unsuitable for reproducible multi-arch builds, add a
follow-up task to pin an official release artifact/checksum or install Codex from
the package method documented by OpenAI at that time.

## Codex Runner Contract

Create `core/codex` with an interface compatible with the current `claude.Runner`
usage pattern:

```go
type Runner interface {
    Run(ctx context.Context, prompt string, o Options) (<-chan Event, error)
}
```

Suggested options:

```go
type Options struct {
    ThreadID             string
    Workdir              string
    Model                string
    Sandbox              string
    ApprovalPolicy       string
    Env                  []string
    SkipGitRepoCheck     bool
    ExtraArgs            []string
    Images               []ImageInput
}
```

Initial command shape:

```bash
codex exec --json \
  --cd "$WORKDIR" \
  --model "$CODEX_MODEL" \
  -c "sandbox_mode=\"$CODEX_SANDBOX\"" \
  -c "approval_policy=\"$CODEX_APPROVAL_POLICY\"" \
  "$PROMPT"
```

Resume shape:

```bash
codex exec resume --json \
  --model "$CODEX_MODEL" \
  -c "sandbox_mode=\"$CODEX_SANDBOX\"" \
  -c "approval_policy=\"$CODEX_APPROVAL_POLICY\"" \
  "$THREAD_ID" \
  "$PROMPT"
```

Notes:

- Preserve the same process-group kill behavior used by Claude, so cancelling a
  run kills Codex and any tool subprocesses.
- Keep stderr tail for errors, but parse JSONL from stdout.
- Set `cmd.Dir` to the chat workspace on every run. Pass `--cd` on new Codex
  sessions; omit it on `codex exec resume` because the resume subcommand already
  targets the persisted session and does not accept all top-level exec flags in
  every CLI build.
- In billing mode, append `CODEX_API_KEY=...` only to `cmd.Env`.
- In subscription mode, append `CODEX_HOME=...`; do not append `CODEX_API_KEY`.
- For image support, use Codex CLI `--image` flags with saved upload paths in a
  follow-up stage. The first stage may reject image inputs with a clear message
  if needed.

## JSONL Event Mapping

Map Codex events into the transport-neutral event model used by chat progress:

| Codex JSONL event | Flock event |
|---|---|
| `thread.started` | store `thread_id` as session id |
| `turn.started` | progress activity, optional |
| `item.started` command execution | `ToolUse` / progress line |
| `item.completed` agent message | `Text` or final text candidate |
| `turn.completed` | terminal `Result` |
| `turn.failed` or `error` | terminal `RunError` |

The parser must be forward-compatible:

- Unknown event types are ignored unless they are explicit errors.
- Missing optional fields do not crash parsing.
- Large JSONL lines use an increased scanner buffer, mirroring `core/claude`.

## Session Persistence

Reuse the existing `core/session` store:

- For Claude, store Claude `session_id`.
- For Codex, store Codex `thread_id`.

The store may remain `chatID -> string`, but logs and comments should use a
neutral term such as "engine session id" where touched.

Behavior:

- Store the Codex `thread_id` as soon as `thread.started` is observed.
- On timeout/cancel, keep the stored id, matching the Claude resume behavior.
- On explicit `/new`, delete the stored id for the chat.
- If a resumed Codex run fails with a stale-thread error, clear the stored id and
  report that the next message will start a fresh Codex thread.

## Workspace and Instructions

The existing per-chat workspace remains the working directory:

```text
/workspace/chat_<id>
```

Codex should consume repo guidance through normal Codex surfaces:

- Keep rendering `CLAUDE.md` for Claude.
- Add an `AGENTS.md` rendering path for Codex, derived from the current
  `CLAUDE.workspace.md.tmpl` where feasible.
- Copy skills/instructions into Codex-recognized locations only when supported
  and documented.

Stage 1 may run Codex with the existing workspace and a generated `AGENTS.md`
that contains the same team workflow rules, branch policy, outbox convention,
and verification requirements.

## Security Requirements

- Codex auth material must live only in `codex_home` or secret env, never in
  `/workspace/chat_<id>`.
- Deny or avoid reading `.env` files from workspaces where possible.
- In subscription mode, fail closed if `CODEX_API_KEY` is present.
- In billing mode, require `CODEX_BILLING_ACK=true`.
- Do not pass `CODEX_ACCESS_TOKEN` or `CODEX_API_KEY` to repository test/build
  commands. Prefer Codex shell environment policy or a minimal child env.
- Keep Flock's allow-list checks unchanged.
- Keep Docker non-root runtime.
- `danger-full-access` may be allowed only inside the existing isolated
  container model and must be an explicit operator choice.

## Configuration Examples

### Subscription Deployment

```env
AI_BACKEND=codex
CODEX_AUTH_MODE=subscription
CODEX_HOME=/home/claude/.codex
CODEX_MODEL=gpt-5.5
CODEX_SANDBOX=workspace-write
CODEX_APPROVAL_POLICY=never
CODEX_REQUIRE_AUTH=true
CODEX_API_KEY=
```

Operator setup:

```bash
docker exec -it duck-bot codex login --device-auth
docker compose restart bot
```

### Enterprise Access Token Deployment

```env
AI_BACKEND=codex
CODEX_AUTH_MODE=subscription
CODEX_ACCESS_TOKEN=...
CODEX_HOME=/home/claude/.codex
CODEX_REQUIRE_AUTH=true
```

Startup may persist the token with `codex login --with-access-token`, or the
runner may pass `CODEX_ACCESS_TOKEN` only to Codex child processes. Persisting
is preferred when token refresh/update behavior is needed across long-running
server sessions.

### API Billing Deployment

```env
AI_BACKEND=codex
CODEX_AUTH_MODE=billing
CODEX_API_KEY=sk-...
CODEX_BILLING_ACK=true
CODEX_MODEL=gpt-5.5
CODEX_SANDBOX=workspace-write
CODEX_APPROVAL_POLICY=never
```

The bot must log:

```text
Codex API billing mode enabled
```

## Implementation Stages

### Stage 1 - Config and Docker Foundations

Goal: make Codex CLI available in the runtime image and add safe configuration
validation without changing default behavior.

Scope:

- Add config fields for `AI_BACKEND` and Codex env vars.
- Update Telegram/VK Dockerfiles to install Codex CLI and bubblewrap.
- Add `codex_home` volume to compose templates.
- Add `.env.example` documentation.
- Validate auth mode conflicts.

Acceptance:

- Existing Claude deployment still starts with no Codex env.
- `AI_BACKEND=codex CODEX_AUTH_MODE=subscription CODEX_API_KEY=x` fails at
  startup with a clear no-billing-fallback error.
- `AI_BACKEND=codex CODEX_AUTH_MODE=billing CODEX_API_KEY=x` fails unless
  `CODEX_BILLING_ACK=true`.
- Image contains a runnable `codex` binary.

### Stage 2 - `core/codex` Runner

Goal: execute `codex exec --json`, parse JSONL, and expose typed events.

Scope:

- Add `core/codex/runner.go`, `decode.go`, tests, and fake codex fixture.
- Implement process-group cancellation.
- Implement stdout JSONL scanner with large-line support.
- Implement stderr tail errors.

Acceptance:

- Fake happy JSONL emits thread init, progress item, final text, and terminal
  result.
- Fake failed JSONL emits `RunError`.
- Cancellation kills a sleeper fake within the same bound as the Claude runner.
- Unknown future event types are ignored safely.

### Stage 3 - Chat Service Backend Wiring

Goal: let `core/chat.Service` run either Claude or Codex without transport
changes.

Scope:

- Add a neutral runner/event adapter if needed, or map Codex events to the
  existing Claude-shaped event model.
- Store Codex `thread_id` in the existing session store.
- Reuse dispatcher, pending, cost/no-cost accounting, outbox, Stop, edit
  supersede, and shutdown drain.

Acceptance:

- `AI_BACKEND=claude` behavior is unchanged.
- `AI_BACKEND=codex` posts one progress message and replaces it with the final
  Codex answer.
- `/stop` cancels the Codex child process and clears pending markers.
- `/new` clears the Codex thread id.

### Stage 4 - Codex Workspace Instructions

Goal: provide Codex-native project guidance equivalent to the current Claude
team instructions.

Scope:

- Render `AGENTS.md` per chat workspace.
- Port the team workflow, branch policy, verification rules, outbox convention,
  and "no AI attribution" rules.
- Keep `CLAUDE.md` for Claude.

Acceptance:

- A Codex run in `/workspace/chat_<id>` sees `AGENTS.md`.
- The generated `AGENTS.md` contains the same operational constraints as the
  current Claude template.
- Claude rendering remains byte-compatible where practical.

### Stage 5 - Subscription Auth UX (done)

Goal: make subscription setup completable and observable WITHOUT host shell
access.

Delivered:

- `core/codexauth` drives `codex login --device-auth`, parsing the verification
  URL and one-time code off the live (ANSI-colored) output while the CLI is still
  polling, and relaying them into the chat.
- `/login` (reserved command, both adapters) starts, re-shows, reports, cancels,
  or forces the sign-in.
- Startup no longer fails when only the interactive login is missing
  (`airunner.BuildWithPendingLogin`); runs are blocked with an actionable notice
  until the login lands, re-checked from `auth.json` on every message so no
  restart is needed.
- The block lives in `core/chat` (`RunGate`), at the single point every run
  passes through, so the four non-adapter sources — a poller relay, a cron fire,
  a workspace follow-up and the restart replay — are gated too instead of
  marching into an unauthenticated CLI once per tick. The adapters keep an early
  check on the message path so an unauthorized deploy never pays for a voice
  transcription or an upload download. A blocked replay keeps its pending marker.
- Where a one-time code may be printed is decided by `codexauth` from the
  destination, not by the adapters inspecting the arguments: `/login status` also
  re-shows a pending code, so an argument-based guard misses it. A non-private
  destination gets a redirect instead of the code, is refused when it tries to
  START a sign-in, and is never subscribed to the prompt broadcast.
- `CODEX_REQUIRE_AUTH=false` still opts out of blocking entirely, and startup
  logs `authorized` next to `credentials_present` so that opt-out cannot read as
  "all good".

Acceptance:

- Missing subscription auth starts the bot and blocks runs with instructions.
- Present `auth.json` allows runs immediately.
- `CODEX_ACCESS_TOKEN` path logs only that access-token auth is configured,
  never the token.

### Stage 6 - Optional Images and App-Server Evaluation

Goal: decide whether the CLI runner is enough or whether Codex app-server/SDK is
needed.

Scope:

- Add `--image` support for saved Telegram/VK photo uploads.
- Evaluate `codex app-server` for richer thread/turn control.
- Keep the current CLI runner unless app-server solves a concrete limitation.

Acceptance:

- Image prompts work or fail with a clear unsupported message.
- A short decision record explains whether to stay on CLI JSONL or move to
  app-server/SDK.

## Testing Strategy

- Unit tests for config validation.
- Unit tests for Codex arg construction.
- Unit tests for JSONL decode and forward compatibility.
- Process lifecycle tests mirroring `core/claude`.
- Chat service tests with fake runner.
- Docker build smoke test.
- Manual smoke test in a real container:

  ```bash
  docker exec -it duck-bot codex login --device-auth
  # send a chat message
  # verify Codex edits a test repo and responds
  ```

## Operational Runbook

### Subscription Setup

1. Deploy with `AI_BACKEND=codex` and `CODEX_AUTH_MODE=subscription`.
2. Start the container. With no persisted `auth.json` and no `CODEX_ACCESS_TOKEN`
   the bot comes up in a "needs login" state: it logs a warning, serves `/login`,
   and answers any other message with a notice pointing at it.
3. From an allow-listed **direct message** (not a group — the reply carries a
   one-time code that would authorize whoever acts on it first), send `/login`.
   The bot replies with a verification link and a one-time code.
4. Open the link, sign in, and enter the code. The bot confirms in the chat.
5. Send a small prompt in a private allowed chat — no restart needed.

`/login status` reports the auth state, `/login cancel` aborts a pending sign-in,
and `/login force` re-authenticates an already-authorized deployment.

Why device code and not the shell recipe this plan originally carried
(`docker exec -it <bot-container> codex login --device-auth`): that still works,
but it needs shell access to the host, and the plain `codex login` an operator
reaches for first cannot work in a container at all. Plain `codex login` starts a
loopback OAuth server on `http://localhost:1455` and prints an authorize URL whose
`redirect_uri` points back at that loopback — opened on a laptop, the callback
lands on the laptop's own localhost, so the CLI waits forever for a callback that
never arrives, having printed no code. `codex login --device-auth` instead prints
a verification URL plus a one-time code and polls, which is what `core/codexauth`
drives and relays into the chat.

### Billing Setup

1. Deploy with `CODEX_AUTH_MODE=billing`.
2. Set `CODEX_API_KEY`.
3. Set `CODEX_BILLING_ACK=true`.
4. Confirm startup log says API billing is enabled.

### Switching Back to Claude

Set:

```env
AI_BACKEND=claude
```

Claude env and volumes remain unchanged.

## Risks

| Risk | Mitigation |
|---|---|
| Accidental API billing | Fail subscription mode when `CODEX_API_KEY` is set; require `CODEX_BILLING_ACK=true` in billing mode. |
| Codex CLI JSONL schema drift | Pin/record Codex version where possible; fake fixtures; forward-compatible decoder. |
| Headless subscription auth expires | Persist `CODEX_HOME`; document device auth and access token path. |
| Secret leakage into repo commands | Keep secrets outside workspace; avoid broad env inheritance; use Codex shell env policy. |
| Sandbox mismatch in Docker | Install bubblewrap; default to `workspace-write`; allow `danger-full-access` only explicitly. |
| Codex requires git repo | Run inside actual cloned repos where possible; use `--skip-git-repo-check` only for safe workspace tasks. |

## Human-Only Gates

- Choose whether subscription auth is personal ChatGPT login or Business/
  Enterprise access token.
- Perform the first `codex login --device-auth` or provide a managed access
  token.
- Approve any API billing deployment by setting `CODEX_BILLING_ACK=true`.
- Validate one real Codex run in the target region/account.

## References

- Codex CLI: https://developers.openai.com/codex/cli.md
- Non-interactive mode: https://developers.openai.com/codex/noninteractive.md
- Authentication: https://developers.openai.com/codex/auth.md
- Environment variables: https://developers.openai.com/codex/environment-variables.md
- Access tokens: https://developers.openai.com/codex/enterprise/access-tokens.md
- Sandbox: https://developers.openai.com/codex/concepts/sandboxing.md
