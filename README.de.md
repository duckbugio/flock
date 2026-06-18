<p align="center">
  <a href="README.md">English</a> ·
  <a href="README.ru.md">Русский</a> ·
  <a href="README.zh.md">中文</a> ·
  <a href="README.es.md">Español</a> ·
  <b>Deutsch</b> ·
  <a href="README.fr.md">Français</a> ·
  <a href="README.pt-BR.md">Português (BR)</a> ·
  <a href="README.ja.md">日本語</a>
</p>

# DuckFlock

**Betreibe ein Claude Code KI-Entwicklungsteam auf deinem Server und steuere es per Chat.** Beschreibe ein Feature in Telegram oder VK; das Team plant es, baut es auf einem Branch, testet es, prüft es und öffnet einen PR — jeder Chat in seinem eigenen isolierten Workspace.

[![CI](https://github.com/duckbugio/flock/actions/workflows/ci.yml/badge.svg)](https://github.com/duckbugio/flock/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](go.mod)
[![image: ghcr.io](https://img.shields.io/badge/image-ghcr.io-2496ED.svg)](https://github.com/orgs/duckbugio/packages)

Es läuft über dein Claude-**Pro/Max-Abonnement** (keine Abrechnung pro Token) oder einen Anthropic-API-Key, wird als **vorgefertigte Docker-Images** ausgeliefert (kein Build-Schritt) und hält jeden Chat in seinem eigenen sandboxed Workspace.

## Schnellstart (Docker)

```bash
git clone https://github.com/duckbugio/flock
cd flock/adapters/telegram
cp .env.example .env        # fill in the REQUIRED block (4 values)
docker compose up -d
```

Das zieht das vorgefertigte Image `ghcr.io/duckbugio/flock-telegram` — kein Build, kein Ansible — schreibe anschließend deinem Bot. Die minimale `.env`:

| Variable | Beschreibung |
|---|---|
| `TELEGRAM_BOT_TOKEN` | von [@BotFather](https://t.me/botfather) |
| `TELEGRAM_BOT_USERNAME` | der @username deines Bots (ohne `@`) |
| `ALLOWED_USERS` | kommagetrennte Telegram-Benutzer-IDs, die den Bot verwenden dürfen |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token` (Abonnement) — *oder* setze `ANTHROPIC_API_KEY` |

Alles Übrige in [`.env.example`](adapters/telegram/.env.example) hat sinnvolle Standardwerte. Aktualisiere es später mit `docker compose pull && docker compose up -d`.

> **Region:** Hoste in einer **von Anthropic unterstützten Region** (einige Länder, z. B. RU/CN, sind geoblockt) — andernfalls schlagen Claude-Aufrufe fehl.

**VK** folgt demselben Muster unter [`adapters/vk/`](adapters/vk/), auf demselben Core aufgebaut und veröffentlicht als `ghcr.io/duckbugio/flock-vk`. Es liefert nur ein Env-Template (keine Compose-Datei): `cp .env.example .env`, dann `docker run --env-file .env ghcr.io/duckbugio/flock-vk`. Die Claude-Authentifizierung und die Core-Einstellungen sind identisch mit Telegram; nur die drei Transport-Variablen ändern sich:

| Variable | Beschreibung |
|---|---|
| `VK_BOT_TOKEN` | Community-Access-Token (VK-Community → Verwalten → API-Nutzung → Access-Token) |
| `VK_GROUP_ID` | die numerische ID deiner Community (Long-Poll-Server + Mention-Parsing) |
| `VK_ALLOWED_USERS` | kommagetrennte VK-Benutzer-IDs, die den Bot verwenden dürfen |

## Höhepunkte

- **Die Konversation ist die Aufgabenquelle** — beschreibe im Chat, was du willst, und prüfe den PR, der zurückkommt; Shell und Editor des Agenten sind innerhalb des Containers sandboxed.
- **Eine echte Entwicklungsteam-Pipeline, kein einzelner Prompt** — Spec-First-Akzeptanzkriterien, Build- und Regressions-Gates und ein Arbiter, der Schleifen durchbricht.
- **Multi-Transport** — heute **Telegram** und **VK**, beide auf demselben Core; eine neue Plattform ist ein schlanker Adapter, kein Fork.
- **PR-Reaktionen ohne eingehende Webhooks** — der Bot *pollt* deinen Git-Host nach neuen Review-Kommentaren und leitet jeden zurück an den Chat, der den PR geöffnet hat.
- **Abo-freundlich** — authentifiziere dich mit einem Claude-Pro/Max-Token (keine Kosten pro Token) oder einem Anthropic-API-Key.

## Wie es funktioniert

```
You (in a chat): "implement X across the api + web services"
  → bot's Claude (Lead) → planner → confirm scope → coder ⇄ tester → PR per repo
                                                      → reviewer (inline comments) ⇄ coder → arbiter
                                                                                        ├ APPROVE → you merge
                                                                                        └ ESCALATE → asks you
```

Die fünf Subagenten — **planner → coder → tester → reviewer → arbiter** — laufen als native Claude Code-Subagenten in [`core/agents/`](core/agents/). Eine einfache Frage wird schlicht beantwortet; eine Build-Anfrage löst das Team aus. Der **Arbiter** ist der risikobewusste, mit Zyklusgrenze versehene Schleifenbrecher, damit Agenten sich niemals endlos drehen. Branches werden `duck/<chatid>/<slug>` benannt, sodass PR-Webhook-/Poll-Ereignisse zurück an den richtigen Chat geleitet werden.

Das Team ist für einen **Microservices**-Workspace ausgelegt: Ein Feature kann sich über mehrere Services erstrecken, und es koordiniert Branches sowie einen quervernetzten PR pro Repo. Die vollständige Pipeline, die Leitplanken und die Rollentabelle findest du in [`core/README.md`](core/README.md).

## Repo-Aufbau (Monorepo)

Das plattformunabhängige Gehirn des Entwicklungsteams liegt in [`core/`](core/); jede Plattform ist ein schlanker Adapter unter `adapters/<name>/`, der es sich teilt.

| Adapter | Pfad | Vorgefertigtes Image |
|---|---|---|
| Telegram | [`adapters/telegram/`](adapters/telegram/) | `ghcr.io/duckbugio/flock-telegram` |
| VK | [`adapters/vk/`](adapters/vk/) | `ghcr.io/duckbugio/flock-vk` |

Künftige Plattformen nutzen denselben Core wieder — siehe [`docs/multi-transport-plan.md`](docs/multi-transport-plan.md).

## Einen Git-Host anbinden (optional, aber zentral)

Setze diese Werte in `.env`, damit das Team Repos klonen und PRs öffnen kann (funktioniert mit **Gitea/GitHub/GitLab**):

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

Für **github.com** setze zusätzlich `GH_TOKEN` (= dein `GIT_TOKEN`), damit die `gh`-CLI PRs öffnen kann.

Der **Poller** ist die empfohlene Methode, um auf Review-Kommentare zu reagieren — er greift *nach außen*, sodass er sogar funktioniert, wenn dein Host den Bot nicht erreichen kann. Er ist aktiv, wenn `ENABLE_PR_REVIEW=true` gesetzt und `GITEA_API_URL` konfiguriert ist. Eine Alternative mit eingehendem Webhook plus Caddy-TLS-Proxy ist nur über das Ansible-Deployment verfügbar (setze `webhook_domain`).

## Weitere Optionen

- **Sprachnachrichten:** `ENABLE_VOICE_MESSAGES=true`, `VOICE_PROVIDER=mistral|openai|local`, plus `MISTRAL_API_KEY` (oder `OPENAI_API_KEY`). Werden transkribiert und als Befehle ausgeführt.
- **dind-Sidecar:** `docker compose --profile dind up -d` stellt dem Team dockerisierte Linter/Tests bereit (setze `DOCKER_HOST=tcp://dind:2375`).
- **Isolation pro Chat:** Jeder Chat erhält `/workspace/chat_<id>` (1:1 → privat; Gruppe → ein gemeinsamer Workspace); Chats sind vollständig isoliert und laufen parallel, begrenzt durch `MAX_CONCURRENT_CHAT_RUNS`. Setze in Gruppen `REQUIRE_GROUP_MENTION=true`, damit der Bot nur antwortet, wenn er per @mention erwähnt wird oder auf ihn geantwortet wird.
- **Ansible-Deployment** (Telegram): Ein-Befehl-VPS-Provisionierung aus `adapters/telegram/deploy` — kopiere `inventories/example` in dein eigenes `inventories/<name>/` (gitignored), fülle Inventory/Vars/Vault aus, dann `ansible-playbook -i inventories/<name>/inventory.ini playbook.yml`. Die Rolle zieht das vorgefertigte Image; setze `bot_image`, um einen Tag festzuhalten.

## Sicherheit

- **Whitelist:** Nur `ALLOWED_USERS` (Telegram) / `VK_ALLOWED_USERS` (VK) dürfen den Bot verwenden — lass sie niemals leer; sie gewährt Shell-/Edit-Zugriff auf deinen Server.
- **Isolation pro Chat:** Verschiedene Chats erhalten separate Workspaces. Das Git-Token wird über ein Deployment hinweg geteilt — scope es entsprechend.
- **Secrets:** Bewahre sie in `.env` (gitignored) auf oder, bei Ansible, in der `vault.yml` einer echten Instanz (gitignored, mit `ansible-vault` verschlüsselbar). Nur `inventories/example/` wird versioniert.
- **Sandbox:** Der Agent läuft als Non-Root-Benutzer; sein Bash/Edit ist auf den Container beschränkt, nicht auf deinen Host.

## Build, Lint, Test

Das Repo verwendet [Task](https://taskfile.dev) als CI-Runner — denselben Einstiegspunkt, den die [CI](.github/workflows/ci.yml) nutzt:

```bash
task lint      # format + vet + linters (in the dev-tools image)
task tests     # Go test suite
task build     # compile the binaries
```

## Lizenz

[MIT](LICENSE) © DuckBug.
