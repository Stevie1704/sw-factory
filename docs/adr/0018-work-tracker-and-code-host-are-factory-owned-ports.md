---
status: accepted
---

# The work tracker and the code host are factory-owned ports

The coordinator, the effect journal, and the gate runner used the types and interfaces of `internal/github` directly. A second provider, such as Azure DevOps (#222), or a Jira tracker with a separate Git host, would have had to imitate GitHub types. The factory now owns two port packages, and `internal/github` is the first adapter that implements them. GitHub behavior does not change.

## Decisions

1. **Two ports, split by side.** `internal/tracker` holds the work-tracker side: the repository identity, the issue snapshot, labels as run-state projection, comments and their authors, the authenticated identity, the coordinator lease, and the readiness check. `internal/codehost` holds the code-host side: pull requests, human reviews, and commit statuses. Each package keeps the narrow interfaces that existed before, for example `IssuePoller`, `CommentReader`, `PullRequestClient`, and `CommitStatusPublisher`. A consumer therefore still receives only the authority it uses. A Jira tracker and a Git host can come from two adapters. Today the factory resolves every code-host seam from the tracker adapter when the composition root leaves it empty. A second code-host adapter fills the code-host seams of `factory.Dependencies` directly. GitHub and Azure DevOps provide both sides from one adapter. The code host refers to `tracker.Repository` because the factory registers one repository that both sides serve.
2. **Names stay neutral and stay the same.** Issue, comment, label, pull request, review, and commit status exist in GitHub and Azure DevOps, so the type names do not change. An author is the login that the adapter reports. A review verdict uses the existing state vocabulary; each adapter maps its provider values to it. The domain terms change where they named GitHub for a provider-neutral concept: "GitHub lifecycle observation" is now "lifecycle observation", and "GitHub lease" is now "coordinator lease".
3. **Stored payloads keep their JSON shape.** Pending-effect payload structs have no JSON tags, so their JSON keys are the Go field names. The port types keep every field name of the GitHub types that they replace. A payload journaled before this change therefore decodes without a schema migration or a payload version. A replay test with literal payloads in the old shape protects this. A later field rename is a stored-data change and needs a migration.
4. **The lease is optional per adapter.** ADR 0016 makes the host lock the ownership authority and the lease a diagnostic projection. An adapter that implements `tracker.LeaseClient` publishes the lease; for an adapter that does not, the coordinator skips renewal. The GitHub adapter keeps the milestone lease of ADR 0016, which now applies only to that adapter.

Only the composition root in `internal/cli` selects the GitHub adapter. `internal/factory`, `internal/effect`, and `internal/gate` do not import `internal/github`, and a test enforces this.

## Considered options

- One port package with one interface per capability. Rejected: it does not show which side a capability belongs to, and a tracker-only provider such as Jira needs that split.
- Versioned payloads that read the old GitHub shape. Rejected: the shape does not change, so a reader for an old version adds code without a use.
- A required lease. Rejected: a tracker without a fitting resource would need a workaround for a diagnostic value.
- A message broker between the coordinator and the adapters. Rejected: the pending-effect journal already owns ordering and replay, and a broker adds at-least-once delivery that the journal would have to reconcile.

## Local tracker extension

[ADR 0029](0029-local-markdown-content-and-local-execution-authority.md) adds a
committed Markdown tracker with independent code-host selection. Existing
combined registrations and serialized tracker payloads retain their identities.
The local adapter owns issue content/projections only; PR comments, reviews,
and statuses use the independently supplied code-host port, even when an issue
number happens to equal a PR number.

`tracker.CommittedIssueReader` exposes complete validated immutable snapshots to
the coordinator without importing the local provider. Source refresh imports
content identity and revision-addressed evidence. `Issue` and
`ListEligibleIssues` refresh first, so they can also allocate the identity
mappings of newly committed files. Only the preview path stays read-only: it
reads existing mappings and never allocates one. `codehost.ReadinessChecker` independently diagnoses PR,
review, and status requirements. The local source check never requests remote
issue labels, Boards work items, or issue-account inference.

Adapters declaring `tracker.SeparateCommentStreams` use a versioned pair of
issue/PR watermarks in the existing `ProcessedCommentID` field. Numeric targets
and comment IDs can coincide across streams without suppressing commands. The
combined remote adapters retain raw watermark values and their stored payload
shape. An explicitly targeted command with equal issue/PR numbers must name its
stream; polling supplies it from the reader that returned the comment.
