# Software Factory

The software factory turns an explicitly authorized GitHub issue into a supervised, review-ready pull request while keeping workflow authority in a deterministic coordinator.

## Language

**Run**:
One supervised execution that owns a frozen issue, specification packet, branch, worktree, and pull request.
_Avoid_: Job, task, workflow instance

**Ideation session**:
The repository-scoped record of work that exists before an issue or a Run:
its conversation turns, brief revisions, plan revisions, approvals, and
publication receipts. It has its own lifecycle (awaiting user, role active,
draft review, approved, publishing, publication incomplete, published,
deferred, investigation needed, no build, cancelled) and its own revision for stale-writer checks.
It owns no branch, worktree, or issue.
_Avoid_: Run, draft run, chat

**Brief**:
The structured statement a session builds: problem, audience, success
criteria, scope and non-goals, decisions with rationale, assumptions, evidence,
and unresolved questions. Each change is a new brief revision; plan nodes
reference its items by ID.
_Avoid_: Spec (for an unpublished brief), summary

**Readiness proposal**:
The ideation role's suggestion of the next session step (ready for planning,
investigation needed, or no build) with its rationale, bound to the brief
revision it was made for. It changes nothing; the user decides with a command
on the brief revision the page shows.
_Avoid_: Approval, handoff

**Deferred question**:
An unresolved question of the brief that the user explicitly hands to
planning without an answer. Planning may plan an investigation for it but must
not answer it. A handoff must defer every unresolved question of its brief
revision.
_Avoid_: Assumption, open point

**Investigation needed**:
The session outcome that material unknowns must be investigated before
planning. The unresolved questions of the brief are the investigation items.
It creates no backlog item, and the session can resume.
_Avoid_: Spike issue, deferred

**Correction**:
A user turn that names the earlier user answer it changes. The ideation role
must revise or withdraw each brief decision that depended on the old answer
and name those decisions as affected decisions.
_Avoid_: Edit, rewrite

**Plan revision**:
One immutable, provider-neutral work-item graph derived from one exact brief
revision and repository context revision. Nodes have stable local IDs, a kind
(`task`, `investigation`, or `parent`), and parent, dependency, and brief
references. A newer brief makes it stale. Plan validation refuses a draft with
a cycle, a contradictory hierarchy, missing content, or a size over its limits;
it never shortens a draft.
_Avoid_: Backlog, issue list

**Session role**:
A factory-owned role that serves an ideation session, not a Run: the ideation
role, which asks questions and proposes brief updates and readiness, and the
planning role, which drafts a plan revision from a frozen brief. It reads a
read-only context snapshot and has no tracker or worktree authority.
_Avoid_: Chat agent, planner run

**Session-role invocation**:
One harness attempt of a session role, pinned to the session revision it
serves, the user turn or request it answers, the brief and context revisions,
the harness policy, the prompt identity, and the worker image. At most one is
active per session and at most one result is accepted per trigger.
_Avoid_: Invocation (for a run attempt), run

**Context snapshot**:
A detached, read-only checkout of the fetched target-branch commit that a
session role inspects. Its commit is the repository context revision recorded
on the invocation and on any plan revision it produces.
_Avoid_: Working tree, worktree (for a run checkout)

**Plan approval**:
The human approval of one plan revision for one target, bound to the fully
rendered publication payload and relationship mapping by their digests. Any
later brief or plan change requires a fresh approval.
_Avoid_: Sign-off, authorization (for execution)

**Publication receipt**:
Append-only evidence of one publication attempt for one approval: the tracker
items and relationships known to exist and, for an incomplete attempt, what
remains. Publication never adds `agent-ready` and never starts a Run.
_Avoid_: Publish log

**Publication ledger**:
The per-session, per-approval list of the item creates and relationship
creates that publish one approved plan, in execution order, with the recovery
state of each (pending, attempted, failed, ambiguous, succeeded) and the
tracker identity each create produced. It is the recovery authority of
publication; a retry resumes it and never regenerates the plan.
_Avoid_: Pending effect (that journal is per Run), job queue

