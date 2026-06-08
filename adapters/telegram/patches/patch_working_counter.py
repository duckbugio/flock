"""Build-time patcher: keep the "Working... (Ns)" counter ticking during long silent tool calls.

The progress message is only re-rendered from the stream callback (`_on_stream`), which fires
on tool/text events. So while a SINGLE long tool call runs — e.g. a full test suite executed
synchronously in the foreground — nothing streams back, the callback never fires, and the
"Working... (Ns)" counter FREEZES at whatever value it had when the call started. The user then
can't tell "still running" from "hung" (and the whole point of running suites in the foreground
is that they ARE long and silent).

There is already an independent typing heartbeat (`_start_typing_heartbeat`) that ticks on a
wall clock to keep the Telegram "typing…" action alive. This patch teaches that heartbeat to
ALSO re-render the progress message every couple of intervals, so the elapsed counter advances
on the wall clock regardless of stream activity. A frozen counter then means a real hang again.

Three edits to src/bot/orchestrator.py:
  A. widen `_start_typing_heartbeat`'s signature with optional progress handles;
  B. add the counter-bump to the inner `_heartbeat` loop;
  C. at the main message path (the only call site with `start_time` + the Stop keyboard), pass
     those handles in. The doc/photo call sites are left typing-only (no long suites there).

Idempotent; fails loudly if an anchor is missing (e.g. after a version bump). Pass --check to
dry-run. Usage: python3 patch_working_counter.py
"""
import importlib.util
import sys
from pathlib import Path

# --- A. signature: add the optional progress handles ---
SIG_OLD = (
    "    def _start_typing_heartbeat(\n"
    "        chat: Any,\n"
    "        interval: float = 2.0,\n"
    '    ) -> "asyncio.Task[None]":\n'
)
SIG_NEW = (
    "    def _start_typing_heartbeat(\n"
    "        chat: Any,\n"
    "        interval: float = 2.0,\n"
    "        progress_msg: Any = None,\n"
    "        tool_log: Any = None,\n"
    "        verbose_level: int = 0,\n"
    "        start_time: Any = None,\n"
    "        reply_markup: Any = None,\n"
    "        format_progress: Any = None,\n"
    '    ) -> "asyncio.Task[None]":\n'
)

# --- B. body: bump the elapsed counter on the wall clock ---
BODY_OLD = (
    "        async def _heartbeat() -> None:\n"
    "            try:\n"
    "                while True:\n"
    "                    await asyncio.sleep(interval)\n"
    "                    try:\n"
    '                        await chat.send_action("typing")\n'
    "                    except Exception:\n"
    "                        pass\n"
    "            except asyncio.CancelledError:\n"
    "                pass\n"
)
BODY_NEW = (
    "        async def _heartbeat() -> None:\n"
    "            hb_start = time.time() if start_time is None else start_time\n"
    "            ticks = 0\n"
    "            try:\n"
    "                while True:\n"
    "                    await asyncio.sleep(interval)\n"
    "                    try:\n"
    '                        await chat.send_action("typing")\n'
    "                    except Exception:\n"
    "                        pass\n"
    "                    ticks += 1\n"
    "                    # Advance the \"Working... (Ns)\" counter on a wall clock so it\n"
    "                    # keeps moving even while one long tool call (e.g. a foreground\n"
    "                    # test suite) streams nothing back — otherwise it freezes and the\n"
    "                    # user can't tell \"running\" from \"hung\".\n"
    "                    if (\n"
    "                        progress_msg is not None\n"
    "                        and format_progress is not None\n"
    "                        and verbose_level >= 1\n"
    "                        and tool_log\n"
    "                        and ticks % 2 == 0\n"
    "                    ):\n"
    "                        try:\n"
    "                            await progress_msg.edit_text(\n"
    "                                format_progress(tool_log, verbose_level, hb_start),\n"
    "                                reply_markup=reply_markup,\n"
    "                            )\n"
    "                        except Exception:\n"
    "                            pass\n"
    "            except asyncio.CancelledError:\n"
    "                pass\n"
)

# --- C. main message call site (unique via its comment line) ---
CALL_OLD = (
    "        # Independent typing heartbeat — stays alive even with no stream events\n"
    "        heartbeat = self._start_typing_heartbeat(chat)\n"
)
CALL_NEW = (
    "        # Independent typing heartbeat — stays alive even with no stream events.\n"
    '        # Pass the progress handles so it ALSO advances the "Working... (Ns)" counter\n'
    "        # during long silent tool calls (e.g. a foreground test suite).\n"
    "        heartbeat = self._start_typing_heartbeat(\n"
    "            chat,\n"
    "            progress_msg=progress_msg,\n"
    "            tool_log=tool_log,\n"
    "            verbose_level=verbose_level,\n"
    "            start_time=start_time,\n"
    "            reply_markup=stop_kb,\n"
    "            format_progress=self._format_verbose_progress,\n"
    "        )\n"
)

EDITS = [("signature", SIG_OLD, SIG_NEW), ("heartbeat body", BODY_OLD, BODY_NEW), ("call site", CALL_OLD, CALL_NEW)]


def main() -> int:
    check = "--check" in sys.argv
    spec = importlib.util.find_spec("src")
    if spec is None or not spec.origin:
        print("[patch] ERROR: bot package 'src' not found", file=sys.stderr)
        return 1
    orch = Path(spec.origin).resolve().parent / "bot" / "orchestrator.py"
    text = orch.read_text(encoding="utf-8")

    already = "format_progress=self._format_verbose_progress" in text

    if check:
        states = {name: (old in text) for name, old, _ in EDITS}
        print("CHECK: already_patched={} matches={}".format(already, states))
        ok = already or all(states.values())
        return 0 if ok else 2

    if already:
        print("[patch] orchestrator.py already ticks the counter via heartbeat; skipping")
        return 0

    for name, old, new in EDITS:
        if old not in text:
            print("[patch] ERROR: anchor not found in orchestrator.py ({})".format(name), file=sys.stderr)
            return 1
        text = text.replace(old, new, 1)

    orch.write_text(text, encoding="utf-8")
    print("[patch] working-counter heartbeat applied to", orch)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
