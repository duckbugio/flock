# Self-contained image for the DuckFlock AI dev-team VK (VKontakte) bot (Go).
# A multi-stage build: compile the static Go binary, then assemble a slim runtime
# with Node + the `claude` CLI and the tools the team shells out to.
# Behaviour is toggled at RUNTIME via env (see .env.example).
# Built & published by .github/workflows/publish-vk.yml -> ghcr.io/duckbugio/flock-vk
# Build context is the REPO ROOT (COPY paths are root-relative): docker build -f adapters/vk/Dockerfile .

# --- Build stage: compile the Go binary ---------------------------------------
# Build on the native BUILDPLATFORM and cross-compile to TARGETARCH (CGO off), so
# multi-arch builds don't pay for emulation.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build

WORKDIR /src

# Cache module downloads on go.mod/go.sum alone.
COPY go.mod go.sum ./
RUN go mod download

# Then the sources the binary needs. cmd/duck-vk imports adapters/vk, so adapters/
# MUST be copied alongside cmd/core/internal (a missing adapters/ broke the
# telegram image build before — see the commit history).
COPY cmd/ ./cmd/
COPY core/ ./core/
COPY adapters/ ./adapters/
COPY internal/ ./internal/

ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -buildvcs=false -trimpath -ldflags "-s -w" -o /out/duck-vk ./cmd/duck-vk

# --- Runtime stage ------------------------------------------------------------
# node:24 (current LTS) supplies Node (for the `claude` CLI). Add git/ripgrep/ffmpeg
# (the CLI shells out to them), the GitHub CLI (`gh`, for PR creation on github.com)
# + the Docker CLI (talks to the optional dind sidecar).
FROM node:24-bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive

# chromium is the Debian system browser that backs the `shot` screenshot helper
# (see /opt/shotter below); it is installed in THIS existing apt layer, under
# the SAME single `apt-get update`, on purpose. An extra apt source / a second
# `apt-get update` previously tipped the QEMU-emulated arm64 build over its memory
# limit (OOM), so we do not add one. Version is intentionally unpinned (apt latest).
# `make` ships here too (it lives in the base Debian repos) so the team can invoke a
# project's own Makefile targets; the other dev-experience runners (task, just) come
# from release tarballs below since they are not in the Debian repos.
# The fonts-* packages are required because --no-install-recommends drops chromium's
# font deps, which would render Cyrillic/emoji as tofu boxes — this is a RU-facing
# bot, so Latin + Cyrillic (noto-core) + colour emoji are installed explicitly
# (noto-cjk is deliberately excluded: it is very large and not needed here).
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates curl gnupg git ripgrep ffmpeg chromium make \
        fonts-liberation fonts-noto-core fonts-noto-color-emoji; \
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
# green. The version is PINNED (no GitHub-API `latest` lookup, so builds are
# reproducible) and the downloaded tarball's sha256 is verified against the official
# per-arch checksum BEFORE install — the build fails on any mismatch.
ARG GH_VERSION=2.95.0
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
        amd64) sha="25d1e4729e8808c9ed3d613e96ebd3f3e44446f2d368c89d878a71a36ddb3d8c" ;; \
        arm64) sha="d41e0b3b6218e5741c8bb4db39b16e53a59e0e06299a8489bd38f623ef7ebaae" ;; \
        *) echo "unsupported arch for gh: $arch" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${arch}.tar.gz" -o /tmp/gh.tgz; \
    echo "${sha}  /tmp/gh.tgz" | sha256sum -c -; \
    tar -xzf /tmp/gh.tgz -C /tmp; \
    install -m 0755 "/tmp/gh_${GH_VERSION}_linux_${arch}/bin/gh" /usr/local/bin/gh; \
    rm -rf /tmp/gh.tgz "/tmp/gh_${GH_VERSION}_linux_${arch}"

# Task (go-task) runner so the team can drive a project's own Taskfile.yml targets
# (e.g. `task lint`, `task test`). Installed from the official GitHub release tarball
# for the SAME reason as `gh` above: an apt source for it would add a second
# `apt-get update` that OOMs the QEMU-emulated arm64 build. The tarball ships the
# `task` binary at its root; arch comes from dpkg (amd64/arm64). The version is
# PINNED and the tarball's sha256 is verified against the official per-arch checksum
# BEFORE install — the build fails on any mismatch.
ARG TASK_VERSION=3.51.1
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
        amd64) sha="da7e92f0ff961ef2aae7cfecbad8d1fd2a08d7b09ba968673adf7ff389b243b5" ;; \
        arm64) sha="49c58bb00eff2449a5553f3b3e694fc424e0dc04d5c669d8831126daee1000f8" ;; \
        *) echo "unsupported arch for task: $arch" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://github.com/go-task/task/releases/download/v${TASK_VERSION}/task_linux_${arch}.tar.gz" -o /tmp/task.tgz; \
    echo "${sha}  /tmp/task.tgz" | sha256sum -c -; \
    tar -xzf /tmp/task.tgz -C /tmp task; \
    install -m 0755 /tmp/task /usr/local/bin/task; \
    rm -f /tmp/task.tgz /tmp/task

