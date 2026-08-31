# Software Factory

The software factory turns an explicitly authorized GitHub issue into a supervised, review-ready pull request while keeping workflow authority in a deterministic coordinator.

## Language

**Run**:
One supervised execution that owns a frozen issue, specification packet, branch, worktree, and pull request.
_Avoid_: Job, task, workflow instance

**GitHub lifecycle observation**:
One coordinator read of the tracked issue and pull request that can identify
successful merge completion, an unmerged closure requiring cancellation, or a
newly submitted authorized review requesting changes. Lifecycle is always read
before any repair, readiness, or infrastructure work.
_Avoid_: Screen scrape, hidden workflow transition

**Specification packet**:
The versioned, frozen statement of product intent for a run, consisting of the claimed issue snapshot, the selected workflow route, and accepted clarifications or revisions.
_Avoid_: Live issue, prompt

**Checkpoint**:
An immutable commit representing accepted work at a stage boundary and identifying the exact subject of gates or review.
_Avoid_: Snapshot, latest code

**Test-stage handoff**:
The required-mode transfer of bounded acceptance coverage, changed test paths, focused red-test command, expected failure reason, observed failure evidence, infrastructure changes, uncovered criteria, and exemption state from the independent test role to implementation. Advisory mode has no test-stage handoff.
_Avoid_: Agent claim, transcript, implementation handoff

**Protected test path**:
A repository-relative test or authorized test-infrastructure path recorded at the test checkpoint with its SHA-256 content identity; implementation cannot edit it directly.
_Avoid_: Mutable test file, permitted production path

**Test objection cycle**:
A bounded implementation-to-test dispute in which implementation submits the
protected test's claim and observable evidence, the original test session may
accept or reject that objection, and an accepted revision must pass independent
red verification before implementation resumes. Automation is gated by the
measured pilot and permits at most two revision attempts before human review.
_Avoid_: Test rewrite, implementation-owned test edit, unbounded repair

**Workflow route**:
The factory-owned stage sequence an authorized issue author selects with one bounded frozen issue marker before claim. `acceptance` runs the independent test role before implementation; `design-acceptance` runs architecture, then the test role, then implementation. An absent marker follows the repository test policy. The route is recorded in the specification packet and is immutable for the run; it is never inferred from changed files, issue prose, or model judgment.
_Avoid_: Inferred workflow, repository-declared route, mode

**Baseline**:
The repository-declared setup and gate suite evaluated against the claimed run's base checkpoint before any agent edit; a blocking failure moves the run to failed preflight unless the frozen issue explicitly targets it. A healthy required-mode run enters the independent test stage; a healthy advisory run enters implementation-owned TDD directly unless the frozen issue selected a workflow route.
_Avoid_: Preflight assumption, agent diagnosis

**Setup fingerprint**:
The SHA-256 identity of the configured manifest and lockfile contents observed by setup for one run phase and exact checkpoint.
_Avoid_: Dependency cache key, mutable latest state

**Recovery diagnosis**:
A read-only comparison of one persisted non-terminal run with its registered repository, worktree, Git projection, and GitHub projections.
_Avoid_: Reconciliation, recovery

**Reconciliation**:
The coordinator's restart pass that consumes one durable pending effect when
safe, repairs the projections that effect owns, and otherwise pauses the run
with a typed discrepancy. It may recreate a missing worker from the frozen
invocation identity and permits one coordinator-owned native harness resume.
_Avoid_: Diagnosis, unbounded retry

**Recovery-required result**:
A typed discrepancy result that reports the agreement state and discovered discrepancies; journaled runs reconcile safe effects automatically and pause unresolved disagreements for a human, while legacy stores retain the fail-closed refusal.
_Avoid_: Recovered run, implicit operator approval

**Startup diagnosis**:
A complete pre-claim report of host configuration, external access, repository, terminal, worker, harness, authentication, and operational-store readiness. Every subsystem contributes its own bounded check, and the doctor reports all failures before a run can start.
_Avoid_: First failure, mid-run diagnosis

**Gate**:
A repository-declared deterministic command whose result is tied to an exact checkpoint and does not depend on model judgment.
_Avoid_: Agent check, review

**Worker**:
The per-run isolated execution environment that exposes only the run worktree, read-only Git metadata, explicitly declared repository caches, and factory-managed credential copies.
_Avoid_: Container in workflow decisions

**WorkerRuntime**:
The portable seam that starts, resumes, commands, stops, and inspects a worker while hiding runtime identifiers, container paths, role homes, invocation packets, result files, and process tracking.
_Avoid_: Docker API

