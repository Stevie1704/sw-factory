# GitHub plan publication

This page describes how the GitHub adapter publishes an approved ideation
plan (#273, ADR 0022), and how to prove it against a disposable repository.

## How a plan maps to GitHub

The GitHub adapter (`internal/github/plan.go`) implements
`tracker.PlanPublisher` through the authenticated `gh` CLI. It uses no other
credential.

| Plan element | GitHub representation |
| --- | --- |
| Plan node | One issue. The title is the node title. The body contains the node body, the acceptance criteria as a task list, a `Traceability` section (session, plan revision, node, context revision, brief items), and, as its last line, the hidden marker `<!-- factory-plan: <session>/<plan revision>/<node> -->`. |
| Single-item plan | One issue, no relationships. |
| Parent and children | The parent specification issue and one issue per child. No milestone is created; the parent is the grouping artifact. |
| Parent edge, native | The child becomes a GitHub sub-issue of the parent (`POST /repos/{o}/{r}/issues/{parent}/sub_issues`). |
| Dependency edge, native | The dependent issue becomes blocked by the other issue (`POST /repos/{o}/{r}/issues/{dependent}/dependencies/blocked_by`). |
| Any edge, textual fallback | The issue bodies contain a `Plan relationships` section (`Part of:`, `Depends on:`, and a `- [ ] Sub-issue:` task list on the parent). After creation, one comment on the sub-issue or the dependent issue says `Part of #N` or `Depends on #N` and carries the marker `<!-- factory-plan-link: <kind> #<from> -> #<to> -->`. GitHub shows the mention as a backlink on the other issue. |
| Labels | None. The adapter adds no label, and it refuses every factory state label, `agent-ready` included. Existing repository labels stay unchanged. |

### Preflight

Preflight only reads. It runs before approval and again before the first
write, and the same plan against an unchanged repository renders the same
bytes.

1. `gh api repos/{o}/{r}`: the account must be able to read the repository,
   the repository must not be archived, and issues must be enabled. A failure
   is a typed `permission` or `unsupported_capability` error.
2. When the plan has relationships: a GraphQL schema introspection of the
   `Issue` type. The field `subIssues` makes parent edges native. The field
   `blockedBy` makes dependency edges native. A missing field selects the
   textual fallback for that kind. The preview and the approved
   relationship mapping show the mode, so publication never changes it after
   approval. If the capability changes later, the next preflight renders
   different bytes, and publication stops with a stale-approval error.
3. Native link access: when the plan uses a native sub-issue or blocking
   dependency, the repository response must grant `triage`, `push`,
   `maintain`, or `admin`. Native links need triage access or higher, and a
   read-only account can open issues but cannot link them. Without that
   access, preflight returns a typed `permission` error before approval. It
   does not switch to the textual fallback, because the host offers the
   native relationship.
4. Plan text: a node title, body, or acceptance criterion that contains
   `<!-- factory-plan` is a typed `invalid_mapping` error. Only the factory
   writes its markers, so plan text cannot make the correlation lookup or
   the link verification match the wrong issue.
5. Limits: a title longer than 256 characters or a body longer than 65,536
   characters is a typed `invalid_mapping` error. With native sub-issues, more
   than 100 sub-issues for one parent or more than eight nested levels is a
   typed `unsupported_capability` error.

GitHub does not document a count limit for links. A rejected link at
publication time is an HTTP 403 or 422, which the adapter reports as a typed
no-change error.

### Correlation and reconciliation

`FindPlanItems` lists the issues that the authenticated account created
(`GET /repos/{o}/{r}/issues?state=all&creator=<login>&sort=created&direction=desc`),
at most 10 pages of 100, and matches the exact marker on the last nonblank
line of the body. It does not use the
search index, because the index lags behind a just-created issue. Pull requests
and issues by other accounts never match. An issue outside the 1,000 newest
issues of the account is not found. The publication ledger never creates an
item again after an unknown outcome without an operator decision, so the
bound cannot cause a blind duplicate.

`FindPlanRelationship` reads the parent's sub-issues, the dependent's
blockers, or the comments of the sub-issue or dependent issue by the
authenticated account.

### Failure classification

| gh result | Planning error | Meaning for publication |
| --- | --- | --- |
| Deadline expired (`hostcmd.TimeoutError`) | `ambiguous` | Unknown outcome; reconcile before a retry. |
| HTTP 401, 403, 404 | `permission` | GitHub made no change. |
| HTTP 410 | `unsupported_capability` | GitHub made no change. |
| HTTP 422 | `invalid_mapping` | GitHub made no change. |
| Rate limit, HTTP 5xx, any other failure | unclassified | Unknown outcome; reconcile before a retry. |

## Disposable-repository acceptance procedure

Scripted coverage: the tests in `internal/github/plan_test.go` and
`internal/github/plan_publication_test.go` drive the adapter and the real
publication service against a scripted `gh`.

**Live evidence: not run.** Creating issues on GitHub is an outward action
that this change was not authorized to perform. Record the live result here
when an operator runs the procedure.

### Preconditions

- A disposable repository that you own, for example
  `<you>/factory-plan-acceptance`, with issues enabled. Do not use a
  repository that has real backlog items.
- `gh auth status` shows the account that the coordinator uses.
- A Factory build that contains #266, #267, #271, #272, and #273, registered
  against the disposable repository.

### Steps and expected artifacts

1. **Single issue.** Make a session whose plan has one node, preview it, and
   approve it. Publish.
   - Expected: one new open issue. The body ends with the
     `Traceability` section and the hidden `factory-plan` marker. The issue has
     no labels. The receipt is `published` and has the issue URL.
2. **Parent and children, native.** Make a plan with a parent node `P`, two
   children `A` and `B`, and `B` depends on `A`. Preview: every relationship
   has mode `native`. Approve and publish.
   - Expected: three issues created in the order P, A, B. A and B are
     sub-issues of P (shown in the parent's sub-issue panel). B shows
     "Blocked by" A. No comments and no labels. The receipt has three URLs and
     three relationships.
3. **Retry after an uncertain result.** Repeat step 2 with a new session.
   Interrupt the coordinator directly after the first issue create request
   (for example, stop the process when the ledger shows `attempted`). Start
   it again and publish again.
   - Expected: no second issue for that node. The ledger adopts the issue
     by its marker or stops as `ambiguous`; after reconciliation, publication
     finishes with exactly three issues.
4. **No execution authorization.** For every created issue, run
   `gh issue view <n> --json labels`.
   - Expected: no `agent-ready` and no other factory state label. No run
     starts (`factory status` shows no active run for these issues).
5. **Textual fallback.** Only on a GitHub host without sub-issues or
   dependencies (for example an older GitHub Enterprise Server): preview the
   step 2 plan.
   - Expected: every relationship has mode `textual`, and every body shows
     the `Plan relationships` section before approval. After publication,
     A and B each have a `Part of #P` comment, and B has a `Depends on #A`
     comment. On github.com, record this step as not applicable.

Record for each step: date, Factory revision, `gh --version`, the issue
URLs, the ledger states, and whether the result matches.

### Cleanup

1. Delete every created issue: `gh issue delete <n> --repo <repo> --yes`
   (deleting an issue needs administrator access; otherwise close the issues
   with `gh issue close`).
2. Delete the disposable repository when you do not need it again:
   `gh repo delete <repo> --yes` (needs the `delete_repo` scope).
3. Run `factory reset` against the test host configuration if you want to
   remove the test sessions; sessions with publication evidence stay until
   then (ADR 0021).

| Step | Live result |
| --- | --- |
| 1 Single issue | not run |
| 2 Parent and children, native | not run |
| 3 Retry after an uncertain result | not run |
| 4 No execution authorization | not run |
| 5 Textual fallback | not run |
