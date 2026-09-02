---
status: accepted
---

# ADR 0005: Embedded role prompts and separate review axes

## Context

Role instructions are factory authority. Repository guidance is useful for
conventions, but it is checked-in untrusted input and must not replace the
factory's ownership, safety, or reporting rules. The issue snapshot is product
intent, not a substitute for the guidance that existed at the run's exact base
checkpoint.

Specification correctness and documented repository standards are different
review questions. Combining their findings or letting a style heuristic block
readiness makes the result difficult to explain and allows a preference to
look like a product defect.

## Decision

Factory role bodies live in per-role Markdown files under `internal/prompt` and
are compiled into the binary with `go:embed`. The workflow registry owns the
role, stage, and prompt version; the prompt package verifies each embedded body
with its checked-in SHA-256 content identity. Persisted invocations retain
their prompt version so restart recovery cannot silently select a different
body. Route-dependent sections are selected from the embedded body; Go adds
only frozen specification, handoff, repair, review-context, and scope data.

Each current role body contains exactly one factory-owned `craft` section,
delimited by `<!-- craft:start -->` and `<!-- craft:end -->`; everything
outside that section is the role's `authority` section. The craft section
contains role-specific guidance about how to do the work, while the authority
section contains ownership, safety, permitted-path, workflow, and reporting
rules. Craft guidance advises craft only and never widens the frozen
specification, moves a workflow stage, changes permitted paths, or alters the
report contract. Where craft guidance and factory-owned rules disagree, the
factory-owned rules decide. An empty craft section is valid, and rendering
removes the markers while retaining nonempty craft prose.

At claim time, the coordinator reads the tracked repository guidance files from
the exact Git base checkpoint and stores the named document projection in the
frozen specification packet. The prompt always frames that projection as
untrusted input, including when the projection is empty. Later prompt builds
use the packet projection rather than rereading the issue or target checkout.

The specification reviewer and documented-standards reviewer remain separate
roles with separate invocation identities, checkpoint-bound results, status
contexts, and pull-request sections. The standards reviewer uses repository
guidance as its primary source, cites a named rule or named baseline heuristic
and the affected hunk, and treats every heuristic-baseline finding as advisory.
Only a concrete violation of a documented repository standard can block on the
standards axis; the two axes are not merged or re-ranked.

## Consequences

Changing factory role authority is a code-reviewed Markdown change that must
also update its prompt version and content identity. Repository prompt files
or runtime prompt configuration cannot replace embedded authority, route
sections, or report contracts. The optional repository `role_craft` map may
replace only a role's embedded craft section; its path, content, and SHA-256
identity are captured from the exact base checkpoint and reused for every later
prompt build. A run has reproducible guidance and craft snapshots even if the
issue or checkout changes later.

Review results explain whether readiness is blocked by frozen behavior or by a
named repository standard, while baseline craft observations remain visible
without becoming a gate.
