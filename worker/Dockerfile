# Repository worker image: the Go project image of Software Factory. Keep the
# base image reference versioned and build it locally before this definition
# when the factory is evaluated offline.
#
# The worker base images carry no project toolchain. This definition adds the
# curated worker skills (target worker-skills) and then the Go toolchain that
# this repository's setup and gates require (the default target). The default
# base is the combined image, because this repository verifies every harness;
# a repository that selects one harness can build on its single-harness
# variant instead.

ARG FACTORY_BASE_IMAGE=ghcr.io/stevie1704/sw-factory-base:v1
ARG GO_VERSION=1.27.4

# The Go toolchain stage follows the final image platform. Copying the
# toolchain avoids adding a second runtime base image to the worker.
FROM golang:${GO_VERSION}-bookworm AS go-toolchain

FROM ${FACTORY_BASE_IMAGE} AS worker-skills

# Every harness reads a skill set from its role home, and the role home is a
# worker-owned volume seeded from this image. Baking the curated set keeps the
# agent-visible instructions pinned by the worker image digest instead of
# leaking a personal host skill directory into a run. worker/SKILLS.md records
# the provenance and the curation rule. The Copilot root is its personal skill
# directory under the private Copilot home. Every base variant has all three
# homes; a home without its harness stays unused.
COPY --chown=factory:factory skills /home/factory/.codex/skills
COPY --chown=factory:factory skills /home/factory/.claude/skills
COPY --chown=factory:factory skills /home/factory/.copilot/skills

FROM worker-skills AS go-worker

ARG GO_VERSION=1.27.4

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

USER factory
