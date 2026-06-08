# Self-contained image for the DuckFlock AI dev-team Telegram bot (Go rewrite).
# A multi-stage build: compile the static Go binary, then assemble a slim runtime
# with Node + the `claude` CLI and the tools the team shells out to.
# Behaviour is toggled at RUNTIME via env (see .env.example).
# Built & published by .github/workflows/publish-telegram.yml -> ghcr.io/duckbugio/flock-telegram
# Build context is the REPO ROOT (COPY paths are root-relative): docker build -f adapters/telegram/Dockerfile .
#
# The previous Python image is preserved as Dockerfile.python for rollback.

# --- Build stage: compile the Go binary ---------------------------------------
# Build on the native BUILDPLATFORM and cross-compile to TARGETARCH (CGO off), so
# multi-arch builds don't pay for emulation.
FROM --platform=$BUILDPLATFORM golang:1.23-bookworm AS build

WORKDIR /src

# Cache module downloads on go.mod/go.sum alone.
COPY go.mod go.sum ./
RUN go mod download

# Then the sources the binary needs.
COPY cmd/ ./cmd/
COPY core/ ./core/
COPY internal/ ./internal/

ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -buildvcs=false -trimpath -ldflags "-s -w" -o /out/flock-telegram ./cmd/flock-telegram

# --- Runtime stage ------------------------------------------------------------
# node:22 supplies Node (for the `claude` CLI). Add git/ripgrep/ffmpeg (the CLI
# shells out to them), the GitHub CLI (`gh`, for PR creation on github.com) + the
# Docker CLI (talks to the optional dind sidecar).
FROM node:22-bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates curl gnupg git ripgrep ffmpeg; \
    install -m 0755 -d /etc/apt/keyrings; \
    curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc; \
    chmod a+r /etc/apt/keyrings/docker.asc; \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian $(. /etc/os-release && echo $VERSION_CODENAME) stable" > /etc/apt/sources.list.d/docker.list; \
    curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /etc/apt/keyrings/githubcli-archive-keyring.gpg; \
    chmod 0644 /etc/apt/keyrings/githubcli-archive-keyring.gpg; \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends docker-ce-cli docker-compose-plugin gh; \
    npm install -g @anthropic-ai/claude-code; \
    npm cache clean --force; \
    apt-get clean; rm -rf /var/lib/apt/lists/*

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
