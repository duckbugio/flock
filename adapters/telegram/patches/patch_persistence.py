"""Build-time patcher: persist PTB chat_data so the bot survives restarts.

The bot keeps its per-chat state — current_directory and (critically) the
claude_session_id used to RESUME the conversation — in context.user_data, which our
ChatScopedContext maps to chat_data. But the upstream bot configures NO telegram.ext
persistence, so chat_data is in-memory and is WIPED on every container restart
(i.e. every deploy). After a deploy the bot then has no claude_session_id to resume
(and the in-memory active_sessions index is gone too), so it starts a FRESH Claude
session and "forgets" the conversation — even though the session history itself is
safe on the claude_home volume.

This wires a PicklePersistence onto the persistent /app/data volume, so chat_data
(including claude_session_id) survives restarts and the bot resumes the prior Claude
session after a deploy.

Idempotent; fails loudly if the anchor is missing (e.g. after a bot version bump).
Must run after patch_chat_scope.py (which leaves the builder.token line intact).
Pass --check to dry-run (report anchor match without writing).

Usage: python3 patch_persistence.py [--check]
"""
import importlib.util
import sys
from pathlib import Path

ANCHOR = "builder.token(self.settings.telegram_token_str)"
PERSIST_PATH = "/app/data/ptb_persistence.pkl"


def main() -> int:
    check = "--check" in sys.argv
    spec = importlib.util.find_spec("src")
    if spec is None or not spec.origin:
        print("[patch] ERROR: bot package 'src' not found", file=sys.stderr)
        return 1
    core = Path(spec.origin).resolve().parent / "bot" / "core.py"
    text = core.read_text(encoding="utf-8")

    already = "PicklePersistence" in text
    matched = any(line.strip() == ANCHOR for line in text.splitlines())

    if check:
        print("CHECK: match={} already_patched={}".format(matched, already))
        return 0 if (matched or already) else 2

    if already:
        print("[patch] core.py already has persistence; skipping")
        return 0

    injected = 0
    out = []
    for line in text.splitlines(keepends=True):
        out.append(line)
        if line.strip() == ANCHOR:
            indent = line[: len(line) - len(line.lstrip())]
            out.append(indent + "from telegram.ext import PicklePersistence as _PicklePersistence\n")
            out.append(indent + 'builder.persistence(_PicklePersistence(filepath="' + PERSIST_PATH + '"))\n')
            injected += 1

    if injected != 1:
        print("[patch] ERROR: expected 1 anchor in core.py, injected {}".format(injected), file=sys.stderr)
        return 1

    core.write_text("".join(out), encoding="utf-8")
    print("[patch] PTB chat_data persistence applied to", core)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
