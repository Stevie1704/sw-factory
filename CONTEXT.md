# Software Factory

The software factory turns an explicitly authorized GitHub issue into a supervised, review-ready pull request while keeping workflow authority in a deterministic coordinator.

## Language

**Run**:
One supervised execution that owns a frozen issue, specification packet, branch, worktree, and pull request.
_Avoid_: Job, task, workflow instance

**Specification packet**:
The versioned, frozen statement of product intent for a run, consisting of the claimed issue snapshot and accepted clarifications or revisions.
_Avoid_: Live issue, prompt

**Checkpoint**:
An immutable commit representing accepted work at a stage boundary and identifying the exact subject of gates or review.
_Avoid_: Snapshot, latest code

**Gate**:
A repository-declared deterministic command whose result is tied to an exact checkpoint and does not depend on model judgment.
_Avoid_: Agent check, review

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
The versioned, host-local SQLite store for current run state. It is separate from repository configuration, disposable run artifacts, and local evaluation summaries.
_Avoid_: Event journal, telemetry, transcript archive
