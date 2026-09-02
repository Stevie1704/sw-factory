---
status: accepted
---

# ADR 0007: Repository-selected role craft is frozen at claim

## Context

The factory's embedded role prompts deliberately separate factory authority
from role-specific craft. Repositories sometimes need to teach a role local
conventions that are too specific to belong in the factory binary, but a live
checkout or mutable prompt file would make a restart observe different
instructions from the original claim. Repository content is also untrusted and
must not be able to change workflow ownership, safety boundaries, or reporting.

## Decision

`factory.yaml` may contain an optional `role_craft` map whose keys are declared
factory roles and whose values are safe repository-relative Markdown paths. At
claim time, the coordinator reads each selected file from the exact immutable
base checkpoint and stores the path, exact content, and SHA-256 digest in the
specification packet. Every later invocation, prompt rebuild, and restart uses
that packet projection; it never rereads the checkout, issue body, or live
repository file.

The captured content replaces only the role's embedded `craft` section. The
embedded prompt version remains the authority identity, and the embedded
authority, route-dependent sections, permitted paths, workflow stages, and
report contract remain unchanged. Supplied craft is passed through the same
fence sanitization as other untrusted prompt data, and factory section markers
are rejected.

Persisted invocation rows retain the selected path and content digest beside
`PromptVersion`. Recovery verifies both the packet content digest and the
persisted invocation identity before rebuilding a prompt; a mismatch is a
typed discrepancy and pauses recovery. Factory doctor checks every configured
entry independently against the target branch head so missing files are
reported before claim.

## Consequences

Repositories gain role-specific craft without gaining a prompt or workflow
extension point. An absent `role_craft` entry preserves the embedded behavior,
and an empty selected file intentionally produces an empty craft section. A
changed or deleted file at the target branch is diagnosed before claim, while
a file changing after claim cannot affect the run. Adding repository craft
changes the configuration and packet contract, not the embedded prompt
version or factory authority.

This decision narrows ADR 0005: repository files cannot replace embedded role
authority, but they may replace the explicitly delimited craft section under
the capture, sanitization, and recovery rules above.
