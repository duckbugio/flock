# AI Provider Architecture Plan

> Status: full technical specification for evolving Flock from a Claude/Codex
> selector into a provider-based AI backend architecture. This document starts
> from the current implementation on the `codex/codex-backend` branch, where
> `AI_BACKEND=claude|codex|openai-compatible` is already wired through
> `internal/airunner` and provider packages.
>
> Implementation note: the provider-foundation pass on this branch adds
> `core/agent`, moves `core/chat` onto neutral agent types, and turns
> `internal/airunner` into a provider registry for Claude, Codex, and an
> answer-only OpenAI-compatible provider. Qwen API-style access is already
> covered by `AI_BACKEND=openai-compatible`; Qwen Code remains a follow-up CLI
> provider package.

## 1. Goal

Make it easy and safe to add new AI services such as Qwen, DeepSeek, OpenRouter,
local Ollama/vLLM, or future coding-agent CLIs without rewriting Telegram/VK
adapters, `core/chat.Service`, dispatcher, sessions, progress rendering, cost
guardrails, or workspace logic.

The target result is a provider architecture:

```text
Telegram / VK adapters
        |
        v
core/chat.Service
        |
        v
core/agent.Runner
        |
        +-- providers/claudecli
        +-- providers/codexcli
        +-- providers/qwencode
        +-- providers/openai-compatible
        +-- providers/local-openai-compatible
```

Adding a new provider must become a small, repeatable task:

1. Add one provider package.
2. Register it in the provider registry.
3. Add config/env fields and validation.
4. Add fake-binary or fake-HTTP tests.
5. Update Docker/runtime docs if the provider needs a CLI.

## 2. Non-goals

- Do not redesign Telegram/VK transports.
- Do not change the per-chat workspace model.
- Do not require per-message backend switching in the first implementation.
- Do not force all providers to support the same features. Capabilities must be
  explicit.
- Do not hide API billing behind subscription-style names.
- Do not build a full coding-agent tool loop for raw chat APIs in the first
  provider refactor.

## 3. Current State

Current implementation on this branch:

- Provider-neutral runtime types live in `core/agent`.
- Claude backend lives in `core/claude`.
- Codex backend lives in `core/codex`.
- OpenAI-compatible answer-only backend lives in `core/openaicompat`.
- `cmd/flock-telegram` and `cmd/duck-vk` call `internal/airunner.Build(cfg)`.
- `internal/airunner` is a provider registry, not a hardcoded
  `if claude else codex` selector.
- `AI_BACKEND=claude|codex|openai-compatible` selects the runner.
- `core/chat.Service` consumes `agent.Runner`, `agent.Options`, and
  `agent.Event`, so Telegram/VK code does not care which provider is selected.

Immediate remaining architecture work:

```text
The shared runtime contract is neutral. The next cleanup is package layout:
move provider implementations into dedicated provider packages when the diff is
worth it, and add Qwen Code as another provider without touching core/chat.
```

## 4. Provider Model

Introduce a neutral package:

```text
core/agent
```

It owns the stable internal contract:

```go
package agent

type Runner interface {
    Run(ctx context.Context, prompt string, opts Options) (<-chan Event, error)
}

type Provider interface {
    Name() string
    DisplayName() string
    Capabilities() Capabilities
}
```

`Runner` is the runtime interface used by `chat.Service`.

`agent.Provider` exposes stable metadata (`Name`, `DisplayName`,
`Capabilities`). `internal/airunner.Provider` extends it with config validation,
aliases, and construction because provider construction depends on
`internal/config`. Provider construction happens once at startup; each `Run`
still starts a one-shot CLI process or one API request loop depending on
provider type.

## 5. Neutral Runtime Types

Move these types from `core/claude` into `core/agent`:

