---
status: accepted
---

# ADR 0003: Factory-owned workflow registry

## Context

The coordinator needs to support more than the implementation role without
turning repository configuration into a second workflow engine. A role has
several coupled identities: its invocation stage, prompt version, permitted
path defaults, report contract, and visible terminal surface. Run transitions
also need one authoritative declaration so each coordinator entry point makes
the same decision after a restart.

Repository configuration is intentionally limited to operational policy such
as harness and model selection. It is checked in with the repository and is
untrusted input relative to factory safety and workflow ownership.

## Decision

The factory-owned registry in `internal/workflow` is the authority for:

- role names, report kinds, invocation stages, prompt versions, and default
  permitted paths;
- the visible surface strategy for each role; and
- persisted stages and report-outcome transitions.

`internal/prompt` builds the common factory prompt from those declarations and
loads role-specific instruction bodies from factory-embedded Markdown, with a
checked-in content identity for each prompt version. It records the selected
prompt version in every invocation packet and store row.
The coordinator resolves role selection, report routing, surface identity, and
outcome transitions through the registry. The operational store persists stage
and role-surface identities as open metadata, so a later factory release can
add a declared stage through a normal migration rather than changing a closed
store enum.

`factory.yaml` may select harness, model, and reasoning policy only for roles
already declared by the registry. It must not declare or redefine roles,
stages, prompts, or transitions; attempts are typed policy errors.

## Consequences

Adding a role is a factory code change with an explicit prompt/report/surface
contract and tests. A repository can opt into the role by selecting its
harness and model in `factory.yaml`, but cannot alter its safety fence,
permitted-path default, or stage graph. Existing implementation, test, and
review invocations retain their persisted identities, while new
non-implementation roles can use their own visible surfaces and handoffs.
