# Versioned factory worker base images. One definition builds four variants
# that share the minimal runtime layers and differ only in the harness layers:
#
#   --target harness-codex    Codex only
#   --target harness-claude   Claude Code only
#   --target harness-copilot  GitHub Copilot CLI only
#   --target harness-all      all three harnesses (the default target)
#
# The final images carry no Node, npm, Yarn, or Go toolchain. npm runs only in
# the install stages, which resolve the pinned packages and their platform
# packages; only the native harness executables, their package manifests, and
# their licenses reach a variant. A repository worker definition adds the
# project toolchain that its gates require (worker/Dockerfile adds Go).

ARG GO_VERSION=1.27.4

# Building the report in a stage that has the target platform selected makes
# the copied binary native to the image platform, including under buildx.
FROM golang:${GO_VERSION}-bookworm AS factory-report-builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /factory-report ./cmd/factory-report \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /factory-worker-headless ./cmd/factory-worker-headless

# One npm installation per harness, so a single-harness variant never depends
# on the packages of another harness. Exact package versions are build
# arguments so the images can be reproduced without silently adopting a newer
# harness release. npm itself is pinned too: it selects the platform package
# for the C library of this stage. No install script runs: each stage copies
# the native executable straight from the platform package.
FROM node:22-bookworm-slim AS harness-installer
ARG NPM_VERSION=12.2.2
RUN npm install --global "npm@${NPM_VERSION}" && npm cache clean --force

# Each harness stage stages its files below /harness at their final paths:
# /opt/factory/harnesses/<harness>/package holds the manifest and the license
# file of the published package (Codex ships none and declares its license in
# the manifest), .../native the complete platform package with the native
# executable, and /usr/local/bin the command link. The manifests stay so the
# image scan still identifies each harness release.
FROM harness-installer AS claude-package
ARG CLAUDE_VERSION=2.1.325
RUN set -eu; \
    npm install --global --ignore-scripts "@anthropic-ai/claude-code@${CLAUDE_VERSION}"; \
    package=/usr/local/lib/node_modules/@anthropic-ai/claude-code; \
    set -- "$package"/node_modules/@anthropic-ai/claude-code-linux-*; \
    test "$#" -eq 1 && test -x "$1/claude"; \
    target=/harness/opt/factory/harnesses/claude-code; \
    mkdir -p "$target/package" "$target/native" /harness/usr/local/bin; \
    cp "$package/package.json" "$package/LICENSE.md" "$package/README.md" "$target/package/"; \
    cp -a "$1/." "$target/native/"; \
    ln -s /opt/factory/harnesses/claude-code/native/claude /harness/usr/local/bin/claude

