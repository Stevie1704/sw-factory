# Visible agent runtime

Issues #6, #12, and #14 add the visible role-agent boundary. The coordinator
owns the run, invocation identity, stage, policy, and report decision. A
harness is an interactive proposal-maker; it does not own GitHub mutations, Git
history, or workflow transitions.

## Harness adapters

Codex and Claude Code are interchangeable. Both implement the same `Runtime`
seam: capability discovery, launch, native session identification, resume, and
graceful stop. An adapter translates one harness-neutral invocation into native
commands and owns no workflow, Git, retry, or terminal-layout decision.

The coordinator resolves the adapter from the frozen repository policy for the
role, so workflow code never names a tool. `Capabilities` reports the adapter
identity and whether it supports native resume; the check-repair path uses it
to refuse continuing a session in a harness that did not create it. Mid-session
migration between Codex and Claude is therefore not possible.

The two adapters differ in how a native session identity is obtained:

| Harness | Native session identity | Resume |
| --- | --- | --- |
| Codex | Persisted by Codex; the adapter snapshots the role home before launch and discovers the new session file | `codex ... resume <uuid>` with the global flags first |
| Claude Code | Assigned by the adapter with `--session-id <uuid>` before launch, which removes the discovery race | `claude ... --resume <uuid>` |

Both adapters disable the harness's own approval gates, because the worker is
the security boundary and an inner gate would stall an unattended invocation.
The Claude launch additionally passes `--strict-mcp-config` with an empty
`--mcp-config` set, so a session loads no MCP server from any configuration
file. A factory role home is created by the worker image and is never seeded
from a host harness directory, so a session inherits no personal plugin, hook,
MCP server, skill, or setting. `DISABLE_AUTOUPDATER=1` keeps Claude Code on the
version pinned by the run's worker image digest, so a tool upgrade cannot
change behaviour halfway through a run.

Harness contract tests drive both adapters through the complete lifecycle
against controlled stub executables before any live run.

## Starting an invocation

After an issue has been claimed, start the visible Codex session with:

```sh
factory agent \
  --config /Users/me/.config/factory/config.yaml \
  --run-id run-123 \
  --codex-auth /Users/me/.codex/auth.json \
  --claude-auth /Users/me/.claude/.credentials.json
```

The command selects the active role automatically, together with the harness,
model, and reasoning effort declared for that role. Only the credential source
of the selected harness is used. For a repository with a
configured `test` role, `factory issue` leaves a healthy run in `test/active`,
and this command starts the test agent. An implementation agent is started only
after the test handoff is accepted, or after an authorized exemption. The
command creates or reuses the control workspace and creates one run workspace
with role surfaces. Dormant status and checks layout
definitions remain in the terminal adapter so they can return when they display
live coordinator and gate output rather than duplicate the one-shot
`factory status` command. The output reports the invocation identifier, run, and opaque terminal
handles. It does not print the role prompt or terminal contents.

### Clean claim handoff and recovery boundary

`factory issue` completes a claim, runs the frozen baseline suite, and persists
the worktree, branch, checkpoint, issue projection, and status-comment identity.
A validated repository advances to `test/active`. A separate coordinator process may
start the first test or implementation invocation when its
read-only recovery diagnosis finds that every checked projection agrees and
the operational store contains no invocation history. This is a completed
claim or test handoff awaiting its first invocation, not an interrupted run.

The exception applies only to first-agent startup. Any persisted invocation,
including a terminal one, or any recovery discrepancy returns the typed
`recovery-required` result without starting a worker, terminal surface, or
harness. Gates, reports, transitions, draft pull requests, and other
progression paths retain the fail-closed boundary until issue #21 provides
complete reconciliation.

The `TerminalRuntime` seam owns workspace, surface, input, notification, and
lifecycle behavior. The macOS adapter invokes cmux; workflow code never sees
cmux or macOS identifiers. The implementation surface launches
`factory-worker-attach`, which receives only the run identifier and harness
arguments. Docker container names and PTY setup remain inside the worker
adapter.

## Invocation packet and report

Each invocation receives a read-only `specification.json` packet under the
worker path `/invocation`. It contains the frozen claim packet, invocation
identity, role, stage, and prompt version. An implementation packet also carries
the accepted test handoff and content hashes of protected test paths. The worker
receives a separate writable `/results` mount containing only that invocation's
result directory.

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

Test-stage completion uses a separate handoff and never accepts production
paths. For example:

```sh
factory-report \
  --outcome completed \
  --summary 'focused behavior test is red on base' \
  --acceptance 'criterion=focused red test' \
  --test-file internal/example_test.go \
  --focused-test-command 'go test ./internal -run TestBehavior' \
  --expected-failure-reason 'expected behavior assertion' \
  --observed-failure 'exit_code=1'
```

The coordinator independently reruns `focused_test_command` inside the worker
and requires the reported `expected_failure_reason` to appear in the captured
output. Only that matching non-zero exit is verified red evidence. A passing
command, worker failure, missing expected reason, or path-ownership dispute moves the run to `test/waiting_for_human`;
the coordinator records only the content-free `test_dispute` evaluation
category and does not revise the test automatically.

On verified red evidence, the coordinator creates a distinct test checkpoint,
records every changed test/infrastructure path and its SHA-256 content hash,
then launches implementation. Implementation report acceptance rechecks those
hashes and rejects any direct edit to a protected test path.

