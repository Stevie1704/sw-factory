# Visible agent runtime

Issue #6 adds the visible implementation-agent boundary. The coordinator owns
the run, invocation identity, stage, policy, and report decision. Codex is an
interactive proposal-maker; it does not own GitHub mutations, Git history, or
workflow transitions.

## Starting an implementation invocation

After an issue has been claimed, start the visible Codex session with:

```sh
factory agent \
  --config /Users/me/.config/factory/config.yaml \
  --run-id run-123 \
  --codex-auth /Users/me/.codex/auth.json
```

The command creates or reuses the control workspace and creates one run
workspace with an implementation surface. Dormant status and checks layout
definitions remain in the terminal adapter so they can return when they display
live coordinator and gate output rather than duplicate the one-shot
`factory status` command. The output reports the invocation identifier, run, and opaque terminal
handles. It does not print the role prompt or terminal contents.

The `TerminalRuntime` seam owns workspace, surface, input, notification, and
lifecycle behavior. The macOS adapter invokes cmux; workflow code never sees
cmux or macOS identifiers. The implementation surface launches
`factory-worker-attach`, which receives only the run identifier and harness
arguments. Docker container names and PTY setup remain inside the worker
adapter.

## Invocation packet and report

Each invocation receives a read-only `specification.json` packet under the
worker path `/invocation`. It contains the frozen claim packet, invocation
identity, role, stage, and prompt version. The worker receives a separate
writable `/results` mount containing only that invocation's result directory.

The harness reports through the worker-image command:

```sh
factory-report \
  --outcome completed \
  --summary 'implementation complete' \
  --change-summary 'implemented the requested behavior' \
  --acceptance 'criterion=focused test' \
  --production-file internal/example.go \
  --focused-command 'go test ./internal/...'
```

The command takes identity and result-path values from coordinator-provided
environment variables. It writes one schema-versioned `report.json` using a
temporary file, sync, and rename. The coordinator reads this file and validates
the invocation identity, outcome shape, worktree state, permitted paths, and
stage before accepting it:

```sh
factory agent-report \
  --config /Users/me/.config/factory/config.yaml \
  --run-id run-123 \
  --invocation-id inv-456
```

The three valid proposals are:

- `completed`, with a change summary, acceptance mapping, changed production
  files, focused commands, and known limitations;
- `needs_clarification`, with one or more uniquely identified questions;
- `cannot_proceed`, with concise observable evidence.

Terminal rendering, scrollback, and screen text are never parsed for stage
completion. A report is a proposal; only coordinator validation changes the
run or invocation state.

## Authentication and session state

Registration may name one explicit host Codex auth file:

```sh
factory register ... --codex-auth /Users/me/.codex/auth.json
```

The worker adapter reads that one file and streams its bytes into a separate,
factory-managed Codex credential volume. The role home contains only a link to
that credential copy, while Codex session files remain in the private run/role
volume. It never mounts the host harness directory and never writes back to the
host source. A fresh role can reuse the credential volume without inheriting
another role's session context.

Codex native session identifiers are recovered from its persisted session files,
not from the terminal screen. Accepted reports retain the native session id and
opaque surface handle so a later coordinator recovery operation can resume the
session.

## Safety boundary

The versioned core prompt is placed after repository guidance and therefore
retains authority over stage ownership, safety rules, private chain-of-thought
handling, and the report schema. Repository guidance can describe conventions,
but it cannot make screen text authoritative or grant the worker GitHub or host
credential access.
