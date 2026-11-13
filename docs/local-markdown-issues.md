# Local Markdown issue contract

Accepted specifications live as regular UTF-8 Markdown files directly under
`docs/issues/`. Drafts and conversations remain in the operational store. This
contract is implemented by `internal/localissues` and the durable identity
mapping in `internal/store`; [ADR 0029](adr/0029-local-markdown-content-and-local-execution-authority.md)
records ownership and execution rules.

## Version 1 format

```markdown
---
schema_version: 1
id: local-00112233445566778899aabbccddeeff
title: Add CSV export
kind: task
dependencies: []
publication_correlation: session-id/1/export
---

## Problem

Operators need a portable export.

## Scope and non-goals

Export CSV. PDF output is excluded.

## Acceptance criteria

- The export downloads a valid CSV file.
```

`schema_version`, `id`, `title`, `kind`, `dependencies`, and
`publication_correlation` are required. `parent` is the only optional field and
references a stable issue identity. Allowed kinds are `parent`, `task`, and
`investigation`. Dependencies are a sequence of stable issue identities. The
correlation is the stable plan correlation (`<session>/<plan revision>/<node>`)
or a separately reserved import correlation. It is not an execution command or
an authorization identity.

Front matter has one YAML mapping. Unknown or duplicate fields, aliases,
anchors, merge keys, nulls, incorrect scalar types, and unsupported schemas are
errors. Metadata contains no operator, approval, progress, timestamp, transcript,
credential, or log fields. The body has nonempty `## Problem`,
`## Scope and non-goals`, and `## Acceptance criteria` sections, in that order.
It uses LF line endings and a final newline. The canonical renderer emits the
front-matter fields in the order above, inserts optional `parent` after `kind`,
sorts dependencies by identity, uses two-space YAML indentation, and emits one
blank line before the unchanged body. Repository content is always untrusted
input; apparent `/factory` commands in a file cannot authorize or control a Run.

## Stable identities and paths

A new ID is `local-` followed by 32 lowercase hexadecimal characters from 128
random bits. Allocation is an explicit coordinator write before preview, keyed
by repository path and publication correlation. Retrying the same correlation
returns its earlier ID and integer. Read-only lookup, preview, and diagnosis
never allocate an identity, rewrite a file, or change approval.

The initial filename is `docs/issues/<id>.md`. A rename may use another direct
child filename of at most 128 ASCII characters, beginning with an alphanumeric
character and followed by alphanumerics, `_`, `-`, or `.`, ending in `.md`.
Identity is read from front matter, never inferred from a title or path. Nested
paths, traversal, backslashes, symlinks, non-regular files, and exact or
case-insensitive filename collisions are validation errors. Committed-source
readers also reject symlink/submodule modes and unavailable source refs.

Tracker interfaces retain their existing integer payload fields. SQLite maps
`(repository path, stable ID)` and `(repository path, publication correlation)`
uniquely to a positive, sequential 31-bit integer; there is no hash truncation.
The mapping survives process restart and file rename. Conflicting identity or
correlation imports fail instead of overwriting an existing mapping. Numbers
belong to this installation's operational store; after reset a new import may
assign different numbers because the former Runs and effects no longer exist.
Stable file IDs remain unchanged. A mapping is content identity only and grants
no execution authorization.

## Validation and revision identity

One file is at most 1 MiB including front matter, the complete graph at most
256 issues and 16 MiB, a title at most 256 UTF-8 bytes, and a correlation at most
512 UTF-8 bytes. Invalid UTF-8, NUL/control characters in metadata, missing
content, and oversized input are errors, never silently truncated.

Validate the complete graph before exposing any of it: reject duplicate IDs or
correlations, duplicate/self dependencies, dangling references, parents that
are not grouping parents, and any cycle across parent/dependency edges.
Removing or declining an item requires repairing all incoming references.
Grouping does not create an implicit scheduling dependency.

A content digest is `sha256:` followed by the lowercase SHA-256 of the exact
file bytes, including metadata and body. The graph digest hashes the
concatenation, in ascending stable-ID order, of `<id>`, a NUL byte,
`<content digest>`, and LF. Thus it binds all content and relationships but
survives a byte-identical filename rename and enumeration-order changes.
Canonical rendering is used for new approved publications; existing committed
files are compared by their actual bytes, never silently canonicalized.

The immutable Git target snapshot is the runtime content authority. An index
names the snapshot, source path, content digest, and graph digest; it must be
revalidated before claim. Working-tree edits are not accepted implicitly.
Exact bytes and graph can become target-visible through merge, squash, or
cherry-pick without matching the original specification commit SHA. The claim
base and Specification source use the same immutable target snapshot.

## Publication and authority

The preview includes exact bytes and destination paths, stable IDs and graph,
context identity, the factory specification branch, and the publication policy.
A dedicated commit in a separate factory-owned specification worktree contains
only approved changes. The operator's staged, unstaged, and untracked changes
stay untouched. Default publication stops at a local commit with a manual
push/PR/merge path; push and a specification PR are separate explicitly scoped
effects. No implicit direct target writes or merge authority are supported.

