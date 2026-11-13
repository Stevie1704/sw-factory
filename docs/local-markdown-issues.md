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
