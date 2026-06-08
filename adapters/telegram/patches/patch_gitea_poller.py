"""Build-time patcher: start the Gitea poller as a background task.

Copies gitea_poller.py into the bot package and launches it from
main.run_application (right before the scheduler block), so it runs alongside the
bot/api-server tasks. The poller only activates when GITEA_API_URL + GIT_TOKEN are
set (see gitea_poller.poller_enabled), so this is a no-op when git isn't configured.

Idempotent; fails loudly if the anchor is missing (e.g. after a version bump).
Pass --check to dry-run.

Usage: python3 patch_gitea_poller.py <path-to-gitea_poller.py>
"""
import importlib.util
import shutil
import sys
from pathlib import Path

ANCHOR = "        # Scheduler (if enabled)\n"
BLOCK = (
    "        # Gitea poller — reach PR comments even when inbound webhooks can't\n"
    "        # be delivered (e.g. RU<->foreign-host network filtering).\n"
    "        from src import gitea_poller as _gitea_poller\n"
    "        if _gitea_poller.poller_enabled(config):\n"
    "            tasks.append(\n"
    "                asyncio.create_task(_gitea_poller.run_poller(event_bus, config))\n"
    "            )\n"
    "            logger.info(\"Gitea poller enabled\")\n"
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
        shutil.copyfile(Path(args[0]).resolve(), pkg / "gitea_poller.py")

    main_py = pkg / "main.py"
    text = main_py.read_text(encoding="utf-8")
    already = "gitea_poller" in text
    matched = ANCHOR in text

    if check:
        print("CHECK: match={} already_patched={}".format(matched, already))
        return 0 if (matched or already) else 2

    if already:
        print("[patch] main.py already starts the gitea poller; skipping")
        return 0
    if not matched:
        print("[patch] ERROR: scheduler anchor not found in main.py", file=sys.stderr)
        return 1

    main_py.write_text(text.replace(ANCHOR, BLOCK + ANCHOR, 1), encoding="utf-8")
    print("[patch] gitea poller wired into", main_py)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
