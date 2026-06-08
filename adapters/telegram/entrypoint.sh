#!/bin/sh
# Runtime setup for the DuckFlock bot image: wire git auth/identity from env and render
# the shared team config into the workspace, then start the bot. Everything optional is
# env-driven — with no git/voice/webhook env set, the bot still runs as a plain assistant.
set -u

# --- Git auth + identity (only when a token is provided) ---
if [ -n "${GIT_TOKEN:-}" ] && [ -n "${GIT_HOST:-}" ]; then
    git config --global "credential.${GIT_SCHEME:-https}://${GIT_HOST}.helper" gitea
fi
git config --global user.name "${GIT_AUTHOR_NAME:-AI Team}"
git config --global user.email "${GIT_AUTHOR_EMAIL:-ai@example.com}"
git config --global init.defaultBranch main

# --- Render the shared CLAUDE.md + subagents into the workspace (best-effort) ---
WORKSPACE="${APPROVED_DIRECTORY:-/workspace}"
export PRE_PR_CYCLES="${PRE_PR_CYCLES:-5}"
export PR_REVIEW_CYCLES="${PR_REVIEW_CYCLES:-10}"
export ENABLE_PR_REVIEW="${ENABLE_PR_REVIEW:-false}"
mkdir -p "${WORKSPACE}/.claude/agents" 2>/dev/null || true
# Drop a possibly stale/root-owned stub (left by older read-only bind-mount setups) so the
# redirect can recreate it as us. Never crash-loop on a workspace permission issue.
rm -f "${WORKSPACE}/CLAUDE.md" 2>/dev/null || true
if ! envsubst '${PRE_PR_CYCLES} ${PR_REVIEW_CYCLES} ${ENABLE_PR_REVIEW} ${GIT_HOST}' \
        < /opt/duck/CLAUDE.workspace.md.tmpl > "${WORKSPACE}/CLAUDE.md" 2>/dev/null; then
    echo "entrypoint: WARNING — could not write ${WORKSPACE}/CLAUDE.md (check volume ownership)" >&2
fi
cp -f /opt/duck/agents/*.md "${WORKSPACE}/.claude/agents/" 2>/dev/null \
    || echo "entrypoint: WARNING — could not refresh ${WORKSPACE}/.claude/agents (check volume ownership)" >&2

exec "$@"
