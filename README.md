<p align="center">
  <b>English</b> ·
  <a href="README.ru.md">Русский</a> ·
  <a href="README.zh.md">中文</a> ·
  <a href="README.es.md">Español</a> ·
  <a href="README.de.md">Deutsch</a> ·
  <a href="README.fr.md">Français</a> ·
  <a href="README.pt-BR.md">Português (BR)</a> ·
  <a href="README.ja.md">日本語</a>
</p>

# DuckFlock

**Run a Claude Code AI dev team on your server and drive it from chat.** Describe a feature in Telegram or VK; the team plans it, builds it on a branch, tests it, reviews it, and opens a PR — each chat in its own isolated workspace.

[![CI](https://github.com/duckbugio/flock/actions/workflows/ci.yml/badge.svg)](https://github.com/duckbugio/flock/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](go.mod)
[![image: ghcr.io](https://img.shields.io/badge/image-ghcr.io-2496ED.svg)](https://github.com/orgs/duckbugio/packages)

DuckFlock is a native Go service that drives the Claude Code CLI as a five-role **dev team** and
exposes it through chat. You talk to it like a colleague: a plain question gets answered; "implement
X across the api and web services" kicks off a real pipeline — spec and acceptance criteria → build →
lint and regression gates → PR → adversarial review with line-anchored inline comments → an arbiter
that decides ship or escalate.

It runs on your Claude **Pro/Max subscription** (no per-token billing) or an Anthropic API key, ships
as **prebuilt Docker images** (no build step), and keeps every chat in its own sandboxed workspace.

## Why DuckFlock

- **The conversation is the task source.** No board, no issue queue — describe what you want in chat
  and review the PR that comes back. The agent's shell and editor are **sandboxed inside the
  container**, not on your host.
- **A real dev-team pipeline, not a single prompt.** Five subagents — **planner → coder → tester →
  reviewer → arbiter** — with spec-first acceptance criteria, build and regression gates, and an
  arbiter that breaks loops so agents never spin forever.
- **Multi-transport.** Two co-equal adapters today — **Telegram** and **VK** — both built on the same
  platform-agnostic core. Adding a new platform is a thin adapter, not a fork.
- **Per-chat isolation, run in parallel.** Every chat (1:1 or group) gets its own
  `/workspace/chat_<id>`; chats can't see each other's files, and different chats are answered
  concurrently.
- **PR reactions without inbound webhooks.** The bot *polls* your git host for new review comments
  and addresses them, routing each back to the chat that opened the PR — works even when your host
  can't reach the bot.
- **Subscription-friendly.** Authenticate with a Claude Pro/Max token (`claude setup-token`) — no
  per-token cost — or fall back to an Anthropic API key.
- **Optional voice.** Voice messages transcribed (Mistral Voxtral / OpenAI Whisper / local) and run
  as commands.

## Repo layout (monorepo)

The platform-agnostic dev-team brain — subagents and orchestration — lives in [`core/`](core/). Each
chat platform is a thin **adapter** under `adapters/<name>/` that shares `core/`:

| Adapter | Path | Prebuilt image |
|---|---|---|
| Telegram | [`adapters/telegram/`](adapters/telegram/) | `ghcr.io/duckbugio/flock-telegram` |
| VK | [`adapters/vk/`](adapters/vk/) | `ghcr.io/duckbugio/flock-vk` |

Future platforms (e.g. MAX) get their own `adapters/<name>/` and reuse the same core — see
[`docs/multi-transport-plan.md`](docs/multi-transport-plan.md).

## Quick start (Telegram, Docker — recommended)

```bash
git clone https://github.com/duckbugio/flock
cd flock/adapters/telegram
cp .env.example .env        # fill in the REQUIRED block (4 values)
docker compose up -d
```

That pulls the prebuilt image `ghcr.io/duckbugio/flock-telegram` — no build, no Ansible. Then message
your bot. The minimum `.env`:

| Variable | What |
|---|---|
| `TELEGRAM_BOT_TOKEN` | from [@BotFather](https://t.me/botfather) |
| `TELEGRAM_BOT_USERNAME` | your bot's @username (no `@`) |
| `ALLOWED_USERS` | comma-separated Telegram user IDs allowed to use the bot |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token` (subscription) — *or* set `ANTHROPIC_API_KEY` |

Everything else in [`.env.example`](adapters/telegram/.env.example) has sensible defaults. Update the
image later with `docker compose pull && docker compose up -d`.

> **Region:** host in an **Anthropic-supported region** (Anthropic geo-blocks some countries, e.g.
> RU/CN) — otherwise Claude calls fail.

## Quick start (VK)

VK is the same pattern under [`adapters/vk/`](adapters/vk/), built on the same core and published as
`ghcr.io/duckbugio/flock-vk`. The VK adapter ships only an env template (no compose file); copy it and
run the image with it, e.g. `docker run --env-file .env ghcr.io/duckbugio/flock-vk`:

```bash
cd flock/adapters/vk
cp .env.example .env        # fill in the REQUIRED block
```

The VK `.env` swaps the three transport variables; the Claude auth and all the core settings are
identical to Telegram:

| Variable | What |
|---|---|
| `VK_BOT_TOKEN` | community access token (VK community → Manage → API usage → access token) |
| `VK_GROUP_ID` | your community's numeric id (long-poll server + mention parse) |
| `VK_ALLOWED_USERS` | comma-separated VK user IDs allowed to use the bot |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token` (subscription) — *or* set `ANTHROPIC_API_KEY` |

See [`adapters/vk/.env.example`](adapters/vk/.env.example) for the full list (defaults match the
Telegram adapter).

## How it works

```
You (in a chat): "implement X across the api + web services"
  → bot's Claude (Lead) → planner → confirm scope → coder ⇄ tester → PR per repo
                                                      → reviewer (inline comments) ⇄ coder → arbiter
                                                                                        ├ APPROVE → you merge
                                                                                        └ ESCALATE → asks you
```

The **arbiter** is the loop-breaker (risk-aware, cycle-limited) so agents never loop forever. A plain
question is just answered; a build request triggers the team. Branches are named
`duck/<chatid>/<slug>` so PR-webhook/poll events route back to the right chat.

The dev team is designed for a **microservices** workspace: a feature can span several services, and
the team coordinates branches and one cross-linked PR per repo. The full pipeline, guardrails, and
role table live in [`core/README.md`](core/README.md).

## The dev team

Roles live in [`core/agents/`](core/agents/) as native Claude Code subagents:

| Subagent | Job |
|---|---|
| [`planner`](core/agents/planner.md) | Spec (≥3 acceptance criteria) + affected services + ordered plan + risks |
| [`coder`](core/agents/coder.md) | Implements on a branch; contract-safe across services |
| [`tester`](core/agents/tester.md) | Runs/writes tests incl. the cross-service seam; green-build gate |
| [`reviewer`](core/agents/reviewer.md) | Adversarial review + RISK score + cross-service checks |
| [`arbiter`](core/agents/arbiter.md) | Enforces the gate; decides continue/ship/escalate — the loop-breaker |

The pipeline borrows patterns (not the framework) from
[ruflo](https://github.com/ruvnet/ruflo) — SPARC spec-gates, multi-repo coordination, and diff
risk-scoring. The orchestration rules ship in the image and are rendered into each workspace as
`CLAUDE.md`. See [`core/README.md`](core/README.md) for the details.

## Connect a git host (optional but core)

Set these in `.env` to let the team clone repos and open PRs (works with **Gitea/GitHub/GitLab**):

```ini
GIT_HOST=git.example.com
GIT_USER=...
GIT_TOKEN=...                 # write-scoped PAT
GIT_AUTHOR_NAME=AI Team
GIT_AUTHOR_EMAIL=ai@example.com
# Poll the host for new PR comments (reliable; no inbound webhook needed):
GITEA_API_URL=https://git.example.com/api/v1
GITEA_POLL_INTERVAL=90
```

For **github.com**, also set `GH_TOKEN` (= your `GIT_TOKEN`) so the `gh` CLI can open PRs.

The **poller** is the recommended way to react to review comments — it works even when your git host
can't reach the bot (e.g. cross-border network filtering), because the bot reaches *out*. It's active
when `ENABLE_PR_REVIEW=true` and `GITEA_API_URL` is set. Inbound webhooks plus a Caddy TLS proxy are
an optional alternative, available only through the Ansible deploy (set `webhook_domain` in the
instance's `group_vars/all/vars.yml`; see the Ansible section below).

## GitHub star nudge (optional, GitHub-only)

When the bot is wired to a GitHub account (`GIT_HOST=github.com` + `GIT_TOKEN`) that has not yet
starred the project, it sends a short message after each successful run inviting you to star it, with
an inline button. Pressing the button stars the repo from the deployment's own account, and the nudge
then stops for good. It is GitHub-only and **auto-off** everywhere else (the gate is the off switch —
no separate flag); it runs entirely off the hot path, so it never delays or breaks a run.

```ini
STAR_NUDGE_REPO=duckbugio/flock   # owner/repo to nudge for / star
# STAR_NUDGE_STORE_PATH=          # default <APPROVED_DIRECTORY>/star_nudge.json
```

The button needs a token with **star-write scope**: a classic PAT with `public_repo`, or a
fine-grained token with the "Starring" account permission. Without it the star simply fails softly
(the run is unaffected).

## Voice messages (optional)

```ini
ENABLE_VOICE_MESSAGES=true
VOICE_PROVIDER=mistral        # mistral | openai | local
MISTRAL_API_KEY=...           # console.mistral.ai  (or OPENAI_API_KEY for whisper)
```

## Optional sidecars (Telegram adapter)

```bash
docker compose --profile dind up -d     # dockerized linters/tests for the team (set DOCKER_HOST=tcp://dind:2375)
```

(The inbound-webhook TLS proxy is provisioned by the Ansible deploy, not a compose profile — set
`webhook_domain` in the instance's `group_vars/all/vars.yml` to enable it.)

## Per-chat isolation

Each chat gets `/workspace/chat_<id>`: 1:1 → your private workspace; group → one shared workspace for
that group's members; different chats are fully isolated and run **in parallel** (capped by
`MAX_CONCURRENT_CHAT_RUNS` for memory). The user-ID whitelist gates who may use the bot. In groups,
set `REQUIRE_GROUP_MENTION=true` to make the bot respond only when @mentioned or replied to.

## Advanced: deploy to a server with Ansible (Telegram adapter)

For a one-command provision of a fresh VPS (installs Docker + swap, renders `.env`, pulls the image,
starts it):

```bash
cd adapters/telegram/deploy
cp -r inventories/example inventories/my-bot          # one dir per bot instance
$EDITOR inventories/my-bot/inventory.ini              # your server IP + SSH key
$EDITOR inventories/my-bot/group_vars/all/vars.yml    # non-secret config
$EDITOR inventories/my-bot/group_vars/all/vault.yml   # secrets (tokens)
ansible-vault encrypt inventories/my-bot/group_vars/all/vault.yml   # optional
ansible-playbook -i inventories/my-bot/inventory.ini playbook.yml
```

Each bot instance is its own `inventories/<name>/`; only `inventories/example/` is committed — real
instances are gitignored. The role pulls the prebuilt image — set `bot_image` to pin a tag. (To build
your own image after forking, run `docker build -f adapters/telegram/Dockerfile .` from the repo root;
the VK image is `docker build -f adapters/vk/Dockerfile .`.)

## Security

- **Whitelist:** only the allowed-users list (`ALLOWED_USERS` for Telegram, `VK_ALLOWED_USERS` for VK)
  may use the bot — never leave it empty; the bot grants shell/edit access to your server.
- **Per-chat isolation:** different chats get separate workspaces. The git token is shared across a
  deployment — scope it accordingly.
- **Secrets:** keep them in `.env` (gitignored) or, for Ansible, in a real instance's
  `inventories/<name>/group_vars/all/vault.yml` (gitignored; `ansible-vault` encryptable). Never
  commit real tokens — only `inventories/example/` is tracked.
- **Isolation:** the agent runs as a non-root user inside the container; its Bash/Edit are confined to
  the container, not your host.

## Build, lint, test

The repo uses [Task](https://taskfile.dev) as its CI runner — the same entrypoint
[CI](.github/workflows/ci.yml) uses:

```bash
task lint      # format + vet + linters (in the dev-tools image)
task tests     # Go test suite
task build     # compile the binaries
```

CI additionally runs `task security-scan` (govulncheck) and a Trivy filesystem scan, and dry-builds
both adapter images on every PR.

## License

[MIT](LICENSE) © DuckBug.