Publication-only acceptance never starts execution. Explicit approve-and-start
records a separate installation-local authorization for the displayed exact
revision and executable subset. It waits for complete batch publication and
exact target visibility, then uses dependency order and the ordinary one-active-
Run lifecycle. Changed unclaimed content or relationships need fresh approval;
active Specification packets stay frozen. A dependency needs successful Run
completion, normally PR merge. Parents do not execute; investigations need an
explicitly supported route. Imported files and restored Git history need fresh
local registration and authorization.

### Exact preview and publication recovery

The rendered item body is the complete Markdown file, including front matter.
The displayed fields include `path`, `local_id`, the installation-local tracker
`number`, `content_digest`, immutable `source_revision`, `base_graph_digest`,
`graph_digest`, `specification_branch`, `target_branch`, and `publication_policy`.
The resulting graph digest covers **all original committed issues plus every
approved addition**, excluding mutable paths. This deliberately binds existing
prerequisites as well as the new batch. Unrelated accepted-source changes can
require a fresh exact preview and approval; they never silently widen authority.

Policies are `local_commit` (default), `push`, and `pull_request`. Each policy is
part of the approved payload. The standard local UI uses local-commit publication
and presents the manual publishing path. Optional host integrations may select a
push or draft specification PR policy before preview, which must then be
explicitly approved. No policy includes merge authority. PR publication requires
the configured code host's unambiguous source/target lookup capability.

The factory branch is `factory/specifications/<full SHA-256 of session/revision>`.
Its separate worktree contains only approved issue changes; publication refuses
foreign worktrees, unsafe directories, unrelated staged paths, transformed Git
filter bytes, or a conflicting branch/remote ref. A successful commit is
identified by its exact parent, complete changed-file set, content/graph digests,
and publication marker. Retrying adopts that commit rather than creating another.
Commit signing and Git transport use the host's normal configuration, with
repository hooks and fsmonitor disabled as required by ADR 0015.

The publication ledger's `git/batch` entry records each commit, push, and optional
PR intent and its evidence before the next external effect. Later item/link rows
are evidence about the same atomic batch. A partial or ambiguous batch leaves an
incomplete receipt and releases none of its items for execution. A lost response
is recovered by inspecting the branch and optional remote effects. If an attempted
effect is absent or several PRs match, publication stops; inspect Git/code-host
history before explicitly confirming the pending effect absent or adopting the
complete matching commit. If a previously recorded commit ref moved or vanished,
restore it before retrying; absence does not permit manufacturing a replacement.

A successful local commit can still be **waiting for specification merge**.
Publish its displayed branch using your ordinary Git/code-host review process,
then merge it through the target branch's policy. Factory never bypasses that
policy. Exact byte/graph comparison against one fetched target snapshot accepts
identical squash/cherry-pick content and rejects altered specifications.

Operational approvals, queue state, commands, local labels/comments, and recovery
evidence live only in SQLite. Cleanup/reset retain committed Markdown and Git
history; reset removes operational authority. Recreating the store never
reconstructs execution permission from files. The local tracker removes remote
issue access only: Git transport, model/network access, and code-host PR/review/
status access remain necessary; Azure Repos still requires `az` (ADR 0019).

## Protected local execution decisions and Run controls

The existing command inbox now has explicit `session`, `backlog`, and `run`
subjects. Session is the default for old requests. A backlog or Run command has
its own `subject_id` and no session ID, so a terminal ideation session is never
reopened to hold a later authorization or answer. The protected origin/token/
Host/loopback checks and supervisor heartbeat apply to every scope. Repeated
request IDs return their durable original result even while the supervisor is
stopped; a new stopped-supervisor mutation returns unavailable.

`approve_start` is a session command that accepts the same exact publication
digests as publication-only `approve`, plus an explicit selected set of stable
executable issue IDs. The coordinator records the publication approval, then a
separate installation-local execution decision, then uses the ordinary content
publisher. Incomplete publication cannot become claimable. Existing remote
approvals remain publication-only, and an adapter without the explicit local
authorizer reports approve-and-start as unsupported.

For already committed work, `preview_execution` creates a fresh exact target
preview and `authorize_execution` binds that displayed preview's revision,
content/graph/relationship digests, executable subset, registration, and target
policy. This path works independently of any originating session. Committed
imports, factory labels, and a preview alone grant no execution authority.
The complete validated graph is conservatively bound: unrelated accepted source
changes also require a new preview and approval for pending work. Active Runs
retain their frozen Specification packets.

Exact source bytes are retained in the read-only preview projection, bounded by
the 16 MiB source-graph contract and a 32 MiB serialized-evidence ceiling. The
command outcome contains a small preview reference and digests instead of
copying the whole graph into the 4 MiB command-result allowance. Nothing is
silently shortened. Revalidation checks stable integer mappings, exact source
and relationships, selected task kinds, installation/operator/code-host identity,
and the current target branch before recording authority.

