# LO integration and Telegram compatibility

This audit accompanies the first Flock LO adapter. It is based on source, not a
successful production bot session. No LO token or verified public Bot API endpoint
was available during implementation.

## Reproducible baseline

- Flock: `3858d8c37f54bbf5c7a6f9880839009418bfef82`.
- LO messenger `origin/main`, verified on 2026-09-09:
  `5174d1405ec409dbc67edd99ac65c1aad3f7b2b0`.
- [LO method registry](https://git.lo.ink/LO/messenger/src/commit/5174d1405ec409dbc67edd99ac65c1aad3f7b2b0/bots/bot-api-service/internal/usecase/botmethod/getme.go).
- [LO sendMessage parameter allow-list](https://git.lo.ink/LO/messenger/src/commit/5174d1405ec409dbc67edd99ac65c1aad3f7b2b0/bots/bot-api-service/internal/usecase/botmethod/sendmessage.go).

These references describe server code. They do not replace a live Flock acceptance
run. Operators must supply `LO_API_URL` and verify their deployment before enabling
optional flags. In this revision draft and command support are implemented server-side;
Flock live delivery remains to be verified with a dedicated bot token.

## What this integration exposes

| Flock behavior | LO main contract | Adapter behavior |
| --- | --- | --- |
| Webhook preflight | `getWebhookInfo` is registered on main | Refuses active webhooks; explicit 404/501 on older deployments falls back to polling conflict detection |
| Incoming text | `getUpdates`, positive offset acknowledgement | Long polling; separate LO user allow-list; group mention gate |
| Persistent answer / progress edits | `sendMessage`, `editMessageText`, `deleteMessage` | Implemented with numeric IDs and plain text |
| Ephemeral progress | `sendMessageDraft` supports private chats and empty-text cleanup | Opt-in `LO_ENABLE_DRAFTS`; stable draft ID per run; explicit empty-text cleanup before a real final `sendMessage`; initial failure falls back to an editable anchor |
| HTML / entities | `sendMessage` accepts parse modes and entities | Formatting source is sent as plain text; unsupported fields are never transmitted |
| Stop button | Callback keyboard fields rejected; `answerCallbackQuery` and `editMessageReplyMarkup` are stubs | `/stop` remains available to allowed users even when request/cost guards reject new work |
| Command menu | `setMyCommands` / `getMyCommands` / `deleteMyCommands` are implemented | Text commands work; registration is separately opt-in |
| Native quoted reply | `reply_parameters` and legacy reply fields rejected | Completion notice is a separate message; inbound quoted text is included in the prompt when supplied |
| Files / generated artifacts | `sendDocument` is a stub; photo/audio/video support does not establish general document parity | Document outbox disabled; received attachments get an explicit unsupported notice |
| Provider slash commands | No platform-specific requirement | Non-reserved commands such as `/loop` reach the configured agent backend |
| Goals and scheduled work | Uses normal bot messaging | Shared Flock goal and scheduler services wired |
| Rate limits | 429 with `retry_after` | Progress and final-delivery backoff use the LO classifier; polling backs off too |
| ID and text sizes | Integer IDs, 4096 UTF-16-unit text budget | Int64-safe JSON; 2048-rune chunks guarantee the LO limit even for emoji |

The optional draft path streams Flock's activity/progress frames. It does not add
model token deltas to agent providers that only report a completed answer. Current
LO draft support is private-chat-only; groups use the persistent anchor fallback.

## Remaining LO work, in migration order

1. **Ship and exercise the Bot API as a deployed product:** establish the public
   endpoint and token onboarding, then test real inbound delivery, edits, 429s,
   restart behavior and final answer delivery against the actual chat clients.
2. **Callbacks and keyboards:** implement `reply_markup`, callback update delivery,
   `answerCallbackQuery`, and markup edits end to end. This restores inline Stop,
   confirmation and navigation flows used by existing Telegram bots.
3. **Documents and inbound media:** complete upload, reusable file IDs, download,
   metadata and limits. Flock needs repository artifacts and screenshots, not only
   photo sends. The adapter will need corresponding download/outbox wiring.
4. **Formatting and reply parity:** wire the adapter to implemented parse modes/entities
   and complete native reply fields with visible client rendering. Test code blocks, escaping, emoji offsets,
   quote targets and edit behavior, not merely accepted JSON.
5. **Drafts and command discovery:** deploy and verify temporary-event delivery,
   expiration, final-message replacement and command menus. The opt-in flags in
   this adapter provide a concrete consumer for those checks.

This is not a complete Telegram Bot API inventory. It covers methods exercised by
Flock and the compatibility gaps they expose; Mini App bridge compatibility needs
its own integration test and is outside this chatbot adapter.

## Flock-specific follow-ups

These are adapter/runtime limitations, not evidence of missing LO endpoints:

- The LO entry point does not yet wire Flock's PR-comment polling, CI watcher,
  pending-run restart recovery or star nudges. Do not advertise full operational
  parity with the Telegram entry point.
- Polling advances its in-memory offset after admission, not after an agent run
  completes. A restart can redeliver the last unacknowledged batch; this is not an
  exactly-once job queue. Run one polling instance per bot token.
- The shared chunker counts Unicode runes. LO uses a conservative half-size budget;
  a future UTF-16-aware chunker could use the full limit for BMP-only text and
  improve the existing Telegram adapter as well.
- Draft support is an optional core transport interface; Telegram and VK retain
  their existing progress behavior. Telegram native drafts are not enabled by this
  change.

## Validation and live acceptance

The adapter has HTTP-fixture tests for method payloads, numeric IDs, retry delays,
credential redaction, redirect refusal, malformed envelopes, draft identity,
UTF-16 rejection, inbound gates, ordered polling acknowledgement, webhook conflicts
and quoted context. Core tests cover native draft completion and cleanup (including cancellation and
cleanup failure), progress size limits,
fallback to the persistent anchor and workspace instructions without file-delivery
promises.

Validation passed on 2026-09-06 using the repository dev-tools image (Go 1.26.6):

- `task default`: tidy, formatting, lint (zero issues), all race tests and builds.
- `task security-scan`: no vulnerabilities found.
- Docker build stage with `FLOCK_COMMAND=flock-lo` on linux/arm64.
- LO Compose configuration validation without loading a credential file.

The full AI runtime image and multi-architecture CI job were not executed locally;
the LO adapter now has its own Dockerfile and executable, with equivalent AI tooling.

Before enabling it for real users, run the [setup](../adapters/lo/README.md) with a
LO test bot and verify: allowed/denied users, a long-running request and `/stop`,
emoji-heavy multi-part answers, restart during polling, and both draft flag values.
Confirm that no transient draft is stored as a final answer and that the final
answer remains visible after reconnecting the chat client.

## Review follow-up (2026-09-09)

The LO runtime now has its own Dockerfile and binary name, with a configurable 3g
memory cap. Reserved group commands do not require a mention, but the allow-list
and rejection of commands addressed to another bot still apply. Polling retries
up to four consecutive 409 conflicts before the fifth stops the process; a successful
poll resets this counter. The stale-batch regression test waits for the second request
before observing backoff, avoiding a race with a slow runner startup.

Server draft IDs remain int64 in Go and are serialized as decimal strings in client
streaming events (`handler_internal_bot_draft.go`), so a 63-bit Flock draft ID is not
converted to a JavaScript number by that delivery path. Real streaming rendering and
cleanup still require the live acceptance run.
