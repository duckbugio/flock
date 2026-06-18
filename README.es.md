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

# DuckFlock

**Ejecuta un equipo de desarrollo de IA con Claude Code en tu servidor y dirígelo desde el chat.** Describe una funcionalidad en Telegram o VK; el equipo la planifica, la construye en una rama, la prueba, la revisa y abre un PR — cada chat en su propio espacio de trabajo aislado.

[![CI](https://github.com/duckbugio/flock/actions/workflows/ci.yml/badge.svg)](https://github.com/duckbugio/flock/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](go.mod)
[![image: ghcr.io](https://img.shields.io/badge/image-ghcr.io-2496ED.svg)](https://github.com/orgs/duckbugio/packages)

DuckFlock es un servicio nativo en Go que dirige el CLI de Claude Code como un **equipo de desarrollo** de cinco roles y lo expone a través del chat. Hablas con él como con un colega: una pregunta sencilla obtiene respuesta; "implementa X en los servicios api y web" pone en marcha una verdadera tubería — especificación y criterios de aceptación → construcción → controles de lint y regresión → PR → revisión adversarial con comentarios en línea anclados a líneas → un árbitro que decide si publicar o escalar.

Funciona con tu **suscripción Pro/Max** de Claude (sin facturación por token) o con una clave de API de Anthropic, se distribuye como **imágenes Docker precompiladas** (sin paso de compilación) y mantiene cada chat en su propio espacio de trabajo aislado en un sandbox.

## Por qué DuckFlock

- **La conversación es la fuente de las tareas.** Sin tablero, sin cola de incidencias — describe lo que quieres en el chat y revisa el PR que llega de vuelta. El shell y el editor del agente están **aislados dentro del contenedor**, no en tu host.
- **Una tubería real de equipo de desarrollo, no un único prompt.** Cinco subagentes — **planner → coder → tester → reviewer → arbiter** — con criterios de aceptación basados en la especificación, controles de construcción y regresión, y un árbitro que rompe los bucles para que los agentes nunca giren indefinidamente.
- **Multitransporte.** Hoy hay dos adaptadores en pie de igualdad — **Telegram** y **VK** — ambos construidos sobre el mismo núcleo agnóstico de plataforma. Añadir una nueva plataforma es un adaptador ligero, no un fork.
- **Aislamiento por chat, ejecución en paralelo.** Cada chat (1:1 o de grupo) obtiene su propio `/workspace/chat_<id>`; los chats no pueden ver los archivos de los demás, y los distintos chats se atienden de forma concurrente.
- **Reacciones a PR sin webhooks entrantes.** El bot *sondea* tu host de git en busca de nuevos comentarios de revisión y los atiende, enrutando cada uno de vuelta al chat que abrió el PR — funciona incluso cuando tu host no puede alcanzar al bot.
- **Compatible con suscripción.** Autentícate con un token Pro/Max de Claude (`claude setup-token`) — sin coste por token — o recurre a una clave de API de Anthropic.
- **Voz opcional.** Los mensajes de voz se transcriben (Mistral Voxtral / OpenAI Whisper / local) y se ejecutan como comandos.

## Estructura del repositorio (monorepo)

El cerebro del equipo de desarrollo agnóstico de plataforma — subagentes y orquestación — vive en [`core/`](core/). Cada plataforma de chat es un **adaptador** ligero bajo `adapters/<name>/` que comparte `core/`:

| Adaptador | Ruta | Imagen precompilada |
|---|---|---|
| Telegram | [`adapters/telegram/`](adapters/telegram/) | `ghcr.io/duckbugio/flock-telegram` |
| VK | [`adapters/vk/`](adapters/vk/) | `ghcr.io/duckbugio/flock-vk` |

Las plataformas futuras (por ejemplo, MAX) obtienen su propio `adapters/<name>/` y reutilizan el mismo núcleo — consulta [`docs/multi-transport-plan.md`](docs/multi-transport-plan.md).

## Inicio rápido (Telegram, Docker — recomendado)

```bash
git clone https://github.com/duckbugio/flock
cd flock/adapters/telegram
cp .env.example .env        # fill in the REQUIRED block (4 values)
docker compose up -d
```

Esto descarga la imagen precompilada `ghcr.io/duckbugio/flock-telegram` — sin construcción, sin Ansible. Luego envíale un mensaje a tu bot. El `.env` mínimo:

| Variable | Qué |
|---|---|
| `TELEGRAM_BOT_TOKEN` | de [@BotFather](https://t.me/botfather) |
| `TELEGRAM_BOT_USERNAME` | el @username de tu bot (sin `@`) |
| `ALLOWED_USERS` | IDs de usuario de Telegram, separados por comas, autorizados a usar el bot |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token` (suscripción) — *o* establece `ANTHROPIC_API_KEY` |

Todo lo demás en [`.env.example`](adapters/telegram/.env.example) tiene valores predeterminados razonables. Actualiza la imagen más adelante con `docker compose pull && docker compose up -d`.

> **Región:** aloja en una **región compatible con Anthropic** (Anthropic geobloquea algunos países, p. ej. RU/CN) — de lo contrario, las llamadas a Claude fallarán.

## Inicio rápido (VK)

VK sigue el mismo patrón bajo [`adapters/vk/`](adapters/vk/), construido sobre el mismo núcleo y publicado como `ghcr.io/duckbugio/flock-vk`. El adaptador de VK incluye solo una plantilla de entorno (sin archivo compose); cópiala y ejecuta la imagen con ella, p. ej. `docker run --env-file .env ghcr.io/duckbugio/flock-vk`:

```bash
cd flock/adapters/vk
cp .env.example .env        # fill in the REQUIRED block
```

El `.env` de VK cambia las tres variables de transporte; la autenticación de Claude y todos los ajustes del núcleo son idénticos a los de Telegram:

| Variable | Qué |
|---|---|
| `VK_BOT_TOKEN` | token de acceso de la comunidad (comunidad de VK → Gestionar → Uso de API → token de acceso) |
| `VK_GROUP_ID` | el id numérico de tu comunidad (servidor long-poll + análisis de menciones) |
| `VK_ALLOWED_USERS` | IDs de usuario de VK, separados por comas, autorizados a usar el bot |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token` (suscripción) — *o* establece `ANTHROPIC_API_KEY` |

Consulta [`adapters/vk/.env.example`](adapters/vk/.env.example) para ver la lista completa (los valores predeterminados coinciden con los del adaptador de Telegram).

## Cómo funciona

```
You (in a chat): "implement X across the api + web services"
  → bot's Claude (Lead) → planner → confirm scope → coder ⇄ tester → PR per repo
                                                      → reviewer (inline comments) ⇄ coder → arbiter
                                                                                        ├ APPROVE → you merge
                                                                                        └ ESCALATE → asks you
```

El **arbiter** es el que rompe los bucles (consciente del riesgo, con límite de ciclos) para que los agentes nunca entren en bucle indefinidamente. Una pregunta sencilla simplemente se responde; una solicitud de construcción activa al equipo. Las ramas se nombran `duck/<chatid>/<slug>` para que los eventos de webhook/sondeo de PR se enruten de vuelta al chat correcto.

El equipo de desarrollo está diseñado para un espacio de trabajo de **microservicios**: una funcionalidad puede abarcar varios servicios, y el equipo coordina las ramas y un PR con enlaces cruzados por repositorio. La tubería completa, las salvaguardas y la tabla de roles viven en [`core/README.md`](core/README.md).

## El equipo de desarrollo

Los roles viven en [`core/agents/`](core/agents/) como subagentes nativos de Claude Code:

| Subagente | Función |
|---|---|
| [`planner`](core/agents/planner.md) | Especificación (≥3 criterios de aceptación) + servicios afectados + plan ordenado + riesgos |
| [`coder`](core/agents/coder.md) | Implementa en una rama; seguro frente a contratos entre servicios |
| [`tester`](core/agents/tester.md) | Ejecuta/escribe pruebas, incl. la costura entre servicios; control de construcción en verde |
| [`reviewer`](core/agents/reviewer.md) | Revisión adversarial + puntuación de RIESGO + comprobaciones entre servicios |
| [`arbiter`](core/agents/arbiter.md) | Hace cumplir el control; decide continuar/publicar/escalar — el que rompe los bucles |

La tubería toma prestados patrones (no el framework) de [ruflo](https://github.com/ruvnet/ruflo) — controles de especificación SPARC, coordinación multirrepositorio y puntuación de riesgo de diffs. Las reglas de orquestación se incluyen en la imagen y se renderizan en cada espacio de trabajo como `CLAUDE.md`. Consulta [`core/README.md`](core/README.md) para los detalles.

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

El **poller** es la forma recomendada de reaccionar a los comentarios de revisión — funciona incluso cuando tu host de git no puede alcanzar al bot (p. ej., filtrado de red transfronterizo), porque el bot es el que sale *hacia fuera*. Está activo cuando `ENABLE_PR_REVIEW=true` y `GITEA_API_URL` está establecido. Los webhooks entrantes más un proxy TLS de Caddy son una alternativa opcional, disponible solo a través del despliegue con Ansible (establece `webhook_domain` en el `group_vars/all/vars.yml` de la instancia; consulta la sección de Ansible más abajo).

## Recordatorio de estrella en GitHub (opcional, solo para GitHub)

Cuando el bot está conectado a una cuenta de GitHub (`GIT_HOST=github.com` + `GIT_TOKEN`) que aún no le ha dado una estrella al proyecto, envía un breve mensaje después de cada ejecución exitosa invitándote a darle una estrella, con un botón en línea. Al pulsar el botón se le da una estrella al repositorio desde la propia cuenta del despliegue, y el recordatorio se detiene definitivamente. Es exclusivo de GitHub y está **desactivado automáticamente** en todos los demás casos (la condición es el interruptor de apagado — sin flag aparte); se ejecuta completamente fuera de la ruta crítica, por lo que nunca retrasa ni rompe una ejecución.

```ini
STAR_NUDGE_REPO=duckbugio/flock   # owner/repo to nudge for / star
# STAR_NUDGE_STORE_PATH=          # default <APPROVED_DIRECTORY>/star_nudge.json
```

El botón necesita un token con **permiso de escritura de estrellas**: un PAT clásico con `public_repo`, o un token de alcance fino con el permiso de cuenta "Starring". Sin él, la estrella simplemente falla de forma silenciosa (la ejecución no se ve afectada).

## Mensajes de voz (opcional)

```ini
ENABLE_VOICE_MESSAGES=true
VOICE_PROVIDER=mistral        # mistral | openai | local
MISTRAL_API_KEY=...           # console.mistral.ai  (or OPENAI_API_KEY for whisper)
```

## Sidecars opcionales (adaptador de Telegram)

```bash
docker compose --profile dind up -d     # dockerized linters/tests for the team (set DOCKER_HOST=tcp://dind:2375)
```

(El proxy TLS de webhook entrante lo aprovisiona el despliegue con Ansible, no es un perfil de compose — establece `webhook_domain` en el `group_vars/all/vars.yml` de la instancia para habilitarlo.)

## Aislamiento por chat

Cada chat obtiene `/workspace/chat_<id>`: 1:1 → tu espacio de trabajo privado; grupo → un espacio de trabajo compartido para los miembros de ese grupo; los distintos chats están totalmente aislados y se ejecutan **en paralelo** (limitado por `MAX_CONCURRENT_CHAT_RUNS` por la memoria). La lista blanca de IDs de usuario controla quién puede usar el bot. En los grupos, establece `REQUIRE_GROUP_MENTION=true` para que el bot responda solo cuando se le @menciona o se le responde.

## Avanzado: desplegar en un servidor con Ansible (adaptador de Telegram)

Para un aprovisionamiento con un solo comando de un VPS nuevo (instala Docker + swap, renderiza `.env`, descarga la imagen, la inicia):

```bash
cd adapters/telegram/deploy
cp -r inventories/example inventories/my-bot          # one dir per bot instance
$EDITOR inventories/my-bot/inventory.ini              # your server IP + SSH key
$EDITOR inventories/my-bot/group_vars/all/vars.yml    # non-secret config
$EDITOR inventories/my-bot/group_vars/all/vault.yml   # secrets (tokens)
ansible-vault encrypt inventories/my-bot/group_vars/all/vault.yml   # optional
ansible-playbook -i inventories/my-bot/inventory.ini playbook.yml
```

Cada instancia del bot tiene su propio `inventories/<name>/`; solo se versiona `inventories/example/` — las instancias reales están en gitignore. El rol descarga la imagen precompilada — establece `bot_image` para fijar una etiqueta. (Para construir tu propia imagen tras hacer un fork, ejecuta `docker build -f adapters/telegram/Dockerfile .` desde la raíz del repositorio; la imagen de VK es `docker build -f adapters/vk/Dockerfile .`.)

## Seguridad

- **Lista blanca:** solo la lista de usuarios permitidos (`ALLOWED_USERS` para Telegram, `VK_ALLOWED_USERS` para VK) puede usar el bot — nunca la dejes vacía; el bot concede acceso de shell/edición a tu servidor.
- **Aislamiento por chat:** los distintos chats obtienen espacios de trabajo separados. El token de git se comparte en todo un despliegue — defínele el alcance en consecuencia.
- **Secretos:** mantenlos en `.env` (en gitignore) o, para Ansible, en el `inventories/<name>/group_vars/all/vault.yml` de una instancia real (en gitignore; cifrable con `ansible-vault`). Nunca subas tokens reales — solo `inventories/example/` se versiona.
- **Aislamiento:** el agente se ejecuta como un usuario sin privilegios de root dentro del contenedor; su Bash/Edit están confinados al contenedor, no a tu host.

## Construir, lint, probar

El repositorio usa [Task](https://taskfile.dev) como su ejecutor de CI — el mismo punto de entrada que usa [CI](.github/workflows/ci.yml):

```bash
task lint      # format + vet + linters (in the dev-tools image)
task tests     # Go test suite
task build     # compile the binaries
```

CI ejecuta además `task security-scan` (govulncheck) y un escaneo del sistema de archivos con Trivy, y hace una construcción de prueba de ambas imágenes de adaptador en cada PR.

## Licencia

[MIT](LICENSE) © DuckBug.