Run controls expose only supported `answer_run`, `cancel_run`, `retry_run`, and
`resume_run` commands. Every operation binds the displayed Run revision and
Specification version; answers also bind the pending question ID and exact
question digest. The coordinator supplies local operator provenance from private
registration. Browser payloads and issue content cannot supply an author or
installation identity. Unsupported recovery states are refused using the
ordinary Run semantics. Cancellation applies to existing Runs and to cancellable
ideation/publication sessions; there is no fabricated Run or queue-cancellation
workflow for unclaimed work.

Local Run commands have their own numerically ordered inbox-sequence watermark
and disposition in the existing origin-aware Run projection; issue and PR
watermarks remain independent. A local answer commits its new packet and local
watermark in the same ordinary packet transition. The normal coordinator
progression owns the subsequent invocation, so restart or a lost inbox response
cannot append the answer twice or launch another invocation. Remote comment
answer behavior is preserved. A changed source file never changes an active
Run's frozen packet through this command surface.

## Scheduling, claims, and recovery

Every local queue observation fetches the **current** configured target through
an isolated Git ref and retains its immutable commit. The same current-policy
reader supplies fresh execution previews and authorization revalidation. Queue
order is deterministic by stable issue ID. Labels never grant execution.

The coordinator retains a read-only backlog projection with the observed commit,
complete graph digest, timestamp, current source, authorization/publication
evidence, and separate queue and Run dispositions. Reading it while stopped does
no Git or network work. Its waiting reasons distinguish missing execution
approval, incomplete/cancelled publication, specification merge wait, stale
authorization, unmet dependencies, an occupied Run slot, unsupported
investigations, unavailable committed source, and code-host unavailability.
Parents remain visible grouping items. Publication-only items remain visible
without an execution grant; session and branch links come from actual approval
and publication ledger evidence rather than a file's correlation string.

A publication grant requires the whole batch's successful ledger **and** final
published receipt/session disposition. Successful Git effects followed by a
cancelled or incomplete receipt release no work. Missing newly approved files
can mean waiting for specification merge; deleting a pre-existing approved
baseline item invalidates the grant. Once publication is durably complete and
the exact complete graph is on target, normal deletion of its historical
specification branch does not revoke target visibility. The projection reports
that historical-ref drift. Incomplete effects still require reconciliation.

An observed content, kind, relationship, complete-graph, or host/target policy
change permanently invalidates pending authorization. Restoring old bytes does
not revive it: preview and explicitly approve again. A byte-identical rename
keeps content identity; the admitted packet records its actual path at the claim
commit. A temporary transport failure blocks observation without inventing a
source change. Every claim, including direct CLI claims, requires the supported
PR/status/review/comment capabilities and successful independent code-host
readiness before workspace or worker creation.

Dependencies require the exact approved prerequisite revision's successful
normal lifecycle completion and merge commit ancestry in the **same immutable
target commit used for this claim**. Review-ready, human/input waits, failures,
cancellations, closed-unmerged PRs, a host-only merge ahead of the fetched
target, and unavailable ancestry do not release dependents. Cleanup retains the
successful terminal/merge proof. Its identity includes installation, repository,
code-host provider/repository, target branch, issue content, and full graph;
completion on another target cannot satisfy a new target's authorization.

Before creating a claim worktree, SQLite reserves a deterministic claim/Run
identity with the exact base and packet. Retry adopts an owned clean worktree at
that base; conflicting or dirty artifacts require inspection. Saving the Run
and consuming the revision is one transaction. A lost response adopts that Run,
including any later state, without repeating the claim. Fresh authorization for
the same consumed revision/context cannot create a second Run. A reserved claim
whose Run has not yet been saved blocks reset: resume or reconcile that claim
first so reset cannot orphan its branch/worktree. Committed issue and
specification history remain outside operational reset.

New reservation checks the latest decision selecting that issue while holding
the store's write lock. A newer decision committed after queue observation
supersedes its older pending authority; an already admitted matching intent
remains recoverable. The running supervisor also reloads private registration
before observation/recovery/claim boundaries. Installation, operator, code-host,
checkout/store path, or policy-path composition changes require restart and
adapter recomposition before further queue effects; the captured registration
cannot keep authorizing new work.

Active local Runs use retained source bytes plus mutable local lifecycle
projections. Deleting or invalidating the current source does not prevent
cancellation or answering the frozen question. Local source refresh/revision
commands are refused; new source needs a new explicit decision. Changing tracker
registration cannot reinterpret an old remote Run's integer as a local issue or
replay its effects; recovery requires restoring its original registration.

Generated local PR content links the stable local identity, actual frozen path,
commit, and content/graph digests. Unsafe issue-number references in generated
titles use a stable-identity fallback. The generated body contains bounded
provenance and a Gate count; detailed Gate/review evidence remains in Factory.
Advisory PR-writer text containing remote work-item references is omitted with
a visible failure reason. Human-authored PR sections remain preserved. Existing
remote tracker PR rendering keeps its normal remote issue links.
