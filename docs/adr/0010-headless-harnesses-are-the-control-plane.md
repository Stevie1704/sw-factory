---
status: accepted
---

# ADR 0010: Headless harnesses are the control plane

## Context

Codex and Claude Code now run through the worker-owned headless process seam.
Their native session identifiers, bounded process state, and authoritative
`factory-report` results provide every lifecycle observation the coordinator
needs. GitHub already provides the durable human supervision surface.

The former terminal topology duplicated those identities in host
configuration, SQLite, recovery, cleanup, and notification paths. It also made
an attached desktop client an accidental availability dependency even though
screen contents were never a correctness protocol.

## Decision

Every production harness runs as a detached process inside its isolated Docker
worker. The coordinator launches, inspects, resumes, cancels, and finishes that
process through `HeadlessRuntime`; it persists only the harness name and opaque
native session identity. Structured reports remain the sole role-result
protocol, and bounded native output remains local diagnostic evidence only.

GitHub issues, comments, labels, reviews, commit statuses, and pull-request
state are the durable operator surfaces. The product has no terminal runtime,
workspace or pane identifiers, attach command, PTY helper, control socket, or
local notification delivery requirement. Optional observation can be designed
later from a concrete user story without entering workflow correctness.

Host configuration advances to schema version 2 and contains repository
registration, GitHub identity, polling, credential sources, repository policy
location, and operational-store location. Repository configuration retains its
independent schema version 1 authority. A version 1 host registration is not
migrated live: operators must use the previous binary to finish or cancel all
non-terminal runs, stop that coordinator, then re-register with the new binary.

## Consequences

Recovery compares SQLite with Docker worker/mount identity, headless process
state, native session identity, report presence, Git/worktree state, and GitHub
projections. Cleanup and reset remove those owned resources directly. Startup
diagnosis checks configuration, repository and GitHub access, Docker isolation,
selected harness protocols and executables, credentials, skills, and SQLite.

The Docker isolation contract is unchanged: pinned images, non-root execution,
dropped capabilities, `no-new-privileges`, explicit mounts, and separate role
and credential volumes remain mandatory.

This decision supersedes ADR 0004, ADR 0008, and ADR 0009. It also supersedes
the tmux proposal in issue #87 and the visible-terminal requirements in parent
specification #1.
