# LO adapter

`flock-lo` connects a dedicated LO bot to the shared Flock agent service. It uses
LO's Telegram-shaped **supported subset**, not the Telegram adapter with a new
base URL. Telegram and VK behavior remain unchanged unless their transports
explicitly opt into the new draft capability.

## First publication (maintainers)

Before announcing the image or enabling Roost deployments, wait for the first
successful Publish LO workflow on main. In the organization package settings,
link `flock-lo` to `duckbugio/flock` and set its visibility to Public. New GHCR
packages are private by default; a successful workflow alone does not establish
anonymous access. Verify `docker pull ghcr.io/duckbugio/flock-lo:latest` from an
environment without registry credentials before marking publication complete.

## Start

```sh
cd adapters/lo
cp .env.example .env
# Set LO_API_URL, LO_BOT_TOKEN, LO_ALLOWED_USERS and AI provider authentication.
docker compose pull
docker compose up -d
```

`LO_API_URL` is the deployed Bot API base, for example an operator-supplied HTTPS
origin with an optional gateway path. Requests are posted to
`<base>/bot<LO_BOT_TOKEN>/<method>`. Do not use the Mini App Connect endpoint.
The production API URL must be supplied by the operator. Compose uses the published
`ghcr.io/duckbugio/flock-lo:latest` image. The dedicated
`adapters/lo/Dockerfile` builds and runs `/usr/local/bin/flock-lo`, with the same
AI tooling as the other adapters. Compose caps memory with `BOT_MEM_LIMIT` (3g by default).

For a host with Go and the selected AI CLI installed:

```sh
go build -o bin/flock-lo ./cmd/flock-lo
# Export the variables from your deployment configuration, then:
./bin/flock-lo
```

The host run also needs `TEAM_TEMPLATE_PATH`, `TEAM_AGENTS_DIR`, and
`TEAM_SKILLS_DIR` pointing at this checkout's `core` files (the container sets up
those paths). See the shared env template for exact option names and defaults.

Only positive IDs listed in `LO_ALLOWED_USERS` can invoke commands or agents.
This allow-list never falls back to Telegram/VK IDs. Workspaces and default state
stores live below `<APPROVED_DIRECTORY>/lo`, isolating equal numeric chat IDs.
Explicit store-path overrides and authentication volumes should also be unique
to the LO deployment.

Graceful shutdown honors `SHUTDOWN_DRAIN_SECONDS` (45 seconds by default, capped
at 60 with a startup warning). Compose allows 75 seconds, leaving room for the
shared dispatcher's ten-second post-cancel delivery window.

Run one polling replica per token. Startup checks `getMe` and `getWebhookInfo`;
an existing webhook is an error, never automatically deleted. Explicit API
404/501 from `getWebhookInfo` logs a warning and continues to polling; `getUpdates`
retries transient 409 conflicts with ten-second waits and stops on the fifth
consecutive conflict (40 seconds of waiting, covering the 30-second long poll).
Cancellation interrupts the wait immediately. Authentication failures and other
startup-check errors remain fatal. `getUpdates`
retains queued messages, advances the offset after dispatch, and does not request
callback/edited-message updates it cannot handle. As with ordinary long polling,
a crash before the next offset acknowledgement can redeliver the last batch;
this adapter does not promise exactly-once agent execution. Interrupted and queued runs
are persisted and resumed on startup before new messages or background events.
Startup refuses an unreadable pending store instead of silently losing recovery.

## Behavior

- Private chats and groups; group prompts require an exact `@botname` token or
  `/command@botname` when `REQUIRE_GROUP_MENTION=true`. This requires a bot
  username from LO. Startup warns if it is missing; use private chats or disable
  the mention requirement until the bot has a username.
- Reserved commands bypass the group mention requirement; `/command@other_bot`
  is still rejected and the user allow-list always applies.
- `/start`, `/help`, `/new`, `/stop`, `/goal`, `/schedule`; other slash commands
  reach the selected AI provider unchanged. `/stop` works while another run is
  active because admission uses Flock's nonblocking dispatcher.
- Rate/cost guards, per-chat workspaces and sessions, optional post-run checks,
  goal evaluation, follow-ups and scheduler use the shared core. Store failures
  at startup do not silently disable the cost cap.
- Plain-text progress is edited in place by default, followed by the final
  answer. Native replies degrade to ordinary notices. No unsupported
  `reply_markup`, `reply_parameters`, `parse_mode` or `rich_message` is sent.