```go
type EventType int

const (
    SystemInit EventType = iota
    Text
    ToolUse
    ToolResult
    Result
    RunError
)

type Event struct {
    Type      EventType
    Text      string
    Tool      string
    ToolInput json.RawMessage
    SessionID string
    Result    *RunResult
    Err       error
}

type RunResult struct {
    Text       string
    SessionID  string
    NumTurns   int
    CostUSD    float64
    DurationMS int64
    IsError    bool
    Subtype    string
}

type Options struct {
    SessionID string
    Workdir   string
    Model     string
    MaxTurns  int
    MCPConfig string
    Effort    string
    Env       []string
    Images    []ImageInput
}

type ImageInput struct {
    MediaType string
    Data      []byte
    Path      string
}
```

Compatibility rule for migration:

- Phase 1 may keep `core/claude` type aliases to reduce diff size:

```go
type Event = agent.Event
type Runner = agent.Runner
type Options = agent.Options
```

- Phase 2 removes direct `core/claude` imports from `core/chat`, adapters, and
  tests. After that, `core/claude` is only a provider implementation.

## 6. Capabilities

Not all providers can do the same things. The bot must know capabilities at
startup and at runtime.

```go
type Capabilities struct {
    ResumeSessions       bool
    StreamEvents         bool
    ToolEvents           bool
    ShellExecution       bool
    FileEditing          bool
    Images               bool
    CostReporting        bool
    SubscriptionAuth     bool
    APIKeyBilling        bool
    RequiresGitRepo      bool
    SupportsSandbox      bool
    SupportsApprovalMode bool
}
```

Examples:

| Provider | Resume | Tools/files | Images | Cost | Auth |
|---|---:|---:|---:|---:|---|
| `claude-cli` | yes | yes | yes | yes | OAuth or Anthropic API key |
| `codex-cli` | yes | yes | later | limited | ChatGPT/Codex auth or API key |
| `qwen-code` | maybe | yes | later | provider-specific | Qwen auth/API key |
| `openai-compatible` | no initially | no initially | model-dependent | estimated | API key |
| `local-openai-compatible` | no initially | no initially | model-dependent | no | local/no key |

Runtime behavior:

- If `ResumeSessions=false`, do not persist provider session ids; each message is
  stateless unless the provider package implements its own conversation summary.
- If `ToolEvents=false`, progress UI should show generic "Working..." updates
  and text deltas only.
- If `CostReporting=false`, do not enforce dollar cost caps from provider usage;
  rate limits still apply.
- If `Images=false`, media uploads fall back to saved-path prompts or a clear
  unsupported notice.

## 7. Provider Registry

Create a registry package:

```text
internal/airunner
```

Final shape:

```go
type Registry struct {
    providers map[string]agent.Provider
}

func DefaultRegistry() Registry {
    r := NewRegistry()
    r.Register(claudecli.Provider{})
    r.Register(codexcli.Provider{})
    r.Register(qwencode.Provider{})
    r.Register(openai.Provider{})
    return r
}

func Build(cfg config.Config) (agent.Runner, agent.Options, agent.ProviderInfo, error)
```

Provider names:

- `claude`
- `codex`
- `qwen-code`
- `openai-compatible`
- `local-openai-compatible`

Rules:

- Empty `AI_BACKEND` defaults to `claude`.
- Unknown `AI_BACKEND` fails startup. With multiple providers, a typo such as
  `qwen` vs `qwen-code` must not silently run Claude.
- Provider aliases may be accepted explicitly:
  - `claude-cli` -> `claude`
  - `codex-cli` -> `codex`
  - `openai` -> `openai-compatible`
  - `openai-compatible-api` -> `openai-compatible`

Use `AI_BACKEND=openai-compatible` for Qwen/DashScope API access today. Reserve
`qwen-code` for the future CLI coding-agent provider so a short `qwen` alias does
not silently switch between two different security and billing models.

## 8. Config Strategy

Keep a shared top-level selector:

```env
AI_BACKEND=claude
```

Use provider-prefixed config:

```env
CLAUDE_*
CODEX_*
QWEN_*
OPENAI_COMPAT_*
LOCAL_AI_*
```