## Specification review

After `factory draft-pr` creates the draft pull request, the next `factory
agent` invocation automatically selects the `spec_review` role at the
`review` stage. The reviewer runs in a fresh role session against the exact
implementation checkpoint. Its read-only invocation packet contains the frozen
specification packet, accepted test and implementation handoffs, protected
test-path hashes, the base-to-checkpoint diff, bounded gate-result summaries,
and any prior findings. It never receives upstream harness transcripts.

The reviewer publishes a completed report with repeated finding flags:

```sh
factory-report \
  --outcome completed \
  --summary 'review complete' \
  --finding 'internal/example.go:42|claim|observable evidence|blocker|correctness|repair the behavior|implementation'
```

Every finding must include a location, claim, evidence, severity, category,
suggested resolution, and suggested owner. Only blocker findings classified as
correctness, security, specification, or documented-standards violations gate
readiness. Taste and scope findings remain visible advisories. The coordinator
attaches the stable `factory/review/specification` Commit Status to the exact
reviewed SHA and invalidates the durable review projection when the checkpoint
changes. Accepted findings appear in both the editable issue status comment and
the generated pull-request review section. A technical test exemption remains
marked provisional in the review packet and projections.

Human skips must be frozen in the issue before claim with an exact marker:

```text
<!-- factory-test-exemption: human | documentation-only change -->
```

The repository must allow human exemptions. Technical exemptions may be
reported by the test agent only when policy allows them; they are provisional
and are carried to later review.

When the harness has reliable measurements or content-free policy signals, it
may also pass `--input-tokens`, `--output-tokens`, `--total-tokens`,
`--cost-micros` with `--cost-currency`, `--budget-exhausted`, and repeated
fixed-category `--exemption`, `--escalation`, or `--blocker` flags. Omitting
usage leaves it explicitly unavailable; the coordinator never estimates it.

Terminal rendering, scrollback, and screen text are never parsed for stage
completion. A report is a proposal; only coordinator validation changes the
run or invocation state.

An accepted `needs_clarification` report finishes the current harness session,
stops the worker, and places the run in `waiting_for_human` without consuming a
retry attempt. The coordinator publishes each question ID and prompt on the
active issue or pull request, mirrors them in the editable status comment, and
notifies cmux. An authorized maintainer answers with a structured GitHub
comment such as `/factory answer clarification-1 use the existing JSON format`.
The answer is stored in the next specification-packet version, and a fresh
invocation receives that packet. A `/factory refresh` command similarly
re-reads the issue, versions the packet, and invalidates downstream results
before resuming the role.

## Authentication and session state

Registration may name one explicit host credential file per harness:

```sh
factory register ... \
  --codex-auth /Users/me/.codex/auth.json \
  --claude-auth /Users/me/.claude/.credentials.json
```

The worker adapter reads the one file belonging to the selected harness and
streams its bytes into a separate, factory-managed credential volume. The role
home contains only a link to that credential copy, while harness session files
remain in the private run/role volume. It never mounts the host harness
directory and never writes back to the host source. A fresh role can reuse the
credential volume without inheriting another role's session context.

Each harness keeps its own source because a host can hold one harness
credential as a file without holding the other. macOS keeps the Claude Code
credential in the login Keychain rather than a file, so `--claude-auth` is
supplied only where a credential file exists; without it, Claude Code uses the
credential the worker itself persisted in its role volume.

Codex native session identifiers are recovered from its persisted session files,
not from the terminal screen. Claude Code accepts a coordinator-assigned
identifier at launch, so no discovery is needed. Accepted reports retain the
native session id and opaque surface handle so a later coordinator recovery
operation can resume the session in the same harness.

## Check-repair loop

When `factory draft-pr` evaluates an accepted implementation checkpoint, it
runs the complete frozen gate suite and retains each result under that exact
checkpoint SHA. Typed deterministic gate failures are assembled into one
bounded check-repair packet containing all gate outcomes, skipped dependency
reasons, and bounded command diagnostics. The next implementation invocation
uses the existing worker role volume, implementation surface, and native Codex
session through the harness resume seam.

The repository's `retry_limits.check_repair` value is frozen into the run. A
repair reservation is durable before native resume, but the consumed attempt
counter advances only after resume and the final run-state write succeed; an
interrupted reservation is marked for reconciliation and blocks blind
relaunch. Setup, worker, GitHub status, harness, and other transport failures
roll back before resume and move the run to `waiting_for_harness` without
consuming the budget. Once the ceiling is reached, the run moves to
`check/waiting_for_human` and receives the `agent-needs-input` label. The
single status comment always shows consumed, pending, and remaining attempts.
After a repair report is accepted, the next checkpoint must have a new SHA and
the full gate suite runs again; no prior checkpoint result authorizes it. Gate
counts, repair attempts, stage durations, budget exhaustion, and available
usage metadata remain in the local content-free evaluation summary.

## Safety boundary

The versioned core prompt is placed after repository guidance and therefore
retains authority over stage ownership, safety rules, private chain-of-thought
handling, and the report schema. Repository guidance can describe conventions,
but it cannot make screen text authoritative or grant the worker GitHub or host
credential access.