- `LO_ENABLE_DRAFTS=true` opts into private-chat `sendMessageDraft`: stable
  nonzero draft ID per run, full accumulated progress text, final `sendMessage`.
  The core keeps draft IDs separate from message IDs and explicitly clears the
  draft with empty text before final delivery, including cancellation. Cleanup
  has a separate two-second budget; its failure never prevents the final send. Initial refusal (including
  groups or older LO versions) falls back to the editable anchor. The core's
  three-second cadence respects the existing backoff behavior; no token-by-token
  database edits are needed. This streams **Flock's progress frame**, not every
  raw model token (the runner's final answer is delivered normally).
- `LO_REGISTER_COMMANDS=true` opts into `setMyCommands`. Failure is logged and
  does not disable text commands. The current server implements this method; registration remains
  opt-in until live acceptance with the deployment's bot token is complete.
- **Inbound photos are downloaded.** The largest size of an incoming photo is fetched
  through `getFile` + `<base>/file/bot<TOKEN>/<file_path>` and written to the chat's
  uploads directory (a sibling of the cloned repositories, so a user's image can never
  enter a commit); the prompt then carries that absolute path AND the picture travels to the
  model as a vision block, so it is seen rather than named. The saved name is corrected to what
  the first bytes say the file is, because the vision block's media type is read back out of
  that name — and bytes that are not an image at all (a storage error page served with 200, an
  empty body) are thrown away with the download-failure sentence rather than declared a JPEG. A
  picture in a format the vision block cannot carry (bmp, ico) is refused BY NAME, because
  core/chat answers `image/jpeg` for every extension it does not know and there is no true type
  to rename it to. Bytes the sniffer does not RECOGNISE are kept — unidentified is not
  disproven — but travel as a PATH ONLY, without a vision block: the model is never told a
  media type nothing confirmed. One rule covers all of it: the picture is shown when the bytes
  were identified and the name on disk agrees with them. Other retained files, including a
  supported image that could not be renamed, reach the run by path only. Refused files do not
  start a run. A photo with no caption starts a run on its own. `MAX_UPLOAD_BYTES` caps the download and
  the cap also holds on the stream, because LO omits `file_size` for files it has not
  measured. Client-supplied names are sanitised; a saved file cannot leave the uploads
  directory.
- **Inbound documents are downloaded the same way**, keeping the name the user gave them: the
  saved path ENDS IN that name behind a collision-safe prefix, so the agent sees
  `…-spec.pdf` rather than an opaque id (the prefix is `fsutil`'s, and it is what lets two
  people send `report.pdf` into one chat). A LO that predates document downloads answers the
  reference WITHOUT a `file_path`; that is reported as "this LO does not hand bots the bytes"
  rather than as a failed download, because the two need different actions. LO fills
  `file_name` only usually, and a document that arrives without one is named from its declared
  MIME type (`document_<id>.pdf`, and `.bin` for anything outside the short table of types a
  chat carries — `documentExtensions` in `media.go`) — an extensionless path is the same opaque
  id this whole paragraph exists to avoid.
- **Voice input** uses `ENABLE_VOICE_MESSAGES` and the shared `VOICE_PROVIDER` configuration.
  Allowed senders pass mention and rate/cost guards before downloading or transcribing.
  `getFile` must expose bytes, and `MAX_UPLOAD_BYTES` caps both declared and streamed size
  before the paid transcription call. Empty transcripts, missing bytes and provider failures
  produce a notice and never start an agent. LO must include the inbound voice relay implementation.
- **Other unsupported attachments** get a per-kind explanation and stop the run.
  The adapter does not answer a caption while pretending to have read unavailable media.
- **Outbound files are opt-in via `LO_ENABLE_DOCUMENTS`.** With it off (the default) the
  outbox sweep stays disabled and agent-created files remain in the workspace — the honest
  behaviour on a LO that predates `sendDocument`, which answers `501`. With it on, artifacts
  are uploaded with their base name only; a full path would describe the host's filesystem to
  everyone in the chat. Turn it on after upgrading the platform, not before: a probe at
  startup cannot tell "not implemented" from a transient failure.
- `sendPhoto`'s upload branch answered `500` on the deployments exercised so far, so an image
  is never sent AS A PHOTO. It still reaches the chat when documents are on: the outbox sweep
  posts every regular file through `sendDocument`, a screenshot included, which is what the
  workspace's screenshot convention already promises the agent. Native callback buttons, rich
  messages and the interactive star nudge are unavailable.
- Interrupted-run recovery replays the durable per-chat FIFO before polling starts;
  markers remain until a clean terminal result. A missing old progress message does not block replay.
- `ENABLE_CI_WATCH` enables the shared GitHub/Gitea watcher; `ENABLE_AUTO_MERGE` retains its
  explicit opt-in behavior. Watch state lives in the LO namespace by default.
- Gitea PR-comment polling uses `ENABLE_PR_REVIEW`, `GITEA_API_URL` and `GIT_TOKEN`.
  Only an exact repository, branch and chat match in the LO workspace can trigger a run.
  Unowned notifications stay unread. CI and review-triggered runs use the shared autonomy budget.
  Use separate git accounts for adapters if both consume notifications: older adapters may
  still acknowledge all routable team notifications from a shared account.

The shared chunker currently measures runes rather than UTF-16. LO advertises a
conservative 2048-rune limit, guaranteeing every chunk fits 4096 UTF-16 units even
when all characters are astral emoji. This intentionally uses smaller chunks for
BMP-only text; the transport also validates the actual UTF-16 length.

## Verification and migration findings

See [the compatibility audit](../../docs/lo-telegram-compatibility.md). Local
contract tests use HTTP fixtures derived from the LO handlers; they do not claim
that a public LO deployment, mobile build or real bot token has been exercised.