Avoid generic names like `AI_API_KEY` for secrets unless they point to another
env key. Generic names become dangerous when multiple providers are configured
at once.

Recommended config structs:

```go
type Config struct {
    AIBackend string `env:"AI_BACKEND" envDefault:"claude"`

    Claude ClaudeConfig
    Codex  CodexConfig
    Qwen   QwenConfig
    OpenAICompat OpenAICompatConfig
}
```

`caarlos0/env` supports nested structs with prefixes if configured; if that is
too invasive, keep flat fields for the first migration and group them with
methods.

## 9. Auth and Billing Rules

Provider auth must be explicit. The bot must never switch from account/subscription
auth to API billing because an API key happens to exist in the container env.

Common auth mode pattern:

```env
<PROVIDER>_AUTH_MODE=subscription|billing|local
```

Provider-specific examples:

```env
CODEX_AUTH_MODE=subscription
CODEX_API_KEY=
CODEX_BILLING_ACK=false
```

```env
QWEN_AUTH_MODE=api_key
QWEN_API_KEY=
QWEN_BILLING_ACK=true
```

```env
OPENAI_COMPAT_AUTH_MODE=api_key
OPENAI_COMPAT_API_KEY_ENV=DASHSCOPE_API_KEY
OPENAI_COMPAT_BILLING_ACK=true
```

Validation rules:

- Subscription/account mode rejects API key env for that provider.
- Billing/API-key mode requires:
  - API key present.
  - Billing acknowledgement flag set.
  - Startup log saying billing mode is enabled, without printing secrets.
- Local mode rejects cloud API keys unless explicitly allowed.
- Secrets should be added only to the child process env or HTTP client config
  for that provider, not to repo test/build command env where possible.

## 10. CLI Provider Contract

Many coding agents are easiest to integrate as CLIs. Define a helper contract
for CLI-style providers without forcing inheritance:

```go
type CLIConfig struct {
    Bin        string
    Home       string
    Model      string
    Env        []string
    Workdir    string
    ExtraArgs  []string
}
```

Each CLI provider must implement:

- argument builder,
- env builder,
- stdout event decoder,
- stderr tail,
- process-group cancellation,
- startup validation,
- fake-binary tests.

Process rules:

- One CLI process per chat message.
- `cmd.Dir = opts.Workdir`.
- Kill process group on cancel/timeout.
- Keep bounded stderr tail for errors.
- Use scanner buffer at least 16 MiB.
- Never print secrets in args, logs, errors, or test snapshots.

## 11. API Provider Contract

OpenAI-compatible APIs are useful for Qwen/DeepSeek/OpenRouter/local servers, but
raw chat APIs are not automatically coding agents. Start with a conservative
API provider:

```text
openai-compatible provider v1 = answer-only assistant
```

It may:

- send the prompt plus lightweight workspace context,
- stream assistant text,
- return final text,
- estimate token/cost if usage is returned.

It must not:

- edit files,
- run shell commands,
- claim PR-building capability,
- run project tests,
- pretend to support Claude/Codex-style tool progress.

Later v2 can add a controlled tool loop:

- `read_file`
- `list_files`
- `write_file`
- `run_command`
- allow/deny policy
- sandbox boundaries
- tool event mapping

That should be a separate spec because it changes the security model.

## 12. Provider Packages

Target layout:

```text
core/agent/
  types.go
  registry.go
  capabilities.go

core/providers/
  claudecli/
    provider.go
    runner.go
    decode.go
    tail.go
    runner_test.go
  codexcli/
    provider.go
    runner.go
    decode.go
    tail.go
    runner_test.go
  qwencode/
    provider.go
    runner.go
    decode.go
    runner_test.go
  openaicompat/
    provider.go
    client.go
    stream.go
    client_test.go
```

If moving directories is too large for one PR, keep current packages temporarily:

```text
core/claude
core/codex
core/qwen
core/openaicompat
```

