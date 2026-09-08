# LO adapter

`flock-lo` connects a dedicated LO bot to the shared Flock agent service. It uses
LO's Telegram-shaped **supported subset**, not the Telegram adapter with a new
base URL. Telegram and VK behavior remain unchanged unless their transports
explicitly opt into the new draft capability.

## Start

```sh
cd adapters/lo
cp .env.example .env
# Set LO_API_URL, LO_BOT_TOKEN, LO_ALLOWED_USERS and AI provider authentication.
docker compose up --build -d
```

`LO_API_URL` is the deployed Bot API base, for example an operator-supplied HTTPS
origin with an optional gateway path. Requests are posted to
`<base>/bot<LO_BOT_TOKEN>/<method>`. Do not use the Mini App Connect endpoint.
There is deliberately no guessed production URL or published LO image. The dedicated
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
retries transient 409 conflicts and stops after five consecutive conflicts. Authentication failures and other
startup-check errors remain fatal. `getUpdates`
retains queued messages, advances the offset after dispatch, and does not request
callback/edited-message updates it cannot handle. As with ordinary long polling,
a crash before the next offset acknowledgement can redeliver the last batch;
this adapter does not promise exactly-once agent execution or automatic resume.

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
  does not disable text commands. It remains off by default for LO main versions
  where the method is still a 501 stub.
- Files, voice, native callback buttons, rich messages, and the interactive star
  nudge are unavailable. Incoming attachments get a clear notice; they are not
  silently stripped from an AI prompt. Outbox delivery is disabled. Agent-created
  files remain in the workspace/repository and must be retrieved there. LO
  workspace instructions explicitly disable automatic file-delivery promises.
- The LO command does not yet wire Telegram's interrupted-run recovery, CI watch
  or PR-comment polling. These are **Flock adapter gaps**, not missing LO API
  methods. The scheduler and goal evaluator are supported.

The shared chunker currently measures runes rather than UTF-16. LO advertises a
conservative 2048-rune limit, guaranteeing every chunk fits 4096 UTF-16 units even
when all characters are astral emoji. This intentionally uses smaller chunks for
BMP-only text; the transport also validates the actual UTF-16 length.

## Verification and migration findings

See [the compatibility audit](../../docs/lo-telegram-compatibility.md). Local
contract tests use HTTP fixtures derived from the LO handlers; they do not claim
that a public LO deployment, mobile build or real bot token has been exercised.
