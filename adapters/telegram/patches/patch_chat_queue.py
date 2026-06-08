"""Build-time patcher: per-chat (instead of global) update serialization.

Replaces StopAwareUpdateProcessor.do_process_update — which funnels every non-priority
update through ONE global lock (so different chats block each other) — with per-chat
dispatch via chat_queue: same chat runs one-at-a-time, different chats run in parallel
(capped by MAX_CONCURRENT_CHAT_RUNS). Also wires edit-supersede and Stop-clears-queue.

Copies chat_queue.py into the package. Idempotent; fails loudly if do_process_update
can't be matched (e.g. after a version bump). Pass --check to dry-run.

Usage: python3 patch_chat_queue.py <path-to-chat_queue.py>
"""
import importlib.util
import re
import shutil
import sys
from pathlib import Path

PATTERN = re.compile(
    r"    async def do_process_update\(\n"
    r".*?\n"
    r"(?=    async def initialize\(self\) -> None:)",
    re.DOTALL,
)

NEW_METHOD = (
    "    async def do_process_update(\n"
    "        self,\n"
    "        update: object,\n"
    "        coroutine: Awaitable[Any],\n"
    "    ) -> None:\n"
    '        """Per-chat serial processing — different chats run in parallel (capped).\n'
    "\n"
    "        Priority (stop:*) bypasses the queue and also clears the chat's pending items;\n"
    "        an edited message supersedes its still-queued original so the corrected text runs.\n"
    '        """\n'
    "        from .. import chat_queue as _chat_queue\n"
    "\n"
    "        chat_id = None\n"
    "        if isinstance(update, Update) and update.effective_chat is not None:\n"
    "            chat_id = update.effective_chat.id\n"
    "\n"
    "        if self._is_priority_callback(update):\n"
    "            if chat_id is not None:\n"
    "                _chat_queue.dispatcher.clear_chat(chat_id)\n"
    "            await coroutine\n"
    "            return\n"
    "\n"
    "        if chat_id is None:\n"
    "            await coroutine\n"
    "            return\n"
    "\n"
    "        message_id = None\n"
    "        msg = update.effective_message if isinstance(update, Update) else None\n"
    "        if msg is not None:\n"
    "            message_id = msg.message_id\n"
    "        if isinstance(update, Update) and update.edited_message is not None:\n"
    "            # Handlers read update.message (None on edits) -> present the edit as a\n"
    "            # normal message so they don't crash; also supersede the queued original.\n"
    "            if update.message is None:\n"
    "                try:\n"
    '                    object.__setattr__(update, "message", update.edited_message)\n'
    "                except Exception:\n"
    "                    pass\n"
    "            if message_id is not None:\n"
    "                _chat_queue.dispatcher.supersede(chat_id, message_id)\n"
    "\n"
    "        await _chat_queue.dispatcher.submit(chat_id, message_id, coroutine)\n"
    "\n"
)


def main() -> int:
    check = "--check" in sys.argv
    args = [a for a in sys.argv[1:] if not a.startswith("--")]

    spec = importlib.util.find_spec("src")
    if spec is None or not spec.origin:
        print("[patch] ERROR: bot package 'src' not found", file=sys.stderr)
        return 1
    pkg = Path(spec.origin).resolve().parent

    if args and not check:
        shutil.copyfile(Path(args[0]).resolve(), pkg / "chat_queue.py")

    proc = pkg / "bot" / "update_processor.py"
    text = proc.read_text(encoding="utf-8")
    already = "chat_queue" in text
    matched = PATTERN.search(text) is not None

    if check:
        print("CHECK: match={} already_patched={}".format(matched, already))
        return 0 if (matched or already) else 2

    if already:
        print("[patch] update_processor already per-chat; skipping")
        return 0
    if not matched:
        print("[patch] ERROR: do_process_update not found in update_processor.py", file=sys.stderr)
        return 1

    proc.write_text(PATTERN.sub(NEW_METHOD, text, count=1), encoding="utf-8")
    print("[patch] per-chat dispatch applied to", proc)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
