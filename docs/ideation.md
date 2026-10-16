# From an idea to a reviewed backlog

This guide is for the person who uses ideation sessions in the local web UI.
It covers the configuration, the user flow, what one approval authorizes, how
to recover from interruptions, and how long session data stays. `factory
guide ideation` prints the same procedure for an operator agent.

The decision records are [ADR 0021](adr/0021-ideation-sessions-and-local-operator-surface.md)
(sessions and the local operator surface),
[ADR 0022](adr/0022-plan-publication-ledger-and-tracker-planning-port.md)
(publication ledger and tracker planning port),
[ADR 0023](adr/0023-session-roles-run-on-headless-sessions.md) (session
roles), [ADR 0024](adr/0024-browser-session-commands-through-a-durable-inbox.md)
(browser commands), [ADR 0025](adr/0025-azure-devops-plan-publication-mapping.md)
(Azure DevOps mapping), and
[ADR 0026](adr/0026-ideation-readiness-is-a-user-decision-on-the-displayed-brief.md)
(readiness is the user's decision). This guide does not restate them.

## The read-only UI contract is extended

Before sessions, `factory ui` was a read-only view: it never wrote to the
operational store, and the work tracker was the only durable operator
surface. Sessions change this on purpose, and only this far:

- The UI can append one kind of record, a session command, to the command
  inbox in the operational store. It never applies a command and never
  changes session or run state.
- The coordinator that `factory start` runs is still the only writer of
  session state and the only process that talks to the tracker.
- The browser pages of a session (conversation, brief, plan review, preview,
  approval) are durable local operator surfaces in addition to the tracker.
  Run pages and run evaluation pages stay read-only.

The exact command API, its HTTP answers, and its request protection are in
[Local web UI](configuration.md#local-web-ui).

## Configure

| What | Where | Reference |
| --- | --- | --- |
| Harness and model of the `ideation` and `planning` roles | `factory.yaml`: `role_harness_defaults`, `model_options`, optional `reasoning_effort_options` | [Factory-owned role, prompt, and stage registry](configuration.md#factory-owned-role-prompt-and-stage-registry) |
| A worker image from this release, with the `grilling` skill in its smoke record | `factory.yaml`: `worker_build` | [Worker image build and digest pinning](configuration.md#worker-image-build-and-digest-pinning) |
| Azure Boards work-item types per plan kind | Host registration: `azure_devops.planning` | [Plan publication mapping](configuration.md#plan-publication-mapping) |
| GitHub | Nothing extra; the registered repository is the target | [Session roles](agent-runtime.md#session-roles) |

A repository without the two role keys can still run issues; only session
roles fail closed. Check the result with `factory config validate` and
`factory doctor` before the first session.

## Use a session

Keep `factory start` running, run `factory ui`, and open `/sessions`.

1. **Idea.** Write the idea in the new-session form. The ideation role reads
   a read-only snapshot of the target branch, never your checkout.
2. **Questions.** Each round shows questions with a recommended answer and
   the facts the role read. Answer one field per question. To change an
   earlier answer, mark the new answer as its correction; the role then
   revisits the decisions that depend on it.
3. **Brief.** The brief (problem, audience, success criteria, scope,
   non-goals, decisions, assumptions, evidence, unresolved questions) is the
   only input of planning. Edit it directly when the role got something
   wrong.
4. **Readiness.** The role proposes a readiness; you decide. Hand off the
   brief revision you see, and name every unresolved question you defer. You
   can instead record that investigation is needed, defer the session, or
   decide not to build.
5. **Plan.** The planning role drafts items of the kinds `parent`, `task`,
   and `investigation`, with parents and dependencies. On
   `/sessions/<id>/plan` you edit, split, remove, and relink items, or ask
   for a new draft. A deferred question usually becomes an investigation
   item.
6. **Preview.** The coordinator renders the exact publication for the
   configured tracker without writing to it.
7. **Approve and publish.** One approval publishes the preview you saw. The
   page shows each created item with its link, and the ledger of every
   operation.

## What one approval authorizes

The approval names the plan revision and the digests of the preview. The
preview is the complete publication: the target, each item's type, title,
body, labels or fields, and correlation key, each parent and dependency link
with its native or textual form, and each intermediate item that the tracker
hierarchy needs. If any of it differs when the coordinator records the
approval or starts publication, the command is refused and nothing is
written.

The approval authorizes the creation of those items and links, once. It
does not authorize `agent-ready`, an implementation run, a change or
deletion of an existing tracker item, or a publication under a different
mapping. Implementation starts only when an authorized person marks a
published issue or work item `agent-ready` later, through the normal issue
flow.

## Recover

- **The page says the coordinator is unavailable.** Run `factory start` and
  send the decision again. Nothing was stored.
- **The page reports a conflict.** Another tab or a role changed the session
  first. Reload and decide on what you now see.
- **You closed the browser or the UI stopped.** Open the session again. Every
  accepted turn, brief and plan revision, approval, and receipt is in the
  store; a sent command shows its outcome.
- **The coordinator stopped.** Start it again. Accepted commands continue.
  A role that was active is reconciled; its result is applied once or its
  failure is shown.
- **You clicked twice or resent a form.** The request ID makes the command
  idempotent: the second send returns the first outcome, and no role starts
  and no tracker item is created a second time.
- **The approval is refused as stale.** Preview again and approve what you
  now see. When publication is refused because the tracker mapping changed
  after approval, save the plan again in the editor; this returns the plan
  to review, so you can preview and approve it. An incomplete publication
  cannot return to review: when its mapping changed, a retry is refused and
  only cancel remains. Items that already exist stay in the tracker.
- **Publication is incomplete.** Retry it. A retry resumes the same approved
  content and looks for each earlier item by its correlation key before it
  creates anything. When an item create has an unknown outcome and no item
  is found, check the tracker and reconcile: adopt the item, or confirm that
  none exists.
- **You cancelled.** Cancelling stops role work and future publication work.
  Items that already exist stay in the tracker.

## Retention and cleanup

Session data stays in the operational store until `factory reset`: turns,
brief and plan revisions, approvals, publication receipts, and the
publication ledger. `factory cleanup` does not select sessions. A published
session is evidence of what was approved and created; keep it while the
items matter. See [Run-artifact cleanup](configuration.md#run-artifact-cleanup)
and [Complete local reset](configuration.md#complete-local-reset).

Neither command removes the Docker role volumes of session roles yet
(`factory-role-ideation-*` and `factory-role-planning-*`), nor the snapshot
worktree of a role that was active when the coordinator stopped. Remove them
by hand after a reset, with the operator's authority.

## Evidence

The release evidence for this feature is in
[ideation acceptance](https://github.com/Stevie1704/sw-factory/blob/main/docs/acceptance/ideation.md).
It lists the deterministic scenarios that ran, the live provider scenarios
that did not, and the baseline for plan edits and clarification requests.
