---
status: accepted
---

# Azure DevOps plan publication maps kinds to process types and correlates by a namespaced tag

ADR 0022 defines the planning port and asks each adapter to choose its own
correlation encoding. Azure Boards differs from GitHub in three ways. Each
project has a process (Agile, Scrum, Basic, or a custom inheritance) with its
own work-item types, backlog levels, required fields, and states. Tags are
the only label mechanism, and ADR 0019 already uses them for the run state.
A work item has native parent and predecessor links. This record defines how
the Azure DevOps adapter (#274) publishes an approved plan within these
limits. It amends decision 3 of ADR 0019 by one tag namespace that is not run
state.

## Decision

1. **The mapping is host configuration.** `azure_devops.planning.levels` of
   a registration lists the backlog levels from the top down. Each level has
   a work-item `type`, the neutral plan `kinds` it receives, an optional
   `required` flag, and optional `fields` values. Without the key, the Agile
   mapping applies: `parent` to Epic, an optional Feature level, and `task` and
   `investigation` to User Story. A PBI-based project configures Product
   Backlog Item in the same shape. The type lives in `internal/config`,
   because the coordinator imports config and must not reach the adapter
   (ADR 0018). The title, description, and tags are adapter-owned and cannot
   be configured.
2. **Preflight validates the mapping against the project's process before
   approval.** It is read-only. It requires the View and Edit work items
   permission in the root area and Create tag definition in the project; it
   requires every mapped type to exist, to be enabled, and to be on a backlog
   level; the configured level order must follow the backlog ranks; every
   always-required field without a default must get a value; a configured
   field must exist and have an allowed value; a configured `System.State`
   must be in the Proposed category; and the parent and predecessor link
   types must be enabled. A failure is a typed no-change error:
   `permission`, `unsupported_capability` for a hierarchy that the plan or the
   process cannot take, or `invalid_mapping`.
3. **Required intermediate items are part of the preview.** When a parent
   link skips a level that is `required`, preflight adds one intermediate
   item per parent and level, with the identity `<parent>--<type slug>`, the
   parent's title, a body that names the plan items it groups, and its own
   correlation key. The approval binds it like any other item. A child on the
   same or a higher level than its parent fails preflight; the adapter never
   rewrites the hierarchy after approval.
4. **A namespaced correlation tag.** Every created item carries exactly one
   tag, `factory-plan:<session>/<plan revision>/<node>`. It is not a factory
   state tag: it never authorizes, reports, or blocks a run, and intake and
   the run-state labels ignore it. `FindPlanItems` queries WIQL for the tag in
   the project, in every state, and reads the matches back to compare the tag
   exactly. WIQL reads the work-item store, not a search index.
5. **Exact content, no execution state.** A create sends the rendered type,
   title, a Markdown description (`multilineFieldsFormat`), the correlation
   tag, and the configured fields, and nothing else: no relation, no
   `agent-ready` or other factory state tag, and no state unless the mapping
   sets a Proposed state. A relationship is one link on the From item:
   `System.LinkTypes.Hierarchy-Reverse` (parent) or
   `System.LinkTypes.Dependency-Reverse` (predecessor: the To item must be
   done first). Every relationship is native; there is no textual fallback.
6. **Outcome classification from the az rest response.** `Unauthorized` and
   `Forbidden` are permission errors; `Bad Request` and `Not Found` are
   invalid mappings; a host-command timeout is ambiguous. Every other failure
   leaves the outcome unknown, so publication reconciles by the correlation
   tag before it creates again. Authentication stays with `az login`; the
   adapter reads and stores no credential and adds no PAT handling.

## Consequences

Every published item creates one tag definition in the project. The project
tag list grows by one entry per published item, and the identity needs the
Create tag definition permission. A plan that a project's process cannot take
fails before approval with an error that names the level, type, or field to
configure. The composition root must pass the registration's
`azure_devops.planning` to `azuredevops.Client.Planning` when it wires
publication (#267); until then the Agile mapping applies. The scripted az
tests in `internal/azuredevops` cover the mapping, preflight failures, and
end-to-end publication; the live procedure in
`docs/acceptance/azure-devops-plan-publication.md` has not been run.

## Considered options

- **Hard-code every leaf as a Product Backlog Item.** Rejected: an Agile
  project has no such type, and the issue requires configurable alternatives.
- **Correlate by a description marker.** Rejected: a WIQL `Contains` on a
  long-text field is a substring match on HTML that Azure DevOps may sanitize,
  and a word search uses an eventually consistent index.
- **One tag per plan revision plus a description marker.** Rejected: it
  bounds the tag growth but makes the exact match depend on the description
  surviving edits and sanitizing.
- **A custom correlation field.** Rejected: it needs process administration,
  which is out of scope.
- **Silently add or skip a missing intermediate level after approval.**
  Rejected: the human approves exact content (ADR 0022 decision 2).
