"""Turn a Gitea pull-request webhook into a PR-responder task.

Instead of the bot's default "summarize this event" prompt, a PR webhook becomes
an instruction for the dev team to fetch the PR branch, address the open review
comments (coder, under the arbiter), push, and reply on the PR. Only acts on the
team's own `duck/<chatid>/*` branches.

Patched into events/handlers.py at build time by patch_webhook.py.
Chat routing (route_for_event) is wired into handle_webhook by patch_webhook_routing.py.
"""
import os
import re
from typing import Any, Dict

# Team branches encode the originating chat: `duck/<chatid>/<slug>`.
_BRANCH_RE = re.compile(r"^duck/(-?\d+)/")


def is_pr_event(payload: Dict[str, Any]) -> bool:
    """True for a Gitea pull-request / PR-review / PR-comment webhook payload."""
    return isinstance(payload, dict) and isinstance(payload.get("pull_request"), dict)


def build_pr_prompt(provider: str, payload: Dict[str, Any]) -> str:
    pr = payload.get("pull_request") or {}
    repo = payload.get("repository") or {}
    comment = payload.get("comment") or {}
    review = payload.get("review") or {}
    action = payload.get("action", "")
    number = pr.get("number")
    title = pr.get("title", "")
    head = (pr.get("head") or {}).get("ref", "")
    base = (pr.get("base") or {}).get("ref", "")
    full_name = repo.get("full_name", "")
    clone_url = repo.get("clone_url") or repo.get("html_url", "")
    pr_url = pr.get("html_url", "")
    body = (comment.get("body") or review.get("body") or "")[:800]
    path = comment.get("path", "")

    return (
        f"A {provider} pull-request webhook fired (action: {action}).\n"
        f'PR #{number} "{title}" in repo {full_name}\n'
        f"  branch: {head} -> {base}\n  url: {pr_url}\n  clone: {clone_url}\n"
        f"  new comment{(' on ' + path) if path else ''}: {body}\n\n"
        "You are the Lead of the dev team. **Only act if the PR branch starts with "
        "`duck/`** (the team's own PRs, named `duck/<chatid>/<slug>`) AND the PR is still "
        "OPEN (not merged/closed); otherwise briefly summarize and STOP.\n"
        "If it is an open `duck/*` branch:\n"
        "1. Work in a fresh scratch dir under the workspace (e.g. `./_pr/<repo>`): clone or "
        "fetch the repo with the configured git credentials and checkout the PR branch.\n"
        "2. Read ALL open review comments on the PR via the git host API (derive the host from "
        "the clone URL; auth header `Authorization: token $GIT_TOKEN`).\n"
        "3. Have `coder` address each comment per CLAUDE.md; run `tester`; the `arbiter` governs "
        "the loop and the configured cycle limits.\n"
        "4. Commit and push to the SAME branch (the PR updates); reply on the PR with a short "
        "summary of the fixes, written in the **PR's own language** (match the language of the "
        "PR title/description/comments). NEVER merge — the human merges.\n"
        "5. Report a concise summary of what changed and the PR status."
    )


def route_for_event(event: Any, default_working_directory: Any):
    """Derive (chat_id, working_directory) for a PR webhook.

    A webhook carries no Telegram chat, so by default the bot broadcasts to a single
    configured chat. The team encodes the originating chat in the branch name as
    ``duck/<chatid>/<slug>``; this parses ``<chatid>`` from the PR's head ref so the run
    uses that chat's isolated workspace and the reply goes to that chat. Falls back to
    (0, default_working_directory) -> broadcast when the ref isn't a team branch (e.g.
    legacy branches) or the chat's workspace is missing.
    """
    payload = getattr(event, "payload", None)
    pr = payload.get("pull_request") if isinstance(payload, dict) else None
    ref = ((pr or {}).get("head") or {}).get("ref") or ""
    match = _BRANCH_RE.match(ref)
    if not match:
        return 0, default_working_directory
    chat_id = int(match.group(1))
    root = os.environ.get("APPROVED_DIRECTORY", "/workspace")
    workspace = os.path.join(root, "chat_{}".format(chat_id))
    if not os.path.isdir(workspace):
        return chat_id, default_working_directory
    return chat_id, workspace
