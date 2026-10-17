# Ideation sessions

An ideation session turns an idea into reviewed, linked backlog items in
GitHub Issues or Azure Boards before any issue or run exists. A human holds
the conversation, edits the brief and the plan, and approves one exact
publication in the local web UI. Publication creates inert backlog items: it
never adds `agent-ready` and never starts a run.

## Prepare the repository

Session roles are optional. A repository that wants sessions declares a
harness and a model for the roles `ideation` and `planning` in `factory.yaml`:

```yaml
role_harness_defaults:
  ideation: codex
  planning: codex
model_options:
  ideation: [<model>]
  planning: [<model>]
```

Without these keys a session role fails closed with a policy rejection. The
worker image must come from this release: session roles report with the
session flags of `factory-report`, and the ideation prompt names the
`grilling` worker skill. After a release upgrade, rebuild the worker image,
pin its digest, and record a skill smoke that includes `grilling`
(`MANDATORY_SKILLS=grilling` adds it to an existing record). Then run:

```sh
factory config validate --file factory.yaml --json
factory doctor
```

Tracker mapping:

- GitHub needs no extra key. The preview uses native sub-issues and blocking
  dependencies when the host offers them, and otherwise shows the textual
  fallback before approval.
- Azure DevOps maps plan kinds to work-item types with
  `azure_devops.planning` in the host registration. Without it, the Agile
  mapping applies: `parent` is an Epic, and `task` and `investigation` are
  User Stories.

## Run a session

```sh
factory start   # the coordinator; it alone applies session commands
factory ui      # the pages, on 127.0.0.1
```

Open `/sessions` in the browser on the same host:

1. Start a session from an idea. The ideation role reads a read-only
   snapshot of the target branch and asks questions in rounds.
2. Answer, correct an earlier answer, or edit the brief directly. The brief
   is the only planning input.
3. Hand off the shown brief revision to planning. Every unresolved question
   must be answered, deferred by name, or recorded as `investigation_needed`.
4. Review the plan on `/sessions/<id>/plan`. Edit items, kinds, parents, and
   dependencies, or ask for a new draft. Plan validation runs on every save.
5. Preview the publication. The coordinator renders it without a tracker
   write.
6. Approve and publish. The page then shows each created item, its link, and
   the ledger of every operation.

Defer, resume, no-build, and cancel are available while they apply. A session
that you defer during plan review resumes in review with the same plan, and one
that you defer after approval resumes with its approval; neither starts a new
planning run. No-build and cancel end the session; cancel never deletes tracker
items.

## What an approval covers

An approval binds the plan revision, the tracker target, the complete
rendered items (type, title, body, labels or fields, correlation key), every
relationship with its native or textual form, and each intermediate item.
The coordinator renders the preview again before it records the approval and
again before publication; any difference refuses the command before a
tracker write.

An approval does not authorize implementation, a run, the `agent-ready`
label or tag, a change to an existing tracker item, or a deletion. Approve,
publish, no-build, and cancel are human decisions; an operator agent does not
send them for the human (see `factory guide authorization`).

## Recover

| What you see | Action |
| --- | --- |
| HTTP 503 `unavailable` | No live coordinator. Run `factory start`, then submit again. Pages stay readable. |
| HTTP 409 `conflict` | Another tab or role changed the session. Reload the page and decide again. |
| Approval refused: the rendered publication changed since the preview | Preview again, review the new preview, approve. |
| Publication refused: the approval does not bind the current rendered publication | The tracker mapping or capability changed after approval. Save the plan again in the editor, preview, and approve. In `publication_incomplete` the plan cannot return to review; restore the mapping, or cancel. |
| Role failure `authentication_expired`, `credentials_unavailable`, or `rate_limited` | Fix the harness login or wait, then send the answer again or hand off again. A failed role is not retried automatically. |
| Role failure `plan_rejected` | The session reason lists every plan problem. Hand off again for a new draft, or edit the earlier draft when one exists. |
| `publication_incomplete` | Retry publication. It resumes the same approval and looks up every earlier create by its correlation key before it creates again. |
| An operation with an unknown outcome | Check the tracker for the item with the correlation key, then reconcile: adopt its ID, or confirm that none exists. Then retry. |
| The coordinator stopped during a command | Run `factory start`. Accepted commands continue; a stopped publication records an incomplete receipt. |

## Retention and cleanup

Sessions, revisions, approvals, and publication receipts stay in the
operational store with no expiry. `factory cleanup` never selects a session.
`factory reset` removes the store, and with it every session and its
publication evidence. Tracker items stay in the tracker.

Known gap: neither cleanup nor reset removes session-role Docker volumes
(`factory-role-ideation-*` and `factory-role-planning-*`), or the snapshot
worktree of a role that was active when the coordinator stopped. With the
operator's authority, list them with
`docker volume ls --filter name=factory-role-ideation-` and
`git worktree list`, and remove them after `factory reset`.

## Inspect

`GET /api/sessions/<id>` on the UI returns the durable session activity as
JSON: revisions, turns, approvals, receipts, ledger operations, and recent
commands. Conversation content stays on the host. It is not telemetry and is
never published to the tracker.
