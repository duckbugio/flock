"""Build-time patcher: install per-chat tenancy into the pip-installed bot.

Copies ``chat_scope.py`` into the bot package and wires it into ``bot/core.py``
(use the chat-scoped context + register the per-chat workspace seeder).
Idempotent and indentation-robust. Fails loudly if the anchors are missing
(e.g. after a bot version bump), so a broken patch can't ship silently.

Usage: python3 patch_chat_scope.py <path-to-chat_scope.py>
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
    chat_scope_src = Path(sys.argv[1]).resolve()
    pkg = find_pkg()
    shutil.copyfile(chat_scope_src, pkg / "chat_scope.py")

    core = pkg / "bot" / "core.py"
    text = core.read_text(encoding="utf-8")
    if "chat_scope" in text:
        print("[patch] core.py already patched; skipping")
        return 0

    injected = 0
    out = []
    for line in text.splitlines(keepends=True):
        out.append(line)
        stripped = line.strip()
        indent = line[: len(line) - len(line.lstrip())]
        if stripped == "builder.token(self.settings.telegram_token_str)":
            out.append(indent + "from .. import chat_scope as _chat_scope\n")
            out.append(
                indent
                + "builder.context_types(ContextTypes(context=_chat_scope.ChatScopedContext))\n"
            )
            injected += 1
        elif stripped == "self.app = builder.build()":
            out.append(indent + "from .. import chat_scope as _chat_scope_install\n")
            out.append(indent + "_chat_scope_install.install(self.app)\n")
            injected += 1

    if injected != 2:
        print(
            "[patch] ERROR: expected 2 anchors in core.py, injected {}".format(injected),
            file=sys.stderr,
        )
        return 1

    core.write_text("".join(out), encoding="utf-8")
    print("[patch] per-chat tenancy applied to", core)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
