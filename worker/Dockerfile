# Repository worker image. Keep the base image reference versioned and build
# it locally before this definition when the factory is evaluated offline.

ARG FACTORY_BASE_IMAGE=ghcr.io/stevie1704/sw-factory-base:v1
ARG GO_VERSION=1.25.0

# The Go toolchain stage follows the final image platform. Copying the
# toolchain avoids adding a second runtime base image to the worker.
FROM golang:${GO_VERSION}-bookworm AS go-toolchain

FROM ${FACTORY_BASE_IMAGE}

ARG GO_VERSION=1.25.0

USER root

LABEL org.opencontainers.image.title="Software Factory Go worker" \
      org.opencontainers.image.version="1" \
      org.opencontainers.image.description="Software Factory worker with the repository Go toolchain" \
      org.opencontainers.image.go-version="${GO_VERSION}"

COPY --from=go-toolchain /usr/local/go /usr/local/go

# The worker's fixed PATH deliberately excludes /usr/local/go/bin. Expose the
# required Go commands at a stable PATH location instead of depending on a
# login profile or image-specific PATH mutation.
RUN ln -s /usr/local/go/bin/go /usr/local/bin/go \
    && ln -s /usr/local/go/bin/gofmt /usr/local/bin/gofmt

# Both harnesses read a skill set from their role home, and the role home is a
# worker-owned volume seeded from this image. Baking the curated set keeps the
# agent-visible instructions pinned by the worker image digest instead of
# leaking a personal host skill directory into a run. worker/SKILLS.md records
# the provenance and the curation rule.
COPY --chown=factory:factory skills /home/factory/.codex/skills
COPY --chown=factory:factory skills /home/factory/.claude/skills

USER factory
