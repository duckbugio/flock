# Security Policy

Flock runs an AI agent with shell and file-edit access inside a per-chat
container, gated by an allow-list. We take its security seriously and appreciate
responsible disclosure.

## Supported versions

Flock ships from `main` (trunk-based); there are no long-lived release branches.
Security fixes land on `main` and are delivered via the published container
images. Always run the latest image.

## Reporting a vulnerability

**Please do not report security vulnerabilities through public GitHub issues,
pull requests, or discussions.**

Instead, use one of these private channels:

1. **Email**: <security@duckbug.io>.
2. **GitHub private vulnerability reporting** (once enabled on this repository):
   the **Security** tab → **Report a vulnerability** opens a private advisory
   visible only to you and the maintainers.

Please include, as far as you can:

- the affected component (adapter, `core/...` package, deployment/Ansible, etc.)
  and version/image tag or commit,
- a description of the issue and its impact,
- step-by-step reproduction (a proof of concept helps),
- any suggested remediation.

## What to expect

- We will acknowledge your report within **5 business days**.
- We will keep you informed as we investigate and work on a fix.
- We will credit you in the advisory once a fix is released, unless you prefer
  to remain anonymous.

Please give us a reasonable window to release a fix before any public
disclosure. We are a volunteer-driven open-source project and will work with you
in good faith.

## Scope and hardening

Some risks are operator configuration rather than code vulnerabilities. Before
reporting, please review the **Security** section of the [README](README.md):

- The bot grants shell/edit access to its host container — never leave
  `ALLOWED_USERS` / `VK_ALLOWED_USERS` empty.
- The git token is shared across a deployment; scope it to only what it needs.
- Keep secrets in a gitignored `.env` (or an encrypted Ansible vault), never in
  a tracked file.

Reports about a misconfigured deployment (e.g. an empty allow-list) are not
considered vulnerabilities in Flock itself, but we welcome documentation
improvements that make safe defaults clearer.
