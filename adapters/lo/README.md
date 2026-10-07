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
retains queued messages, advances the offset after dispatch, and requests message
and callback-query updates. Edited-message updates are not requested. As with ordinary long polling,
a crash before the next offset acknowledgement can redeliver the last batch;
this adapter does not promise exactly-once agent execution. Interrupted and queued runs
are persisted and resumed on startup before new messages or background events.
Startup refuses an unreadable pending store instead of silently losing recovery.

With voice input enabled, raw message references are persisted separately in
`voice-inputs.json` before asynchronous transcription. Up to 32 voice messages
can wait across chats; excess requests receive a busy notice. Transcription does
not block polling, `/stop`, or `/new`. Both commands cancel that chat's voice
queue, including late provider results. Restart replays interrupted preparation
after checking the sender's current access. A crash during the handoff to the
agent's pending store can replay work; this is at-least-once recovery.

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
  workspace's screenshot convention already promises the agent. Rich messages remain unavailable.
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

## Inline buttons

Set `LO_ENABLE_KEYBOARDS=true` in `.env` only after deploying public inline
keyboards and atomic `editMessageText` with `reply_markup` (LO/messenger #354 and
#355). The default is false for older deployments. Update the native LO app to a
build containing keyboard rendering and callback delivery.

Persistent progress messages show Stop while a run is active. The final edit
changes text and clears the keyboard in one request. `/stop` remains available
with keyboards or ephemeral drafts disabled or unavailable. Stop tokens are signed
for the chat and the current bot process; buttons from an earlier process cannot
stop a new run whose counter happens to match. After a restart, use `/stop` for
resumed work.

Callbacks require an allowed LO user and a message authored by this bot in the
matching private chat or a supported group. Stop bypasses new-work cost and rate
guards. Enable the existing GitHub star-nudge settings separately to receive its
confirmation button; account actions are disabled when keyboards are disabled.

Local HTTP and race tests cover the request lifecycle and callback admission.
Production acceptance still requires real bot credentials, a deployed compatible
server, and a compatible native client.

## Native LO secretary

The Go adapter implements the native SDK 0.2 wire contract directly; it does not
import the TypeScript SDK or reuse Telegram consent flags. Use a deployment with
`lo_schema_version=1`, policy versions, independent `lo_rights` and delegated
source contexts. Ordinary bot messages remain a separate path.

Set `SECRETARY_MODE=approval` to receive the exact proposed reply in the connection
owner's private chat with the bot. The owner selects **Send to chat <peer ID>** or
**Discard** there. The decision is persisted before delegated delivery; native LO
settings do not manage generated replies. Secretary buttons remain available when
ordinary progress keyboards are disabled. Set `SECRETARY_MODE=auto` only when
the owner wants automatic replies. `off` is the default. Both modes currently
require the Claude backend, matching Flock's Telegram full-agent secretary.

The connection owner must be in `LO_ALLOWED_USERS` and independently grant both
`receive_messages` and `send_messages`. The peer is not checked against the bot
command allow-list. Owner messages and delegated bot echoes never trigger replies.
Consent and source policy are checked again before transcription, agent execution
and delivery. Edits, deletion and connection changes cancel pending work. Native
server guards remain authoritative for races with manual takeover or revocation.

Secretary runs use normal Flock tools and configured MCP servers. Approval gates
the outgoing reply, not tool actions. Workspaces and sessions are isolated by
owner, connection, conversation and policy version under
`<SECRETARY_WORKSPACE_DIR>/lo`, outside `APPROVED_DIRECTORY`. Compose persists
this root in `secretary_workspace`. Host runs must set
`LO_SECRETARY_TEMPLATE_PATH` to `core/CLAUDE.secretary-lo.md.tmpl`, plus the usual
team paths. Changing the configured mode cancels retained work from the old mode.
A delivery already attempted keeps an unknown-outcome tombstone instead of falsely
claiming cancellation. Legacy version-1 native review proposals are never implicitly
approved during upgrade. Upgrading writes state version 2, which version-1
runtimes cannot read. Keep a protected pre-upgrade state backup; after any v2
activity, use a v2-compatible runtime or roll forward. Do not delete the state or
restore an older checkpoint that loses approvals, attempts or update watermarks.
Do not downgrade to an intermediate v2 runtime that ignores the context and
preview attempt markers: it cannot preserve publication limits. Use the released
runtime with these bounds or a newer compatible build.

The private, atomically written `secretary-state.json` records admission before
poll acknowledgement and stores each completed response before the delegated
write. Uncertain writes retry the exact body and `lo_request_id`, including after
restart, without rerunning tools. Retry attempts and deadlines are persisted:
backoff starts at 15 seconds and doubles to five minutes; a larger server
`retry_after` is respected up to the 48-hour lifetime. A crashed agent run is
cancelled instead of replaying potentially completed tool actions.

State is bounded to 10,000 jobs/invalidation keys, 1,024 connection snapshots
and 32 MiB, including reserved space for eventual replies and metadata. Capacity
pressure evicts the oldest terminal jobs, invalidations or inactive connections;
if live jobs or reserved bytes fill the queue, only the incoming message is
logged and skipped, with polling acknowledgement preserved. Live prepared
responses and their request IDs are never evicted for new work. Owners outside
`LO_ALLOWED_USERS` do not occupy connection snapshots. Terminal payloads are
compacted, and the persisted native update watermark rejects acknowledged update
redelivery after compaction/restart. Deduplication and invalidation records are
retained for up to 48 hours, subject to capacity; the pump also prunes them when
no updates arrive. Run one process per bot token and state volume.

A persistence error, invalid state, unsupported state version or different bot
identity intentionally stops the entire LO process before polling acknowledgement.
Separate command routing does not isolate the process from unsafe storage. The
state must not be silently reset or discarded: prepared writes can be uncertain
and interrupted tool actions cannot safely be replayed. Stop the process, preserve
the state/volume and investigate the recorded request IDs before recovery. Restore
a verified backup or migrate the state with its pending actions reconciled. A
replacement bot needs a separate state volume; rotating a token for the same bot
keeps its identity and state. Do not delete state to force a resend.

Text and configured voice recordings are supported. Unsupported/unavailable
attachments, generation failures, cost rejection and oversized replies are
cancelled without sending a fabricated answer. Rate-limited jobs stay queued
for a later window. Rate/cost denials log `update_id`, `owner_id` and `reason`;
inspect structured bot logs.
Replies must fit one native 4096 UTF-16-unit message. Only terminal agent output
can be delivered; progress, tool traces, outbox files and scheduled follow-ups are
not sent to delegated chats. These limits match the current secretary scope and
are independent of ordinary LO document or streaming flags.

Before live testing: publish the new Flock image, complete the LO secretary
server/client rollout, enable the pilot owner and bot capability, grant rights in
LO, then test review, automatic reply, voice, edit/delete and revoke while running.
Local HTTP contract and race tests do not establish production readiness.

Contract regression fixtures in `testdata/secretary-sdk-0.2.json` are generated
by the actual [SDK 0.2 HTTP adapter](https://github.com/lo-ink/lo-platform-adapters/blob/3cd0095a27467ed4f494b329158a365c44951daa/packages/bot-http-lo/src/secretary.ts).
The generator validates native receipts with that adapter before writing the
fixtures. Go tests compare delegated request bodies against those
fixtures, including IDs above JavaScript's safe integer range. Regenerate after
building the pinned SDK revision:

```sh
node adapters/lo/testdata/generate-secretary-sdk.mjs /path/to/packages/bot-http-lo/dist/secretary.js
```

The merged Bot API also covers lossless string input in
[`TestBusinessCanonicalStringIDsPreservePrecisionAndUseAuthenticatedGeneration`](https://git.lo.ink/LO/messenger/src/commit/9a667c7b8ff1a2f0655c8bed94b5511ee3c5e90a/bots/bot-api-service/internal/usecase/botmethod/business_test.go).

Owner review expires with the source reply window (24 hours). Ordinary preview
messages have no server idempotency contract. Source context is attempted once,
with a durable attempt marker before sending and a confirmed message ID saved
before publishing buttons. A failed or unconfirmed context cancels the reply;
a restart never repeats that context or publishes a blind preview. The exact
reply preview has at most three durable attempts across restarts. A lost response
can therefore create a bounded duplicate preview, but only the persisted notice
message ID can authorize its delegated reply. Exhausted or definitive preview
failures cancel without sending or rerunning the agent. After a possibly committed send, source invalidation or consent revocation
stops retries and reports an unknown delivery outcome. Check the target chat; no
receipt recovery after revocation is promised. Tombstones retain the request ID
and source context while removing reply text.
