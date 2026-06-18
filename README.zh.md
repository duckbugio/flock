<p align="center">
  <a href="README.md">English</a> ·
  <a href="README.ru.md">Русский</a> ·
  <b>中文</b> ·
  <a href="README.es.md">Español</a> ·
  <a href="README.de.md">Deutsch</a> ·
  <a href="README.fr.md">Français</a> ·
  <a href="README.pt-BR.md">Português (BR)</a> ·
  <a href="README.ja.md">日本語</a>
</p>

# DuckFlock

**在你自己的服务器上运行一支 Claude Code AI 开发团队，并通过聊天来驱动它。** 在 Telegram 或 VK 中描述一项功能；团队会为它制定计划、在分支上构建、测试、评审，并发起一个 PR——每个聊天都拥有自己隔离的工作区。

[![CI](https://github.com/duckbugio/flock/actions/workflows/ci.yml/badge.svg)](https://github.com/duckbugio/flock/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](go.mod)
[![image: ghcr.io](https://img.shields.io/badge/image-ghcr.io-2496ED.svg)](https://github.com/orgs/duckbugio/packages)

DuckFlock 是一个原生 Go 服务，它将 Claude Code CLI 驱动为一支由五个角色组成的**开发团队**，并通过聊天对外提供能力。你可以像对待同事一样与它交流：普通的提问会得到解答；而「在 api 和 web 服务中实现 X」则会启动一条真正的流水线——规格说明与验收标准 → 构建 → lint 与回归门禁 → PR → 带有行级锚定内联评论的对抗式评审 → 由一位仲裁者决定是发布还是上报。

它可以基于你的 Claude **Pro/Max 订阅**（无按 token 计费）或一个 Anthropic API 密钥运行，以**预构建的 Docker 镜像**形式发布（无需构建步骤），并将每个聊天保持在各自的沙箱化工作区中。

## 为什么选择 DuckFlock

- **对话即任务来源。** 没有看板，没有工单队列——只需在聊天中描述你想要什么，然后评审返回的 PR。智能体的 shell 和编辑器是**沙箱化在容器内部**的，而不是在你的宿主机上。
- **一条真正的开发团队流水线，而非单个提示词。** 五个子智能体——**planner → coder → tester → reviewer → arbiter**——具备规格优先的验收标准、构建与回归门禁，以及一位打破循环的仲裁者，让智能体永远不会无止境地空转。
- **多传输通道。** 如今有两个对等的适配器——**Telegram** 与 **VK**——二者都构建于同一套平台无关的核心之上。新增一个平台只是一个轻量适配器，而非一次分叉。
- **按聊天隔离，并行运行。** 每个聊天（一对一或群组）都拥有自己的 `/workspace/chat_<id>`；不同聊天之间看不到彼此的文件，而且不同聊天会被并发处理。
- **无需入站 webhook 即可响应 PR。** 机器人会向你的 git 主机*轮询*新的评审评论并加以处理，将每条评论路由回发起该 PR 的聊天——即便你的主机无法访问机器人也照样有效。
- **对订阅友好。** 使用 Claude Pro/Max 令牌进行认证（`claude setup-token`）——无按 token 计费——或退而求其次使用 Anthropic API 密钥。
- **可选语音。** 语音消息会被转写（Mistral Voxtral / OpenAI Whisper / 本地），并作为命令运行。

## 仓库结构（单体仓库）

平台无关的开发团队大脑——子智能体与编排——位于 [`core/`](core/)。每个聊天平台都是 `adapters/<name>/` 下一个共享 `core/` 的轻量**适配器**：

| 适配器 | 路径 | 预构建镜像 |
|---|---|---|
| Telegram | [`adapters/telegram/`](adapters/telegram/) | `ghcr.io/duckbugio/flock-telegram` |
| VK | [`adapters/vk/`](adapters/vk/) | `ghcr.io/duckbugio/flock-vk` |

未来的平台（例如 MAX）会获得各自的 `adapters/<name>/` 并复用同一套核心——参见 [`docs/multi-transport-plan.md`](docs/multi-transport-plan.md)。

## 快速上手（Telegram，Docker——推荐）

```bash
git clone https://github.com/duckbugio/flock
cd flock/adapters/telegram
cp .env.example .env        # fill in the REQUIRED block (4 values)
docker compose up -d
```

这会拉取预构建镜像 `ghcr.io/duckbugio/flock-telegram`——无需构建，也无需 Ansible。然后给你的机器人发消息即可。最简 `.env`：

| 变量 | 含义 |
|---|---|
| `TELEGRAM_BOT_TOKEN` | 来自 [@BotFather](https://t.me/botfather) |
| `TELEGRAM_BOT_USERNAME` | 你机器人的 @username（不带 `@`） |
| `ALLOWED_USERS` | 允许使用该机器人的 Telegram 用户 ID，以逗号分隔 |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token`（订阅）——*或者*设置 `ANTHROPIC_API_KEY` |

[`.env.example`](adapters/telegram/.env.example) 中的其余一切都有合理的默认值。日后可用 `docker compose pull && docker compose up -d` 更新镜像。

> **地区：** 请托管在 **Anthropic 支持的地区**（Anthropic 对部分国家进行地理封锁，例如 RU/CN）——否则 Claude 调用会失败。

## 快速上手（VK）

VK 在 [`adapters/vk/`](adapters/vk/) 下采用相同的模式，构建于同一套核心之上，并以 `ghcr.io/duckbugio/flock-vk` 形式发布。VK 适配器只附带一个环境变量模板（没有 compose 文件）；复制它并据此运行镜像即可，例如 `docker run --env-file .env ghcr.io/duckbugio/flock-vk`：

```bash
cd flock/adapters/vk
cp .env.example .env        # fill in the REQUIRED block
```

VK 的 `.env` 替换了三个传输变量；Claude 认证以及所有核心设置都与 Telegram 完全相同：

| 变量 | 含义 |
|---|---|
| `VK_BOT_TOKEN` | 社区访问令牌（VK 社区 → 管理 → API 使用 → 访问令牌） |
| `VK_GROUP_ID` | 你社区的数字 id（长轮询服务器 + 提及解析） |
| `VK_ALLOWED_USERS` | 允许使用该机器人的 VK 用户 ID，以逗号分隔 |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token`（订阅）——*或者*设置 `ANTHROPIC_API_KEY` |

完整列表见 [`adapters/vk/.env.example`](adapters/vk/.env.example)（默认值与 Telegram 适配器一致）。

## 工作原理

```
You (in a chat): "implement X across the api + web services"
  → bot's Claude (Lead) → planner → confirm scope → coder ⇄ tester → PR per repo
                                                      → reviewer (inline comments) ⇄ coder → arbiter
                                                                                        ├ APPROVE → you merge
                                                                                        └ ESCALATE → asks you
```

**arbiter** 是打破循环者（感知风险、限制循环次数），让智能体永远不会无止境地循环。普通提问会被直接解答；构建请求则触发整个团队。分支以 `duck/<chatid>/<slug>` 命名，以便 PR-webhook/轮询事件能路由回正确的聊天。

该开发团队是为**微服务**工作区而设计的：一项功能可以横跨多个服务，团队会协调各个分支并为每个仓库发起一个交叉链接的 PR。完整的流水线、防护栏与角色表见 [`core/README.md`](core/README.md)。

## 开发团队

各个角色作为原生 Claude Code 子智能体位于 [`core/agents/`](core/agents/)：

| 子智能体 | 职责 |
|---|---|
| [`planner`](core/agents/planner.md) | 规格说明（≥3 条验收标准）+ 受影响服务 + 有序计划 + 风险 |
| [`coder`](core/agents/coder.md) | 在分支上实现；跨服务保持契约安全 |
| [`tester`](core/agents/tester.md) | 运行/编写测试，包括跨服务接缝；绿色构建门禁 |
| [`reviewer`](core/agents/reviewer.md) | 对抗式评审 + 风险评分 + 跨服务检查 |
| [`arbiter`](core/agents/arbiter.md) | 执行门禁；裁决继续/发布/上报——打破循环者 |

该流水线借鉴了 [ruflo](https://github.com/ruvnet/ruflo) 的模式（而非其框架）——SPARC 规格门禁、多仓库协调以及 diff 风险评分。编排规则随镜像一同发布，并在每个工作区中渲染为 `CLAUDE.md`。详情见 [`core/README.md`](core/README.md)。

## 接入 git 主机（可选但属核心）

在 `.env` 中设置以下内容，让团队能够克隆仓库并发起 PR（适用于 **Gitea/GitHub/GitLab**）：

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

对于 **github.com**，还需设置 `GH_TOKEN`（= 你的 `GIT_TOKEN`），以便 `gh` CLI 能发起 PR。

**轮询器**是响应评审评论的推荐方式——即便你的 git 主机无法访问机器人（例如跨境网络过滤）它也能工作，因为是机器人主动向*外*发起请求。当 `ENABLE_PR_REVIEW=true` 且设置了 `GITEA_API_URL` 时它处于激活状态。入站 webhook 加上 Caddy TLS 代理是一种可选的替代方案，仅通过 Ansible 部署提供（在实例的 `group_vars/all/vars.yml` 中设置 `webhook_domain`；参见下文的 Ansible 章节）。

## GitHub Star 提示（可选，仅限 GitHub）

当机器人接入一个尚未 star 本项目的 GitHub 账户（`GIT_HOST=github.com` + `GIT_TOKEN`）时，它会在每次成功运行后发送一条简短消息，邀请你为其加星，并附带一个内联按钮。按下该按钮即会用部署自身的账户为仓库加星，此后该提示便会永久停止。它仅限 GitHub，并在其他所有场景下**自动关闭**（这道门禁就是关闭开关——无需单独的标志位）；它完全运行在热路径之外，因此永远不会延迟或破坏一次运行。

```ini
STAR_NUDGE_REPO=duckbugio/flock   # owner/repo to nudge for / star
# STAR_NUDGE_STORE_PATH=          # default <APPROVED_DIRECTORY>/star_nudge.json
```

该按钮需要一个具备 **star-write 权限范围**的令牌：一个带 `public_repo` 的经典 PAT，或一个带「Starring」账户权限的细粒度令牌。没有它，加星只会静默失败（不影响运行）。

## 语音消息（可选）

```ini
ENABLE_VOICE_MESSAGES=true
VOICE_PROVIDER=mistral        # mistral | openai | local
MISTRAL_API_KEY=...           # console.mistral.ai  (or OPENAI_API_KEY for whisper)
```

## 可选边车（Telegram 适配器）

```bash
docker compose --profile dind up -d     # dockerized linters/tests for the team (set DOCKER_HOST=tcp://dind:2375)
```

（入站 webhook 的 TLS 代理由 Ansible 部署来配置，而非一个 compose profile——在实例的 `group_vars/all/vars.yml` 中设置 `webhook_domain` 以启用它。）

## 按聊天隔离

每个聊天都会获得 `/workspace/chat_<id>`：一对一 → 你的私有工作区；群组 → 为该群组成员共享的一个工作区；不同聊天彼此完全隔离并**并行**运行（受 `MAX_CONCURRENT_CHAT_RUNS` 限制以控制内存）。用户 ID 白名单把关谁可以使用该机器人。在群组中，设置 `REQUIRE_GROUP_MENTION=true` 可让机器人仅在被 @提及或被回复时才响应。

## 进阶：用 Ansible 部署到服务器（Telegram 适配器）

要一条命令配置一台全新的 VPS（安装 Docker + swap、渲染 `.env`、拉取镜像、启动它）：

```bash
cd adapters/telegram/deploy
cp -r inventories/example inventories/my-bot          # one dir per bot instance
$EDITOR inventories/my-bot/inventory.ini              # your server IP + SSH key
$EDITOR inventories/my-bot/group_vars/all/vars.yml    # non-secret config
$EDITOR inventories/my-bot/group_vars/all/vault.yml   # secrets (tokens)
ansible-vault encrypt inventories/my-bot/group_vars/all/vault.yml   # optional
ansible-playbook -i inventories/my-bot/inventory.ini playbook.yml
```

每个机器人实例都是各自的 `inventories/<name>/`；仅 `inventories/example/` 会被提交——真实实例已被 gitignore。该角色会拉取预构建镜像——设置 `bot_image` 以固定某个标签。（要在 fork 之后构建你自己的镜像，在仓库根目录运行 `docker build -f adapters/telegram/Dockerfile .`；VK 镜像为 `docker build -f adapters/vk/Dockerfile .`。）

## 安全

- **白名单：** 只有处于允许用户列表中的人（Telegram 为 `ALLOWED_USERS`，VK 为 `VK_ALLOWED_USERS`）才能使用该机器人——切勿将其留空；机器人会授予对你服务器的 shell/编辑访问权限。
- **按聊天隔离：** 不同聊天会获得各自独立的工作区。git 令牌在整个部署内共享——请据此设定其权限范围。
- **机密：** 将它们保存在 `.env`（已 gitignore）中，或对于 Ansible，保存在某个真实实例的 `inventories/<name>/group_vars/all/vault.yml`（已 gitignore；可用 `ansible-vault` 加密）中。切勿提交真实令牌——只有 `inventories/example/` 会被纳入版本控制。
- **隔离：** 智能体在容器内以非 root 用户运行；其 Bash/Edit 被限定在容器内，而非你的宿主机。

## 构建、lint、测试

本仓库使用 [Task](https://taskfile.dev) 作为其 CI 运行器——与 [CI](.github/workflows/ci.yml) 所用的入口相同：

```bash
task lint      # format + vet + linters (in the dev-tools image)
task tests     # Go test suite
task build     # compile the binaries
```

CI 还会额外运行 `task security-scan`（govulncheck）和一次 Trivy 文件系统扫描，并在每个 PR 上对两个适配器镜像进行试构建。

## 许可证

[MIT](LICENSE) © DuckBug。
