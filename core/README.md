# Lightweight autonomous dev team (native Claude Code subagents)

A small, **abstract** team that lives **inside the Telegram bot**. You describe a feature in
chat; the bot's Claude (the Lead) plans it, implements it on a branch, tests it, reviews it,
and opens a PR. **No board, no issue queue — the conversation is the task source.** You only
review and merge the PR.

Designed for a **microservices** workspace: a feature can span several services, and the team
coordinates branches + PRs across all of them.

## Roles

| Subagent | Job | Tools |
|----------|-----|-------|
| [`planner`](agents/planner.md) | Spec (≥3 acceptance criteria) + which services are affected + ordered plan + risks | read-only |
| [`coder`](agents/coder.md) | Implements on a branch; contract-safe across services | read + edit + bash |
| [`tester`](agents/tester.md) | Runs/writes tests incl. the cross-service seam; green-build gate | read + edit + bash |
| [`reviewer`](agents/reviewer.md) | Adversarial read-only review + **RISK score** + cross-service checks | read-only |
| [`arbiter`](agents/arbiter.md) | Enforces the gate; decides continue/ship/escalate — the loop-breaker | read-only |

Deployed at the project level (`/workspace/.claude/agents/`) so the bot loads them via
`setting_sources=["project"]`.

## Pipeline (per chat request)

```
you (chat): "implement X across the api + web services"
   → Lead → planner → coder → tester → reviewer → arbiter ─┬ CONTINUE ↺ coder
                                                            ├ SHIP → branch(es) → PR per repo (cross-linked)
                                                            └ ESCALATE → asks you in chat
```

The **arbiter — not the reviewer — decides when the loop stops.** Gate to SHIP: acceptance
criteria met + tests green + no open blocker. You merge the PR(s).

## Borrowed from [ruflo](https://github.com/ruvnet/ruflo) (patterns, not the framework)

- **SPARC** — a spec-first lifecycle with quality gates → the `planner`'s ≥3 acceptance
  criteria and the arbiter's explicit ship-gate.
- **multi-repo-swarm** — cross-service coordination → identify all affected services up
  front, same branch name everywhere, one cross-linked PR per repo.
- **agentic-jujutsu** — diff risk scoring → the reviewer's `RISK` level, which the arbiter
  weighs (higher risk → lean ESCALATE / flag in the PR).

Deliberately left out the heavy machinery (MCP swarm server, vector memory/AgentDB,
webhooks, dashboards) — out of scope for a lightweight, subscription-driven setup.

## Guardrails

- Quality gate BEFORE a PR: acceptance criteria met + tests green + arbiter says SHIP.
- Loop termination via the arbiter (biased to SHIP/ESCALATE) + a hard backstop of 3 cycles.
- Never merge; never touch a default branch directly.
- Cap turns/cost per request; on ESCALATE → ask the user in chat.

## How it runs

The bot itself is the orchestrator (no separate runner). The subagents + a focus/Lead
`CLAUDE.md` are mounted into the bot's workspace by the Ansible role; the bot picks them up
per chat message. For a feature too large for one session, an optional detached mode can run
the pipeline in the background and ping you when the PR is ready (build later if needed).
