"""Build-time patcher: install group mention-gating into the pip-installed bot.

Copies ``mention_gate.py`` into the bot package and registers it in ``bot/core.py``
right after the Telegram application is built. Idempotent and indentation-robust;
fails loudly if the anchor is missing (e.g. after a bot version bump), so a broken
patch can't ship silently.

Must run AFTER patch_chat_scope.py (which also edits core.py). That patch appends
*below* the ``self.app = builder.build()`` line and leaves the line itself intact,
so this patcher still finds the anchor.

Usage: python3 patch_mention_gate.py <path-to-mention_gate.py>
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
    gate_src = Path(sys.argv[1]).resolve()
    pkg = find_pkg()
    shutil.copyfile(gate_src, pkg / "mention_gate.py")

    core = pkg / "bot" / "core.py"
    text = core.read_text(encoding="utf-8")
    if "mention_gate" in text:
        print("[patch] core.py already has mention_gate; skipping")
        return 0

    injected = 0
    out = []
    for line in text.splitlines(keepends=True):
        out.append(line)
        if line.strip() == "self.app = builder.build()":
            indent = line[: len(line) - len(line.lstrip())]
            out.append(indent + "from .. import mention_gate as _mention_gate_install\n")
            out.append(indent + "_mention_gate_install.install(self.app)\n")
            injected += 1

    if injected != 1:
        print(
            "[patch] ERROR: expected 1 anchor in core.py, injected {}".format(injected),
            file=sys.stderr,
        )
        return 1

    core.write_text("".join(out), encoding="utf-8")
    print("[patch] group mention-gating applied to", core)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
