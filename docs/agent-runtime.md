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
identity. The check-repair path dispatches on the harness recorded for the
session under repair and refuses to continue when the resolved adapter reports
a different identity, so mid-session migration between Codex and Claude is not
possible.

Each adapter translates the validated model and reasoning effort into its own
arguments: Codex uses `-m` and `-c model_reasoning_effort=`, Claude Code uses
`--model` and `--effort`. The effort names differ between the two, and Claude
Code silently falls back to its default for a level it does not recognize, so
the repository-declared options per role are what constrain the value.

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
MCP server, or setting. The only skills a session sees are the curated set the
worker image installs into both role homes, which the worker image digest pins
for the run; `worker/SKILLS.md` records the set and ADR 0006 records the
boundary. Each role prompt names the skills that role may use, because both
harnesses advertise every installed skill to every role. A skill a role prompt
mandates must therefore stay out of each harness's hidden-skill metadata;
`worker/SKILLS.md` records that activation contract and how it is verified. `DISABLE_AUTOUPDATER=1` keeps Claude Code on the
version pinned by the run's worker image digest, so a tool upgrade cannot
change behavior halfway through a run.

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
of the selected harness is used. In `test_policy.mode: required`,
`factory issue` leaves a healthy run in `test/active`, and this command starts
the independent test agent; implementation starts only after its handoff is
accepted or after an authorized exemption. In `test_policy.mode: advisory`,
the healthy baseline leaves the run in `implementation/active` and this command
starts implementation directly. The implementation prompt owns the complete
red/green/refactor loop in advisory mode, including focused behavioral tests.
A frozen issue that selected a workflow route overrides that fast path: the
`acceptance` route starts the test role first, and the `design-acceptance`
route starts the architecture role first and hands its accepted design to the
test role.
The agent auth flags must name the same sources registered for the repository;
distinct one-off sources are refused because recovery never persists host
credential paths. Change the registered source with `factory register` instead.
The command creates or reuses the control workspace and creates one run workspace
with role surfaces. Dormant status and checks layout
definitions remain in the terminal adapter so they can return when they display
live coordinator and gate output rather than duplicate the one-shot
`factory status` command. The output reports the invocation identifier, run, and opaque terminal
handles. It does not print the role prompt or terminal contents.

### Clean claim handoff and recovery boundary

`factory issue` completes a claim, runs the frozen baseline suite, and persists
the worktree, branch, checkpoint, issue projection, and status-comment identity.
A required-mode repository advances to `test/active`; an advisory repository
advances to `implementation/active`. A selected `acceptance` route advances to
`test/active` and a selected `design-acceptance` route advances to
`architecture/active`, whatever the repository test policy. A separate coordinator process may start
the first test or implementation invocation when its
read-only recovery diagnosis finds that every checked projection agrees and
the operational store contains no invocation history. This is a completed
claim or test handoff awaiting its first invocation, not an interrupted run.

The exception applies only to first-agent startup. A persisted active invocation
with a complete native-session identity is resumed once against its persisted
worker and terminal handles. If the worker is missing or stopped, the
coordinator recreates it from the frozen image digest and invocation mount
identity while preserving the worktree and role volume. An unexpected native
harness exit, including one observed after launch by the supervisor's worker-side
process check, receives exactly one automatic resume attempt; a second
unexpected failure pauses the run for manual recovery. Rate limits enter a
`waiting_for_harness` state, stop the worker, notify the operator, and are
retried by the supervisor without consuming workflow or check-repair budget.
Expired authentication enters `waiting_for_human` with a typed, redacted
failure and tells the operator to refresh credentials. A missing native identity
or any other recovery discrepancy pauses the run for a human. Terminal report
acceptance is replayed from its durable effect record without finalizing the
same invocation twice. Gates, reports, transitions, draft pull requests, and
other progression paths reconcile the durable effect journal and external
identities before they resume an interrupted run.

Invocation packets use append-only schemas. Restart recovery accepts the current
and retained historical packet versions when their required identities and
role-specific context are present; missing optional fields remain absent. Future
or unsupported versions pause recovery rather than guessing at their semantics.

When automatic recovery is exhausted, an operator can use the explicit
recovery commands:

```sh
factory resume --config /Users/me/.config/factory/config.yaml --run-id run-123
factory attach --config /Users/me/.config/factory/config.yaml --run-id run-123
factory auth refresh --config /Users/me/.config/factory/config.yaml --run-id run-123
```

