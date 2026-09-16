# Software Factory

Software Factory is a local, supervised coordinator for turning an explicitly
authorized GitHub issue into a review-ready pull request. It combines a
frozen specification with a host-owned Git workspace, a pinned Docker worker,
headless Codex or Claude Code sessions, deterministic repository gates, and
independent review.

The coordinator is an operator tool, not a hosted service. GitHub remains the
source of issue and pull-request activity, while the coordinator runs locally
and keeps workflow decisions, external projections, and recovery state
explicit. It does not merge pull requests.

This README is an entry point. Detailed contracts intentionally live in the
documents linked below instead of being copied here.

## What you get

At a glance, Factory provides a supervised path from an authorized issue to a
draft pull request, an isolated worker boundary for agent sessions, exact
checkpoint checks, separate specification and documented-standards review, and
restart-safe local operation. The ownership rules for these pieces are in the
[domain context](CONTEXT.md); their implementation contracts are in the
[configuration](docs/configuration.md), [agent runtime](docs/agent-runtime.md),
and [worker runtime](docs/worker-runtime.md) guides.

## Prerequisites

You need a checkout of this repository, Git, a Go toolchain, Docker with a
working daemon, and the GitHub CLI authenticated for the repository. You also
need at least one supported headless harness—Codex or Claude Code—and the
corresponding host authentication source when that harness requires one.

The registered repository must contain a valid
[factory.yaml](factory.yaml). Before using an issue, run the complete startup
diagnosis described in [Configuration and local
operation](docs/configuration.md). It checks the host, repository, GitHub,
worker, harness, authentication, and operational-store prerequisites together.

## Install

From this checkout:

~~~sh
make deps
make build
make install
~~~

The build produces the coordinator, worker report, and headless worker helper.
The install target places the commands in Go's configured binary directory.
Run <code>make check</code> before submitting repository changes. Build and
verify the pinned worker image by following the [worker image
instructions](docs/configuration.md#worker-image-build-and-digest-pinning).

## One issue, start to finish

Use a disposable GitHub repository and issue for a first run. The detailed
commands and safety rules are kept in the canonical documents; this sequence
shows how they fit together.

1. **Prepare the host.** Initialize and register the installation, connect it
   to the target repository, and complete the startup diagnosis. Follow
   [Quick start](docs/configuration.md#quick-start) for the minimal first-run
   sequence, or [Host configuration](docs/configuration.md#host-configuration)
   for the full option set.
2. **Prepare the repository.** Review the checked-in policy, build the worker
   image, and confirm the target issue is authorized. Follow [Checked-in
   repository configuration](docs/configuration.md#checked-in-repository-configuration)
   and [Claiming an issue](docs/configuration.md#claiming-an-issue).
3. **Start supervision.** Start the coordinator or use the deliberate
   one-shot operations described in the [end-to-end
   demonstration](docs/configuration.md#end-to-end-demonstration).
4. **Let the role work.** The selected headless harness works inside the
   isolated worker and submits a structured proposal. The packet, report
   contract, and recovery behavior are documented in [Agent
   runtime](docs/agent-runtime.md#starting-an-invocation).
5. **Continue to the draft.** Follow [Creating the draft pull
   request](docs/configuration.md#creating-the-draft-pull-request) for the
   coordinator-owned checkpoint, gate, and pull-request procedure.
6. **Review and finish.** Read the independent review and repair behavior in
   [Concurrent isolated reviews](docs/agent-runtime.md#concurrent-isolated-reviews),
   then inspect and merge the pull request as a human. Retain or remove local
   run artifacts using [Run-artifact
   cleanup](docs/configuration.md#run-artifact-cleanup).

For an uninterrupted walkthrough with a disposable issue, use the
[canonical end-to-end demonstration](docs/configuration.md#end-to-end-demonstration).

## Where the contracts live

Each operational contract has one owner:

| Topic | Canonical document |
| --- | --- |
| Host and repository configuration, issue operations, polling, GitHub commands, cleanup, and reset | [Configuration and local operation](docs/configuration.md) |
| Harness adapters, invocations, structured reports, authentication, recovery, and reviews | [Agent runtime](docs/agent-runtime.md) |
| Worker operations, stable paths, mounts, process state, and isolation | [Worker runtime](docs/worker-runtime.md) |
| Terms, invariants, ownership, and lifecycle model | [Domain context](CONTEXT.md) |
| Component relationships and architecture overview | [Architecture visualization](docs/architecture.html) |

The architecture page is a visual companion to the text contracts. Architecture
decisions and their rationale are recorded in [docs/adr](docs/adr/).

## Development

The project is a Go module. The standard local verification command is:

~~~sh
make check
~~~

The available Make targets cover dependency setup, formatting, static
analysis, tests, builds, installation, and worker-image verification. Read
the [Makefile](Makefile) for the complete target list.
