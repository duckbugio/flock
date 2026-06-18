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

**Betreibe ein KI-Entwicklungsteam mit Claude Code auf deinem Server und steuere es per Chat.** Beschreibe ein Feature in Telegram oder VK; das Team plant es, baut es auf einem Branch, testet es, prüft es und öffnet einen PR — jeder Chat in seinem eigenen isolierten Workspace.

[![CI](https://github.com/duckbugio/flock/actions/workflows/ci.yml/badge.svg)](https://github.com/duckbugio/flock/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](go.mod)
[![image: ghcr.io](https://img.shields.io/badge/image-ghcr.io-2496ED.svg)](https://github.com/orgs/duckbugio/packages)

DuckFlock ist ein nativer Go-Dienst, der die Claude Code CLI als **Entwicklungsteam** mit fünf Rollen
steuert und über einen Chat zugänglich macht. Du sprichst mit ihm wie mit einer Kollegin oder einem
Kollegen: Eine einfache Frage wird beantwortet; „implementiere X über die api- und web-Services hinweg“
stößt eine echte Pipeline an — Spezifikation und Akzeptanzkriterien → Build → Lint- und Regressions-Gates
→ PR → adversariale Review mit zeilengenauen Inline-Kommentaren → ein Arbiter, der über Auslieferung
oder Eskalation entscheidet.

Es läuft über dein Claude-**Pro/Max-Abonnement** (keine Abrechnung pro Token) oder einen Anthropic-API-Key,
wird als **vorgefertigte Docker-Images** ausgeliefert (kein Build-Schritt) und hält jeden Chat in seinem
eigenen sandboxed Workspace.

## Warum DuckFlock

- **Die Konversation ist die Aufgabenquelle.** Kein Board, keine Issue-Queue — beschreibe im Chat, was
  du willst, und prüfe den PR, der zurückkommt. Shell und Editor des Agenten sind **innerhalb des
  Containers sandboxed**, nicht auf deinem Host.
- **Eine echte Entwicklungsteam-Pipeline, kein einzelner Prompt.** Fünf Subagenten — **planner → coder →
  tester → reviewer → arbiter** — mit Spec-First-Akzeptanzkriterien, Build- und Regressions-Gates und
  einem Arbiter, der Schleifen durchbricht, damit Agenten sich niemals endlos drehen.
- **Multi-Transport.** Heute zwei gleichrangige Adapter — **Telegram** und **VK** — beide auf demselben
  plattformunabhängigen Core aufgebaut. Eine neue Plattform hinzuzufügen ist ein schlanker Adapter,
  kein Fork.
- **Isolation pro Chat, parallele Ausführung.** Jeder Chat (1:1 oder Gruppe) erhält seinen eigenen
  `/workspace/chat_<id>`; Chats können die Dateien der anderen nicht sehen, und verschiedene Chats
  werden nebenläufig beantwortet.
- **PR-Reaktionen ohne eingehende Webhooks.** Der Bot *pollt* deinen Git-Host nach neuen
  Review-Kommentaren und kümmert sich um sie, wobei er jeden zurück an den Chat leitet, der den PR
  geöffnet hat — funktioniert sogar, wenn dein Host den Bot nicht erreichen kann.
- **Abo-freundlich.** Authentifiziere dich mit einem Claude-Pro/Max-Token (`claude setup-token`) —
  ohne Kosten pro Token — oder greife auf einen Anthropic-API-Key zurück.
- **Optionale Sprache.** Sprachnachrichten werden transkribiert (Mistral Voxtral / OpenAI Whisper /
  lokal) und als Befehle ausgeführt.

## Repo-Aufbau (Monorepo)

Das plattformunabhängige Gehirn des Entwicklungsteams — Subagenten und Orchestrierung — liegt in
[`core/`](core/). Jede Chat-Plattform ist ein schlanker **Adapter** unter `adapters/<name>/`, der
sich `core/` teilt:

| Adapter | Pfad | Vorgefertigtes Image |
|---|---|---|
| Telegram | [`adapters/telegram/`](adapters/telegram/) | `ghcr.io/duckbugio/flock-telegram` |
| VK | [`adapters/vk/`](adapters/vk/) | `ghcr.io/duckbugio/flock-vk` |

Künftige Plattformen (z. B. MAX) erhalten ihr eigenes `adapters/<name>/` und nutzen denselben Core
wieder — siehe [`docs/multi-transport-plan.md`](docs/multi-transport-plan.md).

## Schnellstart (Telegram, Docker — empfohlen)

```bash
git clone https://github.com/duckbugio/flock
cd flock/adapters/telegram
cp .env.example .env        # fill in the REQUIRED block (4 values)
docker compose up -d
```

Das zieht das vorgefertigte Image `ghcr.io/duckbugio/flock-telegram` — kein Build, kein Ansible.
Schreibe anschließend deinem Bot. Die minimale `.env`:

| Variable | Beschreibung |
|---|---|
| `TELEGRAM_BOT_TOKEN` | von [@BotFather](https://t.me/botfather) |
| `TELEGRAM_BOT_USERNAME` | der @username deines Bots (ohne `@`) |
| `ALLOWED_USERS` | kommagetrennte Telegram-Benutzer-IDs, die den Bot verwenden dürfen |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token` (Abonnement) — *oder* setze `ANTHROPIC_API_KEY` |

Alles Übrige in [`.env.example`](adapters/telegram/.env.example) hat sinnvolle Standardwerte.
Aktualisiere das Image später mit `docker compose pull && docker compose up -d`.

> **Region:** Hoste in einer **von Anthropic unterstützten Region** (Anthropic geoblockt einige
> Länder, z. B. RU/CN) — andernfalls schlagen Claude-Aufrufe fehl.

## Schnellstart (VK)

VK folgt demselben Muster unter [`adapters/vk/`](adapters/vk/), auf demselben Core aufgebaut und
veröffentlicht als `ghcr.io/duckbugio/flock-vk`. Der VK-Adapter liefert nur ein Env-Template (keine
Compose-Datei); kopiere es und führe das Image damit aus, z. B.
`docker run --env-file .env ghcr.io/duckbugio/flock-vk`:

```bash
cd flock/adapters/vk
cp .env.example .env        # fill in the REQUIRED block
```

Die VK-`.env` tauscht die drei Transport-Variablen aus; die Claude-Authentifizierung und alle
Core-Einstellungen sind identisch mit Telegram:

| Variable | Beschreibung |
|---|---|
| `VK_BOT_TOKEN` | Community-Access-Token (VK-Community → Verwalten → API-Nutzung → Access-Token) |
| `VK_GROUP_ID` | die numerische ID deiner Community (Long-Poll-Server + Mention-Parsing) |
| `VK_ALLOWED_USERS` | kommagetrennte VK-Benutzer-IDs, die den Bot verwenden dürfen |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token` (Abonnement) — *oder* setze `ANTHROPIC_API_KEY` |

Die vollständige Liste findest du in [`adapters/vk/.env.example`](adapters/vk/.env.example) (die
Standardwerte entsprechen dem Telegram-Adapter).

## Wie es funktioniert

```
You (in a chat): "implement X across the api + web services"
  → bot's Claude (Lead) → planner → confirm scope → coder ⇄ tester → PR per repo
                                                      → reviewer (inline comments) ⇄ coder → arbiter
                                                                                        ├ APPROVE → you merge
                                                                                        └ ESCALATE → asks you
```

Der **Arbiter** ist der Schleifenbrecher (risikobewusst, mit Zyklusgrenze), damit Agenten sich niemals
endlos drehen. Eine einfache Frage wird schlicht beantwortet; eine Build-Anfrage löst das Team aus.
Branches werden `duck/<chatid>/<slug>` benannt, sodass PR-Webhook-/Poll-Ereignisse zurück an den
richtigen Chat geleitet werden.

Das Entwicklungsteam ist für einen **Microservices**-Workspace ausgelegt: Ein Feature kann sich über
mehrere Services erstrecken, und das Team koordiniert Branches sowie einen quervernetzten PR pro Repo.
Die vollständige Pipeline, die Leitplanken und die Rollentabelle findest du in
[`core/README.md`](core/README.md).

## Das Entwicklungsteam

Die Rollen liegen in [`core/agents/`](core/agents/) als native Claude Code-Subagenten:

| Subagent | Aufgabe |
|---|---|
| [`planner`](core/agents/planner.md) | Spezifikation (≥3 Akzeptanzkriterien) + betroffene Services + geordneter Plan + Risiken |
| [`coder`](core/agents/coder.md) | Implementiert auf einem Branch; contract-sicher über Services hinweg |
| [`tester`](core/agents/tester.md) | Führt Tests aus/schreibt sie, inkl. der service-übergreifenden Nahtstelle; Green-Build-Gate |
| [`reviewer`](core/agents/reviewer.md) | Adversariale Review + RISK-Bewertung + service-übergreifende Checks |
| [`arbiter`](core/agents/arbiter.md) | Erzwingt das Gate; entscheidet über Fortsetzen/Ausliefern/Eskalieren — der Schleifenbrecher |

Die Pipeline übernimmt Muster (nicht das Framework) von
[ruflo](https://github.com/ruvnet/ruflo) — SPARC-Spec-Gates, Multi-Repo-Koordination und
Diff-Risikobewertung. Die Orchestrierungsregeln sind im Image enthalten und werden in jeden Workspace
als `CLAUDE.md` gerendert. Details findest du in [`core/README.md`](core/README.md).

## Einen Git-Host anbinden (optional, aber zentral)

Setze diese Werte in `.env`, damit das Team Repos klonen und PRs öffnen kann (funktioniert mit
**Gitea/GitHub/GitLab**):

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

Der **Poller** ist die empfohlene Methode, um auf Review-Kommentare zu reagieren — er funktioniert
sogar, wenn dein Git-Host den Bot nicht erreichen kann (z. B. bei grenzüberschreitender
Netzwerkfilterung), weil der Bot *nach außen* greift. Er ist aktiv, wenn `ENABLE_PR_REVIEW=true`
gesetzt und `GITEA_API_URL` konfiguriert ist. Eingehende Webhooks plus ein Caddy-TLS-Proxy sind eine
optionale Alternative, verfügbar nur über das Ansible-Deployment (setze `webhook_domain` in der
`group_vars/all/vars.yml` der Instanz; siehe den Ansible-Abschnitt weiter unten).

## GitHub-Star-Hinweis (optional, nur GitHub)

Wenn der Bot mit einem GitHub-Konto verbunden ist (`GIT_HOST=github.com` + `GIT_TOKEN`), das dem
Projekt noch keinen Stern gegeben hat, sendet er nach jedem erfolgreichen Lauf eine kurze Nachricht
mit der Einladung, einen Stern zu vergeben, samt Inline-Button. Ein Druck auf den Button vergibt den
Stern vom Konto des Deployments selbst, und der Hinweis hört danach endgültig auf. Er ist GitHub-only
und **überall sonst automatisch aus** (das Gate ist der Ausschalter — kein separates Flag); er läuft
vollständig abseits des Hot Path, sodass er einen Lauf niemals verzögert oder unterbricht.

```ini
STAR_NUDGE_REPO=duckbugio/flock   # owner/repo to nudge for / star
# STAR_NUDGE_STORE_PATH=          # default <APPROVED_DIRECTORY>/star_nudge.json
```

Der Button benötigt ein Token mit **Star-Write-Scope**: einen klassischen PAT mit `public_repo` oder
ein fein granuliertes Token mit der Kontoberechtigung „Starring“. Ohne dieses scheitert der Stern
einfach lautlos (der Lauf bleibt davon unberührt).

## Sprachnachrichten (optional)

```ini
ENABLE_VOICE_MESSAGES=true
VOICE_PROVIDER=mistral        # mistral | openai | local
MISTRAL_API_KEY=...           # console.mistral.ai  (or OPENAI_API_KEY for whisper)
```

## Optionale Sidecars (Telegram-Adapter)

```bash
docker compose --profile dind up -d     # dockerized linters/tests for the team (set DOCKER_HOST=tcp://dind:2375)
```

(Der TLS-Proxy für eingehende Webhooks wird vom Ansible-Deployment bereitgestellt, nicht durch ein
Compose-Profil — setze `webhook_domain` in der `group_vars/all/vars.yml` der Instanz, um ihn zu
aktivieren.)

## Isolation pro Chat

Jeder Chat erhält `/workspace/chat_<id>`: 1:1 → dein privater Workspace; Gruppe → ein gemeinsamer
Workspace für die Mitglieder dieser Gruppe; verschiedene Chats sind vollständig isoliert und laufen
**parallel** (begrenzt durch `MAX_CONCURRENT_CHAT_RUNS` aus Speichergründen). Die Benutzer-ID-Whitelist
steuert, wer den Bot verwenden darf. Setze in Gruppen `REQUIRE_GROUP_MENTION=true`, damit der Bot nur
antwortet, wenn er per @mention erwähnt wird oder auf ihn geantwortet wird.

## Fortgeschritten: Deployment auf einen Server mit Ansible (Telegram-Adapter)

Für eine Ein-Befehl-Bereitstellung eines frischen VPS (installiert Docker + Swap, rendert `.env`, zieht
das Image, startet es):

```bash
cd adapters/telegram/deploy
cp -r inventories/example inventories/my-bot          # one dir per bot instance
$EDITOR inventories/my-bot/inventory.ini              # your server IP + SSH key
$EDITOR inventories/my-bot/group_vars/all/vars.yml    # non-secret config
$EDITOR inventories/my-bot/group_vars/all/vault.yml   # secrets (tokens)
ansible-vault encrypt inventories/my-bot/group_vars/all/vault.yml   # optional
ansible-playbook -i inventories/my-bot/inventory.ini playbook.yml
```

Jede Bot-Instanz ist ihr eigenes `inventories/<name>/`; nur `inventories/example/` ist eingecheckt —
echte Instanzen sind gitignored. Die Rolle zieht das vorgefertigte Image — setze `bot_image`, um einen
Tag festzuhalten. (Um nach einem Fork dein eigenes Image zu bauen, führe
`docker build -f adapters/telegram/Dockerfile .` aus dem Repo-Root aus; das VK-Image ist
`docker build -f adapters/vk/Dockerfile .`.)

## Sicherheit

- **Whitelist:** Nur die Liste der erlaubten Benutzer (`ALLOWED_USERS` für Telegram, `VK_ALLOWED_USERS`
  für VK) darf den Bot verwenden — lass sie niemals leer; der Bot gewährt Shell-/Edit-Zugriff auf
  deinen Server.
- **Isolation pro Chat:** Verschiedene Chats erhalten separate Workspaces. Das Git-Token wird über ein
  Deployment hinweg geteilt — scope es entsprechend.
- **Secrets:** Bewahre sie in `.env` (gitignored) auf oder, bei Ansible, in der
  `inventories/<name>/group_vars/all/vault.yml` einer echten Instanz (gitignored; mit `ansible-vault`
  verschlüsselbar). Committe niemals echte Tokens — nur `inventories/example/` wird versioniert.
- **Isolation:** Der Agent läuft als Non-Root-Benutzer innerhalb des Containers; sein Bash/Edit ist auf
  den Container beschränkt, nicht auf deinen Host.

## Build, Lint, Test

Das Repo verwendet [Task](https://taskfile.dev) als CI-Runner — denselben Einstiegspunkt, den die
[CI](.github/workflows/ci.yml) nutzt:

```bash
task lint      # format + vet + linters (in the dev-tools image)
task tests     # Go test suite
task build     # compile the binaries
```

Die CI führt zusätzlich `task security-scan` (govulncheck) und einen Trivy-Dateisystemscan aus und
baut bei jedem PR beide Adapter-Images im Dry-Run.

## Lizenz

[MIT](LICENSE) © DuckBug.
