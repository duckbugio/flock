<p align="center">
  <a href="README.md">English</a> ·
  <a href="README.ru.md">Русский</a> ·
  <a href="README.zh.md">中文</a> ·
  <a href="README.es.md">Español</a> ·
  <a href="README.de.md">Deutsch</a> ·
  <a href="README.fr.md">Français</a> ·
  <b>Português (BR)</b> ·
  <a href="README.ja.md">日本語</a>
</p>

# DuckFlock

**Rode um time de desenvolvimento com IA do Claude Code no seu servidor e comande-o pelo chat.** Descreva uma funcionalidade no Telegram ou no VK; o time a planeja, constrói em uma branch, testa, revisa e abre um PR — cada chat em seu próprio workspace isolado.

[![CI](https://github.com/duckbugio/flock/actions/workflows/ci.yml/badge.svg)](https://github.com/duckbugio/flock/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](go.mod)
[![image: ghcr.io](https://img.shields.io/badge/image-ghcr.io-2496ED.svg)](https://github.com/orgs/duckbugio/packages)

Ele roda na sua **assinatura Pro/Max** do Claude (sem cobrança por token) ou em uma chave de API da Anthropic, é distribuído como **imagens Docker pré-construídas** (sem etapa de build) e mantém cada chat em seu próprio workspace isolado em sandbox.

## Início rápido (Docker)

```bash
git clone https://github.com/duckbugio/flock
cd flock/adapters/telegram
cp .env.example .env        # fill in the REQUIRED block (4 values)
docker compose up -d
```

Isso baixa a imagem pré-construída `ghcr.io/duckbugio/flock-telegram` — sem build, sem Ansible — então é só mandar mensagem para o seu bot. O `.env` mínimo:

| Variável | O que é |
|---|---|
| `TELEGRAM_BOT_TOKEN` | obtido com o [@BotFather](https://t.me/botfather) |
| `TELEGRAM_BOT_USERNAME` | o @username do seu bot (sem `@`) |
| `ALLOWED_USERS` | IDs de usuário do Telegram, separados por vírgula, com permissão de uso do bot |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token` (assinatura) — *ou* defina `ANTHROPIC_API_KEY` |

Todo o resto em [`.env.example`](adapters/telegram/.env.example) tem valores padrão sensatos. Atualize mais tarde com `docker compose pull && docker compose up -d`.

> **Região:** hospede em uma **região suportada pela Anthropic** (alguns países, por exemplo, RU/CN, sofrem geobloqueio) — caso contrário, as chamadas ao Claude falham.

O **VK** segue o mesmo padrão em [`adapters/vk/`](adapters/vk/), construído sobre o mesmo core e publicado como `ghcr.io/duckbugio/flock-vk`. Ele traz apenas um template de env (sem arquivo compose): `cp .env.example .env`, depois `docker run --env-file .env ghcr.io/duckbugio/flock-vk`. A autenticação do Claude e as configurações do core coincidem com as do Telegram; só mudam as três variáveis de transporte:

| Variável | O que é |
|---|---|
| `VK_BOT_TOKEN` | token de acesso da comunidade (comunidade VK → Gerenciar → Uso da API → token de acesso) |
| `VK_GROUP_ID` | o id numérico da sua comunidade (servidor de long-poll + parse de menção) |
| `VK_ALLOWED_USERS` | IDs de usuário do VK, separados por vírgula, com permissão de uso do bot |

## Destaques

- **A conversa é a fonte da tarefa** — descreva o que você quer no chat e revise o PR que volta; o shell e o editor do agente ficam em sandbox dentro do container.
- **Um pipeline de time de desenvolvimento de verdade, não um único prompt** — critérios de aceitação spec-first, gates de build/regressão e um árbitro que quebra loops.
- **Multi-transporte** — **Telegram** e **VK** hoje, ambos sobre o mesmo core; uma nova plataforma é um adaptador fino, não um fork.
- **Reações a PRs sem webhooks de entrada** — o bot *consulta* (poll) seu host git em busca de novos comentários de revisão e roteia cada um de volta ao chat que abriu o PR.
- **Amigável a assinaturas** — autentique-se com um token Pro/Max do Claude (sem custo por token) ou com uma chave de API da Anthropic.

## Como funciona

```
You (in a chat): "implement X across the api + web services"
  → bot's Claude (Lead) → planner → confirm scope → coder ⇄ tester → PR per repo
                                                      → reviewer (inline comments) ⇄ coder → arbiter
                                                                                        ├ APPROVE → you merge
                                                                                        └ ESCALATE → asks you
```

Os cinco subagentes — **planner → coder → tester → reviewer → arbiter** — rodam como subagentes nativos do Claude Code em [`core/agents/`](core/agents/). Uma pergunta simples é apenas respondida; um pedido de build aciona o time. O **arbiter** é quem quebra o loop (ciente de risco, com limite de ciclos) para que os agentes nunca fiquem rodando para sempre. As branches são nomeadas `duck/<chatid>/<slug>` para que eventos de webhook/poll de PR voltem ao chat correto.

O time foi projetado para um workspace de **microsserviços**: uma funcionalidade pode abranger vários serviços, e ele coordena as branches e um PR com referência cruzada por repositório. O pipeline completo, as proteções e a tabela de papéis vivem em [`core/README.md`](core/README.md).

## Estrutura do repositório (monorepo)

O cérebro do time de desenvolvimento agnóstico de plataforma vive em [`core/`](core/); cada plataforma é um adaptador fino em `adapters/<name>/` que o compartilha.

| Adaptador | Caminho | Imagem pré-construída |
|---|---|---|
| Telegram | [`adapters/telegram/`](adapters/telegram/) | `ghcr.io/duckbugio/flock-telegram` |
| VK | [`adapters/vk/`](adapters/vk/) | `ghcr.io/duckbugio/flock-vk` |

Plataformas futuras reaproveitam o mesmo core — veja [`docs/multi-transport-plan.md`](docs/multi-transport-plan.md).

## Conectar um host git (opcional, mas central)

Defina o seguinte no `.env` para permitir que o time clone repositórios e abra PRs (funciona com **Gitea/GitHub/GitLab**):

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

Para o **github.com**, defina também `GH_TOKEN` (= seu `GIT_TOKEN`) para que a CLI `gh` possa abrir PRs.

O **poller** é a forma recomendada de reagir a comentários de revisão — ele sai para *alcançar* o host, então funciona mesmo quando seu host não consegue alcançar o bot. Ele fica ativo quando `ENABLE_PR_REVIEW=true` e `GITEA_API_URL` está definido. Uma alternativa com webhook de entrada + proxy TLS com Caddy está disponível apenas pelo deploy com Ansible (defina `webhook_domain`).

## Outras opções

- **Mensagens de voz:** `ENABLE_VOICE_MESSAGES=true`, `VOICE_PROVIDER=mistral|openai|local`, mais `MISTRAL_API_KEY` (ou `OPENAI_API_KEY`). Transcritas e executadas como comandos.
- **Sidecar dind:** `docker compose --profile dind up -d` dá ao time linters/testes dockerizados (defina `DOCKER_HOST=tcp://dind:2375`).
- **Isolamento por chat:** cada chat recebe `/workspace/chat_<id>` (1:1 → privado; grupo → um workspace compartilhado); os chats são totalmente isolados e rodam em paralelo, limitados por `MAX_CONCURRENT_CHAT_RUNS`. Em grupos, defina `REQUIRE_GROUP_MENTION=true` para responder apenas quando for @mencionado ou respondido.
- **Deploy com Ansible** (Telegram): provisionamento de um VPS em um comando a partir de `adapters/telegram/deploy` — copie `inventories/example` para o seu próprio `inventories/<name>/` (no gitignore), preencha inventory/vars/vault e então `ansible-playbook -i inventories/<name>/inventory.ini playbook.yml`. O role baixa a imagem pré-construída; defina `bot_image` para fixar uma tag.

## Segurança

- **Whitelist:** apenas `ALLOWED_USERS` (Telegram) / `VK_ALLOWED_USERS` (VK) podem usar o bot — nunca a deixe vazia; ela concede acesso de shell/edição ao seu servidor.
- **Isolamento por chat:** chats diferentes recebem workspaces separados. O token git é compartilhado por deployment — defina seu escopo de acordo.
- **Segredos:** mantenha-os no `.env` (no gitignore) ou, para o Ansible, no `vault.yml` de uma instância real (no gitignore, criptografável com `ansible-vault`). Apenas `inventories/example/` é rastreado.
- **Sandbox:** o agente roda como um usuário não-root; seu Bash/Edit ficam confinados ao container, não ao seu host.

## Build, lint, testes

O repositório usa o [Task](https://taskfile.dev) como seu runner de CI — o mesmo entrypoint que a [CI](.github/workflows/ci.yml) usa:

```bash
task lint      # format + vet + linters (in the dev-tools image)
task tests     # Go test suite
task build     # compile the binaries
```

## Licença

[MIT](LICENSE) © DuckBug.