**Correlation key**:
The stable identity `<session>/<plan revision>/<node>` that the adapter
renders into each published tracker item, so that a lookup finds the item
again after an unknown create outcome. Publication looks it up before every
create; more than one match, or no match after an unknown outcome, stops for
explicit reconciliation.
_Avoid_: Idempotency key (providers do not honor one)

**Plan publisher**:
The optional tracker-port capability that preflights and renders a plan
graph and creates, looks up, and links its items. Each adapter implements it
with its own provider commands; the coordinator reaches it only through the
port.
_Avoid_: GitHub publisher (for the concept)

**Textual relationship fallback**:
The explicit text representation of a plan parent or dependency edge where
the tracker offers no native relationship, for example a GitHub host without
sub-issues. The preview shows it in the item bodies and in the relationship
mapping, so the approval binds it; publication never switches between native
and textual after approval.
_Avoid_: Silent downgrade, best-effort link

**Session command**:
One browser request for an ideation session, such as a user turn, a brief
edit, an approval, or a cancellation. It carries a request ID that makes it
idempotent and the session revision the page showed. Acceptance makes it
durable; only the coordinator applies it and records its outcome.
_Avoid_: Action, API call (for the durable request)

**Command inbox**:
The operational-store table where the local UI appends session commands and
the coordinator reads them in order. It is the only path from the browser to
the coordinator; there is no control socket.
_Avoid_: Queue (for run work), control socket

**Work tracker**:
The external system that holds the issues the factory claims, their run-state
labels, the status and command comments, and the optional coordinator lease.
It stays the source of truth for product intent. The coordinator reaches it
only through the tracker port. The adapters are GitHub and Azure DevOps
(Boards work items, with tags as labels).
_Avoid_: GitHub (for the concept), ticket store

**Code host**:
The external system that holds the run branch's pull request, its human
reviews, and the checkpoint commit statuses. The coordinator reaches it only
through the code-host port. The adapters are GitHub and Azure DevOps
(Azure Repos, where a reviewer vote is a review).
_Avoid_: GitHub (for the concept), forge

**Lifecycle observation**:
One coordinator read of the tracked issue and pull request that can identify
successful merge completion, an unmerged closure requiring cancellation, or a
newly submitted authorized review requesting changes. Lifecycle is always read
before any repair, readiness, or infrastructure work.
_Avoid_: GitHub lifecycle observation, screen scrape, hidden workflow transition

**Specification packet**:
The versioned, frozen statement of product intent for a run, consisting of the claimed issue snapshot, the selected workflow route, accepted clarifications or revisions, repository guidance captured at the run's base checkpoint, and any configured repository role-craft content captured at that same checkpoint.
_Avoid_: Live issue, prompt

**Repository guidance**:
Checked-in `AGENTS.md`, `CONTEXT.md`, and `docs/agents/*` guidance captured from the exact base checkpoint and supplied to roles as untrusted input. It can explain repository conventions but cannot redefine factory ownership, workflow stages, safety rules, or report contracts. An empty guidance set is valid.
_Avoid_: Issue body as guidance, live checkout guidance, repository-defined workflow

**Authority section**:
The factory-owned portion of a current embedded role body that declares role ownership, permitted paths, workflow-stage boundaries, safety rules, and report contracts. It remains authoritative over craft guidance and repository guidance.
_Avoid_: Undifferentiated role instructions, authority prose

**Craft section**:
The single factory-owned portion of a current embedded role body containing role-specific guidance about how to do the work. A repository may select replacement craft for a declared role through `role_craft`; the selected bytes are frozen at claim time. Craft guidance advises craft only; it cannot widen the frozen specification, move a workflow stage, change permitted paths, or alter the report contract, and the embedded authority remains authoritative.
_Avoid_: Craft prose, prompt customization, role instructions

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
red verification before implementation resumes. Frozen repository policy may
permit `retry_limits.test_revision` automated attempts before human review.
_Avoid_: Test rewrite, implementation-owned test edit, unbounded repair