But still move the shared contract to `core/agent`.

## 13. Claude Provider

Name: `claude`

Existing behavior must remain unchanged:

- binary: `CLAUDE_BIN`, default `claude`
- model: `CLAUDE_MODEL`
- auth:
  - `CLAUDE_CODE_OAUTH_TOKEN`
  - `ANTHROPIC_API_KEY`
- stream:
  - `--print --output-format stream-json --verbose`
- resume:
  - `--resume <session_id>`
- workspace:
  - `--add-dir <workdir>`
- MCP:
  - `--mcp-config`
- permissions:
  - current `--permission-mode bypassPermissions`

Migration acceptance:

- Golden/fake Claude tests pass before and after the provider refactor.
- Existing Telegram/VK tests pass unchanged or with only import rename changes.

## 14. Codex Provider

Name: `codex`

Current implementation should become the Codex provider.

Config:

```env
CODEX_BIN=codex
CODEX_MODEL=gpt-5.5
CODEX_HOME=/home/claude/.codex
CODEX_AUTH_MODE=subscription
CODEX_ACCESS_TOKEN=
CODEX_API_KEY=
CODEX_REQUIRE_AUTH=true
CODEX_BILLING_ACK=false
CODEX_SANDBOX=workspace-write
CODEX_APPROVAL_POLICY=never
CODEX_SKIP_GIT_REPO_CHECK=false
CODEX_EXTRA_ARGS=
```

Command shape:

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

Event mapping:

| Codex JSONL | Agent event |
|---|---|
| `thread.started` | `SystemInit` |
| `item.started` command execution | `ToolUse` |
| `item.completed` command execution | `ToolResult` |
| `item.completed` agent message | `Text` |
| `turn.completed` | `Result` |
| `turn.failed` / `error` | `RunError` |

## 15. Qwen Code Provider

Name: `qwen-code`

Purpose:

Integrate Qwen Code as a CLI coding-agent provider where available. This is the
preferred Qwen path if the operator wants a coding assistant that can inspect and
modify a repository, not only answer chat prompts.

Expected config:

```env
AI_BACKEND=qwen-code
QWEN_BIN=qwen
QWEN_HOME=/home/claude/.qwen
QWEN_MODEL=qwen3-coder-plus
QWEN_AUTH_MODE=api_key
QWEN_API_KEY=
QWEN_BILLING_ACK=true
QWEN_BASE_URL=
QWEN_EXTRA_ARGS=
```

Important product note:

- Qwen Code docs describe Qwen OAuth / Qwen Code authentication and API-key
  paths, but availability and pricing can change. Treat Qwen account auth as a
  provider-specific billing mode, not as equivalent to Codex subscription. The
  implementation must validate the exact supported auth mechanism at build time
  and document the operator setup.

Implementation stages:

1. Probe CLI help in Docker image:
   - `qwen --version`
   - `qwen --help`
   - `qwen -p --help`
2. Identify machine-readable output:
   - If Qwen Code has JSON/stream output, map it directly.
   - If it only emits text, support `Text` and `Result` first, with generic
     progress.
3. Add fake `qwen` binary tests for:
   - args,
   - env,
   - successful final answer,
   - non-zero exit,
   - secret redaction.
4. Add Docker install only after official installation path is confirmed.

Minimum acceptable v1:

- Can run a prompt non-interactively.
- Can operate in `opts.Workdir`.
- Can return final text.
- Can fail cleanly with stderr tail.
- Does not claim resume support unless the CLI has stable session ids.

## 16. OpenAI-Compatible Provider

Name: `openai-compatible`

Purpose:

Support providers that expose OpenAI-compatible HTTP APIs:

- Alibaba Cloud Model Studio / DashScope Qwen,
- DeepSeek,
- OpenRouter,
- self-hosted LiteLLM,
- vLLM OpenAI server,
- LM Studio server,
- Ollama OpenAI-compatible endpoint.

