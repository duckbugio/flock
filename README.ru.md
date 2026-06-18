<p align="center">
  <a href="README.md">English</a> ·
  <b>Русский</b> ·
  <a href="README.zh.md">中文</a> ·
  <a href="README.es.md">Español</a> ·
  <a href="README.de.md">Deutsch</a> ·
  <a href="README.fr.md">Français</a> ·
  <a href="README.pt-BR.md">Português (BR)</a> ·
  <a href="README.ja.md">日本語</a>
</p>

# Flock

**Запустите команду AI-разработчиков на базе Claude Code на своём сервере и управляйте ею из чата.** Опишите фичу в Telegram или VK; команда спланирует её, реализует в ветке, протестирует, проведёт ревью и откроет PR — каждый чат в собственном изолированном рабочем пространстве.

[![CI](https://github.com/duckbugio/flock/actions/workflows/ci.yml/badge.svg)](https://github.com/duckbugio/flock/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](go.mod)
[![image: ghcr.io](https://img.shields.io/badge/image-ghcr.io-2496ED.svg)](https://github.com/orgs/duckbugio/packages)

Работает на вашей **подписке Claude Pro/Max** (без потокенной оплаты) или на API-ключе Anthropic, поставляется в виде **готовых Docker-образов** (без этапа сборки) и держит каждый чат в собственном изолированном рабочем пространстве.

## Быстрый старт (Docker)

```bash
git clone https://github.com/duckbugio/flock
cd flock/adapters/telegram
cp .env.example .env        # fill in the REQUIRED block (4 values)
docker compose up -d
```

Это подтянет готовый образ `ghcr.io/duckbugio/flock-telegram` — без сборки, без Ansible — после чего напишите своему боту. Минимальный `.env`:

| Переменная | Что это |
|---|---|
| `TELEGRAM_BOT_TOKEN` | от [@BotFather](https://t.me/botfather) |
| `TELEGRAM_BOT_USERNAME` | @username вашего бота (без `@`) |
| `ALLOWED_USERS` | список Telegram user ID через запятую, которым разрешено использовать бота |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token` (подписка) — *или* задайте `ANTHROPIC_API_KEY` |

У всего остального в [`.env.example`](adapters/telegram/.env.example) есть разумные значения по умолчанию. Обновить позже можно командой `docker compose pull && docker compose up -d`.

> **Регион:** размещайте в **поддерживаемом Anthropic регионе** (некоторые страны, например RU/CN, заблокированы по геолокации) — иначе вызовы Claude будут падать.

**VK** устроен по тому же принципу в [`adapters/vk/`](adapters/vk/), построен на том же ядре и публикуется как `ghcr.io/duckbugio/flock-vk`. Он поставляется только с шаблоном env (без compose-файла): `cp .env.example .env`, затем `docker run --env-file .env ghcr.io/duckbugio/flock-vk`. Аутентификация Claude и настройки ядра совпадают с Telegram; меняются только три транспортные переменные:

| Переменная | Что это |
|---|---|
| `VK_BOT_TOKEN` | токен доступа сообщества (сообщество VK → Управление → Работа с API → ключ доступа) |
| `VK_GROUP_ID` | числовой id вашего сообщества (long-poll сервер + разбор упоминаний) |
| `VK_ALLOWED_USERS` | список VK user ID через запятую, которым разрешено использовать бота |

## Ключевые особенности

- **Источник задач — сам разговор** — опишите, что вам нужно, в чате и проверьте пришедший в ответ PR; оболочка и редактор агента изолированы внутри контейнера.
- **Настоящий конвейер команды разработки, а не один промпт** — критерии приёмки на основе спецификации, шлюзы сборки и регрессий, а также арбитр, который разрывает циклы.
- **Несколько транспортов** — сегодня **Telegram** и **VK**, оба на одном ядре; новая платформа — это тонкий адаптер, а не форк.
- **Реакции на PR без входящих вебхуков** — бот *опрашивает* ваш git-хост на предмет новых комментариев ревью и направляет каждый обратно в тот чат, где был открыт PR.
- **Дружелюбен к подписке** — аутентифицируйтесь токеном Claude Pro/Max (без потокенной оплаты) или API-ключом Anthropic.

## Как это работает

```
You (in a chat): "implement X across the api + web services"
  → bot's Claude (Lead) → planner → confirm scope → coder ⇄ tester → PR per repo
                                                      → reviewer (inline comments) ⇄ coder → arbiter
                                                                                        ├ APPROVE → you merge
                                                                                        └ ESCALATE → asks you
```

Пять субагентов — **planner → coder → tester → reviewer → arbiter** — работают как нативные субагенты Claude Code в [`core/agents/`](core/agents/). На обычный вопрос просто даётся ответ; запрос на сборку запускает команду. **Арбитр** — это размыкатель циклов, учитывающий риски и ограниченный числом итераций, благодаря чему агенты никогда не зацикливаются. Ветки именуются `duck/<chatid>/<slug>`, чтобы события вебхуков/опроса PR направлялись обратно в нужный чат.

Команда рассчитана на **микросервисное** рабочее пространство: фича может затрагивать несколько сервисов, и команда координирует ветки и по одному перекрёстно связанному PR на репозиторий. Полный конвейер, ограничители и таблица ролей описаны в [`core/README.md`](core/README.md).

## Структура репозитория (монорепо)

Платформенно-независимый «мозг» команды разработки находится в [`core/`](core/); каждая платформа — это тонкий адаптер в `adapters/<name>/`, использующий общее ядро.

| Адаптер | Путь | Готовый образ |
|---|---|---|
| Telegram | [`adapters/telegram/`](adapters/telegram/) | `ghcr.io/duckbugio/flock-telegram` |
| VK | [`adapters/vk/`](adapters/vk/) | `ghcr.io/duckbugio/flock-vk` |

Будущие платформы переиспользуют то же ядро — см. [`docs/multi-transport-plan.md`](docs/multi-transport-plan.md).

## Подключение git-хоста (опционально, но это ядро)

Задайте эти переменные в `.env`, чтобы команда могла клонировать репозитории и открывать PR (работает с **Gitea/GitHub/GitLab**):

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

Для **github.com** также задайте `GH_TOKEN` (= вашему `GIT_TOKEN`), чтобы CLI `gh` мог открывать PR.

**Поллер** — рекомендуемый способ реагировать на комментарии ревью: он обращается *наружу*, поэтому работает даже тогда, когда ваш хост не может достучаться до бота. Он активен, когда установлены `ENABLE_PR_REVIEW=true` и `GITEA_API_URL`. Альтернатива с входящим вебхуком и TLS-прокси на Caddy доступна только через развёртывание Ansible (задайте `webhook_domain`).

## Прочие опции

- **Голосовые сообщения:** `ENABLE_VOICE_MESSAGES=true`, `VOICE_PROVIDER=mistral|openai|local`, плюс `MISTRAL_API_KEY` (или `OPENAI_API_KEY`). Транскрибируются и выполняются как команды.
- **Сайдкар dind:** `docker compose --profile dind up -d` даёт команде линтеры/тесты в Docker (задайте `DOCKER_HOST=tcp://dind:2375`).
- **Изоляция по чатам:** каждый чат получает `/workspace/chat_<id>` (1:1 → приватное; группа → одно общее рабочее пространство); чаты полностью изолированы и выполняются параллельно с ограничением `MAX_CONCURRENT_CHAT_RUNS`. В группах задайте `REQUIRE_GROUP_MENTION=true`, чтобы отвечать только при @упоминании или в ответ на сообщение.
- **Развёртывание через Ansible** (Telegram): провижининг VPS одной командой из `adapters/telegram/deploy` — скопируйте `inventories/example` в собственный `inventories/<name>/` (в gitignore), заполните inventory/vars/vault, затем `ansible-playbook -i inventories/<name>/inventory.ini playbook.yml`. Роль подтягивает готовый образ; задайте `bot_image`, чтобы зафиксировать тег.

## Безопасность

- **Белый список:** пользоваться ботом могут только `ALLOWED_USERS` (Telegram) / `VK_ALLOWED_USERS` (VK) — никогда не оставляйте список пустым; он предоставляет доступ к оболочке/редактированию на вашем сервере.
- **Изоляция по чатам:** разные чаты получают отдельные рабочие пространства. Git-токен общий для всего развёртывания — выдавайте права соответственно.
- **Секреты:** держите их в `.env` (в gitignore) или, для Ansible, в `vault.yml` реального инстанса (в gitignore, шифруется через `ansible-vault`). Отслеживается только `inventories/example/`.
- **Песочница:** агент работает от имени не-root пользователя; его Bash/Edit ограничены контейнером, а не вашим хостом.

## Сборка, линтинг, тесты

Репозиторий использует [Task](https://taskfile.dev) в качестве CI-раннера — та же точка входа, что и в [CI](.github/workflows/ci.yml):

```bash
task lint      # format + vet + linters (in the dev-tools image)
task tests     # Go test suite
task build     # compile the binaries
```

## Лицензия

[MIT](LICENSE) © DuckBug.