**Workflow route**:
The factory-owned stage sequence an authorized issue author selects with one bounded frozen issue marker before claim. `acceptance` runs the independent test role before implementation; `design-acceptance` runs architecture, then the test role, then implementation. An absent marker follows the repository test policy. The route is recorded in the specification packet and is immutable for the run; it is never inferred from changed files, issue prose, or model judgment.
_Avoid_: Inferred workflow, repository-declared route, mode

**Baseline**:
The repository-declared setup and gate suite evaluated against the claimed run's base checkpoint before any agent edit; a blocking failure moves the run to failed preflight unless the frozen issue explicitly targets it. A healthy required-mode run enters the independent test stage; a healthy advisory run enters implementation-owned TDD directly unless the frozen issue selected a workflow route.
_Avoid_: Preflight assumption, agent diagnosis

**Setup fingerprint**:
The SHA-256 identity of the configured manifest and lockfile contents as committed at one exact checkpoint, for one run phase. Both the gate run that records it and the launch admission that checks it read that content from the checkpoint, never from the working tree, because the working tree moves on when an agent edits a configured setup file. A configured file the checkpoint does not track as a regular file has no fingerprint, and the baseline fails.
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
A complete pre-claim report of host configuration, external access, repository,
GitHub, Docker worker isolation, selected headless harnesses, authentication,
skills, and operational-store readiness. Every subsystem contributes its own
bounded check, and the doctor reports all failures before a run can start.
_Avoid_: First failure, mid-run diagnosis

**Gate**:
A repository-declared deterministic command whose result is tied to an exact checkpoint and does not depend on model judgment. Gates run inside the Worker; they are the only place for checks a repository requires, because factory Git operations on the host never run repository Git hooks (ADR 0015). A non-blocking gate is advisory: its command failure stays visible but neither fails the suite nor prevents readiness.
_Avoid_: Agent check, review, Git hook

**Worker**:
The per-run isolated execution environment that exposes only the run worktree, read-only Git metadata, the host directories of explicitly declared repository caches, and factory-managed credential copies.
_Avoid_: Container in workflow decisions

**Headless process seam**:
The ADR 0010 worker-owned boundary for every production harness process. A
`HeadlessRuntime` starts, resumes, inspects, cancels, and finishes one detached
process inside the isolated worker; it owns process state and bounded output,
while the coordinator receives only adapter-neutral lifecycle observations and
typed outcomes. It never starts a harness process on the coordinator host or
creates a coordinator-side process or local UI topology.
_Avoid_: Host-side agent, coordinator-owned PID, attached process

**Factory release**:
A versioned, published build of Software Factory: one archive with the
`factory` binary per supported host platform, `checksums.txt`, `release.json`,
and one worker base image. The coordinator of a release accepts only a worker
image built from the worker base image of the same Factory release.
_Avoid_: Queue release, version (alone)

**Worker base image**:
The digest-pinned image that a Factory release names and that every repository
worker image builds on. It carries both harnesses and the worker-only helpers,
and its label names its Factory release.
_Avoid_: Factory image, base tag

**Release resources**:
The onboarding procedure, its reference documents, the operator onboarding
skill, the worker skill set, and the smoke and verification scripts that the
`factory` binary embeds. `factory onboard prepare` exports them to a directory
that the operator owns, with a manifest that names the exact `factory`
executable and the resource identity; `factory onboard` exports them for one
launcher session.
_Avoid_: Factory checkout, assets

**Gate proof**:
A run of setup and every declared gate, before host registration, in the
pinned worker against a disposable copy of the candidate files. `factory
onboard verify` runs it with the production worker runtime and gate runner,
publishes nothing, and identifies it by an evidence ID that changes with the
candidate files, the gates, the history, and the worker digest. It does not
replace `factory doctor`.
_Avoid_: Gate check, dry run