Config:

```env
AI_BACKEND=openai-compatible
OPENAI_COMPAT_BASE_URL=https://dashscope-intl.aliyuncs.com/compatible-mode/v1
OPENAI_COMPAT_MODEL=qwen-plus
OPENAI_COMPAT_API_KEY_ENV=DASHSCOPE_API_KEY
OPENAI_COMPAT_AUTH_MODE=api_key
OPENAI_COMPAT_BILLING_ACK=true
OPENAI_COMPAT_TIMEOUT_SECONDS=300
```

Provider behavior v1:

- Use the OpenAI-compatible chat/completions or responses endpoint.
- Use a non-streaming Chat Completions request first for maximum compatibility.
- Return `Result` with final text.
- Do not emit tool events unless a controlled tool loop exists.
- Do not persist `SessionID` unless the API returns a stable conversation id
  and the provider supports resume semantics.

Security:

- Do not expose API key to project commands.
- Do not include full workspace files by default.
- Optional workspace context must be small, explicit, and bounded.

## 17. Local OpenAI-Compatible Provider

Name: `local-openai-compatible`

Purpose:

Use a local model server for low-cost/offline answering.

Config:

```env
AI_BACKEND=local-openai-compatible
LOCAL_AI_BASE_URL=http://host.docker.internal:1234/v1
LOCAL_AI_MODEL=qwen2.5-coder
LOCAL_AI_AUTH_MODE=local
LOCAL_AI_TIMEOUT_SECONDS=300
```

Rules:

- No API key required by default.
- No cost tracking.
- No file editing/tool execution in v1.
- Must log clearly that this is answer-only unless a tool loop is enabled.

## 18. Progress UI Mapping

Provider events should map into current progress behavior:

- `ToolUse` updates active work line.
- `ToolResult` completes/advances tool state.
- `Text` may be ignored for progress and saved for final result.
- `Result` replaces progress message with final answer.
- `RunError` replaces progress message with a clear failure notice.

For answer-only providers:

- Emit periodic generic progress ticks from `chat.Service` as today.
- Do not fabricate tool names.
- Final result still uses the same chunking/rendering path.

## 19. Sessions and Resume

Session semantics must be provider-specific:

```go
type SessionPolicy int

const (
    SessionNone SessionPolicy = iota
    SessionProviderID
    SessionSyntheticSummary
)
```

Rules:

- Claude: `SessionProviderID`.
- Codex: `SessionProviderID`.
- Qwen Code: depends on CLI support.
- OpenAI-compatible v1: `SessionNone`.
- Future chat API with summary memory: `SessionSyntheticSummary`.

Do not store fake provider session ids just to fit the old model. If a provider
cannot resume, each run should start fresh and this should be visible in
capabilities/startup logs.

## 20. Cost and Rate Guardrails

Rate limits remain provider-independent.

Cost caps depend on provider:

- If provider reports real cost: use reported cost.
- If provider reports tokens and pricing is configured: estimate cost.
- If provider is subscription/account-based and cost is not real billing:
  disable dollar cost cap or treat it as token-budget cap only if explicitly
  configured.
- If provider is local: no cost cap.

Add future config:

```env
AI_COST_MODE=reported|estimated|disabled
AI_INPUT_TOKEN_PRICE_USD_PER_1M=
AI_OUTPUT_TOKEN_PRICE_USD_PER_1M=
```

Do not use notional costs to deny users unless the operator opted into that
behavior.

## 21. Docker Runtime

Base runtime must include only common tools:

- git,
- ripgrep,
- ffmpeg,
- chromium/shot helper,
- docker CLI,
- bubblewrap where Codex-like sandboxing needs it.

Provider-specific CLI installs:

- Claude CLI: keep existing install.
- Codex CLI: keep official standalone install.
- Qwen Code: add only after official install command and headless behavior are
  verified.

Provider state volumes:

```yaml
volumes:
  claude_home:
  codex_home:
  qwen_home:
```

