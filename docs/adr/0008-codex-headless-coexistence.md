---
status: accepted
---

# ADR 0008: Run Codex through a headless worker seam during terminal migration

## Context

The factory's first harness adapter used a visible terminal because interactive
verification was the original product boundary. Codex also exposes a documented
JSONL execution mode, and a role-agent process does not need a terminal to write
the factory-owned `factory-report`. Keeping all-Codex runs attached to cmux
adds a host process and a recovery topology that unattended execution does not
need.

## Decision

All-Codex repository roles use a factory-owned `HeadlessRuntime`. Its adapter
translates a neutral invocation into `codex exec --json` inside the invocation's
existing pinned Docker worker. The worker launches a detached helper without a
TTY or attached stdin, stores bounded output in the role-home volume, and
returns only logical invocation identity plus a worker-owned lifecycle state.

The adapter persists the native Codex thread identity only from the
machine-readable `thread.started` event. Capacity, authentication, launch,
unexpected-exit, timeout, and cancellation outcomes remain typed adapter
signals. JSON events, stdout, stderr, and model text never become a workflow
result; `factory-report` remains the sole report boundary and all existing
report validation remains coordinator-owned.

Native process state is durable in the role volume. A per-invocation lock makes
fresh launch, cancellation, and exact-session replacement safe across detached
Docker exec calls. Fresh launch is idempotent after response loss; exact-session
resume may replace only a dead exited, cancelled, or lost process; and
cancellation waits through graceful termination and forced escalation before
publishing its terminal state. Recovery therefore distinguishes an active
process, an exited process with a report, and a lost process requiring the
existing bounded resume policy without terminal identities. The worker keeps
head-and-tail output within its inspection bound, and the adapter applies the
factory's typed failure mapping both during launch discovery and after a
terminal process inspection.

The interactive Claude path and explicitly injected legacy runtime remain
temporarily supported. Issue #164 migrates Claude to this seam; issue #165
removes terminal orchestration after that migration and after persisted
interactive invocations have drained.

## Consequences

All-Codex startup diagnosis and normal execution do not require a cmux process
or socket. The worker hardening, role-home isolation, credential projection,
image digest, invocation packet, and report contract are shared with the
interactive path. The temporary compatibility bridge lets existing durable
effect journals finalize or resume a headless session without changing workflow
packages or teaching them Codex protocol.

The worker image carries one additional small helper binary and headless
contract checks become part of Docker-backed diagnosis. The worker build also
runs one offline real-Docker lifecycle verification covering skill roots,
frozen prompts, reports, cancellation, coordinator-process restart inspection,
and native resume. A mixed repository still needs the interactive terminal
until issue #164 and issue #165 complete; the coexistence is deliberate and
bounded by those follow-up issues.
