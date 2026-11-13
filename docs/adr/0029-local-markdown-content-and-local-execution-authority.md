---
status: accepted
---

# Local Markdown content and local execution authority

Spec #291 needs an issue tracker without a remote issue service. This extends
ADRs 0018, 0021, 0022, and 0024; it preserves their existing remote-provider
behavior and the immutable Specification packet of an active Run.

## Decision

Accepted local issue content is committed UTF-8 Markdown under `docs/issues/`.
The [local Markdown issue contract](../local-markdown-issues.md) fixes its
schema, identity, rendering, graph validation, and limits. Markdown is the
authority for content. SQLite is the authority for identity-to-integer
mapping, approvals, execution authorization, queue and lifecycle projections,
operator commands, and recovery. An indexed copy is evidence about a particular
committed revision, never a substitute for revalidating that source.

Tracker and code host are independently selected in private host configuration.
Omitted selectors preserve the existing combined GitHub or Azure registration.
A local tracker can use either supported code host. It makes no remote issue
calls; code-host PR, review, status, and Git transport requirements remain real.
The adapters stay behind the ports of ADR 0018. Local issue numbers and code-host
PR numbers are distinct streams even when their integers happen to match.

Publication is content creation only. The coordinator explicitly reserves stable
identities durably before rendering a preview. Preflight is read-only and cannot
allocate IDs or grant approval. A dedicated specification commit includes only
exactly approved files, in a factory-owned branch and separate worktree. It never
switches, stages, or commits the operator's ordinary checkout. Every Git operation
retains ADR 0015 hook and repository-helper suppression. Local commit is the
default; push and a specification PR need explicit scope displayed in the preview.
There is no implicit force-push, target write, merge, or branch-protection bypass.
Git batch publication extends the ledger through a narrow capability because a
series of per-item commits cannot provide atomic batch visibility.

The local operator surface extends ADRs 0021 and 0024 beyond session commands to
backlog and Run commands. Commands still enter a bounded, strict, protected,
durable inbox; only the coordinator consumes them. Each command identifies its
scope, idempotent request, expected revision, and exact displayed subject. Backlog
commands do not reopen terminal sessions. A clarification answer binds the Run,
question, and Specification revision. Exposed cancellation/retry/resume operations
must invoke existing coordinator semantics. No browser code performs Git effects,
starts a worker, or changes Run state. The existing Host/origin/token/loopback
protections and explicit supervisor-unavailable results apply to every mutation.

**Accept and commit** is publication-only. **Approve and start** adds a separate,
durable execution decision that binds the registered installation/repository,
host-local operator, stable identities, exact source bytes and complete graph,
preview/publication digests, and selected executable subset. Neither the Plan
publisher nor old GitHub/Azure approvals grant execution authority. No issue file
field or body command can establish an authorized operator. Discovering or
cloning files grants no authority. A separate installation must register and
explicitly approve them itself.

No item in a batch becomes claimable until the whole approved batch is committed
and its exact approved bytes and relationships are present in one fetched target
snapshot. Approval compares content rather than the specification commit SHA,
so an identical squash/cherry-pick is valid. Claim base and the frozen source
must come from that same immutable snapshot. Missing refs, conflicting files,
partial publication, ambiguous effects, and failed push/PR operations remain
visible with durable recovery evidence; retry adopts successful effects rather
than recreating branches, commits, PRs, or Runs.

Only explicitly authorized executable tasks can be scheduled. A grouping parent
never creates a Run or an implicit dependency. Investigations remain visibly
non-executable unless an existing route explicitly supports them. Dependencies
require successful lifecycle completion, normally an implementation PR merge;
review-ready, failure, cancellation, or an unmerged closure never satisfies one.
The one-non-terminal-Run rule remains authoritative through restart, input waits,
and infrastructure retries. Changed content or relationships before claim require
fresh approval; an active Run keeps its frozen Specification packet. A changed
prerequisite cannot release a dependent authorized against the old graph.

Admission reserves the exact claim packet and target base durably before Git
worktree creation, then atomically saves the Run and consumes the authorized
revision. Deterministic recovery adopts matching owned artifacts and cannot
reclaim a consumed issue/content in the same installation, repository,
code-host and target context. The claim key, the consumed-revision guard, and
the dependency-completion lookup exclude the graph digest: the graph digest
gates authorization validity only. An earlier decision also keyed claims by the
graph digest. That let an unrelated graph change hide a completed prerequisite
and let a fresh approval claim identical content a second time, which broke
"a dependency is satisfied by successful completion" and "a revision is never
claimed twice". Claim rows stored with the earlier graph-keyed key stay
readable, and the guard matches them by their fields. Consequence: a
cancelled or failed revision is also not claimable again after an unrelated
graph change; running it again needs a content change. Retained cleanup proof includes the actual merge
commit, which must be an ancestor of the same target snapshot as a dependent
claim. Observed preclaim source/graph/policy drift permanently invalidates that
grant; restoring old content requires a fresh explicit decision.

Active local packets retain exact source bytes and provenance independently of
the current graph; mutable lifecycle reads project against that frozen issue.
Local refresh/revision cannot replace the accepted source. Generated code-host
PR text uses local identities without remote integer-closing syntax, and unsafe
advisory generated references are omitted while human-owned sections survive.

Cleanup and reset retain committed source files and Git history. Reset removes
operational authorization, identity projections, commands, and execution records.
Recreating the store imports content only and never auto-runs retained files.

## Consequences

The local tracker removes GitHub Issues/Boards access requirements, not code-host,
model/network, or Git transport requirements. Azure Repos still uses `az` under
ADR 0019. If policy blocks that access too, doctor and the UI must report an
unsupported full issue-to-PR path before implementation. Fully local/manual
completion, browser automation, alternative Azure authentication, and changes to
company restrictions are follow-up scope.