`factory resume` retries harness capacity or performs a manual native resume
without spending the automatic recovery allowance. A manually resumed session
sets a durable attach gate; workflow progression and report acceptance remain
blocked until `factory attach` restores the worker and visible terminal
topology and clears that gate. `factory auth refresh` reads the explicitly
registered host credential source and reseeds only the factory-managed worker
credential volume; it never writes the source file or host harness directory.

The `TerminalRuntime` seam owns workspace, surface, input, notification, and
lifecycle behavior. The macOS adapter invokes cmux; workflow code never sees
cmux or macOS identifiers. The implementation surface launches
`factory-worker-attach`, which receives only the run identifier and harness
arguments. Docker container names and PTY setup remain inside the worker
adapter.

## Invocation packet and report

Each invocation receives a read-only `specification.json` packet under the
worker path `/invocation`. It contains the frozen claim packet, invocation
identity, role, stage, prompt version, `test_policy_mode`, and the frozen
`route`. A required-mode implementation packet also carries the accepted test
handoff and content hashes of protected test paths. An implementation-owned
advisory packet carries no test-stage disposition; its normal implementation
handoff may include behavioral tests and test infrastructure. A test invocation
on the `design-acceptance` route also receives `design_handoff`, the accepted
architecture design it must exercise. The worker receives a separate writable `/results` mount
containing only that invocation's result directory.

The packet is the complete frozen claim; the prompt is a projection of it. A
role prompt fences the claimed issue, the accepted clarifications, and the
frozen run parameters the role acts on - target branch, route, test policy
mode, declared gates, and the captured guidance paths - and points at
`/invocation/specification.json` for everything else. Repository guidance
therefore reaches the role once, inside its own untrusted fence, and
coordinator-owned configuration such as declared cache paths, worker build,
and harness or model policy stays out of the context window.

A harness also discovers its own project instruction file from its working
root, which is the mounted worktree, and loads it as unlabelled instructions: a
second copy that is mutable during the run and that an implementation role
could rewrite for itself.

The Codex adapter closes that channel. It launches with
`-c project_doc_max_bytes=0`, which suppresses `AGENTS.md` at the workspace
root and in nested directories. The override bounds project documents only; the
pinned worker skill set lives in the role home (`$CODEX_HOME/skills`,
`$HOME/.claude/skills`) and stays available.

The Claude adapter does not close it yet, so a Claude invocation still
auto-discovers `CLAUDE.md` and its local variants from the worktree. The
harness offers only `--bare` and `--safe-mode`, and neither is usable here:
`--safe-mode` disables the pinned worker skills along with the project file,
and `--bare` additionally drops hooks, plugins, and every credential source
except `ANTHROPIC_API_KEY`, which the factory-managed credential store does not
supply. Until the harness exposes a narrower control, a Claude role can read
mutable worktree guidance that the factory did not freeze, and the prompt's
precedence rule is the only bound on it.

A check repair, a review repair, and a test-objection revision resume the
harness session that already read the role's first prompt. Such a launch builds
a continuation prompt: the invocation identity, the coordinator-owned repair or
objection context, and the factory-owned rules. It repeats neither the frozen
specification, the repository guidance, nor the role body, because the resumed
session already holds them. A repair that cannot resume a native session starts
a fresh session and receives the complete prompt. The packet records
`continuation`, so restart recovery rebuilds the same prompt shape.

Only a required-mode implementation report may include one structured
`test_objection` entry. It identifies the protected test, the claim under
dispute, and observable evidence; it does not authorize an implementation edit
to the protected path. When `test_policy.allow_automated_objections` is
disabled, the coordinator records the objection and pauses for a human. After
the measured pilot authorizes automation and an authorized maintainer's latest
#26 decision comment says `Decision: proceed`, the coordinator resumes the
original test session with the current implementation context. A later `revise
and repeat` or `stop` decision closes the gate. The test role returns an
accepted or rejected response; an accepted revision must produce new test
content and pass an independently rerun focused command with the expected red
failure before the implementation role resumes. A rejection, verification
failure, or an objection after the second attempt pauses for human disposition.

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

In required mode, test-stage completion uses a separate handoff and never
accepts production paths. For example:

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
category. An implementation objection follows the separately gated objection
cycle described above; ordinary unverifiable test-stage reports never trigger
that cycle.

On verified red evidence in required mode, the coordinator creates a distinct
test checkpoint, records every changed test/infrastructure path and its SHA-256
content hash, then launches implementation. Implementation report acceptance
rechecks those hashes and rejects any direct edit to a protected test path.

## Concurrent isolated reviews

