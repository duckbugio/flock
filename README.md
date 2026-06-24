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

# Flock

**Run a Claude Code AI dev team on your server and drive it from chat.** Describe a feature in Telegram or VK; the team plans it, builds it on a branch, tests it, reviews it, and opens a PR — each chat in its own isolated workspace.

[![CI](https://github.com/duckbugio/flock/actions/workflows/ci.yml/badge.svg)](https://github.com/duckbugio/flock/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](go.mod)
[![image: ghcr.io](https://img.shields.io/badge/image-ghcr.io-2496ED.svg)](https://github.com/orgs/duckbugio/packages)

It runs on your Claude **Pro/Max subscription** (no per-token billing) or an Anthropic API key, ships as **prebuilt Docker images** (no build step), and keeps every chat in its own sandboxed workspace.

## Quick start (Docker)

```bash
git clone https://github.com/duckbugio/flock
cd flock/adapters/telegram
cp .env.example .env        # fill in the REQUIRED block (4 values)
docker compose up -d
```

That pulls the prebuilt image `ghcr.io/duckbugio/flock-telegram` — no build, no Ansible — then message your bot. The minimum `.env`:

| Variable | What |
|---|---|
| `TELEGRAM_BOT_TOKEN` | from [@BotFather](https://t.me/botfather) |
| `TELEGRAM_BOT_USERNAME` | your bot's @username (no `@`) |
| `ALLOWED_USERS` | comma-separated Telegram user IDs allowed to use the bot |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token` (subscription) — *or* set `ANTHROPIC_API_KEY` |

Everything else in [`.env.example`](adapters/telegram/.env.example) has sensible defaults. Update later with `docker compose pull && docker compose up -d`.

> **Region:** host in an **Anthropic-supported region** (some countries, e.g. RU/CN, are geo-blocked) — otherwise Claude calls fail.

**VK** is the same pattern under [`adapters/vk/`](adapters/vk/), built on the same core and published as `ghcr.io/duckbugio/flock-vk`. It ships only an env template (no compose file): `cp .env.example .env`, then `docker run --env-file .env ghcr.io/duckbugio/flock-vk`. Claude auth and core settings match Telegram; only the three transport vars change:

| Variable | What |
|---|---|
| `VK_BOT_TOKEN` | community access token (VK community → Manage → API usage → access token) |
| `VK_GROUP_ID` | your community's numeric id (long-poll server + mention parse) |
| `VK_ALLOWED_USERS` | comma-separated VK user IDs allowed to use the bot |

## Highlights

- **The conversation is the task source** — describe what you want in chat and review the PR that comes back; the agent's shell and editor are sandboxed inside the container.
- **A real dev-team pipeline, not a single prompt** — spec-first acceptance criteria, build/regression gates, and an arbiter that breaks loops.
- **Multi-transport** — **Telegram** and **VK** today, both on the same core; a new platform is a thin adapter, not a fork.
- **PR reactions without inbound webhooks** — the bot *polls* your git host for new review comments and routes each back to the chat that opened the PR.
- **Subscription-friendly** — authenticate with a Claude Pro/Max token (no per-token cost) or an Anthropic API key.

## How it works

```
You (in a chat): "implement X across the api + web services"
  → bot's Claude (Lead) → planner → confirm scope → coder ⇄ tester → PR per repo
                                                      → reviewer (inline comments) ⇄ coder → arbiter
                                                                                        ├ APPROVE → you merge
                                                                                        └ ESCALATE → asks you
```

The five subagents — **planner → coder → tester → reviewer → arbiter** — run as native Claude Code subagents in [`core/agents/`](core/agents/). A plain question is just answered; a build request triggers the team. The **arbiter** is the risk-aware, cycle-limited loop-breaker so agents never spin forever. Branches are named `duck/<chatid>/<slug>` so PR-webhook/poll events route back to the right chat.

The team is built for a **microservices** workspace: a feature can span several services, and it coordinates branches and one cross-linked PR per repo. The full pipeline, guardrails, and role table live in [`core/README.md`](core/README.md).

## Repo layout (monorepo)

The platform-agnostic dev-team brain lives in [`core/`](core/); each platform is a thin adapter under `adapters/<name>/` that shares it.

| Adapter | Path | Prebuilt image |
|---|---|---|
| Telegram | [`adapters/telegram/`](adapters/telegram/) | `ghcr.io/duckbugio/flock-telegram` |
| VK | [`adapters/vk/`](adapters/vk/) | `ghcr.io/duckbugio/flock-vk` |

Future platforms reuse the same core — see [`docs/multi-transport-plan.md`](docs/multi-transport-plan.md).

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

The **poller** is the recommended way to react to review comments — it reaches *out*, so it works even when your host can't reach the bot. It's active when `ENABLE_PR_REVIEW=true` and `GITEA_API_URL` is set. An inbound-webhook + Caddy TLS proxy alternative is available only through the Ansible deploy (set `webhook_domain`).

## Other options

- **Voice messages:** `ENABLE_VOICE_MESSAGES=true`, `VOICE_PROVIDER=mistral|openai|local`, plus `MISTRAL_API_KEY` (or `OPENAI_API_KEY`). Transcribed and run as commands.
- **dind sidecar:** `docker compose --profile dind up -d` gives the team dockerized linters/tests (set `DOCKER_HOST=tcp://dind:2375`).
- **Per-chat isolation:** each chat gets `/workspace/chat_<id>` (1:1 → private; group → one shared workspace); chats are fully isolated and run in parallel, capped by `MAX_CONCURRENT_CHAT_RUNS`. In groups, set `REQUIRE_GROUP_MENTION=true` to respond only when @mentioned or replied to.
- **Ansible deploy** (Telegram): one-command VPS provision from `adapters/telegram/deploy` — copy `inventories/example` to your own `inventories/<name>/` (gitignored), fill inventory/vars/vault, then `ansible-playbook -i inventories/<name>/inventory.ini playbook.yml`. The role pulls the prebuilt image; set `bot_image` to pin a tag.

## Security

Found a vulnerability? Please disclose it privately — see **[SECURITY.md](SECURITY.md)**. Hardening notes for operators:

- **Whitelist:** only `ALLOWED_USERS` (Telegram) / `VK_ALLOWED_USERS` (VK) may use the bot — never leave it empty; it grants shell/edit access to your server.
- **Per-chat isolation:** different chats get separate workspaces. The git token is shared across a deployment — scope it accordingly.
- **Secrets:** keep them in `.env` (gitignored) or, for Ansible, in a real instance's `vault.yml` (gitignored, `ansible-vault` encryptable). Only `inventories/example/` is tracked.
- **Sandbox:** the agent runs as a non-root user; its Bash/Edit are confined to the container, not your host.

## Build, lint, test

The repo uses [Task](https://taskfile.dev) as its CI runner — the same entrypoint [CI](.github/workflows/ci.yml) uses:

```bash
task lint      # format + vet + linters (in the dev-tools image)
task tests     # Go test suite
task build     # compile the binaries
```

## License

[MIT](LICENSE) © DuckBug.

## Contributing

We welcome contributions! See **[CONTRIBUTING.md](CONTRIBUTING.md)** for the full guide — development setup, the `task` lint/test/build workflow, and conventions. In short:

1. **Fork the repository**: Create your own fork of the repository on GitHub.
2. **Create a new branch**: Make a new branch for your feature or bugfix.
3. **Make your changes**: Implement your feature or fix the bug.
4. **Write tests**: Ensure that your changes are covered by tests.
5. **Submit a pull request**: Push your changes to your fork and submit a pull request to the main repository.

## Code of Conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md). Please be respectful and considerate in your interactions with others; harassment and discrimination of any kind will not be tolerated. Report concerns to conduct@duckbug.io.
