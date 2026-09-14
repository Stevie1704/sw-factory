---
status: accepted
---

# ADR 0003: Factory-owned workflow registry

## Context

The coordinator needs to support more than the implementation role without
turning repository configuration into a second workflow engine. A role has
several coupled identities: its invocation stage, prompt version, permitted
path defaults, and report contract. Run transitions
also need one authoritative declaration so each coordinator entry point makes
the same decision after a restart.

Repository configuration is intentionally limited to operational policy such
as harness and model selection. It is checked in with the repository and is
untrusted input relative to factory safety and workflow ownership.

## Decision

The factory-owned registry in `internal/workflow` is the authority for:

- role names, report kinds, invocation stages, prompt versions, and default
  permitted paths; and
- persisted stages and report-outcome transitions.

`internal/prompt` builds the common factory prompt from those declarations and
loads role-specific instruction bodies from factory-embedded Markdown, with a
checked-in content identity for each prompt version. It records the selected
prompt version in every invocation packet and store row.
The coordinator resolves role selection, report routing, and outcome
transitions through the registry. The operational store persists stages as open
metadata, so a later factory release can add a declared stage through a normal
migration rather than changing a closed store enum.

`factory.yaml` may select harness, model, and reasoning policy only for roles
already declared by the registry. It must not declare or redefine roles,
stages, prompts, or transitions; attempts are typed policy errors.

## Consequences

Adding a role is a factory code change with an explicit prompt/report
contract and tests. A repository can opt into the role by selecting its
harness and model in `factory.yaml`, but cannot alter its safety fence,
permitted-path default, or stage graph. Existing implementation, test, and
review invocations retain their persisted identities, while new roles can use
their own headless native sessions and handoffs.

ADR 0010 supersedes this record's former terminal-surface strategy. The
factory-owned registry remains authoritative for workflow semantics only.
