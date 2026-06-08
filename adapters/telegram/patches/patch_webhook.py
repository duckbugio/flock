"""Build-time patcher: route Gitea PR webhooks to the PR-responder prompt.

Copies webhook_responder.py into the bot package and patches
events/handlers.py's _build_webhook_prompt to use it for pull-request events.
Idempotent; fails loudly if the anchor is missing (e.g. after a version bump).

Usage: python3 patch_webhook.py <path-to-webhook_responder.py>
"""
import importlib.util
import shutil
import sys
from pathlib import Path


def find_pkg() -> Path:
    spec = importlib.util.find_spec("src")
    if spec is None or not spec.origin:
        print("[patch] ERROR: bot package 'src' not found", file=sys.stderr)
        sys.exit(1)
    return Path(spec.origin).resolve().parent


def main() -> int:
    mod_src = Path(sys.argv[1]).resolve()
    pkg = find_pkg()
    shutil.copyfile(mod_src, pkg / "webhook_responder.py")

    handlers = pkg / "events" / "handlers.py"
    text = handlers.read_text(encoding="utf-8")
    if "webhook_responder" in text:
        print("[patch] handlers.py already patched; skipping")
        return 0

    anchor = "payload_summary = self._summarize_payload(event.payload)"
    injected = 0
    out = []
    for line in text.splitlines(keepends=True):
        if line.strip() == anchor and injected == 0:
            indent = line[: len(line) - len(line.lstrip())]
            out.append(indent + "from .. import webhook_responder as _wr\n")
            out.append(indent + "if _wr.is_pr_event(event.payload):\n")
            out.append(indent + "    return _wr.build_pr_prompt(event.provider, event.payload)\n")
            injected += 1
        out.append(line)

    if injected != 1:
        print(
            "[patch] ERROR: anchor not found in handlers.py, injected {}".format(injected),
            file=sys.stderr,
        )
        return 1

    handlers.write_text("".join(out), encoding="utf-8")
    print("[patch] PR-responder applied to", handlers)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
