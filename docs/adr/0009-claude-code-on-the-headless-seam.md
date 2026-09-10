---
status: accepted
---

# ADR 0009: Run Claude Code on the headless seam and make terminal-free execution harness-neutral

## Context

ADR 0008 moved Codex onto a factory-owned `HeadlessRuntime` and left Claude Code
on the terminal-backed path. The coordinator therefore decided terminal-free
mode by naming Codex: dispatch, startup diagnosis, operator notification, and
restart recovery each compared the selected harness against one constant. That
made headless execution look like a Codex property rather than a factory
capability, and it kept cmux a prerequisite for every repository that selected
Claude Code for any role.

Claude Code exposes a documented non-interactive mode that fits the same seam.
It also differs from Codex in ways the seam must not absorb: it accepts a
caller-supplied session identifier instead of publishing a discovered one, and
it names API failures with a stable machine category rather than a failure
event code.

## Decision

Claude Code implements `HeadlessRuntime`. The adapter runs
`claude -p --output-format stream-json --verbose` inside the invocation's
existing pinned Docker worker, with the same detached, TTY-free, stdin-free
launch, the same private role home, the same strict empty MCP configuration,
the same disabled self-update, and the same separately seeded credential
material as the interactive Claude path it replaces. No Claude SDK, CLI tool
loop, file operation, or shell command runs on the coordinator host.

The adapter owns its native session protocol. A fresh launch assigns
`--session-id` and reads the `system`/`init` event to learn the identity the
process actually holds; it deliberately does not demand its freshly generated
identifier back, because the worker treats a fresh start as idempotent and a
replay after response loss must adopt the surviving process instead of
cancelling it. A resume passes `--resume` and refuses any identity other than
the recorded one, so a forked conversation cannot silently replace an
invocation's session.

Failure classification reads only machine categories. The `error` field on an
`api_retry` system event or on an API-error message names the category. Because
Claude Code retries a recoverable API failure before it gives up, an observed
category is discarded when the run finished successfully: a final `result` event
that reports neither `is_error` nor an `error` subtype clears it. A category
survives a failed result, and it also survives a process that died before any
result, where the last named category is the best evidence the factory has. Result text, stderr prose, and model output are never a category and
never a workflow result; `factory-report` remains the only accepted result.

Terminal-free mode is now decided by capability, not by harness name. A
repository qualifies when it declares at least one role and every declared role
selects a harness for which a headless adapter exists. Startup diagnosis,
lifecycle dispatch, operator notification, and restart recovery share that one
definition, so a mixed per-role Codex and Claude selection runs through the same
workflow path.

The factory keeps Claude Code on the CLI's own seeded credentials rather than an
SDK or API-key integration. The worker's explicit environment carries only
non-secret coordinator protocol values, and the CLI's API-key path (`--bare`)
also drops the curated worker skill set that role prompts mandate.

## Consequences

Codex and Claude Code are two real production adapters behind one terminal-free
interface, which is what proves headless execution is a factory capability. A
repository selecting either harness, in any per-role mix, starts, recovers, and
is diagnosed without cmux; `factory attach` refuses a headless invocation, and a
manually resumed headless session needs no attach gate.

The shared lifecycle now lives in one adapter-neutral core, and each adapter
contributes only its command translation, its session event, and its
classification. The real-Docker worker verification exercises that core once per
production adapter.

An expired Claude OAuth credential remains an operator event repaired with
`factory auth refresh`, and the documented limitation around mutable Claude
project guidance is unchanged: a headless run still auto-discovers `CLAUDE.md`
from the mounted worktree, and it also runs worktree-declared hooks without a
trust prompt, bounded by the isolated worker rather than by the harness.

ADR 0008's terminal coexistence is now bounded only by issue #165, which can
remove interactive terminal orchestration once no persisted invocation needs it.
