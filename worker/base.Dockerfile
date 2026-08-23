# Versioned factory worker base image. The repository worker definition adds
# only the project toolchain that its gates require.

ARG GO_VERSION=1.25.0

# Building the report in a stage that has the target platform selected makes
# the copied binary native to the image platform, including under buildx.
FROM golang:${GO_VERSION}-bookworm AS factory-report-builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /factory-report ./cmd/factory-report

FROM node:22-bookworm-slim

ARG CLAUDE_VERSION=2.1.232
ARG CODEX_VERSION=0.148.0
ARG FACTORY_BASE_VERSION=1

LABEL org.opencontainers.image.title="Software Factory worker base" \
      org.opencontainers.image.version="${FACTORY_BASE_VERSION}" \
      org.opencontainers.image.description="Pinned harnesses and factory reporting command for supervised workers"

RUN apt-get update && apt-get install -y --no-install-recommends \
      bash \
      ca-certificates \
      curl \
      git \
      jq \
      less \
      procps \
      ripgrep \
    && rm -rf /var/lib/apt/lists/*

# Exact package versions are build arguments so the base image can be
# reproduced without silently adopting a newer harness release.
RUN npm install --global \
      "@anthropic-ai/claude-code@${CLAUDE_VERSION}" \
      "@openai/codex@${CODEX_VERSION}" \
    && npm cache clean --force

# The npm shim always marks Codex as npm-managed, so its TUI opens an
# interactive "Update now (runs `npm install -g @openai/codex`)" prompt on
# startup. A worker must never mutate its own pinned toolchain, and the prompt
# also consumes the first keystrokes sent to the harness. Expose the vendored
# native binary instead of the shim.
RUN set -eu; \
    native="$(find /usr/local/lib/node_modules/@openai/codex -type f -path '*/vendor/*/bin/codex' | head -n 1)"; \
    test -n "$native"; \
    ln -sf "$native" /usr/local/bin/codex; \
    /usr/local/bin/codex --version

COPY --from=factory-report-builder /factory-report /usr/local/bin/factory-report

# The worker adapter supplies this same PATH with env -i. Keep every command
# used by setup, gates, and harnesses on it without relying on a profile.
ENV HOME=/home/factory \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    TERM=xterm-256color \
    COLORTERM=truecolor \
    LANG=C.UTF-8 \
    LC_ALL=C.UTF-8

# The run worktree is always mounted at the same stable path, and the harness
# otherwise opens an interactive trust prompt that consumes the coordinator's
# prompt instead of the composer.
RUN useradd --create-home --uid 10001 --user-group --shell /bin/bash factory \
    && mkdir -p /home/factory/.claude /home/factory/.codex \
       /work /git /cache /invocation /results /run/factory-auth \
    && printf '[projects."/work"]\ntrust_level = "trusted"\n' > /home/factory/.codex/config.toml \
    && chown -R factory:factory /home/factory /work /git /cache /invocation /results /run/factory-auth \
    && chown root:root /run/factory-auth \
    && chmod 0755 /run/factory-auth \
    && chmod 0755 /usr/local/bin/factory-report

USER factory
WORKDIR /work

CMD ["sleep", "infinity"]
