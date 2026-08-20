# Spike worker image for issue #2. Throwaway exploration, not product code.
#
# PID 1 is `sleep infinity`, so every harness runs under `docker exec -it`.
# Ctrl+C then reaches the harness and never the container's init process.
FROM node:22-bookworm-slim

ARG CLAUDE_VERSION=2.1.232
ARG CODEX_VERSION=0.148.0

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      git \
      jq \
      less \
      procps \
      ripgrep \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g \
      "@anthropic-ai/claude-code@${CLAUDE_VERSION}" \
      "@openai/codex@${CODEX_VERSION}" \
    && npm cache clean --force

# The spec requires the worker to set a compatible terminal type explicitly.
ENV TERM=xterm-256color \
    COLORTERM=truecolor \
    LANG=C.UTF-8 \
    LC_ALL=C.UTF-8

RUN useradd --create-home --uid 10001 --shell /bin/bash factory \
    && mkdir -p /home/factory/.claude /home/factory/.codex /work /results \
    && chown -R factory:factory /home/factory /work /results

COPY bin/factory-report /usr/local/bin/factory-report
RUN chmod 0755 /usr/local/bin/factory-report

USER factory
WORKDIR /work

ENV HOME=/home/factory \
    FACTORY_RESULT_DIR=/results

CMD ["sleep", "infinity"]
