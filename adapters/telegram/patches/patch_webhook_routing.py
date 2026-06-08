"""Build-time patcher: route PR webhooks back to the chat that opened the PR.

The upstream handle_webhook runs every webhook in a single default workspace as a
default user and publishes AgentResponseEvent(chat_id=0) -> broadcast to the
configured notification chat(s). That can't serve multiple users in different chats.

This rewrites handle_webhook to call webhook_responder.route_for_event(), which parses
the chat id from the `duck/<chatid>/<slug>` branch name in the PR payload, so the run
uses that chat's isolated workspace and the reply goes to that chat (NotificationService
sends to a specific chat_id when it is non-zero). Falls back to broadcast for non-team
branches (e.g. legacy).

Must run AFTER patch_webhook.py (which copies webhook_responder.py into the package).
Idempotent; fails loudly if handle_webhook can't be matched (e.g. after a version bump).
Pass --check to dry-run.

Usage: python3 patch_webhook_routing.py [--check]
"""
import importlib.util
import re
import sys
from pathlib import Path

# Match the whole handle_webhook method, up to (not including) handle_scheduled.
PATTERN = re.compile(
    r"    async def handle_webhook\(self, event: Event\) -> None:\n"
    r".*?\n"
    r"(?=    async def handle_scheduled\(self, event: Event\) -> None:)",
    re.DOTALL,
)

NEW_METHOD = (
    "    async def handle_webhook(self, event: Event) -> None:\n"
    '        """Process a webhook event through Claude, routed to the originating chat."""\n'
    "        if not isinstance(event, WebhookEvent):\n"
    "            return\n"
    "\n"
    "        from .. import webhook_responder as _wr\n"
    "\n"
    "        chat_id, working_directory = _wr.route_for_event(\n"
    "            event, self.default_working_directory\n"
    "        )\n"
    "\n"
    "        logger.info(\n"
    '            "Processing webhook event through agent",\n'
    "            provider=event.provider,\n"
    "            event_type=event.event_type_name,\n"
    "            delivery_id=event.delivery_id,\n"
    "            routed_chat_id=chat_id,\n"
    "        )\n"
    "\n"
    "        prompt = self._build_webhook_prompt(event)\n"
    "\n"
    "        try:\n"
    "            response = await self.claude.run_command(\n"
    "                prompt=prompt,\n"
    "                working_directory=working_directory,\n"
    "                user_id=self.default_user_id,\n"
    "            )\n"
    "\n"
    "            if response.content:\n"
    "                await self.event_bus.publish(\n"
    "                    AgentResponseEvent(\n"
    "                        chat_id=chat_id,\n"
    "                        text=response.content,\n"
    "                        originating_event_id=event.id,\n"
    "                    )\n"
    "                )\n"
    "        except Exception:\n"
    "            logger.exception(\n"
    '                "Agent execution failed for webhook event",\n'
    "                provider=event.provider,\n"
    "                event_id=event.id,\n"
    "            )\n"
    "\n"
)


def main() -> int:
    check = "--check" in sys.argv
    spec = importlib.util.find_spec("src")
    if spec is None or not spec.origin:
        print("[patch] ERROR: bot package 'src' not found", file=sys.stderr)
        return 1
    handlers = Path(spec.origin).resolve().parent / "events" / "handlers.py"
    text = handlers.read_text(encoding="utf-8")

    already = "route_for_event" in text
    matched = PATTERN.search(text) is not None

    if check:
        print("CHECK: match={} already_patched={}".format(matched, already))
        return 0 if (matched or already) else 2

    if already:
        print("[patch] handlers.py already routes webhooks; skipping")
        return 0
    if not matched:
        print("[patch] ERROR: handle_webhook block not found in handlers.py", file=sys.stderr)
        return 1

    handlers.write_text(PATTERN.sub(NEW_METHOD, text, count=1), encoding="utf-8")
    print("[patch] webhook chat-routing applied to", handlers)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
