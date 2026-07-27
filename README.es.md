<p align="center">
  <a href="README.md">English</a> ·
  <a href="README.ru.md">Русский</a> ·
  <a href="README.zh.md">中文</a> ·
  <b>Español</b> ·
  <a href="README.de.md">Deutsch</a> ·
  <a href="README.fr.md">Français</a> ·
  <a href="README.pt-BR.md">Português (BR)</a> ·
  <a href="README.ja.md">日本語</a>
</p>

# Flock

**Ejecuta un equipo de desarrollo con IA de Claude Code en tu servidor y dirígelo desde el chat.** Describe una funcionalidad en Telegram o VK; el equipo la planifica, la construye en una rama, la prueba, la revisa y abre un PR — cada chat en su propio espacio de trabajo aislado.

[![CI](https://github.com/duckbugio/flock/actions/workflows/ci.yml/badge.svg)](https://github.com/duckbugio/flock/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](go.mod)
[![image: ghcr.io](https://img.shields.io/badge/image-ghcr.io-2496ED.svg)](https://github.com/orgs/duckbugio/packages)

Funciona con tu **suscripción Pro/Max** de Claude (sin facturación por token) o con una clave de API de Anthropic, se distribuye como **imágenes Docker precompiladas** (sin paso de compilación) y mantiene cada chat en su propio espacio de trabajo aislado en un sandbox.

## Inicio rápido (Docker)

```bash
git clone https://github.com/duckbugio/flock
cd flock/adapters/telegram
cp .env.example .env        # fill in the REQUIRED block (4 values)
docker compose up -d
```

Esto descarga la imagen precompilada `ghcr.io/duckbugio/flock-telegram` — sin construcción, sin Ansible — y luego envíale un mensaje a tu bot. El `.env` mínimo:

| Variable | Qué |
|---|---|
| `TELEGRAM_BOT_TOKEN` | de [@BotFather](https://t.me/botfather) |
| `TELEGRAM_BOT_USERNAME` | el @username de tu bot (sin `@`) |
| `ALLOWED_USERS` | IDs de usuario de Telegram, separados por comas, autorizados a usar el bot |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token` (suscripción) — *o* establece `ANTHROPIC_API_KEY` |

Todo lo demás en [`.env.example`](adapters/telegram/.env.example) tiene valores predeterminados razonables. Actualiza más adelante con `docker compose pull && docker compose up -d`.

> **Región:** aloja en una **región compatible con Anthropic** (algunos países, p. ej. RU/CN, están geobloqueados) — de lo contrario, las llamadas a Claude fallarán.

**VK** sigue el mismo patrón bajo [`adapters/vk/`](adapters/vk/), construido sobre el mismo núcleo y publicado como `ghcr.io/duckbugio/flock-vk`. Incluye solo una plantilla de entorno (sin archivo compose): `cp .env.example .env`, luego `docker run --env-file .env ghcr.io/duckbugio/flock-vk`. La autenticación de Claude y los ajustes del núcleo coinciden con los de Telegram; solo cambian las tres variables de transporte:

| Variable | Qué |
|---|---|
| `VK_BOT_TOKEN` | token de acceso de la comunidad (comunidad de VK → Gestionar → Uso de API → token de acceso) |
| `VK_GROUP_ID` | el id numérico de tu comunidad (servidor long-poll + análisis de menciones) |
| `VK_ALLOWED_USERS` | IDs de usuario de VK, separados por comas, autorizados a usar el bot |

## Aspectos destacados

- **La conversación es la fuente de las tareas** — describe lo que quieres en el chat y revisa el PR que llega de vuelta; el shell y el editor del agente están aislados en un sandbox dentro del contenedor.
- **Un pipeline real de equipo de desarrollo, no un único prompt** — criterios de aceptación basados en la especificación, controles de construcción/regresión y un árbitro que rompe los bucles.
- **Multitransporte** — **Telegram** y **VK** hoy, ambos sobre el mismo núcleo; una nueva plataforma es un adaptador ligero, no un fork.
- **Reacciones a PR sin webhooks entrantes** — el bot *sondea* tu host de git en busca de nuevos comentarios de revisión y enruta cada uno de vuelta al chat que abrió el PR.
- **Compatible con suscripción** — autentícate con un token Pro/Max de Claude (sin coste por token) o con una clave de API de Anthropic.

## Cómo funciona

```
You (in a chat): "implement X across the api + web services"
  → bot's Claude (Lead) → planner → confirm scope → coder ⇄ tester → PR per repo
                                                      → reviewer (inline comments) ⇄ coder → arbiter
                                                                                        ├ APPROVE → you merge
                                                                                        └ ESCALATE → asks you
```

Los cinco subagentes — **planner → coder → tester → reviewer → arbiter** — se ejecutan como subagentes nativos de Claude Code en [`core/agents/`](core/agents/). Una pregunta sencilla simplemente se responde; una solicitud de construcción activa al equipo. El **arbiter** es el que rompe los bucles, consciente del riesgo y con límite de ciclos, para que los agentes nunca giren indefinidamente. Las ramas se nombran `duck/<chatid>/<slug>` para que los eventos de webhook/sondeo de PR se enruten de vuelta al chat correcto.

El equipo está diseñado para un espacio de trabajo de **microservicios**: una funcionalidad puede abarcar varios servicios, y coordina las ramas y un PR con enlaces cruzados por repositorio. El pipeline completo, las salvaguardas y la tabla de roles viven en [`core/README.md`](core/README.md).

## Estructura del repositorio (monorepo)

El cerebro del equipo de desarrollo, agnóstico de plataforma, vive en [`core/`](core/); cada plataforma es un adaptador ligero bajo `adapters/<name>/` que lo comparte.

| Adaptador | Ruta | Imagen precompilada |
|---|---|---|
| Telegram | [`adapters/telegram/`](adapters/telegram/) | `ghcr.io/duckbugio/flock-telegram` |
| VK | [`adapters/vk/`](adapters/vk/) | `ghcr.io/duckbugio/flock-vk` |

Las plataformas futuras reutilizan el mismo núcleo — consulta [`docs/multi-transport-plan.md`](docs/multi-transport-plan.md).

## Conectar un host de git (opcional pero fundamental)

Establece esto en `.env` para permitir que el equipo clone repositorios y abra PR (funciona con **Gitea/GitHub/GitLab**):

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

Para **github.com**, establece también `GH_TOKEN` (= tu `GIT_TOKEN`) para que el CLI de `gh` pueda abrir PR.

El **poller** es la forma recomendada de reaccionar a los comentarios de revisión — sale *hacia fuera*, así que funciona incluso cuando tu host no puede alcanzar al bot. Está activo cuando `ENABLE_PR_REVIEW=true` y `GITEA_API_URL` está establecido. Una alternativa con webhook entrante + proxy TLS de Caddy está disponible solo a través del despliegue con Ansible (establece `webhook_domain`).

## Otras opciones

- **Mensajes de voz:** `ENABLE_VOICE_MESSAGES=true`, `VOICE_PROVIDER=mistral|openai|local`, más `MISTRAL_API_KEY` (o `OPENAI_API_KEY`). Se transcriben y se ejecutan como comandos.
- **Sidecar dind:** `docker compose --profile dind up -d` le da al equipo linters/pruebas dockerizados (establece `DOCKER_HOST=tcp://dind:2375`).
- **Aislamiento por chat:** cada chat obtiene `/workspace/chat_<id>` (1:1 → privado; grupo → un espacio de trabajo compartido); los chats están totalmente aislados y se ejecutan en paralelo, limitado por `MAX_CONCURRENT_CHAT_RUNS`. En los grupos, establece `REQUIRE_GROUP_MENTION=true` para responder solo cuando se te @menciona o se te responde.
- **Despliegue con Ansible** (Telegram): aprovisionamiento de un VPS con un solo comando desde `adapters/telegram/deploy` — copia `inventories/example` a tu propio `inventories/<name>/` (en gitignore), rellena el inventario/vars/vault, luego `ansible-playbook -i inventories/<name>/inventory.ini playbook.yml`. El rol descarga la imagen precompilada en la primera ejecución y luego la conserva: una reejecución normal no va a buscar una compilación más nueva — reinicia el contenedor con la imagen a la que esa etiqueta ya apunta localmente en ese host, salvo que en el host ya no quede una copia de esa etiqueta (primera instalación, o un `compose down` más la limpieza semanal): entonces Compose la descarga de nuevo para poder levantar la pila. Para actualizar, vuelve a ejecutarlo con `-e bot_image_pull=true`, lo que actualiza toda la pila (el bot más los sidecars en uso — dind, Caddy) y recrea los contenedores; establece `bot_image` para fijar una etiqueta — un despliegue fijado cambia de versión editando ese valor y reejecutando, no con el interruptor: una etiqueta fija no tiene nada más nuevo que resolver.

## Seguridad

- **Lista blanca:** solo `ALLOWED_USERS` (Telegram) / `VK_ALLOWED_USERS` (VK) pueden usar el bot — nunca la dejes vacía; concede acceso de shell/edición a tu servidor.
- **Aislamiento por chat:** los distintos chats obtienen espacios de trabajo separados. El token de git se comparte en todo un despliegue — defínele el alcance en consecuencia.
- **Secretos:** mantenlos en `.env` (en gitignore) o, para Ansible, en el `vault.yml` de una instancia real (en gitignore, cifrable con `ansible-vault`). Solo `inventories/example/` se versiona.
- **Sandbox:** el agente se ejecuta como un usuario sin privilegios de root; su Bash/Edit están confinados al contenedor, no a tu host.

## Construir, lint, probar

El repositorio usa [Task](https://taskfile.dev) como su ejecutor de CI — el mismo punto de entrada que usa [CI](.github/workflows/ci.yml):

```bash
task lint      # format + vet + linters (in the dev-tools image)
task tests     # Go test suite
task build     # compile the binaries
```

## Licencia

[MIT](LICENSE) © DuckBug.

## Contribuir y seguridad

Guías para contribuir y para reportar vulnerabilidades: [CONTRIBUTING.md](CONTRIBUTING.md), [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md), [SECURITY.md](SECURITY.md).