Mounts:

```yaml
- claude_home:/home/claude/.claude
- codex_home:/home/claude/.codex
- qwen_home:/home/claude/.qwen
```

Do not store provider auth inside `/workspace`.

## 22. Configuration Examples

Claude:

```env
AI_BACKEND=claude
CLAUDE_CODE_OAUTH_TOKEN=...
ANTHROPIC_API_KEY=
CLAUDE_MODEL=claude-opus-4-8
```

Codex subscription:

```env
AI_BACKEND=codex
CODEX_AUTH_MODE=subscription
CODEX_HOME=/home/claude/.codex
CODEX_API_KEY=
CODEX_BILLING_ACK=false
CODEX_MODEL=gpt-5.5
```

Codex billing:

```env
AI_BACKEND=codex
CODEX_AUTH_MODE=billing
CODEX_API_KEY=sk-...
CODEX_BILLING_ACK=true
```

Qwen Code CLI:

```env
AI_BACKEND=qwen-code
QWEN_BIN=qwen
QWEN_HOME=/home/claude/.qwen
QWEN_MODEL=qwen3-coder-plus
QWEN_AUTH_MODE=api_key
QWEN_API_KEY=...
QWEN_BILLING_ACK=true
```

Alibaba/Qwen OpenAI-compatible:

```env
AI_BACKEND=openai-compatible
OPENAI_COMPAT_BASE_URL=https://dashscope-intl.aliyuncs.com/compatible-mode/v1
OPENAI_COMPAT_MODEL=qwen-plus
OPENAI_COMPAT_API_KEY_ENV=DASHSCOPE_API_KEY
OPENAI_COMPAT_BILLING_ACK=true
DASHSCOPE_API_KEY=...
```

This is the recommended Qwen path today. It is answer-only in v1: it can answer
through the bot but will not inspect/edit the repository or run commands.

Local LM Studio:

```env
AI_BACKEND=local-openai-compatible
LOCAL_AI_BASE_URL=http://host.docker.internal:1234/v1
LOCAL_AI_MODEL=qwen2.5-coder
LOCAL_AI_AUTH_MODE=local
```

## 23. Implementation Plan

### Phase 0: Stabilize Current Codex Branch

- Keep current `AI_BACKEND=claude|codex` behavior while adding provider
  foundation.
- Ensure tests cover subscription mode rejecting `CODEX_API_KEY`.
- Ensure Docker persists `codex_home`.
- Merge only after current behavior is green.

Status on this branch: complete.

### Phase 1: Extract `core/agent`

- Add `core/agent` with neutral types.
- Add type aliases in `core/claude` for compatibility.
- Change `core/chat`, adapters, and tests to import `core/agent`.
- Keep Claude behavior unchanged.
- Tests: full `go test ./...`.

Status on this branch: complete.

### Phase 2: Provider Registry

- Introduce `agent.Provider` and provider registry.
- Convert `internal/airunner.Build` to registry lookup.
- Register Claude and Codex.
- Unknown backend becomes startup error.
- Add tests for:
  - default Claude,
  - Codex selection,
  - unknown backend error,
  - provider validation errors.

Status on this branch: complete.

### Phase 3: Move Providers Into Dedicated Packages

- Move or alias:
  - `core/claude` -> `core/providers/claudecli`
  - `core/codex` -> `core/providers/codexcli`
- Keep compatibility imports temporarily if needed.
- Remove old provider-neutral aliases from `core/claude` after consumers move.

### Phase 4: Add Qwen Code Provider

- Confirm official CLI install and headless run command.
- Add fake CLI tests first.
- Implement text-only v1 if no stable JSON stream exists.
- Add Docker install and `qwen_home` volume.
- Add `.env.example` and Ansible vars.

Status on this branch: future work. Qwen API access is covered earlier through
`openai-compatible`; this phase is only for Qwen Code CLI as a coding agent.

### Phase 5: Add OpenAI-Compatible Provider

