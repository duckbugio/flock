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

# DuckFlock

**Запустите AI-команду разработки на базе Claude Code на своём сервере и управляйте ею из чата.** Опишите фичу в Telegram или VK; команда спланирует её, реализует в ветке, протестирует, проведёт ревью и откроет PR — каждый чат в собственном изолированном рабочем пространстве.

[![CI](https://github.com/duckbugio/flock/actions/workflows/ci.yml/badge.svg)](https://github.com/duckbugio/flock/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](go.mod)
[![image: ghcr.io](https://img.shields.io/badge/image-ghcr.io-2496ED.svg)](https://github.com/orgs/duckbugio/packages)

DuckFlock — это нативный сервис на Go, который управляет CLI Claude Code как **командой разработки** из пяти ролей и предоставляет к ней доступ через чат. Вы общаетесь с ней как с коллегой: на обычный вопрос получаете ответ; запрос «реализуй X в сервисах api и web» запускает настоящий конвейер — спецификация и критерии приёмки → сборка → проверки линтинга и регрессий → PR → состязательное ревью с привязанными к строкам инлайн-комментариями → арбитр, который решает, отправлять или эскалировать.

Сервис работает на вашей **подписке Claude Pro/Max** (без оплаты по токенам) или с API-ключом Anthropic, поставляется в виде **готовых Docker-образов** (без этапа сборки) и держит каждый чат в собственном изолированном рабочем пространстве.

## Зачем нужен DuckFlock

- **Источник задач — это разговор.** Никаких досок, никакой очереди тикетов — опишите, что вам нужно, в чате и проверьте пришедший в ответ PR. Командная оболочка и редактор агента **изолированы внутри контейнера**, а не на вашем хосте.
- **Настоящий конвейер команды разработки, а не один промпт.** Пять субагентов — **planner → coder → tester → reviewer → arbiter** — с критериями приёмки на основе спецификации, проверками сборки и регрессий, а также арбитром, который разрывает циклы, чтобы агенты никогда не зацикливались.
- **Несколько транспортов.** Сегодня — два равноправных адаптера: **Telegram** и **VK**, оба построены на одном платформенно-независимом ядре. Добавление новой платформы — это тонкий адаптер, а не форк.
- **Изоляция по чатам, параллельное выполнение.** Каждый чат (личный или групповой) получает собственный `/workspace/chat_<id>`; чаты не видят файлы друг друга, а разные чаты обрабатываются одновременно.
- **Реакции на PR без входящих вебхуков.** Бот *опрашивает* ваш git-хост на предмет новых комментариев ревью и обрабатывает их, направляя каждый обратно в тот чат, где был открыт PR — работает даже тогда, когда хост не может достучаться до бота.
- **Дружелюбен к подписке.** Аутентифицируйтесь токеном Claude Pro/Max (`claude setup-token`) — без оплаты по токенам — или используйте API-ключ Anthropic в качестве запасного варианта.
- **Опциональный голос.** Голосовые сообщения транскрибируются (Mistral Voxtral / OpenAI Whisper / локально) и выполняются как команды.

## Структура репозитория (монорепо)

Платформенно-независимый «мозг» команды разработки — субагенты и оркестрация — находится в [`core/`](core/). Каждая чат-платформа — это тонкий **адаптер** в `adapters/<name>/`, использующий общий `core/`:

| Адаптер | Путь | Готовый образ |
|---|---|---|
| Telegram | [`adapters/telegram/`](adapters/telegram/) | `ghcr.io/duckbugio/flock-telegram` |
| VK | [`adapters/vk/`](adapters/vk/) | `ghcr.io/duckbugio/flock-vk` |

Будущие платформы (например, MAX) получают собственный `adapters/<name>/` и переиспользуют то же ядро — см. [`docs/multi-transport-plan.md`](docs/multi-transport-plan.md).

## Быстрый старт (Telegram, Docker — рекомендуется)

```bash
git clone https://github.com/duckbugio/flock
cd flock/adapters/telegram
cp .env.example .env        # fill in the REQUIRED block (4 values)
docker compose up -d
```

Это подтянет готовый образ `ghcr.io/duckbugio/flock-telegram` — без сборки, без Ansible. После этого напишите своему боту. Минимальный `.env`:

| Переменная | Что это |
|---|---|
| `TELEGRAM_BOT_TOKEN` | от [@BotFather](https://t.me/botfather) |
| `TELEGRAM_BOT_USERNAME` | @username вашего бота (без `@`) |
| `ALLOWED_USERS` | список Telegram-ID пользователей через запятую, которым разрешено использовать бота |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token` (подписка) — *или* задайте `ANTHROPIC_API_KEY` |

У всего остального в [`.env.example`](adapters/telegram/.env.example) есть разумные значения по умолчанию. Образ можно обновить позже командой `docker compose pull && docker compose up -d`.

> **Регион:** размещайте в **поддерживаемом Anthropic регионе** (Anthropic блокирует некоторые страны по геолокации, например RU/CN) — иначе вызовы Claude будут падать.

## Быстрый старт (VK)

VK устроен по тому же принципу в [`adapters/vk/`](adapters/vk/), построен на том же ядре и публикуется как `ghcr.io/duckbugio/flock-vk`. Адаптер VK поставляется только с шаблоном env (без compose-файла); скопируйте его и запустите образ с ним, например `docker run --env-file .env ghcr.io/duckbugio/flock-vk`:

```bash
cd flock/adapters/vk
cp .env.example .env        # fill in the REQUIRED block
```

В `.env` для VK меняются три транспортные переменные; аутентификация Claude и все настройки ядра идентичны Telegram:

| Переменная | Что это |
|---|---|
| `VK_BOT_TOKEN` | токен доступа сообщества (сообщество VK → Управление → Работа с API → ключ доступа) |
| `VK_GROUP_ID` | числовой id вашего сообщества (long-poll сервер + разбор упоминаний) |
| `VK_ALLOWED_USERS` | список VK-ID пользователей через запятую, которым разрешено использовать бота |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token` (подписка) — *или* задайте `ANTHROPIC_API_KEY` |

Полный список см. в [`adapters/vk/.env.example`](adapters/vk/.env.example) (значения по умолчанию совпадают с адаптером Telegram).

## Как это работает

```
You (in a chat): "implement X across the api + web services"
  → bot's Claude (Lead) → planner → confirm scope → coder ⇄ tester → PR per repo
                                                      → reviewer (inline comments) ⇄ coder → arbiter
                                                                                        ├ APPROVE → you merge
                                                                                        └ ESCALATE → asks you
```

**Арбитр** — это размыкатель циклов (учитывает риски, ограничен числом итераций), благодаря чему агенты никогда не зацикливаются. На обычный вопрос просто даётся ответ; запрос на сборку запускает команду. Ветки именуются `duck/<chatid>/<slug>`, чтобы события вебхуков/опроса PR направлялись обратно в нужный чат.

Команда разработки рассчитана на **микросервисное** рабочее пространство: фича может затрагивать несколько сервисов, и команда координирует ветки и по одному перекрёстно связанному PR на репозиторий. Полный конвейер, ограничители и таблица ролей описаны в [`core/README.md`](core/README.md).

## Команда разработки

Роли живут в [`core/agents/`](core/agents/) как нативные субагенты Claude Code:

| Субагент | Задача |
|---|---|
| [`planner`](core/agents/planner.md) | Спецификация (≥3 критериев приёмки) + затронутые сервисы + упорядоченный план + риски |
| [`coder`](core/agents/coder.md) | Реализует в ветке; безопасен с точки зрения контрактов между сервисами |
| [`tester`](core/agents/tester.md) | Запускает/пишет тесты, включая стык между сервисами; шлюз зелёной сборки |
| [`reviewer`](core/agents/reviewer.md) | Состязательное ревью + оценка риска (RISK) + межсервисные проверки |
| [`arbiter`](core/agents/arbiter.md) | Следит за соблюдением шлюза; решает продолжить/отправить/эскалировать — размыкатель циклов |

Конвейер заимствует паттерны (а не сам фреймворк) у [ruflo](https://github.com/ruvnet/ruflo) — шлюзы спецификации SPARC, координацию между репозиториями и оценку риска по диффу. Правила оркестрации поставляются в образе и разворачиваются в каждом рабочем пространстве как `CLAUDE.md`. Подробности см. в [`core/README.md`](core/README.md).

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

**Поллер** — рекомендуемый способ реагировать на комментарии ревью: он работает даже тогда, когда ваш git-хост не может достучаться до бота (например, при трансграничной сетевой фильтрации), потому что бот сам обращается *наружу*. Он активен, когда установлены `ENABLE_PR_REVIEW=true` и `GITEA_API_URL`. Входящие вебхуки вместе с TLS-прокси на Caddy — опциональная альтернатива, доступная только через развёртывание Ansible (задайте `webhook_domain` в `group_vars/all/vars.yml` инстанса; см. раздел про Ansible ниже).

## Напоминание о звезде на GitHub (опционально, только для GitHub)

Когда бот подключён к аккаунту GitHub (`GIT_HOST=github.com` + `GIT_TOKEN`), который ещё не поставил звезду проекту, он отправляет короткое сообщение после каждого успешного запуска с предложением поставить звезду и инлайн-кнопкой. Нажатие кнопки ставит звезду репозиторию от имени аккаунта самого развёртывания, после чего напоминание прекращается окончательно. Функция работает только для GitHub и **автоматически выключена** везде в остальных случаях (условие активации и есть выключатель — отдельный флаг не нужен); она выполняется полностью вне горячего пути, поэтому никогда не задерживает и не ломает запуск.

```ini
STAR_NUDGE_REPO=duckbugio/flock   # owner/repo to nudge for / star
# STAR_NUDGE_STORE_PATH=          # default <APPROVED_DIRECTORY>/star_nudge.json
```

Кнопке нужен токен с правом **star-write**: классический PAT с `public_repo` или fine-grained токен с разрешением аккаунта «Starring». Без него установка звезды просто мягко завершится неудачей (на запуск это не повлияет).

## Голосовые сообщения (опционально)

```ini
ENABLE_VOICE_MESSAGES=true
VOICE_PROVIDER=mistral        # mistral | openai | local
MISTRAL_API_KEY=...           # console.mistral.ai  (or OPENAI_API_KEY for whisper)
```

## Опциональные сайдкары (адаптер Telegram)

```bash
docker compose --profile dind up -d     # dockerized linters/tests for the team (set DOCKER_HOST=tcp://dind:2375)
```

(TLS-прокси для входящих вебхуков разворачивается через Ansible, а не как профиль compose — чтобы включить его, задайте `webhook_domain` в `group_vars/all/vars.yml` инстанса.)

## Изоляция по чатам

Каждый чат получает `/workspace/chat_<id>`: личный → ваше приватное рабочее пространство; групповой → одно общее рабочее пространство для участников этой группы; разные чаты полностью изолированы и выполняются **параллельно** (с ограничением `MAX_CONCURRENT_CHAT_RUNS` по памяти). Белый список ID-пользователей определяет, кто может использовать бота. В группах задайте `REQUIRE_GROUP_MENTION=true`, чтобы бот отвечал только при @упоминании или в ответ на его сообщение.

## Продвинуто: развёртывание на сервер с помощью Ansible (адаптер Telegram)

Для развёртывания свежего VPS одной командой (устанавливает Docker + swap, рендерит `.env`, подтягивает образ, запускает его):

```bash
cd adapters/telegram/deploy
cp -r inventories/example inventories/my-bot          # one dir per bot instance
$EDITOR inventories/my-bot/inventory.ini              # your server IP + SSH key
$EDITOR inventories/my-bot/group_vars/all/vars.yml    # non-secret config
$EDITOR inventories/my-bot/group_vars/all/vault.yml   # secrets (tokens)
ansible-vault encrypt inventories/my-bot/group_vars/all/vault.yml   # optional
ansible-playbook -i inventories/my-bot/inventory.ini playbook.yml
```

Каждый инстанс бота — это собственный `inventories/<name>/`; в репозиторий коммитится только `inventories/example/` — реальные инстансы добавлены в gitignore. Роль подтягивает готовый образ — задайте `bot_image`, чтобы зафиксировать тег. (Чтобы собрать собственный образ после форка, выполните `docker build -f adapters/telegram/Dockerfile .` из корня репозитория; образ VK — `docker build -f adapters/vk/Dockerfile .`.)

## Безопасность

- **Белый список:** пользоваться ботом могут только пользователи из списка разрешённых (`ALLOWED_USERS` для Telegram, `VK_ALLOWED_USERS` для VK) — никогда не оставляйте его пустым; бот предоставляет доступ к оболочке/редактированию на вашем сервере.
- **Изоляция по чатам:** разные чаты получают отдельные рабочие пространства. Git-токен общий для всего развёртывания — выдавайте права соответственно.
- **Секреты:** держите их в `.env` (в gitignore) или, для Ansible, в `inventories/<name>/group_vars/all/vault.yml` реального инстанса (в gitignore; шифруется через `ansible-vault`). Никогда не коммитьте реальные токены — отслеживается только `inventories/example/`.
- **Изоляция:** агент работает от имени не-root пользователя внутри контейнера; его Bash/Edit ограничены контейнером, а не вашим хостом.

## Сборка, линтинг, тесты

Репозиторий использует [Task](https://taskfile.dev) в качестве CI-раннера — та же точка входа, что и в [CI](.github/workflows/ci.yml):

```bash
task lint      # format + vet + linters (in the dev-tools image)
task tests     # Go test suite
task build     # compile the binaries
```

CI дополнительно запускает `task security-scan` (govulncheck) и сканирование файловой системы Trivy, а также выполняет тестовую сборку обоих образов адаптеров на каждом PR.

## Лицензия

[MIT](LICENSE) © DuckBug.
