"""Per-chat serialized dispatch for PTB updates.

The upstream StopAwareUpdateProcessor serializes ALL non-priority updates through a
single global lock — one at a time across the whole bot, so different chats block each
other. This replaces that with PER-CHAT serialization: within a chat, updates run one at
a time (FIFO); DIFFERENT chats run in PARALLEL, capped by a global semaphore
(MAX_CONCURRENT_CHAT_RUNS, default 4) to bound memory on a small host.

Also supports, for the queue:
- supersede(chat_id, message_id): an edited message marks the still-queued original to be
  skipped, so the CORRECTED text runs instead of the stale one (Telegram keeps the same
  message_id on edit).
- clear_chat(chat_id): the Stop button skips everything still queued for that chat.

Wired into bot/update_processor.py at build time by patch_chat_queue.py.
"""
import asyncio
import os
from typing import Any, Awaitable, Dict, Optional


def _cap() -> int:
    try:
        return max(1, int(os.environ.get("MAX_CONCURRENT_CHAT_RUNS", "4")))
    except (TypeError, ValueError):
        return 4


class _Item:
    __slots__ = ("message_id", "coroutine", "done", "skip", "started")

    def __init__(self, message_id: Optional[int], coroutine: Awaitable[Any], done: "asyncio.Future") -> None:
        self.message_id = message_id
        self.coroutine = coroutine
        self.done = done
        self.skip = False
        self.started = False


class _Lane:
    def __init__(self) -> None:
        self.queue: "asyncio.Queue[_Item]" = asyncio.Queue()
        self.pending: Dict[int, _Item] = {}  # message_id -> latest not-yet-started item
        self.worker: Optional[asyncio.Task] = None


class ChatDispatcher:
    """Routes each update through its chat's serial lane; lanes run concurrently up to a cap."""

    def __init__(self) -> None:
        self._lanes: Dict[Any, _Lane] = {}
        self._sem: Optional[asyncio.Semaphore] = None

    def _semaphore(self) -> asyncio.Semaphore:
        if self._sem is None:
            self._sem = asyncio.Semaphore(_cap())
        return self._sem

    def _lane(self, chat_id: Any) -> _Lane:
        lane = self._lanes.get(chat_id)
        if lane is None:
            lane = _Lane()
            self._lanes[chat_id] = lane
        return lane

    async def submit(self, chat_id: Any, message_id: Optional[int], coroutine: Awaitable[Any]) -> None:
        """Enqueue handling for a chat; returns once it has been processed (or skipped)."""
        lane = self._lane(chat_id)
        done = asyncio.get_running_loop().create_future()
        item = _Item(message_id, coroutine, done)
        if message_id is not None:
            lane.pending[message_id] = item
        if lane.worker is None:
            lane.worker = asyncio.create_task(self._run_lane(lane))
        await lane.queue.put(item)
        await done

    def supersede(self, chat_id: Any, message_id: int) -> None:
        """An edit arrived for message_id: skip the still-queued original (run the edit instead)."""
        lane = self._lanes.get(chat_id)
        if lane is None:
            return
        item = lane.pending.get(message_id)
        if item is not None and not item.started:
            item.skip = True

    def clear_chat(self, chat_id: Any) -> None:
        """Stop: skip everything still queued for this chat."""
        lane = self._lanes.get(chat_id)
        if lane is None:
            return
        for item in list(lane.pending.values()):
            if not item.started:
                item.skip = True

    async def _run_lane(self, lane: _Lane) -> None:
        while True:
            item = await lane.queue.get()
            item.started = True
            if item.message_id is not None and lane.pending.get(item.message_id) is item:
                del lane.pending[item.message_id]
            try:
                if item.skip:
                    _close(item.coroutine)
                else:
                    async with self._semaphore():
                        await item.coroutine
            except asyncio.CancelledError:
                _close(item.coroutine)
                raise
            except Exception:  # keep the lane worker alive; PTB handles handler errors itself
                pass
            finally:
                _resolve(item.done)


def _close(coroutine: Any) -> None:
    close = getattr(coroutine, "close", None)
    if callable(close):
        try:
            close()
        except Exception:
            pass


def _resolve(fut: "asyncio.Future") -> None:
    if fut is not None and not fut.done():
        fut.set_result(None)


# Module singleton used by the patched update processor.
dispatcher = ChatDispatcher()