After `factory draft-pr` creates the draft pull request, the coordinator starts
the `spec_review` and `standards_review` roles against the same exact
implementation checkpoint. Their external sessions run concurrently, while
the coordinator applies reports serially. Each role receives a fresh
read-only worker surface, private home and temporary storage, and its own
invocation identity. Neither role receives implementation or test handoffs,
upstream harness transcripts, or the other reviewer's conclusions. A reviewer
may receive only findings previously accepted for that same role and
checkpoint.

The specification reviewer evaluates the frozen requirement and observable
behavior. The standards reviewer evaluates, in order, non-overridable factory
safety rules, the frozen specification, scoped repository instructions,
contribution and architecture documentation, and nearby conventions. A
technical test exemption is provisional evidence for the standards reviewer,
not an automatic waiver. Both reviewers inspect the same immutable
base-to-checkpoint diff and must report that checkpoint SHA.

The diff is captured with a bounded context width, because a review role has
the checkpoint worktree mounted read-only and can open any file it needs. It is
mounted with the invocation packet and named in the prompt rather than repeated
in it: a launch prompt travels as one command argument, and Linux refuses an
argument longer than 32 pages, so a prompt that grew with the reviewed change
could not be executed at all. Every adapter now refuses an oversized prompt
before launch with a typed error naming the size. A diff beyond the packet bound
is left out of the packet rather than stopping the run: the packet records its
size, a `git diff --name-only` that lists the changed paths, and a per-path
`git diff` the reviewer completes with one path at a time. The listing adds
`--no-renames`, so a rename's old path is listed rather than hidden behind its
new one, and `-z`, so no path is C-quoted; the role body requires the reviewer
to quote each path it appends. The per-path command omits `--binary`, because
the reviewer reads hunks rather than applying a patch. The role body owns that
procedure and the rule that reading the checkpoint is evidence, so both review
prompt versions were bumped with it. The reviewer reads the change in its own
read-only worktree, so neither the packet nor any single command grows with the
checkpoint. Implementation is not bounded by checkpoint size, so review is not
either.

The reviewer publishes a completed report with repeated finding flags:

```sh
factory-report \
  --outcome completed \
  --summary 'review complete' \
  --finding 'internal/example.go:42|claim|observable evidence|blocker|correctness|repair the behavior|implementation'
```

Every finding must include a location, claim, evidence, severity, category,
suggested resolution, and suggested owner. A blocker finding gates readiness
only on the axis that owns its category: the specification reviewer gates on
correctness, security, and specification violations, and the standards
reviewer gates on documented-standards violations. Taste and scope findings,
and any finding that crosses into the other axis, remain visible advisories. The coordinator
attaches the stable `factory/review/specification` and
`factory/review/standards` Commit Status contexts to the exact reviewed SHA,
and invalidates both durable review projections when the checkpoint changes.
Accepted findings appear in both the editable issue status comment and the
generated pull-request review section. The run becomes ready only after every
configured reviewer has completed successfully.

Human skips must be frozen in the issue before claim with an exact marker:

```text
<!-- factory-test-exemption: human | documentation-only change -->
```

The repository must allow human exemptions for required-mode skips. Technical
exemptions may be reported by the required-mode test agent only when policy
allows them; they are provisional and are carried to later review. Advisory
implementation reports do not use test-stage exemptions or disputes, but an
implementation-owned advisory handoff may still include its own behavioral
tests without invoking the protected-test objection cycle.

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

A persisted invocation is void when its launch fails before it produces a native
session identity and its result directory contains no regular `report.json`.
The coordinator marks that invocation `superseded` while rolling back the
worker and surface, before unattended progression publishes its waiting state,
and persists the void decision on the invocation. Restart reconciliation treats
only that marked superseded launch as history with no live projection.
A native session identity or report file protects the invocation history from
being discarded, and so does any indeterminate observation: when the durable
effect journal or the report path cannot be read, the coordinator keeps the
invocation `cannot_proceed`, names the discrepancy in its error, and does not
void the launch. If the run is `waiting_for_human` with no active invocation,
the lifecycle reason names this void rule and `/factory retry`; that command
supersedes only the latest invocation satisfying the same rule and reopens the
existing role boundary with the existing specification packet. It does not
create a new packet version or retry automatically.

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
uses the existing worker role volume, implementation surface, and native
session for the recorded harness through the harness resume seam.

The repository's `retry_limits.check_repair` value is frozen into the run. A
repair reservation is durable before native resume, but the consumed attempt
counter advances only after resume and the final run-state write succeed; an
interrupted reservation is marked for reconciliation and blocks blind
relaunch. Setup, worker, GitHub status, harness, and other transport failures
roll back before resume and move the run to `waiting_for_harness` without
consuming the budget. Once the budget is exhausted, the run moves to
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