# just runner so the team can drive a project's own justfile recipes (e.g. `just
# check`). Same tarball-not-apt rationale as `gh`/`task` above (arm64 OOM avoidance).
# just's release assets are named by LLVM target triple, NOT dpkg arch, so we map
# amd64 -> x86_64-unknown-linux-musl and arm64 -> aarch64-unknown-linux-musl; the
# tag carries no leading `v`. The tarball ships the `just` binary at its root. The
# version is PINNED and the tarball's sha256 is verified against the official
# per-arch checksum BEFORE install — the build fails on any mismatch.
ARG JUST_VERSION=1.53.0
RUN set -eux; \
    case "$(dpkg --print-architecture)" in \
        amd64) triple="x86_64-unknown-linux-musl"; \
               sha="7fedeb22c7e14d9ef1551e8b793700866d80f409f9884b0e80ebb65c11d4874d" ;; \
        arm64) triple="aarch64-unknown-linux-musl"; \
               sha="f29d8e72380bc144465f632c7d59da311205eef2923d57511708b05b82f2e64f" ;; \
        *) echo "unsupported arch for just: $(dpkg --print-architecture)" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://github.com/casey/just/releases/download/${JUST_VERSION}/just-${JUST_VERSION}-${triple}.tar.gz" -o /tmp/just.tgz; \
    echo "${sha}  /tmp/just.tgz" | sha256sum -c -; \
    tar -xzf /tmp/just.tgz -C /tmp just; \
    install -m 0755 /tmp/just /usr/local/bin/just; \
    rm -f /tmp/just.tgz /tmp/just

# `shot` screenshot helper: the Node implementation (driving system chromium via
# puppeteer-core) lives at /opt/shotter/shot.mjs; the thin wrapper goes on PATH at
# /usr/local/bin/shot. puppeteer-core is installed as a LOCAL dependency in the
# script's own directory (NOT global) because shot.mjs is an ES module, and ESM
# `import` resolution walks up from the script dir / ignores NODE_PATH — so the
# package must live in /opt/shotter/node_modules to resolve. It is installed UNDER
# /opt/shotter (NOT /opt/duck) on purpose: the later `chown -R claude:claude
# /opt/duck` re-owns the dev-team config to the runtime user, and the tooling must
# NOT be re-owned — keeping it at /opt/shotter leaves it root-owned + world-readable
# (chmod -R a+rX), so the uid 1000 `claude` runtime user can run it but cannot tamper
# with its own tooling. The COPY/npm run as root at build time; chromium runs as the
# unprivileged user at runtime.
COPY tools/shotter/shot.mjs tools/shotter/package.json tools/shotter/package-lock.json /opt/shotter/
COPY tools/shotter/shot /usr/local/bin/shot
RUN set -eux; \
    npm ci --omit=dev --prefix /opt/shotter; \
    npm cache clean --force; \
    chmod 0755 /usr/local/bin/shot; \
    chmod -R a+rX /opt/shotter

# Non-root runtime user; pre-create writable dirs so named volumes inherit uid 1000.
# /app/data is kept for volume-layout parity with the previous image. The node base
# image already occupies uid 1000 (the `node` user), so drop it and re-create `claude`
# at the SAME uid 1000 the existing volumes are owned by.
RUN userdel -r node 2>/dev/null || true; \
    useradd -m -u 1000 -s /bin/bash claude \
 && mkdir -p /workspace /app/data /home/claude/.claude /opt/duck/agents \
 && chown -R claude:claude /workspace /app /home/claude/.claude /opt/duck

# The compiled bot.
COPY --from=build /out/duck-vk /usr/local/bin/duck-vk

# Shared team config — rendered per chat into the workspace by core/workspace at runtime.
COPY --chown=claude:claude core/agents/*.md /opt/duck/agents/
COPY --chown=claude:claude core/CLAUDE.workspace.md.tmpl /opt/duck/CLAUDE.workspace.md.tmpl

USER claude
ENV HOME=/home/claude \
    PATH=/home/claude/.local/bin:$PATH

WORKDIR /app
# The Go binary wires git + renders the workspace itself (no shell entrypoint needed).
ENTRYPOINT ["/usr/local/bin/duck-vk"]
