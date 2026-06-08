"""Build-time patcher: don't discard the Claude session on a TIMEOUT.

facade.run_command treats ANY error during a resumed run as "resume failed" — it
removes the session and retries fresh. So a turn that merely runs past
CLAUDE_TIMEOUT_SECONDS loses the whole conversation, and the next message starts
cold. But a timeout is NOT a resume failure: the session is fine, the run just took
too long. This narrows the discard-and-retry to NON-timeout errors, so on a timeout
the session is kept and the next message resumes it.

Idempotent; fails loudly if the anchor is missing (e.g. after a version bump).
Pass --check to dry-run.

Usage: python3 patch_session_timeout.py
"""
import importlib.util
import sys
from pathlib import Path

OLD = (
    "                if should_continue:\n"
    "                    logger.warning(\n"
    '                        "Session resume failed, starting fresh session",\n'
)
NEW = (
    "                # A timeout is not a resume failure — the session is fine, the run\n"
    "                # just took too long; keep it so the next message resumes it.\n"
    '                if should_continue and "timed out" not in str(resume_error).lower():\n'
    "                    logger.warning(\n"
    '                        "Session resume failed, starting fresh session",\n'
)


def main() -> int:
    check = "--check" in sys.argv
    spec = importlib.util.find_spec("src")
    if spec is None or not spec.origin:
        print("[patch] ERROR: bot package 'src' not found", file=sys.stderr)
        return 1
    facade = Path(spec.origin).resolve().parent / "claude" / "facade.py"
    text = facade.read_text(encoding="utf-8")

    already = "not in str(resume_error)" in text
    matched = OLD in text

    if check:
        print("CHECK: match={} already_patched={}".format(matched, already))
        return 0 if (matched or already) else 2

    if already:
        print("[patch] facade.py already keeps the session on timeout; skipping")
        return 0
    if not matched:
        print("[patch] ERROR: resume-except anchor not found in facade.py", file=sys.stderr)
        return 1

    facade.write_text(text.replace(OLD, NEW, 1), encoding="utf-8")
    print("[patch] session-keep-on-timeout applied to", facade)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