# The npm shim of Codex marks it as npm-managed, so its CLI opens an
# interactive "Update now" prompt on startup. A worker must never mutate its
# own pinned toolchain, and the prompt also consumes the first keystrokes sent
# to the harness. The link names the vendored native binary instead, which
# finds its bundled ripgrep and resources beside it.
FROM harness-installer AS codex-package
ARG CODEX_VERSION=0.148.0
RUN set -eu; \
    npm install --global --ignore-scripts "@openai/codex@${CODEX_VERSION}"; \
    package=/usr/local/lib/node_modules/@openai/codex; \
    set -- "$package"/node_modules/@openai/codex-linux-*; \
    test "$#" -eq 1; \
    target=/harness/opt/factory/harnesses/codex; \
    mkdir -p "$target/package" "$target/native" /harness/usr/local/bin; \
    cp "$package/package.json" "$package/README.md" "$target/package/"; \
    cp -a "$1/." "$target/native/"; \
    set -- "$target"/native/vendor/*/bin/codex; \
    test "$#" -eq 1 && test -x "$1"; \
    ln -s "${1#/harness}" /harness/usr/local/bin/codex

# The npm loader of the Copilot package needs Node; the link names the native
# executable it would start.
#
# The Copilot CLI is started only through factory-copilot, which projects the
# dedicated token into its own process and restores the image-owned user
# settings before every launch (docs/worker-runtime.md). The pinned CLI only
# reports new releases; autoUpdate=false in the settings and
# COPILOT_AUTO_UPDATE=false keep it from downloading one. The gh CLI is
# deliberately absent: its login would be a fallback Copilot credential.
FROM harness-installer AS copilot-package
COPY worker/factory-copilot /harness/usr/local/bin/factory-copilot
COPY worker/copilot-settings.json /harness/usr/local/share/factory-copilot/settings.json
# The release docs/spikes/0003-copilot-cli-protocol.md proved. Change it only
# together with a new compatibility record.
ARG COPILOT_VERSION=1.0.98
RUN set -eu; \
    npm install --global --ignore-scripts "@github/copilot@${COPILOT_VERSION}"; \
    package=/usr/local/lib/node_modules/@github/copilot; \
    set -- "$package"/node_modules/@github/copilot-linux-*; \
    test "$#" -eq 1 && test -x "$1/copilot"; \
    target=/harness/opt/factory/harnesses/copilot; \
    mkdir -p "$target/package" "$target/native" /harness/usr/local/bin; \
    cp "$package/package.json" "$package/LICENSE.md" "$package/README.md" "$target/package/"; \
    cp -a "$1/." "$target/native/"; \
    ln -s /opt/factory/harnesses/copilot/native/copilot /harness/usr/local/bin/copilot; \
    chmod 0755 /harness/usr/local/bin/factory-copilot; \
    chmod 0444 /harness/usr/local/share/factory-copilot/settings.json

# The shared minimal runtime of every variant. It must use the C library of
# the install stage (Debian bookworm, glibc), because npm selects the glibc
# platform packages there.
FROM debian:bookworm-slim AS runtime

ARG FACTORY_BASE_VERSION=1
# The Factory release this base belongs to. A release coordinator refuses a
# worker image whose inherited label names another release
# (internal/worker/release.go); a local build is a development base.
ARG FACTORY_RELEASE=development

LABEL org.opencontainers.image.title="Software Factory worker base" \
      org.opencontainers.image.version="${FACTORY_BASE_VERSION}" \
      org.opencontainers.image.description="Pinned harnesses and factory reporting command for supervised workers" \
      io.github.stevie1704.sw-factory.release="${FACTORY_RELEASE}"

# Upgrading first applies Debian security fixes that the base image does not
# carry yet. The worker needs bash (factory-copilot), Git, jq and base64
# (headless state), and procps; the agents use ripgrep, less, and curl. The
# native harness executables need only the C library of the base image.
RUN apt-get update && apt-get upgrade -y --no-install-recommends \
    && apt-get install -y --no-install-recommends \
      bash \
      ca-certificates \
      curl \
      git \
      jq \
      less \
      procps \
      ripgrep \
    && rm -rf /var/lib/apt/lists/*

COPY --from=factory-report-builder /factory-report /usr/local/bin/factory-report
COPY --from=factory-report-builder /factory-worker-headless /usr/local/bin/factory-worker-headless

# The worker adapter supplies this same PATH with env -i. Keep every command
# used by setup, gates, and harnesses on it without relying on a profile.
ENV HOME=/home/factory \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    TERM=xterm-256color \
    COLORTERM=truecolor \
    LANG=C.UTF-8 \
    LC_ALL=C.UTF-8

# Every harness home exists in every variant, so a repository worker
# definition bakes the curated skills the same way on each variant. A home
# without its harness stays unused.
RUN useradd --create-home --uid 10001 --user-group --shell /bin/bash factory \
    && mkdir -p /home/factory/.claude /home/factory/.codex /home/factory/.copilot \
       /work /git /cache /invocation /results /run/factory-auth \
    && chown -R factory:factory /home/factory /work /git /cache /invocation /results /run/factory-auth \
    && chown root:root /run/factory-auth \
    && chmod 0755 /run/factory-auth \
    && chmod 0700 /home/factory/.copilot \
    && chmod 0755 /usr/local/bin/factory-report /usr/local/bin/factory-worker-headless

USER factory
WORKDIR /work

CMD ["sleep", "infinity"]

# Harness layers. Each variant copies exactly the staged files of its
# harnesses onto the shared runtime, and its harnesses label names them. The
# combined variant reuses the Codex variant and the same harness layers, so a
# registry stores each harness layer once.

FROM runtime AS harness-codex
USER root
COPY --from=codex-package /harness/ /
# The run worktree is always mounted at the same stable path, and the harness
# otherwise opens an interactive trust prompt that consumes the coordinator's
# prompt instead of the composer.
RUN printf '[projects."/work"]\ntrust_level = "trusted"\n' > /home/factory/.codex/config.toml \
    && chown factory:factory /home/factory/.codex/config.toml
LABEL io.github.stevie1704.sw-factory.harnesses="codex"
USER factory

FROM runtime AS harness-claude
COPY --from=claude-package /harness/ /
LABEL io.github.stevie1704.sw-factory.harnesses="claude"

FROM runtime AS harness-copilot
COPY --from=copilot-package /harness/ /
LABEL io.github.stevie1704.sw-factory.harnesses="copilot"

FROM harness-codex AS harness-all
COPY --from=claude-package /harness/ /
COPY --from=copilot-package /harness/ /
LABEL io.github.stevie1704.sw-factory.harnesses="claude codex copilot"
