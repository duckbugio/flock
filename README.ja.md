<p align="center">
  <a href="README.md">English</a> ·
  <a href="README.ru.md">Русский</a> ·
  <a href="README.zh.md">中文</a> ·
  <a href="README.es.md">Español</a> ·
  <a href="README.de.md">Deutsch</a> ·
  <a href="README.fr.md">Français</a> ·
  <a href="README.pt-BR.md">Português (BR)</a> ·
  <b>日本語</b>
</p>

# DuckFlock

**Claude Code を使った AI 開発チームをサーバー上で動かし、チャットから指示しましょう。** Telegram や VK で機能を説明するだけで、チームがそれを計画し、ブランチ上で実装し、テストし、レビューして、PR を作成します。各チャットはそれぞれ隔離された専用ワークスペースで動作します。

[![CI](https://github.com/duckbugio/flock/actions/workflows/ci.yml/badge.svg)](https://github.com/duckbugio/flock/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](go.mod)
[![image: ghcr.io](https://img.shields.io/badge/image-ghcr.io-2496ED.svg)](https://github.com/orgs/duckbugio/packages)

Claude の **Pro/Max サブスクリプション**（トークン課金なし）または Anthropic API キーで動作し、**ビルド済み Docker イメージ**として配布され（ビルド手順は不要）、各チャットをそれぞれサンドボックス化されたワークスペースに隔離します。

## クイックスタート（Docker）

```bash
git clone https://github.com/duckbugio/flock
cd flock/adapters/telegram
cp .env.example .env        # fill in the REQUIRED block (4 values)
docker compose up -d
```

これによりビルド済みイメージ `ghcr.io/duckbugio/flock-telegram` が取得されます。ビルドも Ansible も不要で、あとは Bot にメッセージを送るだけです。最小限の `.env` は次のとおりです。

| 変数 | 内容 |
|---|---|
| `TELEGRAM_BOT_TOKEN` | [@BotFather](https://t.me/botfather) から取得 |
| `TELEGRAM_BOT_USERNAME` | Bot の @ユーザー名（`@` は不要） |
| `ALLOWED_USERS` | Bot の利用を許可する Telegram ユーザー ID のカンマ区切りリスト |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token`（サブスクリプション） — *または* `ANTHROPIC_API_KEY` を設定 |

[`.env.example`](adapters/telegram/.env.example) のそれ以外の項目には妥当なデフォルト値が設定されています。更新は後から `docker compose pull && docker compose up -d` で行えます。

> **リージョン:** **Anthropic がサポートするリージョン**でホストしてください（一部の国、例えば RU/CN などはジオブロックされています）。そうでない場合、Claude の呼び出しは失敗します。

**VK** も同じパターンで [`adapters/vk/`](adapters/vk/) に用意されており、同じコアの上に構築され、`ghcr.io/duckbugio/flock-vk` として公開されています。配布されるのは env テンプレートのみ（compose ファイルはありません）。`cp .env.example .env` のあと、`docker run --env-file .env ghcr.io/duckbugio/flock-vk` を実行します。Claude の認証とコア設定は Telegram と同じで、トランスポート用の 3 つの変数のみが異なります。

| 変数 | 内容 |
|---|---|
| `VK_BOT_TOKEN` | コミュニティアクセストークン（VK コミュニティ → 管理 → API の利用 → アクセストークン） |
| `VK_GROUP_ID` | コミュニティの数値 ID（ロングポーリングサーバー + メンション解析用） |
| `VK_ALLOWED_USERS` | Bot の利用を許可する VK ユーザー ID のカンマ区切りリスト |

## 主な特長

- **会話そのものがタスクの起点** — チャットでやりたいことを説明し、返ってくる PR をレビューするだけ。エージェントのシェルとエディタはコンテナ内にサンドボックス化されています。
- **単発のプロンプトではなく、本物の開発チームのパイプライン** — 仕様優先の受け入れ基準、ビルド/回帰テストのゲート、そしてループを断ち切る arbiter を備えています。
- **マルチトランスポート** — 現在は **Telegram** と **VK** に対応し、いずれも同じコアの上で動作します。新しいプラットフォームの追加はフォークではなく薄いアダプターで済みます。
- **インバウンド Webhook なしで PR に反応** — Bot は新しいレビューコメントを git ホストに対して*ポーリング*し、それぞれを PR を作成したチャットへ振り分けます。
- **サブスクリプションに優しい** — Claude Pro/Max トークン（トークン課金なし）または Anthropic API キーで認証できます。

## 仕組み

```
You (in a chat): "implement X across the api + web services"
  → bot's Claude (Lead) → planner → confirm scope → coder ⇄ tester → PR per repo
                                                      → reviewer (inline comments) ⇄ coder → arbiter
                                                                                        ├ APPROVE → you merge
                                                                                        └ ESCALATE → asks you
```

5 つのサブエージェント — **planner → coder → tester → reviewer → arbiter** — は、[`core/agents/`](core/agents/) にネイティブな Claude Code サブエージェントとして配置されています。単なる質問にはそのまま回答し、ビルドの依頼があるとチームが起動します。**arbiter** はリスクを認識しサイクルを制限するループブレーカーで、エージェントが永遠に空回りしないようにします。ブランチは `duck/<chatid>/<slug>` という名前が付けられ、PR の webhook/ポーリングイベントが適切なチャットへ振り分けられます。

このチームは**マイクロサービス**ワークスペース向けに作られています。機能は複数のサービスにまたがることがあり、チームはブランチを調整し、リポジトリごとに相互リンクされた PR を 1 つ作成します。パイプライン全体、ガードレール、役割の一覧は [`core/README.md`](core/README.md) にあります。

## リポジトリ構成（モノレポ）

プラットフォーム非依存の開発チームの頭脳は [`core/`](core/) にあります。各プラットフォームはそれを共有する `adapters/<name>/` 配下の薄いアダプターです。

| アダプター | パス | ビルド済みイメージ |
|---|---|---|
| Telegram | [`adapters/telegram/`](adapters/telegram/) | `ghcr.io/duckbugio/flock-telegram` |
| VK | [`adapters/vk/`](adapters/vk/) | `ghcr.io/duckbugio/flock-vk` |

今後のプラットフォームも同じコアを再利用します — [`docs/multi-transport-plan.md`](docs/multi-transport-plan.md) を参照してください。

## git ホストを接続する（オプションですが中核機能）

チームがリポジトリをクローンして PR を作成できるよう、`.env` に以下を設定します（**Gitea/GitHub/GitLab** で動作します）。

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

**github.com** の場合は、`gh` CLI が PR を作成できるよう `GH_TOKEN`（= あなたの `GIT_TOKEN`）も設定してください。

**ポーラー**はレビューコメントに反応するための推奨手段です。こちらから外向きに接続するため、ホストから Bot に到達できない環境でも動作します。`ENABLE_PR_REVIEW=true` かつ `GITEA_API_URL` が設定されているときに有効になります。インバウンド Webhook + Caddy TLS プロキシという代替手段は、Ansible デプロイ経由でのみ利用できます（`webhook_domain` を設定）。

## その他のオプション

- **音声メッセージ:** `ENABLE_VOICE_MESSAGES=true`、`VOICE_PROVIDER=mistral|openai|local`、加えて `MISTRAL_API_KEY`（または `OPENAI_API_KEY`）。文字起こしされてコマンドとして実行されます。
- **dind サイドカー:** `docker compose --profile dind up -d` で、Docker 化されたリンター/テストをチームに提供します（`DOCKER_HOST=tcp://dind:2375` を設定）。
- **チャットごとの隔離:** 各チャットには `/workspace/chat_<id>` が割り当てられます（1:1 → プライベート、グループ → 1 つの共有ワークスペース）。チャットは完全に隔離され、`MAX_CONCURRENT_CHAT_RUNS` を上限として並列実行されます。グループでは `REQUIRE_GROUP_MENTION=true` を設定すると、@メンションまたは返信されたときのみ応答します。
- **Ansible デプロイ**（Telegram）: `adapters/telegram/deploy` からワンコマンドで VPS をプロビジョニングします。`inventories/example` を自分用の `inventories/<name>/`（gitignore 対象）にコピーし、inventory/vars/vault を埋めてから `ansible-playbook -i inventories/<name>/inventory.ini playbook.yml` を実行します。ロールはビルド済みイメージを取得します。タグを固定するには `bot_image` を設定してください。

## セキュリティ

- **ホワイトリスト:** `ALLOWED_USERS`（Telegram）/ `VK_ALLOWED_USERS`（VK）のみが Bot を利用できます。決して空のままにしないでください。これはサーバーへのシェル/編集アクセスを付与するものです。
- **チャットごとの隔離:** 異なるチャットには別々のワークスペースが割り当てられます。git トークンはデプロイ全体で共有されるため、それに応じてスコープを設定してください。
- **シークレット:** シークレットは `.env`（gitignore 対象）に、Ansible の場合は実際のインスタンスの `vault.yml`（gitignore 対象、`ansible-vault` で暗号化可能）に保管してください。追跡対象は `inventories/example/` のみです。
- **サンドボックス:** エージェントは非 root ユーザーとして動作します。その Bash/Edit はコンテナ内に限定され、ホストには及びません。

## ビルド・リント・テスト

このリポジトリは CI ランナーとして [Task](https://taskfile.dev) を使用しており、[CI](.github/workflows/ci.yml) が使うのと同じエントリポイントです。

```bash
task lint      # format + vet + linters (in the dev-tools image)
task tests     # Go test suite
task build     # compile the binaries
```

## ライセンス

[MIT](LICENSE) © DuckBug.
