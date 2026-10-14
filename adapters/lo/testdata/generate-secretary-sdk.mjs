// Regenerate from the published SDK 0.6 public entry point; no network requests.
// Usage: node adapters/lo/testdata/generate-secretary-sdk.mjs /path/to/consumer
import { readFile, writeFile } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';
import { createRequire } from 'node:module';
import { resolve } from 'node:path';
const consumer = createRequire(pathToFileURL(resolve(process.argv[2], 'package.json')));
const entry = pathToFileURL(consumer.resolve('@lo-ink/bot-sdk'));
const { version } = JSON.parse(await readFile(new URL('../package.json', entry), 'utf8'));
if (version !== '0.6.0') throw Error('Expected published Bot SDK 0.6.0');
const { createLoSecretaryClient } = await import(entry);
const source = 'https://github.com/LO-ink/lo-bot-sdk/tree/b9525c01079d165ee8ac171c2fe936a4a072cf08';
const connectionId = '15eac687-9383-4da1-9644-826693923e44';
const context = {
 conversationId: '19', chatId: '77', policyVersion: '9223372036854775807',
 sourceMessageId: '9223372036854775806', sourceRevision: '1',
};
const action = { connectionId, context, requestId: 'flock:sdk-contract', text: 'SDK reply' };
const connection = {
 id: connectionId, user: { id: 1, is_bot: false, first_name: 'Owner' }, is_enabled: true,
 lo_schema_version: 1, lo_policy_version: context.policyVersion, date: 1,
 lo_rights: ['receive_messages', 'send_messages', 'mark_read', 'edit_sent', 'delete_sent', 'delete_all'],
};
const sent = {
 message_id: '2', business_connection_id: connectionId, chat: { id: 77, type: 'private' },
 from: { id: 1, is_bot: false, first_name: 'Owner' }, date: 1, text: action.text, lo_secretary_bot_id: '1000000000000001',
};
const fixtures = [];
for (const [operation, input, result] of [
 ['getConnection', { connectionId }, connection],
 ['sendText', action, sent],
]) {
 const calls = [];
 const client = createLoSecretaryClient({
  token: '1000000000000001:synthetic_sdk_contract',
  fetch: async (url, options) => {
   calls.push({ method: new URL(url).pathname.split('/').at(-1), body: JSON.parse(options.body) });
   return Response.json({ ok: true, result });
  },
 });
 if (operation === 'getConnection') {
  const normalized = await client.getConnection(input.connectionId);
  if (normalized.policyVersion !== context.policyVersion) throw Error('Lossless connection revision changed');
 } else await client.sendText(input);
 if (calls.length !== 1) throw Error('SDK must issue exactly one synthetic request');
 if (operation === 'sendText' && (calls[0].body.lo_context.policy_version !== context.policyVersion ||
  calls[0].body.lo_context.source_message_id !== context.sourceMessageId))
  throw Error('Lossless context IDs changed in the public SDK request');
 fixtures.push({ operation, ...calls[0], result });
}
await writeFile(new URL('./secretary-sdk-0.6.json', import.meta.url), JSON.stringify({ source, sdkVersion: version, fixtures }, null, 2) + '\n');
