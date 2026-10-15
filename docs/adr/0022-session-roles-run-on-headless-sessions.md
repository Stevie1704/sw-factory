---
status: accepted
---

# Session roles run on headless sessions with explicit session ownership

Ideation sessions (ADR 0021) need two agent roles before any issue or run
exists: an ideation role that talks with the user and proposes brief updates,
and a planning role that turns a frozen brief into a draft plan graph. The
existing role, prompt, worker, harness, and report seams are run-shaped: the
invocation, packet, and report each require a run ID.

## Decision

1. **Session roles are declared beside the run registry.** `ideation` and
   `planning` are factory-owned session roles in `internal/workflow` with a
   versioned prompt, a report kind, the session states they start from, and
   explicit capabilities: a read-only repository snapshot, no worktree
   mutation, and no tracker mutation. No run stage selects them. Repository
   policy may select their harness, model, and effort in
   `role_harness_defaults`, `model_options`, and `reasoning_effort_options`.
   The roles are optional, so every existing `factory.yaml` stays valid; a
   session role without policy fails closed with a typed policy rejection
   (ADR 0002). Session roles need no new worker skill, so startup diagnosis
   and existing worker images are unchanged.
2. **Ownership is explicit; no run is fabricated.** A session-role invocation
   is a row of `ideation_invocations` (schema version 43). It names the session,
   the session revision it serves, the trigger it answers (the user turn, or
   the planning request), the brief and repository context revisions, the
   harness, model, and effort, the prompt version and SHA-256, the worker image
   digest that pins the skill set, the native session, and the snapshot and
   packet paths. The frozen packet (`/invocation/session.json`) and the
   structured report name `session_id` and `session_revision`; a session
   report must leave `run_id` empty. The worker and harness seams name worker
   resources from an owner key. For a session the coordinator derives the owner
   `session-<id>` and the worker `session-<id>-<role>`; that key names Docker
   resources only and is never stored as a run.
3. **Roles read a reproducible snapshot.** Each invocation fetches the target
   branch and adds a detached worktree at the fetched commit. The worker mounts
   it read-only, and the commit is the pinned context revision. The user's
   checkout is never touched, and the role has no tracker or code-host client.
4. **Reports carry structure; the coordinator applies it.** The ideation report
   carries a message, questions, an optional complete brief, and an optional
   readiness proposal (`ready_for_planning`, `investigation_needed`,
   `no_build`) that the user decides on. The planning report carries a draft
   plan graph with the node shape of a stored plan revision, or questions.
   The payloads travel as JSON files (`factory-report --ideation-file`,
   `--plan-file`) because they do not fit flags. The coordinator validates the
   report and applies the role turn, brief revision, and plan revision through
   one store write at the pinned session revision. A report from an older
   revision or from an ended invocation changes nothing, and partial unique
   indexes allow one active invocation per session and one accepted result per
   trigger. Plan graph policy (cycles, shape, limits) stays with plan
   validation behind a coordinator seam.
5. **Waiting releases work; recovery uses durable state only.** After an
   accepted result the coordinator finishes the native process, stops the
   worker, and removes the snapshot; the session waits for the user with no
   active work. The role volume stays, so the next ideation turn resumes the
   recorded native session on the same harness. The supervisor reconciles
   active invocations on every pass from the store and the worker-owned
   process state, so a result, a failure, or an interrupted launch is handled
   without a browser, a terminal, or the coordinator that started it.
6. **Failures reuse the typed vocabulary.** Expired authentication, capacity
   limits, unexpected exits, a native-session mismatch on resume, credential
   projection failures, interrupted launches, and rejected reports end the
   invocation with a recorded failure kind and return the session to the user
   with a visible reason. A planning role that ends without a draft keeps an
   earlier draft under review (`draft_kept`). Nothing retries automatically: the
   user resubmits the same turn, which the failed attempt did not consume.

## Consequences

The command dispatcher (#267) starts and cancels roles through
`Service.WithSessionRoles`. The ideation UI and its question-round guidance
(#269) and plan validation (#270) build on the report contracts and the
`ValidatePlan` seam. The `factory-report` binary inside the worker image must
include the session flags before a session role can report, so session roles
need a worker image built from this version. Reset and cleanup do not yet
remove the session-role Docker volumes and the snapshot worktree of an
invocation that was active when the coordinator stopped; a released
invocation leaves only its role volume and its packet directory.

## Considered options

- **Reuse `run_id` for the session ID.** Rejected: every run-shaped reader
  (recovery, cleanup, evaluation, status) would treat a session as a run.
- **A nullable session column on `invocations`.** Rejected: the run recovery,
  cleanup, and inspection paths would all need to learn to skip it.
- **Mount the user's checkout read-only.** Rejected: uncommitted work is not
  reproducible and would leak unpublished content into the role.
- **Retry capacity failures automatically.** Deferred: a session has a person
  waiting, so a visible reason and an explicit resubmission are simpler.
