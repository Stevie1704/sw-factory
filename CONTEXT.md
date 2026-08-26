# Software Factory

The software factory turns an explicitly authorized GitHub issue into a supervised, review-ready pull request while keeping workflow authority in a deterministic coordinator.

## Language

**Run**:
One supervised execution that owns a frozen issue, specification packet, branch, worktree, and pull request.
_Avoid_: Job, task, workflow instance

**GitHub lifecycle observation**:
One coordinator read of the tracked issue and pull request that can identify
successful merge completion or an unmerged closure requiring cancellation.
_Avoid_: Screen scrape, automatic resume

**Specification packet**:
The versioned, frozen statement of product intent for a run, consisting of the claimed issue snapshot and accepted clarifications or revisions.
_Avoid_: Live issue, prompt

**Checkpoint**:
An immutable commit representing accepted work at a stage boundary and identifying the exact subject of gates or review.
_Avoid_: Snapshot, latest code

**Test-stage handoff**:
The bounded acceptance coverage, changed test paths, focused red-test command, expected failure reason, observed failure evidence, infrastructure changes, uncovered criteria, and exemption state transferred from test to implementation.
_Avoid_: Agent claim, transcript, implementation handoff

**Protected test path**:
A repository-relative test or authorized test-infrastructure path recorded at the test checkpoint with its SHA-256 content identity; implementation cannot edit it directly.
_Avoid_: Mutable test file, permitted production path

**Baseline**:
The repository-declared setup and gate suite evaluated against the claimed run's base checkpoint before any agent edit; a blocking failure moves the run to failed preflight unless the frozen issue explicitly targets it. A healthy configured run enters test stage before implementation.
_Avoid_: Preflight assumption, agent diagnosis

**Setup fingerprint**:
The SHA-256 identity of the configured manifest and lockfile contents observed by setup for one run phase and exact checkpoint.
_Avoid_: Dependency cache key, mutable latest state

**Recovery diagnosis**:
A read-only comparison of one persisted non-terminal run with its registered repository, worktree, Git projection, and GitHub projections.
_Avoid_: Reconciliation, recovery

**Recovery-required result**:
A typed fail-closed refusal that reports the agreement state and discovered discrepancies without claiming that the run was recovered; issue #21 supersedes it with complete reconciliation.
_Avoid_: Recovered run, automatic continuation

**Gate**:
A repository-declared deterministic command whose result is tied to an exact checkpoint and does not depend on model judgment.
_Avoid_: Agent check, review

**Worker**:
The per-run isolated execution environment that exposes only the run worktree, read-only Git metadata, explicitly declared repository caches, and factory-managed credential copies.
_Avoid_: Container in workflow decisions

**WorkerRuntime**:
The portable seam that starts, resumes, commands, stops, and inspects a worker while hiding runtime identifiers, container paths, role homes, invocation packets, result files, and process tracking.
_Avoid_: Docker API

**Invocation**:
One immutable harness attempt within a run, with its own invocation packet, result directory, visible surface handles, prompt version, and native session identifier when known.
_Avoid_: Terminal transcript

**Surface**:
An operator-visible terminal pane owned by a `TerminalRuntime`; its handle is opaque to workflow code and its screen is never a correctness protocol.
_Avoid_: Screen scrape

**Harness**:
A configured interactive coding tool, such as Codex, launched through the harness seam with a role-specific prompt and native resume behavior.
_Avoid_: Lead agent

**Role**:
The coordinator-owned responsibility assigned to an invocation, such as implementation, test, or review; repository guidance cannot change role ownership.
_Avoid_: Persona

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
Host-local YAML that registers the one repository, its GitHub identity, authorized users, polling and cmux settings, and the external operational-data location.
_Avoid_: Repository policy

**Repository configuration**:
Checked-in `factory.yaml` that declares the repository's target branch, setup, deterministic gates, harness and model policy, budgets, worker build, and base synchronization.
_Avoid_: Host configuration

**Operational store**:
The versioned, host-local SQLite store for current run state. Its current-state
tables are separate from repository configuration, disposable run artifacts,
and the isolated local evaluation-summary projection.
_Avoid_: Event journal, telemetry, transcript archive