**Onboarding plan**:
The persisted record of one repository onboarding, outside the target
checkout. It keeps the evidence of each onboarding step with the identities
of the inputs that evidence depends on, so `factory onboard status` can tell
a later agent which steps are verified, pending, blocked, stale, or skipped.
Only supported commands record evidence. It describes work; it is not a
workflow controller and not the operational store.
_Avoid_: Onboarding checklist, onboarding state machine

**Worker skill set**:
The curated craft skills the worker image installs into both role homes, pinned by the worker image digest and scoped per role by the embedded role prompts. A skill a role prompt mandates by name must also stay out of each harness's hidden-skill metadata, because a harness that withholds a skill from its model-visible catalog leaves that role unable to follow its own instructions.
_Avoid_: Personal skill, installed skill

**Worker skill smoke evidence**:
The recorded result of a real worker invocation in which one harness loaded and used the role-mandated skills, keyed by the immutable worker image digest and the harness version observed in that image. Startup reads the record instead of repeating the paid, nondeterministic model call, and a rebuilt image invalidates every record keyed by the previous digest.
_Avoid_: Skill test, live startup check

**Capture limit**:
The fixed per-stream byte limit the worker runtime buffers for one command,
inspection, or lifecycle operation. A stream that writes past it produces a
typed output-limit failure with no partial command result, and that failure
outranks ordinary exit-code classification.
_Avoid_: Truncation, output cap, log limit

**Worker limits**:
The host-owned memory, CPU, PID, and container-log bounds every worker
container starts with. A repository commit cannot raise them. A command or a
detached harness process that the memory limit kills is a typed
out-of-memory infrastructure failure, not an ordinary exit.
_Avoid_: Quota, resource config

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
The coordinator-owned repair context built from an authorized maintainer's
completed instructions: one or more applicable `CHANGES_REQUESTED` reviews,
holding their review bodies and inline findings, or one `/factory repair`
supervision comment holding its instruction. Concurrent applicable reviews form
one packet. Every human repair packet sits outside the bounded factory repair
rounds, never consumes the review-repair budget, and records the maintainer
source and event identity that produced it.
_Avoid_: Review reply, advisory feedback

**Gate failure diagnostic**:
The host-side record of the latest failed gate suite in one run phase,
preserving its exact checkpoint, setup observation, and bounded failing-command
output. Its lifecycle summary can appear in status; fuller output requires
explicit local inspection and carries no publication authority.
_Avoid_: Gate log, failure dump

**Gate output**:
The bounded host-side record of setup and every declared gate outcome at one
exact checkpoint, including passing gates. It is retained local diagnostic
content, not a complete history of repeated attempts or permission to publish
repository output.
_Avoid_: Complete gate history, support bundle

**Review diff**:
The exact base-to-checkpoint diff a review role judges. Every new review
invocation materialises it as the regular read-only `/invocation/review.diff`
file beside `specification.json`, including a zero-byte artifact. `review_context`
records only that stable worker path, the byte count, and the SHA-256 content
identity. Reviewers page the file with bounded line windows and line-numbered
search, so one very large changed file remains readable. Restart recovery
requires a regular file with the recorded size and SHA-256; a missing or
changed artifact is a recovery discrepancy and is not regenerated.
_Avoid_: Inline diff, diff limit

**Review round**:
The complete independent review of one immutable checkpoint against its base.
Its specification and documented-standards axes remain separate even when each
axis needs several review units.
_Avoid_: Reviewer invocation, review checkpoint

**Review unit**:
One bounded, manifest-assigned portion of a review diff judged by a fresh
invocation on one review axis. It partitions review workload without creating
another checkpoint, review round, or independently published result.
_Avoid_: Partial checkpoint, partial review

**Review-unit manifest**:
The durable ordered assignment of every changed line and non-text change in one
review round to exactly one primary review unit. Both review axes use the same
manifest, and bounded context may overlap between units.
_Avoid_: Review plan, live partition

**Review workload**:
The UTF-8 byte size of the self-contained diff evidence assigned to a review
unit, including its headers and overlapping context.
_Avoid_: Token count, changed-line count

