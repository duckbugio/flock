// Regenerate with the built LO SDK 0.2 HTTP adapter at the pinned source revision below.
// Usage: node adapters/lo/testdata/generate-secretary-sdk.mjs /path/to/dist/secretary.js
import { writeFile } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';
const { wireSecretaryRequest, normalizeSecretaryResult } = await import(pathToFileURL(process.argv[2]));
const source = 'https://github.com/lo-ink/lo-platform-adapters/blob/3cd0095a27467ed4f494b329158a365c44951daa/packages/bot-http-lo/src/secretary.ts';
const connectionId = '15eac687-9383-4da1-9644-826693923e44';
const context = {
  conversationId: '19', chatId: '77', policyVersion: '9223372036854775807',
  sourceMessageId: '9223372036854775806', sourceRevision: '1',
};
const action = { connectionId, context, requestId: 'flock:sdk-contract', text: 'SDK reply' };
const connection = {
  id: connectionId, user: { id: 1, is_bot: false }, is_enabled: true,
  lo_schema_version: 1, lo_policy_version: context.policyVersion, date: 1,
  lo_rights: ['receive_messages', 'send_messages', 'mark_read', 'edit_sent', 'delete_sent', 'delete_all'],
};
const sent = {
  message_id: '2', business_connection_id: connectionId, chat: { id: 77, type: 'private' },
  from: { id: 1, is_bot: false }, date: 1, text: action.text, lo_secretary_bot_id: '1000000000000001',
};
const draft = {
  lo_draft_id: '69417813-99f5-4dc5-a6a8-4c45151b1e5c', business_connection_id: connectionId,
  lo_conversation_id: context.conversationId, chat_id: context.chatId,
  lo_source_message_id: context.sourceMessageId, lo_revision: '1', state: 'draft', mode: 'review',
  text: action.text, reason: 'manual_review', date: 1, expires_at: 2,
  lo_secretary_bot_id: '1000000000000001',
};
const cases = [
  ['getConnection', { connectionId }, connection],
  ['sendText', action, sent],
  ['proposeDraft', { ...action, reason: 'manual_review' }, draft],
];
const fixtures = cases.map(([operation, input, result]) => {
  // The real adapter both produces the request and validates its corresponding native receipt.
  normalizeSecretaryResult(operation, input, result);
  return { operation, ...wireSecretaryRequest(operation, input), result };
});
await writeFile(new URL('./secretary-sdk-0.2.json', import.meta.url), JSON.stringify({ source, fixtures }, null, 2) + '\n');
