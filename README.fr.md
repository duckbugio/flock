<p align="center">
  <a href="README.md">English</a> ·
  <a href="README.ru.md">Русский</a> ·
  <a href="README.zh.md">中文</a> ·
  <a href="README.es.md">Español</a> ·
  <a href="README.de.md">Deutsch</a> ·
  <b>Français</b> ·
  <a href="README.pt-BR.md">Português (BR)</a> ·
  <a href="README.ja.md">日本語</a>
</p>

# Flock

**Faites tourner une équipe de développement IA Claude Code sur votre serveur et pilotez-la depuis une conversation.** Décrivez une fonctionnalité sur Telegram ou VK ; l'équipe la planifie, la développe sur une branche, la teste, la relit et ouvre une PR — chaque conversation dans son propre espace de travail isolé.

[![CI](https://github.com/duckbugio/flock/actions/workflows/ci.yml/badge.svg)](https://github.com/duckbugio/flock/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](go.mod)
[![image: ghcr.io](https://img.shields.io/badge/image-ghcr.io-2496ED.svg)](https://github.com/orgs/duckbugio/packages)

Elle fonctionne avec votre **abonnement Claude Pro/Max** (sans facturation au token) ou une clé API Anthropic, est livrée sous forme d'**images Docker pré-construites** (aucune étape de build) et conserve chaque conversation dans son propre espace de travail cloisonné.

## Démarrage rapide (Docker)

```bash
git clone https://github.com/duckbugio/flock
cd flock/adapters/telegram
cp .env.example .env        # fill in the REQUIRED block (4 values)
docker compose up -d
```

Cela récupère l'image pré-construite `ghcr.io/duckbugio/flock-telegram` — aucun build, aucun Ansible — puis envoyez un message à votre bot. Le `.env` minimal :

| Variable | Description |
|---|---|
| `TELEGRAM_BOT_TOKEN` | depuis [@BotFather](https://t.me/botfather) |
| `TELEGRAM_BOT_USERNAME` | le @username de votre bot (sans `@`) |
| `ALLOWED_USERS` | identifiants d'utilisateurs Telegram séparés par des virgules, autorisés à utiliser le bot |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token` (abonnement) — *ou* définissez `ANTHROPIC_API_KEY` |

Tout le reste dans [`.env.example`](adapters/telegram/.env.example) a des valeurs par défaut raisonnables. Mettez à jour plus tard avec `docker compose pull && docker compose up -d`.

> **Région :** hébergez dans une **région prise en charge par Anthropic** (certains pays, par exemple RU/CN, sont bloqués géographiquement) — sinon les appels à Claude échouent.

**VK** suit le même schéma sous [`adapters/vk/`](adapters/vk/), construit sur le même cœur et publié sous `ghcr.io/duckbugio/flock-vk`. Il ne fournit qu'un modèle d'environnement (pas de fichier compose) : `cp .env.example .env`, puis `docker run --env-file .env ghcr.io/duckbugio/flock-vk`. L'authentification Claude et les paramètres du cœur sont identiques à ceux de Telegram ; seules les trois variables de transport changent :

| Variable | Description |
|---|---|
| `VK_BOT_TOKEN` | token d'accès de la communauté (communauté VK → Gérer → Utilisation de l'API → token d'accès) |
| `VK_GROUP_ID` | l'identifiant numérique de votre communauté (serveur long-poll + analyse des mentions) |
| `VK_ALLOWED_USERS` | identifiants d'utilisateurs VK séparés par des virgules, autorisés à utiliser le bot |

## Points forts

- **La conversation est la source des tâches** — décrivez ce que vous voulez dans la conversation et examinez la PR qui revient ; le shell et l'éditeur de l'agent sont cloisonnés à l'intérieur du conteneur.
- **Un véritable pipeline d'équipe de développement, pas un simple prompt** — critères d'acceptation définis d'abord par la spécification, contrôles de build et de régression, et un arbitre qui rompt les boucles.
- **Multi-transport** — **Telegram** et **VK** aujourd'hui, tous deux sur le même cœur ; une nouvelle plateforme est un adaptateur léger, pas un fork.
- **Réactions aux PR sans webhooks entrants** — le bot *interroge* votre hôte git pour détecter les nouveaux commentaires de revue et achemine chacun vers la conversation qui a ouvert la PR.
- **Compatible avec les abonnements** — authentifiez-vous avec un token Claude Pro/Max (sans coût au token) ou une clé API Anthropic.

## Fonctionnement

```
You (in a chat): "implement X across the api + web services"
  → bot's Claude (Lead) → planner → confirm scope → coder ⇄ tester → PR per repo
                                                      → reviewer (inline comments) ⇄ coder → arbiter
                                                                                        ├ APPROVE → you merge
                                                                                        └ ESCALATE → asks you
```

Les cinq sous-agents — **planner → coder → tester → reviewer → arbiter** — s'exécutent comme des sous-agents Claude Code natifs dans [`core/agents/`](core/agents/). Une simple question reçoit simplement une réponse ; une demande de build déclenche l'équipe. L'**arbiter** est le briseur de boucles conscient du risque et limité en cycles, afin que les agents ne tournent jamais indéfiniment. Les branches sont nommées `duck/<chatid>/<slug>` de sorte que les événements de webhook/sondage de PR soient routés vers la bonne conversation.

L'équipe est conçue pour un espace de travail en **microservices** : une fonctionnalité peut s'étendre sur plusieurs services, et elle coordonne les branches ainsi qu'une PR avec liens croisés par dépôt. Le pipeline complet, les garde-fous et le tableau des rôles se trouvent dans [`core/README.md`](core/README.md).

## Organisation du dépôt (monorepo)

Le cerveau de l'équipe de développement, agnostique de plateforme, se trouve dans [`core/`](core/) ; chaque plateforme est un adaptateur léger sous `adapters/<name>/` qui le partage.

| Adaptateur | Chemin | Image pré-construite |
|---|---|---|
| Telegram | [`adapters/telegram/`](adapters/telegram/) | `ghcr.io/duckbugio/flock-telegram` |
| VK | [`adapters/vk/`](adapters/vk/) | `ghcr.io/duckbugio/flock-vk` |

Les futures plateformes réutilisent le même cœur — voir [`docs/multi-transport-plan.md`](docs/multi-transport-plan.md).

## Connecter un hôte git (optionnel mais central)

Définissez ces variables dans `.env` pour permettre à l'équipe de cloner des dépôts et d'ouvrir des PR (fonctionne avec **Gitea/GitHub/GitLab**) :

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

Pour **github.com**, définissez également `GH_TOKEN` (= votre `GIT_TOKEN`) afin que la CLI `gh` puisse ouvrir des PR.

Le **poller** est le moyen recommandé pour réagir aux commentaires de revue — il tend la main *vers l'extérieur*, il fonctionne donc même lorsque votre hôte ne peut pas joindre le bot. Il est actif lorsque `ENABLE_PR_REVIEW=true` et que `GITEA_API_URL` est défini. Une alternative par webhook entrant associé à un proxy TLS Caddy n'est disponible qu'au travers du déploiement Ansible (définissez `webhook_domain`).

## Autres options

- **Messages vocaux :** `ENABLE_VOICE_MESSAGES=true`, `VOICE_PROVIDER=mistral|openai|local`, plus `MISTRAL_API_KEY` (ou `OPENAI_API_KEY`). Transcrits et exécutés comme des commandes.
- **Sidecar dind :** `docker compose --profile dind up -d` fournit à l'équipe des linters/tests dockerisés (définissez `DOCKER_HOST=tcp://dind:2375`).
- **Isolation par conversation :** chaque conversation reçoit `/workspace/chat_<id>` (1:1 → privé ; groupe → un espace de travail partagé) ; les conversations sont totalement isolées et s'exécutent en parallèle, plafonnées par `MAX_CONCURRENT_CHAT_RUNS`. Dans les groupes, définissez `REQUIRE_GROUP_MENTION=true` pour ne répondre que lorsque le bot est @mentionné ou cité en réponse.
- **Déploiement Ansible** (Telegram) : provisionnement d'un VPS en une commande depuis `adapters/telegram/deploy` — copiez `inventories/example` vers votre propre `inventories/<name>/` (ignoré par git), remplissez inventory/vars/vault, puis `ansible-playbook -i inventories/<name>/inventory.ini playbook.yml`. Le rôle récupère l'image pré-construite ; définissez `bot_image` pour épingler un tag.

## Sécurité

- **Liste blanche :** seuls `ALLOWED_USERS` (Telegram) / `VK_ALLOWED_USERS` (VK) peuvent utiliser le bot — ne la laissez jamais vide ; elle accorde un accès shell/édition à votre serveur.
- **Isolation par conversation :** différentes conversations obtiennent des espaces de travail séparés. Le token git est partagé au sein d'un déploiement — limitez sa portée en conséquence.
- **Secrets :** conservez-les dans `.env` (ignoré par git) ou, pour Ansible, dans le `vault.yml` d'une instance réelle (ignoré par git, chiffrable avec `ansible-vault`). Seul `inventories/example/` est suivi.
- **Sandbox :** l'agent s'exécute en tant qu'utilisateur non-root ; ses commandes Bash/Edit sont confinées au conteneur, pas à votre hôte.

## Build, lint, test

Le dépôt utilise [Task](https://taskfile.dev) comme exécuteur de CI — le même point d'entrée que [CI](.github/workflows/ci.yml) :

```bash
task lint      # format + vet + linters (in the dev-tools image)
task tests     # Go test suite
task build     # compile the binaries
```

## Licence

[MIT](LICENSE) © DuckBug.