**Review fan-out**:
The number of review units in one review round. Each configured review axis
runs one invocation for every unit in that shared manifest.
_Avoid_: Concurrency, review count

**Review concurrency**:
The number of review invocations one host runs at the same time across both
axes. It describes local machine and harness capacity, never review coverage,
and its ceiling is frozen when the review round is created.
_Avoid_: Fan-out, parallel review depth

**Authorized fan-out**:
The larger review fan-out a maintainer explicitly approves for one exact
manifest identity when it needs more units than the repository's normal
fan-out. It raises the unit count only: it never enlarges a unit, changes an
assignment, or repartitions the round.
_Avoid_: Fan-out override, unit budget increase

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

**Coordinator lease**:
The operator-visible projection of the coordinator heartbeat. It is optional
per tracker adapter; the Azure DevOps adapter has none. The GitHub adapter
publishes it as one closed milestone titled `factory coordinator lease` that
the coordinator account created. Every renewal rewrites only the factory block in its description with
the coordinator, the active run, and the complete renewal and expiry times. A
past expiry identifies stale ownership. The host lock, not the coordinator lease, is
the ownership authority.
_Avoid_: GitHub lease, lease status, `factory/lease` Commit Status, distributed lock

**Run activity**:
The published distinction between an active run whose next transition the
coordinator owns, an active invocation whose harness is executing, and a
waiting state. The `agent-running` label alone cannot express it.
_Avoid_: Agent-running label, run status

**Invocation**:
One immutable harness attempt within a run, with its own invocation packet,
factory prompt version, frozen repository role-craft source and SHA-256 identity
when configured, and native session identifier when known.
_Avoid_: Process transcript

**Harness**:
A configured coding tool, such as Codex or Claude Code, launched through the
headless harness seam with a role-specific prompt and native resume behavior.
_Avoid_: Lead agent, attached tool

**Role**:
The coordinator-owned responsibility assigned to an invocation, such as implementation, architecture, test, or review. The factory-owned role registry couples each role to its invocation stage, embedded Markdown prompt version, default permitted paths, and report contract; repository guidance cannot change role ownership.
_Avoid_: Persona

**Workflow registry**:
The factory-owned declaration of roles, embedded role prompt identities, stages,
and report-outcome transitions. Specification and documented-standards review
are separate concurrent axes with separate durable findings and statuses;
repository configuration selects harness, model policy, and optional craft
files for declared roles but cannot add or redefine workflow authority.
_Avoid_: Repository-defined workflow, prompt configuration

**Invocation packet**:
The read-only, versioned file containing the frozen specification, role identity, and any frozen repository role-craft content and source identity that the coordinator mounts into one worker invocation.
_Avoid_: Live issue

**Structured report**:
The schema-versioned, content-limited proposal written by `factory-report`; the coordinator validates it before making any workflow decision.
_Avoid_: Harness output

**Credential store**:
A factory-managed, harness-specific credential copy kept separate from role session state and never populated by mounting the host harness directory.
_Avoid_: Host auth mount

**Review blocker**:
A concrete violation that prevents readiness, on the axis that owns it. The specification axis blocks on correctness, security, and frozen-specification violations; the standards axis blocks on documented-standards violations. A finding that crosses into the other axis is advisory, as is a standards-review heuristic baseline finding: only a concrete violation of a named repository rule can block on the standards axis.
_Avoid_: Suggestion, preference, advisory finding

**Generated section**:
The coordinator-owned part of a pull-request body between
`<!-- factory-generated:start -->` and `<!-- factory-generated:end -->`. It
holds the run identity, checkpoint, gate results, review projections, and the
closing keyword. Each regeneration replaces only this section.
_Avoid_: PR template, factory block

