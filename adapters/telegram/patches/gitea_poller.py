"""Poll Gitea for new PR review activity and feed it into the event bus.

Inbound webhooks from Gitea can't always reach this bot (e.g. RU<->foreign-host
network filtering), but the bot CAN reach Gitea (it clones repos and posts reviews).
This inverts the dependency: a background loop polls the bot user's Gitea
*notifications* for unread Pull threads, and for each new comment/review on a
``duck/<chatid>/...`` branch it publishes the SAME ``WebhookEvent`` the inbound
webhook server would have. The existing chat-routing + PR-responder path
(``handle_webhook`` -> ``route_for_event``) then handles it unchanged.

Enabled when ``GITEA_API_URL`` + ``GIT_TOKEN`` are set and ``GITEA_POLL_INTERVAL`` > 0.
Started as a background task in ``main.run_application`` by ``patch_gitea_poller.py``.
"""
import asyncio
import os
from typing import Any, Dict

import httpx
import structlog

from .events.types import WebhookEvent

logger = structlog.get_logger(__name__)


def _env(name: str, default: str = "") -> str:
    return (os.environ.get(name) or default).strip()


def _interval() -> int:
    try:
        return int(_env("GITEA_POLL_INTERVAL", "90"))
    except ValueError:
        return 90


def _pr_review_enabled() -> bool:
    """The published-PR review/fix loop (this poller + the CLAUDE.md Phase 2) is OFF by
    default; set ENABLE_PR_REVIEW=true to turn it on."""
    return _env("ENABLE_PR_REVIEW").lower() in {"1", "true", "yes", "on"}


def poller_enabled(config: Any) -> bool:
    """True when published-PR review is enabled AND Gitea polling is configured."""
    return _pr_review_enabled() and bool(_env("GITEA_API_URL") and _env("GIT_TOKEN")) and _interval() > 0


async def run_poller(event_bus: Any, config: Any) -> None:
    base = _env("GITEA_API_URL").rstrip("/")
    token = _env("GIT_TOKEN")
    self_login = _env("GIT_USER").lower()
    interval = max(30, _interval())
    headers = {"Authorization": "token " + token, "Accept": "application/json"}
    logger.info("Gitea poller started", base=base, interval=interval)

    async with httpx.AsyncClient(timeout=20.0, headers=headers) as client:
        while True:
            try:
                await _poll_once(client, base, self_login, event_bus)
            except asyncio.CancelledError:
                raise
            except Exception:
                logger.exception("Gitea poll cycle failed")
            await asyncio.sleep(interval)


async def _poll_once(client: httpx.AsyncClient, base: str, self_login: str, event_bus: Any) -> None:
    resp = await client.get(
        base + "/notifications",
        params={"status-types": "unread", "subject-type": "Pull", "limit": 50},
    )
    if resp.status_code >= 400:
        logger.warning("Gitea notifications poll failed", status=resp.status_code)
        return
    for thread in resp.json() or []:
        try:
            await _handle_thread(client, base, self_login, event_bus, thread)
        except Exception:
            logger.exception("Failed handling notification thread", thread_id=thread.get("id"))


async def _handle_thread(
    client: httpx.AsyncClient, base: str, self_login: str, event_bus: Any, thread: Dict[str, Any]
) -> None:
    subject = thread.get("subject") or {}
    thread_id = thread.get("id")
    raw_url = subject.get("url") or ""
    if not raw_url:
        await _mark_read(client, base, thread_id)
        return

    # Gitea points the subject at the issue URL; the PR (with head.ref) is /pulls/N.
    pr_resp = await client.get(raw_url.replace("/issues/", "/pulls/"))
    if pr_resp.status_code >= 400:
        return  # transient — leave unread, retry next cycle
    pr = pr_resp.json() or {}
    # Only act on OPEN PRs — the human merges; there's nothing to fix on a merged/closed PR.
    if pr.get("state") != "open" or pr.get("merged"):
        await _mark_read(client, base, thread_id)
        return
    ref = ((pr.get("head") or {}).get("ref")) or ""
    if not ref.startswith("duck/"):
        await _mark_read(client, base, thread_id)  # not a team branch — clear it
        return

    comment: Dict[str, Any] = {}
    comment_url = subject.get("latest_comment_url") or ""
    if comment_url:
        c_resp = await client.get(comment_url)
        if c_resp.status_code < 400:
            comment = c_resp.json() or {}

    # Never react to our own comments (avoid self-trigger loops).
    author = (((comment.get("user") or {}).get("login")) or "").lower()
    if author and self_login and author == self_login:
        await _mark_read(client, base, thread_id)
        return

    repo = (pr.get("base") or {}).get("repo") or thread.get("repository") or {}
    payload: Dict[str, Any] = {
        "action": "reviewed",
        "pull_request": pr,
        "repository": repo,
        "comment": comment,
    }
    event = WebhookEvent(
        provider="gitea",
        event_type_name="pull_request_comment",
        payload=payload,
        delivery_id="poll-" + str(thread_id or comment.get("id") or ""),
    )
    await event_bus.publish(event)
    logger.info("Gitea poller published PR event", pr=pr.get("number"), branch=ref)
    await _mark_read(client, base, thread_id)


async def _mark_read(client: httpx.AsyncClient, base: str, thread_id: Any) -> None:
    if not thread_id:
        return
    try:
        await client.patch(
            base + "/notifications/threads/" + str(thread_id),
            params={"to-status": "read"},
        )
    except Exception:
        logger.warning("Failed to mark notification read", thread_id=thread_id)
