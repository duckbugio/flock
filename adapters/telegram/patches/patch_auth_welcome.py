"""Build-time patcher: silence the per-session auth "welcome" banner.

The bot's auth middleware (src/bot/middleware/auth.py) greets the user with
"🔓 Welcome! You are now authenticated. Session started at …" whenever there is no
active auth session. Auth sessions are kept IN MEMORY, so every container restart
(i.e. every deploy) makes the next message re-trigger the banner — which looks like a
context reset even though the Claude conversation is resumed normally (by project
path). This removes the banner. Authentication itself is untouched.

Idempotent. Fails loudly if the block is missing (e.g. after a bot version bump).
Pass --check to dry-run (report match without writing).

Usage: python3 patch_auth_welcome.py [--check]
"""
import importlib.util
import re
import sys
from pathlib import Path

# Whitespace-tolerant: matches the optional comment + the `if event.effective_message`
# guard + the two-line welcome reply_text() call, regardless of exact indentation.
PATTERN = re.compile(
    r"(?:[ \t]*# Welcome message for new session\n)?"
    r"[ \t]*if event\.effective_message:\n"
    r"[ \t]*await event\.effective_message\.reply_text\(\n"
    r'[ \t]*f"🔓 Welcome![^\n]*\n'
    r'[ \t]*f"Session started at[^\n]*\n'
    r"[ \t]*\)\n"
)

NEW = (
    "        # Welcome banner suppressed by patch_auth_welcome.py: it re-fired on every\n"
    "        # deploy (auth sessions are in-memory, reset on restart) and looked like a\n"
    "        # context reset. Auth still happens; the Claude session/context is unaffected.\n"
)


def main() -> int:
    check = "--check" in sys.argv
    spec = importlib.util.find_spec("src")
    if spec is None or not spec.origin:
        print("[patch] ERROR: bot package 'src' not found", file=sys.stderr)
        return 1
    auth = Path(spec.origin).resolve().parent / "bot" / "middleware" / "auth.py"
    text = auth.read_text(encoding="utf-8")

    already = "Welcome banner suppressed" in text
    matched = PATTERN.search(text) is not None

    if check:
        print("CHECK: match={} already_patched={}".format(matched, already))
        return 0 if (matched or already) else 2

    if already:
        print("[patch] auth.py welcome banner already suppressed; skipping")
        return 0
    if not matched:
        print("[patch] ERROR: welcome-banner block not found in auth.py", file=sys.stderr)
        return 1

    auth.write_text(PATTERN.sub(NEW, text, count=1), encoding="utf-8")
    print("[patch] auth welcome banner suppressed in", auth)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