- Add HTTP client. Start with non-streaming Chat Completions for maximum
  compatibility.
- Support answer-only mode.
- Add fake HTTP server tests.
- Add config for base URL, model, API key env, timeout.
- Add docs warning that this is not a full coding-agent mode yet.

Status on this branch: v1 complete.

### Phase 6: Optional Tool Loop for API Providers

Separate future spec. This is where raw APIs can become coding agents.

## 24. Testing Requirements

Unit tests:

- Config validation per provider.
- Registry selection.
- Secret rejection in subscription/local modes.
- Billing ack required in API-key modes.
- CLI arg construction.
- CLI env construction.
- JSON/text stream decoders.
- Non-zero CLI exit with stderr tail.
- API stream parser.
- HTTP error handling.

Integration-style tests:

- Fake Claude binary.
- Fake Codex binary.
- Fake Qwen binary.
- Fake OpenAI-compatible HTTP server.

Regression tests:

- Existing Claude tests must pass.
- Existing Telegram/VK message path tests must pass.
- Session persistence works for providers with `ResumeSessions=true`.
- Session persistence is skipped for providers with `ResumeSessions=false`.

Verification commands:

```bash
go test ./...
git diff --check
docker compose -f adapters/telegram/docker-compose.yml config
```

For Docker image changes:

```bash
docker build -f adapters/telegram/Dockerfile .
docker build -f adapters/vk/Dockerfile .
```

## 25. Acceptance Criteria

Architecture:

- `core/chat` imports `core/agent`, not `core/claude`.
- `cmd/*` does not construct provider packages directly.
- New providers register through one registry.
- Provider capabilities are visible in startup logs.

Safety:

- Account/subscription modes reject API keys for the same provider.
- API-key modes require billing ack.
- Secrets are never logged.
- Provider auth state is stored outside `/workspace`.

Behavior:

- Default deploy still uses Claude.
- Existing Claude behavior is unchanged.
- Codex remains selectable and tested.
- Qwen API access works through `openai-compatible` without changes to
  Telegram/VK adapters.
- Adding Qwen Code CLI later requires no changes to `core/chat`.
- Adding OpenAI-compatible answer-only provider requires no changes to
  Telegram/VK adapters.

Docs:

- `.env.example` documents every provider mode.
- Ansible templates expose provider selector and secrets.
- Docker compose includes provider home volumes only when needed or documents
  always-on empty volumes.

## 26. Risks and Mitigations

| Risk | Mitigation |
|---|---|
| Provider CLI output format changes | Fake fixtures plus tolerant decoders; unknown events ignored. |
| API billing accidentally enabled | Per-provider auth validation and billing ack. |
| A raw API provider is mistaken for a coding agent | Capabilities and startup log say answer-only; no fake tool events. |
| Session model differs by provider | Explicit session policy per provider. |
| Docker image becomes too large | Keep provider installs staged and optional where possible. |
| Provider-specific config spreads through app code | Registry/build layer owns provider config mapping. |
| Qwen auth/pricing changes | Treat Qwen auth modes as provider-specific and verify official docs before implementation. |

## 27. Open Questions

1. Should unknown `AI_BACKEND` fail startup immediately in the next PR, or only
   after all docs/templates are updated?
2. Should Qwen Code be installed in the default image or in a separate image tag?
3. Should answer-only providers be allowed in production chats, or should they
   require `AI_ALLOW_ANSWER_ONLY=true`?
4. Should provider selection remain per-container only, or should a later phase
   allow per-chat provider routing?

## 28. Official References

- Codex CLI JSONL, auth, sandboxing, and env behavior:
  <https://developers.openai.com/codex/>
- Qwen Code documentation:
  <https://qwenlm.github.io/qwen-code-docs/>
- Alibaba Cloud Model Studio OpenAI-compatible API:
  <https://www.alibabacloud.com/help/en/model-studio/compatibility-of-openai-with-dashscope>
