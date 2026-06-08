"""Per-chat tenancy for claude-code-telegram.

The bot stores conversation state (``current_directory`` / ``claude_session_id``)
in ``context.user_data`` — which is per *user*. This module makes that state
per *chat* instead: all participants of a chat share one isolated workspace +
Claude session, while different chats stay fully isolated. Auth (the user-id
whitelist) is untouched.

Injected into the installed bot at build time by ``patch_chat_scope.py``.
"""
import os
from pathlib import Path

from telegram import Update
from telegram.ext import CallbackContext, TypeHandler


class ChatScopedContext(CallbackContext):
    """A context whose ``user_data`` is actually the per-CHAT store.

    Every ``context.user_data[...]`` access in the bot transparently reads/writes
    ``chat_data`` instead, so a group shares one workspace + session and separate
    chats are isolated. Falls back to user-scoped data when there is no chat.
    """

    @property
    def user_data(self):  # type: ignore[override]
        chat_scoped = self.chat_data
        if chat_scoped is not None:
            return chat_scoped
        return super().user_data


def _workspace_root() -> Path:
    return Path(os.environ.get("APPROVED_DIRECTORY", "/workspace"))


async def _seed_chat_workspace(update: Update, context) -> None:
    """Give each chat its own ``/workspace/chat_<id>`` and link shared team config."""
    chat = update.effective_chat
    if chat is None:
        return
    data = context.user_data  # chat-scoped via ChatScopedContext
    if data.get("current_directory"):
        return

    root = _workspace_root()
    chat_dir = root / "chat_{}".format(chat.id)
    try:
        chat_dir.mkdir(parents=True, exist_ok=True)
        # Make the shared, read-only team config (subagents + CLAUDE.md) visible
        # from inside the per-chat working directory.
        for name in (".claude", "CLAUDE.md"):
            link = chat_dir / name
            target = root / name
            if target.exists() and not link.exists():
                try:
                    link.symlink_to(target, target_is_directory=target.is_dir())
                except OSError:
                    pass
    except OSError:
        pass
    data["current_directory"] = str(chat_dir)


def install(application) -> None:
    """Register the per-chat seeder to run before the bot's main handlers."""
    application.add_handler(TypeHandler(Update, _seed_chat_workspace), group=-100)
