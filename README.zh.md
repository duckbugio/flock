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

# Flock

**在你自己的服务器上运行一支 Claude Code AI 开发团队，并通过聊天来驱动它。** 在 Telegram 或 VK 中描述一项功能；团队会为它制定计划、在分支上构建、测试、评审，并发起一个 PR——每个聊天都拥有各自隔离的工作区。

[![CI](https://github.com/duckbugio/flock/actions/workflows/ci.yml/badge.svg)](https://github.com/duckbugio/flock/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](go.mod)
[![image: ghcr.io](https://img.shields.io/badge/image-ghcr.io-2496ED.svg)](https://github.com/orgs/duckbugio/packages)

它基于你的 Claude **Pro/Max 订阅**（无按 token 计费）或一个 Anthropic API 密钥运行，以**预构建的 Docker 镜像**形式发布（无需构建步骤），并将每个聊天保持在各自的沙箱化工作区中。

## 快速上手（Docker）

```bash
git clone https://github.com/duckbugio/flock
cd flock/adapters/telegram
cp .env.example .env        # fill in the REQUIRED block (4 values)
docker compose up -d
```

这会拉取预构建镜像 `ghcr.io/duckbugio/flock-telegram`——无需构建，也无需 Ansible——然后给你的机器人发消息即可。最简 `.env`：

| 变量 | 含义 |
|---|---|
| `TELEGRAM_BOT_TOKEN` | 来自 [@BotFather](https://t.me/botfather) |
| `TELEGRAM_BOT_USERNAME` | 你机器人的 @username（不带 `@`） |
| `ALLOWED_USERS` | 允许使用该机器人的 Telegram 用户 ID，以逗号分隔 |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token`（订阅）——*或者*设置 `ANTHROPIC_API_KEY` |

[`.env.example`](adapters/telegram/.env.example) 中的其余一切都有合理的默认值。日后可用 `docker compose pull && docker compose up -d` 更新。

> **地区：** 请托管在 **Anthropic 支持的地区**（部分国家/地区会被地理封锁，例如 RU/CN）——否则 Claude 调用会失败。

**VK** 在 [`adapters/vk/`](adapters/vk/) 下采用相同的模式，构建于同一套核心之上，并以 `ghcr.io/duckbugio/flock-vk` 形式发布。它只附带一个环境变量模板（没有 compose 文件）：`cp .env.example .env`，然后 `docker run --env-file .env ghcr.io/duckbugio/flock-vk`。Claude 认证与核心设置都与 Telegram 一致；只有三个传输变量不同：

| 变量 | 含义 |
|---|---|
| `VK_BOT_TOKEN` | 社区访问令牌（VK 社区 → 管理 → API 使用 → 访问令牌） |
| `VK_GROUP_ID` | 你社区的数字 id（长轮询服务器 + 提及解析） |
| `VK_ALLOWED_USERS` | 允许使用该机器人的 VK 用户 ID，以逗号分隔 |

## 亮点

- **对话即任务来源**——在聊天中描述你想要什么，然后评审返回的 PR；智能体的 shell 和编辑器都被沙箱化在容器内部。
- **一条真正的开发团队流水线，而非单个提示词**——规格优先的验收标准、构建/回归门禁，以及一位打破循环的仲裁者。
- **多传输通道**——如今有 **Telegram** 与 **VK**，二者都构建于同一套核心之上；新增一个平台只是一个轻量适配器，而非一次分叉。
- **无需入站 webhook 即可响应 PR**——机器人会向你的 git 主机*轮询*新的评审评论，并将每条评论路由回发起该 PR 的聊天。
- **对订阅友好**——使用 Claude Pro/Max 令牌（无按 token 计费）或 Anthropic API 密钥进行认证。

## 工作原理

```
You (in a chat): "implement X across the api + web services"
  → bot's Claude (Lead) → planner → confirm scope → coder ⇄ tester → PR per repo
                                                      → reviewer (inline comments) ⇄ coder → arbiter
                                                                                        ├ APPROVE → you merge
                                                                                        └ ESCALATE → asks you
```

五个子智能体——**planner → coder → tester → reviewer → arbiter**——作为原生 Claude Code 子智能体运行于 [`core/agents/`](core/agents/)。普通提问会被直接解答；构建请求则触发整个团队。**arbiter** 是感知风险、限制循环次数的打破循环者，让智能体永远不会无止境地空转。分支以 `duck/<chatid>/<slug>` 命名，以便 PR-webhook/轮询事件能路由回正确的聊天。

该开发团队是为**微服务**工作区而设计的：一项功能可以横跨多个服务，团队会协调各个分支并为每个仓库发起一个交叉链接的 PR。完整的流水线、防护栏与角色表见 [`core/README.md`](core/README.md)。

## 仓库结构（单体仓库）

平台无关的开发团队大脑位于 [`core/`](core/)；每个平台都是 `adapters/<name>/` 下一个共享它的轻量适配器。

| 适配器 | 路径 | 预构建镜像 |
|---|---|---|
| Telegram | [`adapters/telegram/`](adapters/telegram/) | `ghcr.io/duckbugio/flock-telegram` |
| VK | [`adapters/vk/`](adapters/vk/) | `ghcr.io/duckbugio/flock-vk` |

未来的平台会复用同一套核心——参见 [`docs/multi-transport-plan.md`](docs/multi-transport-plan.md)。

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

**轮询器**是响应评审评论的推荐方式——它主动向*外*发起请求，因此即便你的主机无法访问机器人它也照样有效。当 `ENABLE_PR_REVIEW=true` 且设置了 `GITEA_API_URL` 时它处于激活状态。一种入站 webhook 加上 Caddy TLS 代理的替代方案仅通过 Ansible 部署提供（设置 `webhook_domain`）。

## 其他选项

- **语音消息：** `ENABLE_VOICE_MESSAGES=true`、`VOICE_PROVIDER=mistral|openai|local`，外加 `MISTRAL_API_KEY`（或 `OPENAI_API_KEY`）。语音会被转写并作为命令运行。
- **dind 边车容器：** `docker compose --profile dind up -d` 为团队提供容器化的 linters/测试（设置 `DOCKER_HOST=tcp://dind:2375`）。
- **按聊天隔离：** 每个聊天都会获得一个 `/workspace/chat_<id>`（一对一 → 私有；群组 → 一个共享工作区）；不同聊天彼此完全隔离并行运行，受 `MAX_CONCURRENT_CHAT_RUNS` 限制。在群组中，设置 `REQUIRE_GROUP_MENTION=true` 可让机器人仅在被 @提及或被回复时才响应。
- **Ansible 部署**（Telegram）：从 `adapters/telegram/deploy` 一条命令配置一台 VPS——将 `inventories/example` 复制为你自己的 `inventories/<name>/`（已 gitignore），填好 inventory/vars/vault，然后执行 `ansible-playbook -i inventories/<name>/inventory.ini playbook.yml`。该角色在首次运行时拉取预构建镜像，之后一直沿用它：普通的重复运行不会去寻找更新的构建——它只是用该标签在这台主机本地已指向的镜像重启容器；除非这台主机上已没有该标签的副本（首次安装，或 `compose down` 加上每周清理），此时 Compose 会重新拉取它以便启动整个技术栈。要更新，请附带 `-e bot_image_pull=true` 重新运行，这会更新整个技术栈（bot 以及已启用的 sidecar——dind、Caddy）并重建容器；设置 `bot_image` 以固定某个标签。

## 安全

- **白名单：** 只有 `ALLOWED_USERS`（Telegram）/ `VK_ALLOWED_USERS`（VK）才能使用该机器人——切勿将其留空；它会授予对你服务器的 shell/编辑访问权限。
- **按聊天隔离：** 不同聊天会获得各自独立的工作区。git 令牌在整个部署内共享——请据此设定其权限范围。
- **机密：** 将它们保存在 `.env`（已 gitignore）中，或对于 Ansible，保存在某个真实实例的 `vault.yml`（已 gitignore，可用 `ansible-vault` 加密）中。只有 `inventories/example/` 会被纳入版本控制。
- **沙箱：** 智能体以非 root 用户运行；其 Bash/Edit 被限定在容器内，而非你的宿主机。

## 构建、lint、测试

本仓库使用 [Task](https://taskfile.dev) 作为其 CI 运行器——与 [CI](.github/workflows/ci.yml) 所用的入口相同：

```bash
task lint      # format + vet + linters (in the dev-tools image)
task tests     # Go test suite
task build     # compile the binaries
```

## 许可证

[MIT](LICENSE) © DuckBug.

## 贡献与安全

参与贡献和报告安全漏洞的指南：[CONTRIBUTING.md](CONTRIBUTING.md)、[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)、[SECURITY.md](SECURITY.md)。
