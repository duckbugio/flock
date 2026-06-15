# Self-contained image for the DuckFlock AI dev-team Telegram bot (Go rewrite).
# A multi-stage build: compile the static Go binary, then assemble a slim runtime
# with Node + the `claude` CLI and the tools the team shells out to.
# Behaviour is toggled at RUNTIME via env (see .env.example).
# Built & published by .github/workflows/publish-telegram.yml -> ghcr.io/duckbugio/flock-telegram
# Build context is the REPO ROOT (COPY paths are root-relative): docker build -f adapters/telegram/Dockerfile .

# --- Build stage: compile the Go binary ---------------------------------------
# Build on the native BUILDPLATFORM and cross-compile to TARGETARCH (CGO off), so
# multi-arch builds don't pay for emulation.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build

WORKDIR /src

# Cache module downloads on go.mod/go.sum alone.
COPY go.mod go.sum ./
RUN go mod download

# Then the sources the binary needs.
COPY cmd/ ./cmd/
COPY core/ ./core/
COPY adapters/ ./adapters/
COPY internal/ ./internal/

ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -buildvcs=false -trimpath -ldflags "-s -w" -o /out/flock-telegram ./cmd/flock-telegram

# --- Runtime stage ------------------------------------------------------------
# node:24 (current LTS) supplies Node (for the `claude` CLI). Add git/ripgrep/ffmpeg
# (the CLI shells out to them), the GitHub CLI (`gh`, for PR creation on github.com)
# + the Docker CLI (talks to the optional dind sidecar).
FROM node:24-bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive

# chromium is the Debian system browser that backs the `shot` screenshot helper
# (see /opt/duck/shotter below); it is installed in THIS existing apt layer, under
# the SAME single `apt-get update`, on purpose. An extra apt source / a second
# `apt-get update` previously tipped the QEMU-emulated arm64 build over its memory
# limit (OOM), so we do not add one. Version is intentionally unpinned (apt latest).
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates curl gnupg git ripgrep ffmpeg chromium; \
    install -m 0755 -d /etc/apt/keyrings; \
    curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc; \
    chmod a+r /etc/apt/keyrings/docker.asc; \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian $(. /etc/os-release && echo $VERSION_CODENAME) stable" > /etc/apt/sources.list.d/docker.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends docker-ce-cli docker-compose-plugin; \
    npm install -g @anthropic-ai/claude-code; \
    npm cache clean --force; \
    apt-get clean; rm -rf /var/lib/apt/lists/*

# GitHub CLI (gh) for PR creation on github.com — installed from the official release
# tarball, NOT the apt repo: that extra apt source tipped the QEMU-emulated arm64
# build over its memory limit during `apt-get update` (OOM: "Cannot allocate
# memory"). One arch-matched binary is far lighter and keeps the multi-arch build
# green. Uses the latest release, falling back to a pinned version if the GitHub API
# is unreachable/rate-limited at build time.
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    ver="$(curl -fsSL https://api.github.com/repos/cli/cli/releases/latest 2>/dev/null | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p')"; \
    [ -n "$ver" ] || ver="2.45.0"; \
    curl -fsSL "https://github.com/cli/cli/releases/download/v${ver}/gh_${ver}_linux_${arch}.tar.gz" -o /tmp/gh.tgz; \
    tar -xzf /tmp/gh.tgz -C /tmp; \
    install -m 0755 "/tmp/gh_${ver}_linux_${arch}/bin/gh" /usr/local/bin/gh; \
    rm -rf /tmp/gh.tgz "/tmp/gh_${ver}_linux_${arch}"

# `shot` screenshot helper: the Node implementation (driving system chromium via
# puppeteer-core) lives at /opt/duck/shotter/shot.mjs; the thin wrapper goes on
# PATH at /usr/local/bin/shot. puppeteer-core is installed as a LOCAL dependency in
# the script's own directory (NOT global) because shot.mjs is an ES module, and ESM
# `import` resolution walks up from the script dir / ignores NODE_PATH — so the
# package must live in /opt/duck/shotter/node_modules to resolve. Everything is
# root-owned + world-readable/executable so the uid 1000 `claude` runtime user can
# run it as a system tool. The COPY/npm run as root at build time; chromium runs as
# the unprivileged user at runtime.
COPY tools/shotter/shot.mjs tools/shotter/package.json tools/shotter/package-lock.json /opt/duck/shotter/
COPY tools/shotter/shot /usr/local/bin/shot
RUN set -eux; \
    npm install --omit=dev --prefix /opt/duck/shotter; \
    npm cache clean --force; \
    chmod 0755 /usr/local/bin/shot; \
    chmod 0644 /opt/duck/shotter/shot.mjs; \
    chmod -R a+rX /opt/duck/shotter

# Non-root runtime user; pre-create writable dirs so named volumes inherit uid 1000.
# /app/data is kept for volume-layout parity with the previous image. The node base
# image already occupies uid 1000 (the `node` user), so drop it and re-create `claude`
# at the SAME uid 1000 the existing volumes are owned by.
RUN userdel -r node 2>/dev/null || true; \
    useradd -m -u 1000 -s /bin/bash claude \
 && mkdir -p /workspace /app/data /home/claude/.claude /opt/duck/agents \
 && chown -R claude:claude /workspace /app /home/claude/.claude /opt/duck

# The compiled bot.
COPY --from=build /out/flock-telegram /usr/local/bin/flock-telegram

# Shared team config — rendered per chat into the workspace by core/workspace at runtime.
COPY --chown=claude:claude core/agents/*.md /opt/duck/agents/
COPY --chown=claude:claude core/CLAUDE.workspace.md.tmpl /opt/duck/CLAUDE.workspace.md.tmpl

USER claude
ENV HOME=/home/claude \
    PATH=/home/claude/.local/bin:$PATH

WORKDIR /app
# The Go binary wires git + renders the workspace itself (no shell entrypoint needed).
ENTRYPOINT ["/usr/local/bin/flock-telegram"]
