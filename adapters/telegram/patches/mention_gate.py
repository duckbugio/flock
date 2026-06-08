"""Group-chat mention gating for claude-code-telegram.

The upstream bot answers EVERY message in any chat — its message handler has no
mention/chat-type filter. In a shared group that means it would react to humans
talking to each other (and burn a Claude turn per message). This fixes that:

- private chats: unchanged — the bot answers everything.
- group / supergroup: the bot stays silent UNLESS the message is addressed to it:
    (a) an @mention of the bot, or
    (b) a reply to one of the bot's own messages, or
    (c) a /command.

Implemented as a high-priority TypeHandler (group=-50 — after the per-chat seeder
at -100, before the normal handlers at 0). When a group message isn't addressed to
the bot it raises ApplicationHandlerStop, suppressing the handlers that would
otherwise invoke Claude. Fails OPEN: if anything is uncertain, it does not block.

Injected into the installed bot at build time by patch_mention_gate.py.
"""
import os

from telegram import MessageEntity, Update
from telegram.ext import ApplicationHandlerStop, TypeHandler


def _gate_enabled() -> bool:
    """Group mention-gating is OFF by default (the bot answers every group message);
    set REQUIRE_GROUP_MENTION=true to only answer when @mentioned or replied-to."""
    return (os.environ.get("REQUIRE_GROUP_MENTION") or "").strip().lower() in {"1", "true", "yes", "on"}

_GROUP_CHAT_TYPES = {"group", "supergroup"}


def _addressed_to_bot(update: Update, bot_username: str, bot_id: int) -> bool:
    msg = update.effective_message
    if msg is None:
        return True  # not a normal message (callback query, etc.) — don't interfere

    text = msg.text or msg.caption or ""

    # (c) a command — let PTB's CommandHandler do the @bot disambiguation
    if text.startswith("/"):
        return True

    # (b) a reply to one of the bot's own messages
    reply = msg.reply_to_message
    if reply is not None and reply.from_user is not None and reply.from_user.id == bot_id:
        return True

    # (a) an explicit @mention or text-mention of the bot
    uname = (bot_username or "").lstrip("@").lower()
    for ent in list(msg.entities or []) + list(msg.caption_entities or []):
        if ent.type == MessageEntity.MENTION and uname:
            frag = text[ent.offset : ent.offset + ent.length].lstrip("@").lower()
            if frag == uname:
                return True
        elif ent.type == MessageEntity.TEXT_MENTION and ent.user is not None:
            if ent.user.id == bot_id:
                return True

    return False


async def _gate(update: Update, context) -> None:
    chat = update.effective_chat
    if chat is None or chat.type not in _GROUP_CHAT_TYPES:
        return  # private chat → unchanged

    bot = context.bot
    try:
        bot_username = bot.username or ""
        bot_id = bot.id
    except RuntimeError:
        return  # bot not fully initialised yet — fail open, don't block

    if _addressed_to_bot(update, bot_username, bot_id):
        return  # addressed to us → let the normal handlers run

    raise ApplicationHandlerStop  # unrelated group chatter → stay silent


def install(application) -> None:
    """Register the group mention-gate — only when REQUIRE_GROUP_MENTION is enabled.
    Default (unset/false): no gate, so the bot answers every group message."""
    if not _gate_enabled():
        return
    application.add_handler(TypeHandler(Update, _gate), group=-50)
