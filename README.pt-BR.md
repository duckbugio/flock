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

**Rode um time de desenvolvimento de IA com Claude Code no seu servidor e comande-o pelo chat.** Descreva uma funcionalidade no Telegram ou VK; o time planeja, desenvolve em uma branch, testa, revisa e abre um PR — cada chat em seu próprio workspace isolado.

[![CI](https://github.com/duckbugio/flock/actions/workflows/ci.yml/badge.svg)](https://github.com/duckbugio/flock/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](go.mod)
[![image: ghcr.io](https://img.shields.io/badge/image-ghcr.io-2496ED.svg)](https://github.com/orgs/duckbugio/packages)

O DuckFlock é um serviço nativo em Go que comanda a CLI do Claude Code como um **time de desenvolvimento** de cinco papéis e o expõe pelo chat. Você conversa com ele como um colega: uma pergunta simples recebe uma resposta; "implemente X nos serviços api e web" dispara um pipeline de verdade — spec e critérios de aceitação → build → gates de lint e regressão → PR → revisão adversarial com comentários inline ancorados em linhas → um árbitro que decide entre publicar ou escalar.

Ele roda na sua **assinatura Pro/Max** do Claude (sem cobrança por token) ou em uma chave de API da Anthropic, é distribuído como **imagens Docker pré-construídas** (sem etapa de build) e mantém cada chat em seu próprio workspace em sandbox.

## Por que DuckFlock

- **A conversa é a fonte da tarefa.** Sem quadro, sem fila de issues — descreva o que você quer no chat
  e revise o PR que volta. O shell e o editor do agente ficam **em sandbox dentro do container**, não
  no seu host.
- **Um pipeline de time de desenvolvimento de verdade, não um único prompt.** Cinco subagentes — **planner → coder → tester →
  reviewer → arbiter** — com critérios de aceitação spec-first, gates de build e regressão, e um
  árbitro que quebra loops para que os agentes nunca fiquem rodando para sempre.
- **Multi-transporte.** Dois adaptadores equivalentes hoje — **Telegram** e **VK** — ambos construídos sobre o mesmo
  core agnóstico de plataforma. Adicionar uma nova plataforma é um adaptador fino, não um fork.
- **Isolamento por chat, executados em paralelo.** Cada chat (1:1 ou em grupo) recebe seu próprio
  `/workspace/chat_<id>`; os chats não enxergam os arquivos uns dos outros, e chats diferentes são respondidos
  simultaneamente.
- **Reações a PRs sem webhooks de entrada.** O bot *consulta* (poll) seu host git em busca de novos comentários de revisão
  e os trata, roteando cada um de volta ao chat que abriu o PR — funciona mesmo quando seu host
  não consegue alcançar o bot.
- **Amigável a assinaturas.** Autentique-se com um token Pro/Max do Claude (`claude setup-token`) — sem
  custo por token — ou recorra a uma chave de API da Anthropic.
- **Voz opcional.** Mensagens de voz transcritas (Mistral Voxtral / OpenAI Whisper / local) e executadas
  como comandos.

## Estrutura do repositório (monorepo)

O cérebro do time de desenvolvimento agnóstico de plataforma — subagentes e orquestração — vive em [`core/`](core/). Cada
plataforma de chat é um **adaptador** fino em `adapters/<name>/` que compartilha o `core/`:

| Adaptador | Caminho | Imagem pré-construída |
|---|---|---|
| Telegram | [`adapters/telegram/`](adapters/telegram/) | `ghcr.io/duckbugio/flock-telegram` |
| VK | [`adapters/vk/`](adapters/vk/) | `ghcr.io/duckbugio/flock-vk` |

Plataformas futuras (por exemplo, MAX) ganham seu próprio `adapters/<name>/` e reaproveitam o mesmo core — veja
[`docs/multi-transport-plan.md`](docs/multi-transport-plan.md).

## Início rápido (Telegram, Docker — recomendado)

```bash
git clone https://github.com/duckbugio/flock
cd flock/adapters/telegram
cp .env.example .env        # fill in the REQUIRED block (4 values)
docker compose up -d
```

Isso baixa a imagem pré-construída `ghcr.io/duckbugio/flock-telegram` — sem build, sem Ansible. Em seguida, mande mensagem
para o seu bot. O `.env` mínimo:

| Variável | O que é |
|---|---|
| `TELEGRAM_BOT_TOKEN` | obtido com o [@BotFather](https://t.me/botfather) |
| `TELEGRAM_BOT_USERNAME` | o @username do seu bot (sem `@`) |
| `ALLOWED_USERS` | IDs de usuário do Telegram, separados por vírgula, com permissão de uso do bot |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token` (assinatura) — *ou* defina `ANTHROPIC_API_KEY` |

Todo o resto em [`.env.example`](adapters/telegram/.env.example) tem valores padrão sensatos. Atualize a
imagem mais tarde com `docker compose pull && docker compose up -d`.

> **Região:** hospede em uma **região suportada pela Anthropic** (a Anthropic faz geobloqueio de alguns países, por exemplo,
> RU/CN) — caso contrário, as chamadas ao Claude falham.

## Início rápido (VK)

O VK segue o mesmo padrão em [`adapters/vk/`](adapters/vk/), construído sobre o mesmo core e publicado como
`ghcr.io/duckbugio/flock-vk`. O adaptador do VK traz apenas um template de env (sem arquivo compose); copie-o e
rode a imagem com ele, por exemplo, `docker run --env-file .env ghcr.io/duckbugio/flock-vk`:

```bash
cd flock/adapters/vk
cp .env.example .env        # fill in the REQUIRED block
```

O `.env` do VK troca as três variáveis de transporte; a autenticação do Claude e todas as configurações do core são
idênticas às do Telegram:

| Variável | O que é |
|---|---|
| `VK_BOT_TOKEN` | token de acesso da comunidade (comunidade VK → Gerenciar → Uso da API → token de acesso) |
| `VK_GROUP_ID` | o id numérico da sua comunidade (servidor de long-poll + parse de menção) |
| `VK_ALLOWED_USERS` | IDs de usuário do VK, separados por vírgula, com permissão de uso do bot |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token` (assinatura) — *ou* defina `ANTHROPIC_API_KEY` |

Veja [`adapters/vk/.env.example`](adapters/vk/.env.example) para a lista completa (os padrões coincidem com os do
adaptador do Telegram).

## Como funciona

```
You (in a chat): "implement X across the api + web services"
  → bot's Claude (Lead) → planner → confirm scope → coder ⇄ tester → PR per repo
                                                      → reviewer (inline comments) ⇄ coder → arbiter
                                                                                        ├ APPROVE → you merge
                                                                                        └ ESCALATE → asks you
```

O **arbiter** é quem quebra o loop (ciente de risco, com limite de ciclos) para que os agentes nunca entrem em loop para sempre. Uma pergunta
simples é apenas respondida; um pedido de build aciona o time. As branches são nomeadas
`duck/<chatid>/<slug>` para que eventos de webhook/poll de PR voltem ao chat correto.

O time de desenvolvimento foi projetado para um workspace de **microsserviços**: uma funcionalidade pode abranger vários serviços, e
o time coordena as branches e um PR com referência cruzada por repositório. O pipeline completo, as proteções e a
tabela de papéis vivem em [`core/README.md`](core/README.md).

## O time de desenvolvimento

Os papéis vivem em [`core/agents/`](core/agents/) como subagentes nativos do Claude Code:

| Subagente | Função |
|---|---|
| [`planner`](core/agents/planner.md) | Spec (≥3 critérios de aceitação) + serviços afetados + plano ordenado + riscos |
| [`coder`](core/agents/coder.md) | Implementa em uma branch; seguro quanto a contratos entre serviços |
| [`tester`](core/agents/tester.md) | Roda/escreve testes incl. a junção entre serviços; gate de build verde |
| [`reviewer`](core/agents/reviewer.md) | Revisão adversarial + pontuação de RISCO + verificações entre serviços |
| [`arbiter`](core/agents/arbiter.md) | Aplica o gate; decide continuar/publicar/escalar — quem quebra o loop |

O pipeline toma emprestados padrões (não o framework) do
[ruflo](https://github.com/ruvnet/ruflo) — spec-gates do SPARC, coordenação multi-repositório e
pontuação de risco de diff. As regras de orquestração são distribuídas na imagem e renderizadas em cada workspace como
`CLAUDE.md`. Veja [`core/README.md`](core/README.md) para os detalhes.

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

O **poller** é a forma recomendada de reagir a comentários de revisão — funciona mesmo quando seu host git
não consegue alcançar o bot (por exemplo, filtragem de rede transfronteiriça), porque é o bot que sai para alcançá-lo. Ele fica ativo
quando `ENABLE_PR_REVIEW=true` e `GITEA_API_URL` está definido. Webhooks de entrada mais um proxy TLS com Caddy são
uma alternativa opcional, disponível apenas pelo deploy com Ansible (defina `webhook_domain` no
`group_vars/all/vars.yml` da instância; veja a seção sobre Ansible abaixo).

## Convite para dar estrela no GitHub (opcional, apenas GitHub)

Quando o bot está conectado a uma conta do GitHub (`GIT_HOST=github.com` + `GIT_TOKEN`) que ainda não
deu estrela no projeto, ele envia uma mensagem curta após cada execução bem-sucedida convidando você a dar a estrela, com
um botão inline. Pressionar o botão dá a estrela no repositório a partir da própria conta do deployment, e o convite
então para definitivamente. É exclusivo do GitHub e **desligado automaticamente** em qualquer outro lugar (o gate é o interruptor de desligar —
sem flag separada); roda inteiramente fora do caminho crítico, então nunca atrasa nem quebra uma execução.

```ini
STAR_NUDGE_REPO=duckbugio/flock   # owner/repo to nudge for / star
# STAR_NUDGE_STORE_PATH=          # default <APPROVED_DIRECTORY>/star_nudge.json
```

O botão precisa de um token com **escopo de escrita de estrelas**: um PAT clássico com `public_repo`, ou um
token fine-grained com a permissão de conta "Starring". Sem ele, a estrela simplesmente falha de forma suave
(a execução não é afetada).

## Mensagens de voz (opcional)

```ini
ENABLE_VOICE_MESSAGES=true
VOICE_PROVIDER=mistral        # mistral | openai | local
MISTRAL_API_KEY=...           # console.mistral.ai  (or OPENAI_API_KEY for whisper)
```

## Sidecars opcionais (adaptador do Telegram)

```bash
docker compose --profile dind up -d     # dockerized linters/tests for the team (set DOCKER_HOST=tcp://dind:2375)
```

(O proxy TLS para webhook de entrada é provisionado pelo deploy com Ansible, não por um profile do compose — defina
`webhook_domain` no `group_vars/all/vars.yml` da instância para habilitá-lo.)

## Isolamento por chat

Cada chat recebe `/workspace/chat_<id>`: 1:1 → seu workspace privado; grupo → um workspace compartilhado para
os membros daquele grupo; chats diferentes são totalmente isolados e rodam **em paralelo** (limitados por
`MAX_CONCURRENT_CHAT_RUNS` por questão de memória). A allowlist de IDs de usuário controla quem pode usar o bot. Em grupos,
defina `REQUIRE_GROUP_MENTION=true` para fazer o bot responder apenas quando for @mencionado ou respondido.

## Avançado: implantar em um servidor com Ansible (adaptador do Telegram)

Para um provisionamento em um comando de um VPS novo (instala Docker + swap, renderiza o `.env`, baixa a imagem,
inicia tudo):

```bash
cd adapters/telegram/deploy
cp -r inventories/example inventories/my-bot          # one dir per bot instance
$EDITOR inventories/my-bot/inventory.ini              # your server IP + SSH key
$EDITOR inventories/my-bot/group_vars/all/vars.yml    # non-secret config
$EDITOR inventories/my-bot/group_vars/all/vault.yml   # secrets (tokens)
ansible-vault encrypt inventories/my-bot/group_vars/all/vault.yml   # optional
ansible-playbook -i inventories/my-bot/inventory.ini playbook.yml
```

Cada instância do bot é seu próprio `inventories/<name>/`; apenas `inventories/example/` é versionado — instâncias
reais ficam no gitignore. O role baixa a imagem pré-construída — defina `bot_image` para fixar uma tag. (Para construir
sua própria imagem após o fork, rode `docker build -f adapters/telegram/Dockerfile .` a partir da raiz do repositório;
a imagem do VK é `docker build -f adapters/vk/Dockerfile .`.)

## Segurança

- **Allowlist:** apenas a lista de usuários permitidos (`ALLOWED_USERS` para Telegram, `VK_ALLOWED_USERS` para VK)
  pode usar o bot — nunca a deixe vazia; o bot concede acesso de shell/edição ao seu servidor.
- **Isolamento por chat:** chats diferentes recebem workspaces separados. O token git é compartilhado por
  deployment — defina seu escopo de acordo.
- **Segredos:** mantenha-os no `.env` (no gitignore) ou, para o Ansible, no
  `inventories/<name>/group_vars/all/vault.yml` de uma instância real (no gitignore; criptografável com `ansible-vault`). Nunca
  versione tokens reais — apenas `inventories/example/` é rastreado.
- **Isolamento:** o agente roda como um usuário não-root dentro do container; seu Bash/Edit ficam confinados ao
  container, não ao seu host.

## Build, lint, testes

O repositório usa o [Task](https://taskfile.dev) como seu runner de CI — o mesmo entrypoint que a
[CI](.github/workflows/ci.yml) usa:

```bash
task lint      # format + vet + linters (in the dev-tools image)
task tests     # Go test suite
task build     # compile the binaries
```

A CI executa adicionalmente `task security-scan` (govulncheck) e um scan de filesystem do Trivy, e faz dry-build de
ambas as imagens de adaptador em cada PR.

## Licença

[MIT](LICENSE) © DuckBug.
