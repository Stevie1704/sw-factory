---
status: accepted
---

# Ideation sessions are a separate aggregate with a local operator surface

Specification #265 adds conversation, briefing, planning, approval, and
publication for work that exists before an issue or an implementation run.
Two current contracts do not cover it. ADR 0010 makes GitHub (and, through ADR
0018, the work tracker) the only durable operator surface, and it has no
control socket. The [local web UI contract](../configuration.md#local-web-ui)
is read-only: the UI never changes the operational store. This record extends
both contracts and defines the data, lifecycle, and retention of sessions.

## Decision

1. **A session is not a Run.** An ideation session is a repository-scoped
   aggregate with its own tables in the operational store (schema version 42):
   `ideation_sessions`, `ideation_turns`, `ideation_brief_revisions`,
   `ideation_plan_revisions`, `ideation_approvals`, and
   `ideation_publication_receipts`. A session needs no remote issue, no branch,
   no worktree, and no run row. The migration only adds tables, so stored runs
   stay readable. Role invocations for a session reference the session and its
   revision; they never fabricate a run.
2. **The workflow layer declares the lifecycle; the store persists it.** The
   store has a closed set of session states. `workflow.SessionLifecycle`
   declares which event moves which state to which state, as the workflow
   registry does for run stages (ADR 0003). Repositories cannot change it.
   Session states are separate from run statuses:

   | State | Meaning |
   | --- | --- |
   | `awaiting_user` | The session waits for the user. No role is active. |
   | `role_active` | An ideation or planning invocation is in flight. |
   | `draft_review` | A plan draft waits for review. |
   | `approved` | An approval binds the current draft. Publication has not started. |
   | `publishing` | Publication of the approval is in progress. |
   | `publication_incomplete` | Publication stopped before every operation succeeded. Retry or reconcile continues the same approval. |
   | `published` | Terminal. Every approved item and relationship exists. |
   | `deferred` | The user parked the session. It resumes where it was parked: in the conversation, in plan review with the same plan, or approved. |
   | `no_build` | Terminal. The user decided not to build. |
   | `cancelled` | Terminal. Cancellation never deletes tracker items. |

   ADR 0026 adds `investigation_needed`: the user decided that material
   unknowns need investigation first. The session can resume.

3. **Every write names the revision it read.** Each accepted write advances the
   session revision by one inside one SQLite transaction. A write with an older
   expected revision fails with a typed revision conflict, so a second browser
   tab or a late role report cannot overwrite newer work. Turns, approvals, and
   receipts carry a caller-chosen ID that is also the idempotency key: a retry
   with the same ID and content returns the stored record and appends nothing;
   the same ID with different content is refused.
4. **Briefs and plans are append-only revisions.** A plan revision records its
   source brief revision and its repository context revision, and its nodes
   have stable local IDs, a kind, parent and dependency references, and
   references to brief items. A newer brief makes older drafts stale at read
   time; their stored content never changes.
5. **An approval binds the exact publication.** An approval stores the plan
   revision, the target, the fully rendered publication payload, and the
   relationship mapping, with SHA-256 digests of both byte sequences.
   Publication compares its freshly rendered values with the approval and does
   not re-render approved content. A new brief or plan revision clears the
   current approval in the same transaction, so any change before publication
   needs a fresh approval. Rendering and provider details stay with the
   publication work (#272); the store keeps what it receives.
6. **Local sessions extend the operator surface of ADR 0010.** For sessions,
   the persisted local conversation, brief, drafts, and approval commands are
   durable operator surfaces next to the tracker. Headless harnesses remain the
   only control plane and structured reports remain the only role-result
   protocol. The browser reaches the coordinator through a durable command
   inbox in the operational store, not through a control socket: the UI
   validates a request and appends one command row with a unique request ID and
   an expected revision through a narrow write handle (#267). The coordinator is
   the only process that consumes commands and changes session state. When the
   supervisor heartbeat is not live, the UI returns an explicit unavailable
   result and starts no second writer. Thus the UI stays read-only for runs and
   for session state; appending a command row is its only write.
7. **Retention keeps evidence until an explicit reset.**
   - Sessions have no automatic expiry. `factory cleanup` selects only terminal
     runs and never deletes session rows.
   - Open and deferred sessions stay until the user closes them.
   - Unpublished closed sessions (`cancelled`, `no_build`) keep their turns,
     brief and plan revisions, and approvals for inspection and recovery. A
     later deletion command may remove such a session as a whole. It must
     refuse a session that has publication evidence.
   - Publication evidence is every approval of a session that started
     publication, its publication receipts, and the per-operation publication
     ledger (#272). It stays while the session exists, because recovery needs
     it to reconcile before a repeated create.
   - `factory reset` removes the operational store and with it every session
     and all publication evidence. Tracker items that publication created stay
     in the tracker.
   - Session content stays on the host. It never enters evaluation summaries
     or any outbound telemetry (ADR 0001).

## Consequences

Coordinator commands (#267), session roles (#268), plan validation (#270),
review (#271), and publication (#272) build on these types in
`internal/store` and on `workflow.SessionLifecycle`. The "Ideation sessions"
part of the local web UI contract in `docs/configuration.md` records the UI
extension; ADR 0024 records the command inbox and routes (#267). Plan
graph policy (cycles, hierarchy shape, size limits) belongs to plan
validation, not to the store, which checks only identity and reference
integrity. Because sessions have no automatic expiry, a busy installation can
grow its store until a deletion command or a reset exists; session rows are
small text and the risk of losing publication evidence is worse.

## Considered options

- **A placeholder run or issue per session.** Rejected: it forces session
  outcomes into the run lifecycle and creates tracker objects before approval.
- **A generic workflow-definition language.** Rejected: ADR 0003 keeps workflow
  semantics factory-owned and typed; one small declaration is enough.
- **A local control socket from the UI to the coordinator.** Rejected: ADR 0010
  has no control socket, a socket loses requests on a crash, and the UI must
  keep working while the coordinator is stopped.
- **Deleting unpublished sessions with run cleanup after seven days.**
  Rejected for now: a deferred or cancelled conversation is user work with no
  external copy, and cleanup has no session preview yet.