**Unattended progression**:
The persistent coordinator's bounded pass that drives one claimed run through
baseline, its route-selected stages, result acceptance, the exact-checkpoint
gates, the bounded check-repair loop, the push, one draft pull request, the
independent review round, the bounded review-repair loop, and pull-request
readiness without an operator running a stage-driving command. Each pass also
applies the structured commands an operator left on the run's issue or draft
pull request, so no comment needs a command-polling CLI invocation either. It
stops at the first human or infrastructure waiting state, and it resumes the
issue queue after a terminal pull-request outcome.
_Avoid_: Autonomous agent, agent-driven workflow, auto-merge

**Human repair packet**:
The coordinator-owned repair context built from one or more applicable
`CHANGES_REQUESTED` reviews by authorized maintainers, holding their completed
review bodies and inline findings. Concurrent applicable reviews form one
packet outside the bounded factory repair rounds and never consume the
review-repair budget.
_Avoid_: Comment command, review reply, advisory feedback

**Review watermark**:
The persisted identity of the last human review a run applied. It makes
repeated polling and a coordinator restart unable to apply the same review
twice.
_Avoid_: Comment watermark, last poll time

**Queue release**:
The terminal transition of the active run - a merged pull request completes it
and an unmerged closure cancels it - that ends the one-active-run constraint
and lets the same coordinator process claim the next oldest eligible issue.
Waiting-for-human and retryable-infrastructure states never release it.
_Avoid_: Cleanup, retention, restart

**Run activity**:
The published distinction between an active run whose next transition the
coordinator owns, an active invocation whose harness is executing, and a
waiting state. The `agent-running` label alone cannot express it.
_Avoid_: Agent-running label, run status

**Invocation**:
One immutable harness attempt within a run, with its own invocation packet, role-owned visible surface handles, factory prompt version, and native session identifier when known.
_Avoid_: Terminal transcript

**Surface**:
An operator-visible terminal pane owned by a `TerminalRuntime`; its handle is opaque to workflow code and its screen is never a correctness protocol.
_Avoid_: Screen scrape

**Harness**:
A configured interactive coding tool, such as Codex, launched through the harness seam with a role-specific prompt and native resume behavior.
_Avoid_: Lead agent

**Role**:
The coordinator-owned responsibility assigned to an invocation, such as implementation, architecture, test, or review. The factory-owned role registry couples each role to its invocation stage, prompt version, default permitted paths, report contract, and visible surface strategy; repository guidance cannot change role ownership.
_Avoid_: Persona

**Workflow registry**:
The factory-owned declaration of roles, prompts, stages, visible surfaces, and report-outcome transitions. Repository configuration selects harness and model policy for declared roles but cannot add or redefine workflow authority.
_Avoid_: Repository-defined workflow, prompt configuration

**Invocation packet**:
The read-only, versioned file containing the frozen specification and role identity that the coordinator mounts into one worker invocation.
_Avoid_: Live issue

**Structured report**:
The schema-versioned, content-limited proposal written by `factory-report`; the coordinator validates it before making any workflow decision.
_Avoid_: Terminal output

**Credential store**:
A factory-managed, harness-specific credential copy kept separate from role session state and never populated by mounting the host harness directory.
_Avoid_: Host auth mount

**Review blocker**:
A concrete correctness, security, specification, or documented-standards violation that prevents readiness.
_Avoid_: Suggestion, preference, advisory finding

**Local evaluation summary**:
A content-free record of run outcomes, effort, escalations, and human dispositions retained locally to evaluate and tune the factory.
_Avoid_: Telemetry, transcript, audit log

**Outbound telemetry**:
Automatic transmission of product-usage or evaluation data from the operator's workstation to an external recipient. The factory prohibits outbound telemetry.
_Avoid_: Local evaluation summary

**Pilot**:
An evidence-gathering delivery phase that compares the supervised factory with a direct-harness baseline before more elaborate workflow automation is authorized.
_Avoid_: Production readiness, tracer bullet

**Host configuration**:
Host-local YAML that registers the one repository, its GitHub identity, authorized users, polling and terminal settings, and the external operational-data location.
_Avoid_: Repository policy

**Repository configuration**:
Checked-in `factory.yaml` that declares the repository's target branch, setup, deterministic gates, harness and model policy for factory-declared roles, budgets, worker build, and base synchronization. It cannot declare roles, prompts, stages, or transitions.
_Avoid_: Host configuration

**Operational store**:
The versioned, host-local SQLite store for current run state. Its current-state
tables are separate from repository configuration, disposable run artifacts,
and the isolated local evaluation-summary projection.
_Avoid_: Event journal, telemetry, transcript archive

**Cleanup**:
The explicit, seven-day retention operation that previews and removes one or
more terminal runs' local worktrees, local branches, workers, role sessions,
terminal workspaces, generated outputs, and operational rows while retaining
remote branches, credential stores, and local evaluation summaries. It is the
last operation that knows a run's workspace handles, so a workspace it cannot
close is reported for manual closure instead of being lost silently.
_Avoid_: Remote branch deletion, automatic summary deletion