**PR summary**:
The advisory markdown that the optional `pr_writer` role writes for one
hand-off checkpoint, with Summary, Evidence, and Merge Danger. The coordinator
saves it with the checkpoint, then places it directly above the generated
section between its own `factory-pr-writer` markers, with a note that an agent
wrote it. It explains the change to the human reviewer; the generated section
stays the factual record. It is never a gate, and a failed PR summary leaves
the hand-off with the generated section only.
_Avoid_: PR description, merge verdict

**Local evaluation summary**:
A content-free record of run outcomes, effort, escalations, and human dispositions retained locally to evaluate and tune the factory.
_Avoid_: Telemetry, transcript, audit log

**Outbound telemetry**:
Automatic transmission of product-usage or evaluation data from the operator's workstation to an external recipient. The factory prohibits outbound telemetry.
_Avoid_: Local evaluation summary

**Suspected Factory defect report**:
A local Markdown draft, prepared by `factory bug-report` from allowlisted evidence and completed by the operator, that describes a suspected defect in Factory itself. Only `factory bug-report submit` publishes it, to Factory's upstream tracker. It is not outbound telemetry and not a confirmed root cause.
_Avoid_: Bug bundle, crash report, telemetry

**Defect proposal**:
An optional, bounded field of a worker's structured report that proposes a suspected Factory defect report for its own Invocation. It changes no outcome and authorizes no publication.
_Avoid_: Defect outcome, worker bug report

**Pilot**:
A planned evidence-gathering delivery phase comparing the supervised factory with a direct-harness baseline. The measured pilot completed no matched comparisons and is not runtime authority.
_Avoid_: Production readiness, tracer bullet

**Host configuration**:
Host-local schema-version-2 YAML that registers the one repository, its GitHub
identity, authorized users, polling, credential sources, checked-in repository
configuration path, external operational-data location, and the cache root with
the host directory of every repository cache.
_Avoid_: Repository policy

**Repository configuration**:
Checked-in `factory.yaml` that declares the repository's target branch, setup, deterministic gates, harness and model policy for factory-declared roles, optional repository role-craft file selections, budgets, repository cache names, worker build, and base synchronization. It cannot declare roles, prompts, stages, or transitions, it cannot name a host path, and role craft cannot replace embedded authority.
_Avoid_: Host configuration

**Repository cache**:
One named directory the worker reuses between runs. Repository configuration
owns the name and the container-side purpose; host configuration owns the host
directory, which must resolve inside the operator's cache root. A cache that
one side declares and the other does not map is a startup diagnosis failure.
_Avoid_: Repository-declared cache path, default cache location

**Operational store**:
The versioned, host-local SQLite store for current run and ideation-session
state. Its current-state tables are separate from repository configuration, disposable run artifacts,
and the isolated local evaluation-summary projection.
_Avoid_: Event journal, telemetry, transcript archive

**Effect abandonment**:
The human-reviewed decision to discard one ambiguous pending effect after
inspecting its external outcome. It leaves a non-terminal run paused and a
terminal run's outcome unchanged.
_Avoid_: Run cancellation, automatic recovery

**Cleanup**:
The explicit, seven-day retention operation that previews and removes one or
more completed or cancelled runs' local worktrees, local branches, workers,
role sessions, generated outputs, and operational rows while retaining remote
branches, credential stores, and local evaluation summaries.
_Avoid_: Remote branch deletion, automatic summary deletion

**Reset**:
The explicit, whole-installation operation that returns one registered factory
installation to its pre-`init` local state: every run's worktree, local branch,
Git projection, generated outputs, workers, and role volumes, plus the
factory-managed credential volumes, the repository's coordinator lock, the operational database with
its sidecars and its own migration backups, the evaluation projection inside
that database, and the host configuration last. It is not retention: it has no
cutoff and selects every persisted run. It carries every non-terminal run to a
terminal outcome through the ordinary lifecycle projection before deleting the
store that identifies it, refuses a genuinely live run, and retains the source
checkout, installed binaries, worker images, repository caches, host credential
sources, GitHub history and label definitions, and remote branches.
_Avoid_: Cleanup, uninstall, deregistration, factory reinstall
