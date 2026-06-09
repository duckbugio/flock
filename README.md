# DuckFlock

Run a **Claude Code AI dev team** and drive it from **Telegram** — with an **isolated
workspace per chat**. Describe a feature in a chat; the team plans it, implements it on a
branch, tests it, reviews it (line-anchored inline comments), and opens a PR. It runs on your
Claude **subscription** (no per-token billing) or an Anthropic API key.

A native Go service that drives the Claude Code CLI as a dev team and exposes it through a
Telegram adapter — per-chat isolation, parallel chats, PR poller, voice, … — all shipped as one
prebuilt image.

> **Repo layout (monorepo).** The platform-agnostic dev-team brain — subagents + orchestration —
> lives in [`core/`](core/); the Telegram bot is one **adapter** under
> [`adapters/telegram/`](adapters/telegram/) (its image, compose, and Ansible deploy).
> Future chat platforms get their own `adapters/<name>/` and share `core/`.

## What you get

- A Telegram bot that is a full **Claude Code** agent on your server (read/edit/bash, git).
- A 5-role **dev team** (subagents): **planner → coder → tester → reviewer → arbiter**, with
  a real pipeline — spec + acceptance criteria → build → lint/regression gates → PR →
  adversarial review with **inline comments in the PR's language** → arbiter.
- **Per-chat isolation** — every chat (1:1 or group) gets its own `/workspace/chat_<id>`;
  different chats can't see each other's files. **Different chats are answered in parallel.**
- **PR reactions without inbound webhooks** — the bot *polls* your git host for new review
  comments and addresses them, routed back to the chat that opened the PR.
- **Voice messages** (optional) — transcribed (Mistral Voxtral / OpenAI Whisper / local) and
  run as commands.
- Auth via Claude **Pro/Max subscription** (`claude setup-token`) or an Anthropic API key.
- The agent's shell is **sandboxed inside the container**, not on your host.

## Quick start (Docker — recommended)

```bash
git clone https://github.com/duckbugio/flock
cd flock/adapters/telegram
cp .env.example .env        # fill in the REQUIRED block (5 lines)
docker compose up -d
```

That pulls the prebuilt image `ghcr.io/duckbugio/flock-telegram` — no build, no Ansible. Then
message your bot. The minimum `.env`:

| Variable | What |
|---|---|
| `TELEGRAM_BOT_TOKEN` | from [@BotFather](https://t.me/botfather) |
| `TELEGRAM_BOT_USERNAME` | your bot's @username (no `@`) |
| `ALLOWED_USERS` | comma-separated Telegram user IDs allowed to use the bot |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token` (subscription) — *or* set `ANTHROPIC_API_KEY` |

Everything else in [`.env.example`](adapters/telegram/.env.example) has sensible defaults. Update the image
later with `docker compose pull && docker compose up -d`.

> **Region:** host in an **Anthropic-supported region** (Anthropic geo-blocks some countries,
> e.g. RU/CN) — otherwise Claude calls fail.

## How it works

```
You (in a chat): "implement X across the api + web services"
  → bot's Claude (Lead) → planner → confirm scope → coder ⇄ tester → PR per repo
                                                      → reviewer (inline comments) ⇄ coder → arbiter
                                                                                        ├ APPROVE → you merge
                                                                                        └ ESCALATE → asks you
```

The **arbiter** is the loop-breaker (risk-aware, cycle-limited) so agents never loop forever.
A plain question is just answered; a build request triggers the team. Branches are named
`duck/<chatid>/<slug>` so PR-webhook/poll events route back to the right chat.

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

The **poller** is the recommended way to react to review comments — it works even when your
git host can't reach the bot (e.g. cross-border network filtering), because the bot reaches
*out*. Inbound webhooks + a Caddy TLS proxy are an optional alternative (`--profile caddy`,
see `.env.example`).

## Voice messages (optional)

```ini
VOICE_PROVIDER=mistral        # mistral | openai | local
MISTRAL_API_KEY=...           # console.mistral.ai  (or OPENAI_API_KEY for whisper)
```

## Optional sidecars

```bash
docker compose --profile dind up -d     # dockerized linters/tests for the team (set DOCKER_HOST=tcp://dind:2375)
docker compose --profile caddy up -d    # inbound-webhook TLS proxy (needs a public WEBHOOK_DOMAIN)
```

## Per-chat isolation

Each chat gets `/workspace/chat_<id>`: 1:1 → your private workspace; group → one shared
workspace for that group's members; different chats are fully isolated and run **in parallel**
(capped by `MAX_CONCURRENT_CHAT_RUNS` for memory). The user-ID whitelist gates who may use the
bot. In groups the bot only responds when **@mentioned** or replied to.

## Advanced: deploy to a server with Ansible

For a one-command provision of a fresh VPS (installs Docker + swap, renders `.env`, pulls the
image, starts it):

```bash
cd adapters/telegram/deploy
cp -r inventories/example inventories/my-bot          # one dir per bot instance
$EDITOR inventories/my-bot/inventory.ini              # your server IP + SSH key
$EDITOR inventories/my-bot/group_vars/all/vars.yml    # non-secret config
$EDITOR inventories/my-bot/group_vars/all/vault.yml   # secrets (tokens)
ansible-vault encrypt inventories/my-bot/group_vars/all/vault.yml   # optional
ansible-playbook -i inventories/my-bot/inventory.ini playbook.yml
```

Each bot instance is its own `inventories/<name>/` (e.g. the maintained `gitea-lo-duck`); only
`inventories/example/` is committed — real instances are gitignored. The role pulls the prebuilt
image — set `bot_image` to pin a tag. (Build your own with `docker build -f
adapters/telegram/Dockerfile .` from the repo root if you've forked the repo.)

## The dev team

Roles live in [`core/agents/`](core/agents/) as native Claude Code subagents. The pipeline,
the orchestrator prompt, the guardrails, and the patterns borrowed from
[ruflo](https://github.com/ruvnet/ruflo) (SPARC spec-gates, multi-repo coordination, diff
risk-scoring) are in [core/README.md](core/README.md). The orchestration rules ship in the
image and are rendered into each workspace as `CLAUDE.md`.

## Security

- **Whitelist:** only `ALLOWED_USERS` may use the bot — never leave it empty; the bot grants
  shell/edit access to your server.
- **Per-chat isolation:** different chats get separate workspaces. The git token is shared
  across a deployment — scope it accordingly.
- **Secrets:** keep them in `.env` (gitignored) or, for Ansible, in a real instance's
  `inventories/<name>/group_vars/all/vault.yml` (gitignored; `ansible-vault` encryptable).
  Never commit real tokens — only `inventories/example/` is tracked.
- **Isolation:** the agent runs as a non-root user inside the container; its Bash/Edit are
  confined to the container, not your host.

## License

[MIT](LICENSE).
