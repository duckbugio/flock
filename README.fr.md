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

# DuckFlock

**Faites tourner une équipe de développement IA Claude Code sur votre serveur et pilotez-la depuis un chat.** Décrivez une fonctionnalité dans Telegram ou VK ; l'équipe la planifie, la développe sur une branche, la teste, la relit et ouvre une PR — chaque chat dans son propre espace de travail isolé.

[![CI](https://github.com/duckbugio/flock/actions/workflows/ci.yml/badge.svg)](https://github.com/duckbugio/flock/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](go.mod)
[![image: ghcr.io](https://img.shields.io/badge/image-ghcr.io-2496ED.svg)](https://github.com/orgs/duckbugio/packages)

DuckFlock est un service Go natif qui pilote la CLI Claude Code comme une **équipe de développement** à cinq rôles et l'expose à travers le chat. Vous lui parlez comme à un collègue : une simple question reçoit une réponse ; « implémente X dans les services api et web » lance un véritable pipeline — spécification et critères d'acceptation → build → contrôles de lint et de régression → PR → revue contradictoire avec commentaires en ligne ancrés aux lignes → un arbitre qui décide d'expédier ou d'escalader.

Il fonctionne avec votre **abonnement Claude Pro/Max** (pas de facturation au token) ou une clé API Anthropic, est livré sous forme d'**images Docker pré-construites** (aucune étape de build) et conserve chaque chat dans son propre espace de travail isolé.

## Pourquoi DuckFlock

- **La conversation est la source des tâches.** Pas de tableau, pas de file d'attente de tickets — décrivez ce que vous voulez dans le chat et examinez la PR qui revient. Le shell et l'éditeur de l'agent sont **isolés à l'intérieur du conteneur**, pas sur votre hôte.
- **Un véritable pipeline d'équipe de développement, pas un simple prompt.** Cinq sous-agents — **planner → coder → tester → reviewer → arbiter** — avec des critères d'acceptation définis d'abord par la spécification, des contrôles de build et de régression, et un arbitre qui rompt les boucles pour que les agents ne tournent jamais indéfiniment.
- **Multi-transport.** Deux adaptateurs équivalents aujourd'hui — **Telegram** et **VK** — tous deux construits sur le même cœur agnostique de plateforme. Ajouter une nouvelle plateforme se résume à un adaptateur léger, pas à un fork.
- **Isolation par chat, exécution en parallèle.** Chaque chat (en tête-à-tête ou en groupe) reçoit son propre `/workspace/chat_<id>` ; les chats ne peuvent pas voir les fichiers des autres, et différents chats sont traités simultanément.
- **Réactions aux PR sans webhooks entrants.** Le bot *interroge* votre hôte git pour détecter les nouveaux commentaires de revue et les traite, en routant chacun vers le chat qui a ouvert la PR — fonctionne même lorsque votre hôte ne peut pas joindre le bot.
- **Compatible avec les abonnements.** Authentifiez-vous avec un token Claude Pro/Max (`claude setup-token`) — sans coût au token — ou repliez-vous sur une clé API Anthropic.
- **Vocal optionnel.** Les messages vocaux sont transcrits (Mistral Voxtral / OpenAI Whisper / local) et exécutés comme des commandes.

## Organisation du dépôt (monorepo)

Le cerveau agnostique de plateforme de l'équipe de développement — sous-agents et orchestration — se trouve dans [`core/`](core/). Chaque plateforme de chat est un **adaptateur** léger sous `adapters/<name>/` qui partage `core/` :

| Adaptateur | Chemin | Image pré-construite |
|---|---|---|
| Telegram | [`adapters/telegram/`](adapters/telegram/) | `ghcr.io/duckbugio/flock-telegram` |
| VK | [`adapters/vk/`](adapters/vk/) | `ghcr.io/duckbugio/flock-vk` |

Les futures plateformes (par exemple MAX) obtiennent leur propre `adapters/<name>/` et réutilisent le même cœur — voir [`docs/multi-transport-plan.md`](docs/multi-transport-plan.md).

## Démarrage rapide (Telegram, Docker — recommandé)

```bash
git clone https://github.com/duckbugio/flock
cd flock/adapters/telegram
cp .env.example .env        # fill in the REQUIRED block (4 values)
docker compose up -d
```

Cela récupère l'image pré-construite `ghcr.io/duckbugio/flock-telegram` — aucun build, aucun Ansible. Ensuite, envoyez un message à votre bot. Le `.env` minimal :

| Variable | Description |
|---|---|
| `TELEGRAM_BOT_TOKEN` | depuis [@BotFather](https://t.me/botfather) |
| `TELEGRAM_BOT_USERNAME` | le @username de votre bot (sans `@`) |
| `ALLOWED_USERS` | identifiants d'utilisateurs Telegram séparés par des virgules autorisés à utiliser le bot |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token` (abonnement) — *ou* définissez `ANTHROPIC_API_KEY` |

Tout le reste dans [`.env.example`](adapters/telegram/.env.example) a des valeurs par défaut raisonnables. Mettez à jour l'image plus tard avec `docker compose pull && docker compose up -d`.

> **Région :** hébergez dans une **région prise en charge par Anthropic** (Anthropic bloque géographiquement certains pays, par exemple RU/CN) — sinon les appels à Claude échouent.

## Démarrage rapide (VK)

VK suit le même schéma sous [`adapters/vk/`](adapters/vk/), construit sur le même cœur et publié sous `ghcr.io/duckbugio/flock-vk`. L'adaptateur VK ne fournit qu'un modèle d'environnement (pas de fichier compose) ; copiez-le et exécutez l'image avec, par exemple `docker run --env-file .env ghcr.io/duckbugio/flock-vk` :

```bash
cd flock/adapters/vk
cp .env.example .env        # fill in the REQUIRED block
```

Le `.env` de VK remplace les trois variables de transport ; l'authentification Claude et tous les paramètres du cœur sont identiques à Telegram :

| Variable | Description |
|---|---|
| `VK_BOT_TOKEN` | token d'accès de la communauté (communauté VK → Gérer → Utilisation de l'API → token d'accès) |
| `VK_GROUP_ID` | l'identifiant numérique de votre communauté (serveur long-poll + analyse des mentions) |
| `VK_ALLOWED_USERS` | identifiants d'utilisateurs VK séparés par des virgules autorisés à utiliser le bot |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token` (abonnement) — *ou* définissez `ANTHROPIC_API_KEY` |

Consultez [`adapters/vk/.env.example`](adapters/vk/.env.example) pour la liste complète (les valeurs par défaut correspondent à l'adaptateur Telegram).

## Fonctionnement

```
You (in a chat): "implement X across the api + web services"
  → bot's Claude (Lead) → planner → confirm scope → coder ⇄ tester → PR per repo
                                                      → reviewer (inline comments) ⇄ coder → arbiter
                                                                                        ├ APPROVE → you merge
                                                                                        └ ESCALATE → asks you
```

L'**arbiter** est le briseur de boucles (conscient du risque, limité en cycles) afin que les agents ne bouclent jamais indéfiniment. Une simple question reçoit simplement une réponse ; une demande de build déclenche l'équipe. Les branches sont nommées `duck/<chatid>/<slug>` de sorte que les événements de webhook/sondage de PR soient routés vers le bon chat.

L'équipe de développement est conçue pour un espace de travail en **microservices** : une fonctionnalité peut s'étendre sur plusieurs services, et l'équipe coordonne les branches et une PR avec liens croisés par dépôt. Le pipeline complet, les garde-fous et le tableau des rôles se trouvent dans [`core/README.md`](core/README.md).

## L'équipe de développement

Les rôles se trouvent dans [`core/agents/`](core/agents/) sous forme de sous-agents Claude Code natifs :

| Sous-agent | Rôle |
|---|---|
| [`planner`](core/agents/planner.md) | Spécification (≥3 critères d'acceptation) + services affectés + plan ordonné + risques |
| [`coder`](core/agents/coder.md) | Implémente sur une branche ; respecte les contrats entre services |
| [`tester`](core/agents/tester.md) | Exécute/écrit les tests, y compris la jonction inter-services ; contrôle de build vert |
| [`reviewer`](core/agents/reviewer.md) | Revue contradictoire + score de RISQUE + contrôles inter-services |
| [`arbiter`](core/agents/arbiter.md) | Applique le contrôle ; décide continuer/expédier/escalader — le briseur de boucles |

Le pipeline emprunte des motifs (pas le framework) à [ruflo](https://github.com/ruvnet/ruflo) — contrôles de spécification SPARC, coordination multi-dépôts et notation du risque des diffs. Les règles d'orchestration sont livrées dans l'image et rendues dans chaque espace de travail sous forme de `CLAUDE.md`. Voir [`core/README.md`](core/README.md) pour les détails.

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

Le **poller** est le moyen recommandé pour réagir aux commentaires de revue — il fonctionne même lorsque votre hôte git ne peut pas joindre le bot (par exemple un filtrage réseau transfrontalier), car c'est le bot qui tend la main *vers l'extérieur*. Il est actif lorsque `ENABLE_PR_REVIEW=true` et que `GITEA_API_URL` est défini. Les webhooks entrants associés à un proxy TLS Caddy constituent une alternative optionnelle, disponible uniquement via le déploiement Ansible (définissez `webhook_domain` dans le `group_vars/all/vars.yml` de l'instance ; voir la section Ansible ci-dessous).

## Incitation à étoiler sur GitHub (optionnel, GitHub uniquement)

Lorsque le bot est relié à un compte GitHub (`GIT_HOST=github.com` + `GIT_TOKEN`) qui n'a pas encore étoilé le projet, il envoie un court message après chaque exécution réussie vous invitant à l'étoiler, avec un bouton en ligne. Appuyer sur le bouton étoile le dépôt depuis le compte propre du déploiement, et l'incitation s'arrête alors définitivement. Elle est réservée à GitHub et **automatiquement désactivée** partout ailleurs (la condition est l'interrupteur d'arrêt — pas de drapeau distinct) ; elle s'exécute entièrement en dehors du chemin critique, de sorte qu'elle ne retarde ni n'interrompt jamais une exécution.

```ini
STAR_NUDGE_REPO=duckbugio/flock   # owner/repo to nudge for / star
# STAR_NUDGE_STORE_PATH=          # default <APPROVED_DIRECTORY>/star_nudge.json
```

Le bouton nécessite un token disposant du **droit d'écriture sur les étoiles** : un PAT classique avec `public_repo`, ou un token à granularité fine avec l'autorisation de compte « Starring ». Sans cela, l'étoile échoue simplement en douceur (l'exécution n'est pas affectée).

## Messages vocaux (optionnel)

```ini
ENABLE_VOICE_MESSAGES=true
VOICE_PROVIDER=mistral        # mistral | openai | local
MISTRAL_API_KEY=...           # console.mistral.ai  (or OPENAI_API_KEY for whisper)
```

## Sidecars optionnels (adaptateur Telegram)

```bash
docker compose --profile dind up -d     # dockerized linters/tests for the team (set DOCKER_HOST=tcp://dind:2375)
```

(Le proxy TLS pour webhooks entrants est provisionné par le déploiement Ansible, et non par un profil compose — définissez `webhook_domain` dans le `group_vars/all/vars.yml` de l'instance pour l'activer.)

## Isolation par chat

Chaque chat reçoit `/workspace/chat_<id>` : tête-à-tête → votre espace de travail privé ; groupe → un espace de travail partagé pour les membres de ce groupe ; différents chats sont totalement isolés et s'exécutent **en parallèle** (plafonné par `MAX_CONCURRENT_CHAT_RUNS` pour la mémoire). La liste blanche d'identifiants d'utilisateurs détermine qui peut utiliser le bot. Dans les groupes, définissez `REQUIRE_GROUP_MENTION=true` pour que le bot ne réponde que lorsqu'il est @mentionné ou cité en réponse.

## Avancé : déployer sur un serveur avec Ansible (adaptateur Telegram)

Pour un provisionnement en une commande d'un VPS neuf (installe Docker + swap, génère le `.env`, récupère l'image, la démarre) :

```bash
cd adapters/telegram/deploy
cp -r inventories/example inventories/my-bot          # one dir per bot instance
$EDITOR inventories/my-bot/inventory.ini              # your server IP + SSH key
$EDITOR inventories/my-bot/group_vars/all/vars.yml    # non-secret config
$EDITOR inventories/my-bot/group_vars/all/vault.yml   # secrets (tokens)
ansible-vault encrypt inventories/my-bot/group_vars/all/vault.yml   # optional
ansible-playbook -i inventories/my-bot/inventory.ini playbook.yml
```

Chaque instance de bot est son propre `inventories/<name>/` ; seul `inventories/example/` est versionné — les instances réelles sont ignorées par git. Le rôle récupère l'image pré-construite — définissez `bot_image` pour épingler un tag. (Pour construire votre propre image après un fork, exécutez `docker build -f adapters/telegram/Dockerfile .` depuis la racine du dépôt ; l'image VK est `docker build -f adapters/vk/Dockerfile .`.)

## Sécurité

- **Liste blanche :** seule la liste des utilisateurs autorisés (`ALLOWED_USERS` pour Telegram, `VK_ALLOWED_USERS` pour VK) peut utiliser le bot — ne la laissez jamais vide ; le bot accorde un accès shell/édition à votre serveur.
- **Isolation par chat :** différents chats obtiennent des espaces de travail séparés. Le token git est partagé au sein d'un déploiement — limitez sa portée en conséquence.
- **Secrets :** conservez-les dans `.env` (ignoré par git) ou, pour Ansible, dans le `inventories/<name>/group_vars/all/vault.yml` d'une instance réelle (ignoré par git ; chiffrable avec `ansible-vault`). Ne validez jamais de vrais tokens — seul `inventories/example/` est suivi.
- **Isolation :** l'agent s'exécute en tant qu'utilisateur non-root à l'intérieur du conteneur ; ses commandes Bash/Edit sont confinées au conteneur, pas à votre hôte.

## Build, lint, test

Le dépôt utilise [Task](https://taskfile.dev) comme exécuteur de CI — le même point d'entrée que [CI](.github/workflows/ci.yml) :

```bash
task lint      # format + vet + linters (in the dev-tools image)
task tests     # Go test suite
task build     # compile the binaries
```

La CI exécute en plus `task security-scan` (govulncheck) et un scan de système de fichiers Trivy, et construit à blanc les deux images d'adaptateur à chaque PR.

## Licence

[MIT](LICENSE) © DuckBug.
