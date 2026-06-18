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

**Claude Code の AI 開発チームをサーバー上で動かし、チャットから操作。** Telegram や VK で機能を説明するだけで、チームがそれを計画し、ブランチ上で構築し、テストし、レビューして PR を開きます。各チャットはそれぞれ隔離されたワークスペースで動作します。

[![CI](https://github.com/duckbugio/flock/actions/workflows/ci.yml/badge.svg)](https://github.com/duckbugio/flock/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](go.mod)
[![image: ghcr.io](https://img.shields.io/badge/image-ghcr.io-2496ED.svg)](https://github.com/orgs/duckbugio/packages)

DuckFlock は、Claude Code CLI を 5 つの役割からなる **開発チーム** として動かし、チャット経由で公開するネイティブな Go サービスです。同僚と話すように操作できます。単純な質問にはそのまま回答が返り、「api と web のサービスにまたがって X を実装して」と頼めば実際のパイプラインが起動します。仕様と受け入れ基準 → 構築 → lint と回帰テストのゲート → PR → 行に紐づくインラインコメント付きの敵対的レビュー → 出荷かエスカレートかを決定する arbiter、という流れです。

Claude の **Pro/Max サブスクリプション**（トークン課金なし）または Anthropic API キーで動作し、**ビルド済みの Docker イメージ** として配布され（ビルド工程は不要）、各チャットをそれぞれサンドボックス化されたワークスペースに保ちます。

## DuckFlock を選ぶ理由

- **会話そのものがタスクの源泉。** ボードも課題キューも不要。やりたいことをチャットで説明し、返ってくる PR をレビューするだけです。エージェントのシェルとエディタは、ホスト上ではなく **コンテナ内にサンドボックス化** されています。
- **単一プロンプトではなく、本物の開発チームのパイプライン。** **planner → coder → tester → reviewer → arbiter** という 5 つのサブエージェントが、仕様を起点とした受け入れ基準、ビルドと回帰テストのゲート、そしてエージェントが永遠にループしないようループを断ち切る arbiter とともに連携します。
- **マルチトランスポート。** 現状は対等な 2 つのアダプター（**Telegram** と **VK**）があり、いずれも同じプラットフォーム非依存のコア上に構築されています。新しいプラットフォームの追加は薄いアダプターで済み、フォークは不要です。
- **チャットごとに隔離し、並列実行。** すべてのチャット（1 対 1 でもグループでも）が独自の `/workspace/chat_<id>` を持ち、チャット同士は互いのファイルを見ることができず、異なるチャットは並行して応答されます。
- **インバウンド Webhook なしで PR に反応。** ボットは新しいレビューコメントを git ホストに *ポーリング* して対応し、それぞれを PR を開いたチャットへ振り分けます。ホストからボットに到達できない場合でも機能します。
- **サブスクリプションに優しい。** Claude Pro/Max のトークン（`claude setup-token`）で認証すればトークン課金は不要です。あるいは Anthropic API キーにフォールバックできます。
- **任意で音声対応。** ボイスメッセージを文字起こしし（Mistral Voxtral / OpenAI Whisper / ローカル）、コマンドとして実行します。

## リポジトリ構成（モノレポ）

プラットフォーム非依存の開発チームの頭脳（サブエージェントとオーケストレーション）は [`core/`](core/) にあります。各チャットプラットフォームは `adapters/<name>/` 配下の薄い **アダプター** で、`core/` を共有します。

| アダプター | パス | ビルド済みイメージ |
|---|---|---|
| Telegram | [`adapters/telegram/`](adapters/telegram/) | `ghcr.io/duckbugio/flock-telegram` |
| VK | [`adapters/vk/`](adapters/vk/) | `ghcr.io/duckbugio/flock-vk` |

将来のプラットフォーム（例: MAX）はそれぞれ独自の `adapters/<name>/` を持ち、同じコアを再利用します。[`docs/multi-transport-plan.md`](docs/multi-transport-plan.md) を参照してください。

## クイックスタート（Telegram、Docker — 推奨）

```bash
git clone https://github.com/duckbugio/flock
cd flock/adapters/telegram
cp .env.example .env        # fill in the REQUIRED block (4 values)
docker compose up -d
```

これによりビルド済みイメージ `ghcr.io/duckbugio/flock-telegram` が取得されます。ビルドも Ansible も不要です。あとはボットにメッセージを送るだけです。最小限の `.env` は次のとおりです。

| 変数 | 内容 |
|---|---|
| `TELEGRAM_BOT_TOKEN` | [@BotFather](https://t.me/botfather) から取得 |
| `TELEGRAM_BOT_USERNAME` | ボットの @ユーザー名（`@` は不要） |
| `ALLOWED_USERS` | ボットの利用を許可する Telegram ユーザー ID（カンマ区切り） |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token`（サブスクリプション） — *または* `ANTHROPIC_API_KEY` を設定 |

[`.env.example`](adapters/telegram/.env.example) のそれ以外の項目には妥当なデフォルト値が設定されています。イメージを後から更新するには `docker compose pull && docker compose up -d` を実行します。

> **リージョン:** **Anthropic がサポートするリージョン** でホストしてください（Anthropic は一部の国、例えば RU/CN を地域ブロックしています）。そうでないと Claude の呼び出しが失敗します。

## クイックスタート（VK）

VK も [`adapters/vk/`](adapters/vk/) 配下で同じパターンに従い、同じコア上に構築され、`ghcr.io/duckbugio/flock-vk` として公開されています。VK アダプターには env テンプレートのみが同梱され（compose ファイルはありません）、それをコピーしてイメージとともに実行します。例えば `docker run --env-file .env ghcr.io/duckbugio/flock-vk` のようにします。

```bash
cd flock/adapters/vk
cp .env.example .env        # fill in the REQUIRED block
```

VK の `.env` ではトランスポート関連の 3 つの変数だけが入れ替わります。Claude の認証とコアの設定はすべて Telegram と同一です。

| 変数 | 内容 |
|---|---|
| `VK_BOT_TOKEN` | コミュニティのアクセストークン（VK コミュニティ → 管理 → API 利用 → アクセストークン） |
| `VK_GROUP_ID` | コミュニティの数値 ID（long-poll サーバー + メンション解析用） |
| `VK_ALLOWED_USERS` | ボットの利用を許可する VK ユーザー ID（カンマ区切り） |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude setup-token`（サブスクリプション） — *または* `ANTHROPIC_API_KEY` を設定 |

完全な一覧は [`adapters/vk/.env.example`](adapters/vk/.env.example) を参照してください（デフォルト値は Telegram アダプターと一致します）。

## 仕組み

```
You (in a chat): "implement X across the api + web services"
  → bot's Claude (Lead) → planner → confirm scope → coder ⇄ tester → PR per repo
                                                      → reviewer (inline comments) ⇄ coder → arbiter
                                                                                        ├ APPROVE → you merge
                                                                                        └ ESCALATE → asks you
```

**arbiter** はループを断ち切る役割（リスクを認識し、サイクル数に上限を設ける）で、エージェントが永遠にループすることを防ぎます。単純な質問にはそのまま回答が返り、構築の依頼はチームを起動します。ブランチは `duck/<chatid>/<slug>` という名前が付けられ、PR の webhook/ポーリングのイベントが正しいチャットへ振り分けられるようになっています。

開発チームは **マイクロサービス** のワークスペースを想定して設計されています。1 つの機能が複数のサービスにまたがる場合があり、チームはブランチを調整し、リポジトリごとに相互リンクされた PR を 1 つ開きます。パイプライン、ガードレール、役割の一覧の詳細は [`core/README.md`](core/README.md) にあります。

## 開発チーム

各役割は、ネイティブな Claude Code のサブエージェントとして [`core/agents/`](core/agents/) にあります。

| サブエージェント | 役割 |
|---|---|
| [`planner`](core/agents/planner.md) | 仕様（受け入れ基準 3 件以上） + 影響を受けるサービス + 順序付けされた計画 + リスク |
| [`coder`](core/agents/coder.md) | ブランチ上で実装。サービス間で契約を壊さない |
| [`tester`](core/agents/tester.md) | サービス間の接合部を含めてテストを実行/作成。グリーンビルドのゲート |
| [`reviewer`](core/agents/reviewer.md) | 敵対的レビュー + RISK スコア + サービス間のチェック |
| [`arbiter`](core/agents/arbiter.md) | ゲートを強制し、継続/出荷/エスカレートを決定。ループを断ち切る役割 |

このパイプラインは [ruflo](https://github.com/ruvnet/ruflo) からパターン（フレームワークそのものではなく）を借用しています。SPARC の仕様ゲート、マルチリポジトリの調整、差分のリスクスコアリングなどです。オーケストレーションのルールはイメージに同梱され、各ワークスペースに `CLAUDE.md` としてレンダリングされます。詳細は [`core/README.md`](core/README.md) を参照してください。

## git ホストの接続（任意だが中核）

チームがリポジトリをクローンして PR を開けるよう、`.env` に以下を設定します（**Gitea/GitHub/GitLab** で動作します）。

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

**github.com** の場合は、`gh` CLI が PR を開けるよう `GH_TOKEN`（= あなたの `GIT_TOKEN`）も設定してください。

**ポーラー** はレビューコメントに反応するための推奨される方法です。git ホストからボットに到達できない場合（例えば国境を越えたネットワークフィルタリング）でも、ボット側が *外へ* 到達するため機能します。`ENABLE_PR_REVIEW=true` かつ `GITEA_API_URL` が設定されているときに有効になります。インバウンド Webhook と Caddy の TLS プロキシは任意の代替手段で、Ansible によるデプロイでのみ利用できます（インスタンスの `group_vars/all/vars.yml` に `webhook_domain` を設定します。下記の Ansible のセクションを参照してください）。

## GitHub のスター誘導（任意、GitHub 限定）

ボットが GitHub アカウント（`GIT_HOST=github.com` + `GIT_TOKEN`）に接続されていて、そのアカウントがまだプロジェクトにスターを付けていない場合、実行が成功するたびにスターを付けるよう促す短いメッセージをインラインボタン付きで送信します。ボタンを押すと、デプロイ自身のアカウントからリポジトリにスターが付き、その後この誘導は二度と表示されなくなります。これは GitHub 限定で、それ以外の環境では **自動的にオフ** です（このゲート自体がオフスイッチであり、別途フラグはありません）。完全にホットパスの外で動作するため、実行を遅らせたり壊したりすることは決してありません。

```ini
STAR_NUDGE_REPO=duckbugio/flock   # owner/repo to nudge for / star
# STAR_NUDGE_STORE_PATH=          # default <APPROVED_DIRECTORY>/star_nudge.json
```

ボタンには **star-write スコープ** を持つトークンが必要です。`public_repo` を付与したクラシック PAT、または「Starring」アカウント権限を持つ fine-grained トークンです。それがなければスター付けは静かに失敗するだけです（実行には影響しません）。

## ボイスメッセージ（任意）

```ini
ENABLE_VOICE_MESSAGES=true
VOICE_PROVIDER=mistral        # mistral | openai | local
MISTRAL_API_KEY=...           # console.mistral.ai  (or OPENAI_API_KEY for whisper)
```

## 任意のサイドカー（Telegram アダプター）

```bash
docker compose --profile dind up -d     # dockerized linters/tests for the team (set DOCKER_HOST=tcp://dind:2375)
```

（インバウンド Webhook 用の TLS プロキシは compose のプロファイルではなく Ansible のデプロイでプロビジョニングされます。有効にするにはインスタンスの `group_vars/all/vars.yml` に `webhook_domain` を設定してください。）

## チャットごとの隔離

各チャットは `/workspace/chat_<id>` を持ちます。1 対 1 ならあなた専用のワークスペース、グループならそのグループのメンバーで共有される 1 つのワークスペースになります。異なるチャットは完全に隔離され、**並列に** 実行されます（メモリのため `MAX_CONCURRENT_CHAT_RUNS` で上限が設けられます）。ユーザー ID のホワイトリストが、誰がボットを使えるかを制御します。グループでは `REQUIRE_GROUP_MENTION=true` を設定すると、ボットは @メンションされたときや返信されたときにのみ応答します。

## 応用: Ansible でサーバーにデプロイ（Telegram アダプター）

まっさらな VPS をワンコマンドでプロビジョニングするには（Docker + swap をインストールし、`.env` をレンダリングし、イメージを取得して起動します）。

```bash
cd adapters/telegram/deploy
cp -r inventories/example inventories/my-bot          # one dir per bot instance
$EDITOR inventories/my-bot/inventory.ini              # your server IP + SSH key
$EDITOR inventories/my-bot/group_vars/all/vars.yml    # non-secret config
$EDITOR inventories/my-bot/group_vars/all/vault.yml   # secrets (tokens)
ansible-vault encrypt inventories/my-bot/group_vars/all/vault.yml   # optional
ansible-playbook -i inventories/my-bot/inventory.ini playbook.yml
```

各ボットインスタンスはそれぞれ独自の `inventories/<name>/` を持ちます。コミットされるのは `inventories/example/` のみで、実際のインスタンスは gitignore されています。このロールはビルド済みイメージを取得します。タグを固定するには `bot_image` を設定してください。（フォーク後に独自のイメージをビルドするには、リポジトリのルートから `docker build -f adapters/telegram/Dockerfile .` を実行します。VK イメージは `docker build -f adapters/vk/Dockerfile .` です。）

## セキュリティ

- **ホワイトリスト:** 許可されたユーザーの一覧（Telegram は `ALLOWED_USERS`、VK は `VK_ALLOWED_USERS`）に含まれる者だけがボットを使えます。決して空のままにしないでください。ボットはサーバーへのシェル/編集アクセスを付与します。
- **チャットごとの隔離:** 異なるチャットには別々のワークスペースが割り当てられます。git トークンはデプロイ全体で共有されるため、それに応じてスコープを設定してください。
- **シークレット:** `.env`（gitignore 済み）に保管するか、Ansible の場合は実際のインスタンスの `inventories/<name>/group_vars/all/vault.yml`（gitignore 済み。`ansible-vault` で暗号化可能）に保管してください。実際のトークンは決してコミットしないでください。追跡されるのは `inventories/example/` のみです。
- **隔離:** エージェントはコンテナ内の非 root ユーザーとして動作し、その Bash/Edit はホストではなくコンテナ内に限定されます。

## ビルド、lint、テスト

このリポジトリは CI ランナーとして [Task](https://taskfile.dev) を使用しています。これは [CI](.github/workflows/ci.yml) が使うのと同じエントリポイントです。

```bash
task lint      # format + vet + linters (in the dev-tools image)
task tests     # Go test suite
task build     # compile the binaries
```

CI ではさらに `task security-scan`（govulncheck）と Trivy のファイルシステムスキャンを実行し、すべての PR で両方のアダプターイメージをドライビルドします。

## ライセンス

[MIT](LICENSE) © DuckBug.
