# Azure DevOps plan publication

This page describes how the Azure DevOps adapter publishes an approved
ideation plan to Azure Boards (#274, ADR 0022, ADR 0025), and how to prove it
against a disposable project.

## How a plan maps to Azure Boards

The adapter (`internal/azuredevops/plan.go`, `planwrite.go`) implements
`tracker.PlanPublisher` through `az rest`. The credential is the `az login`
identity (ADR 0019); the adapter reads no PAT and stores no credential.

| Plan element | Azure Boards representation |
| --- | --- |
| Plan node | One work item of the type that `azure_devops.planning` maps its kind to. The title is the node title. The Markdown description contains the node body, the acceptance criteria as a task list, and a `Traceability` section (session, plan revision, node, context revision, brief items, correlation tag). |
| Default mapping (Agile) | `parent` is an Epic; `task` and `investigation` are User Stories; the Feature level is optional. |
| Configured mapping | Any levels of the project's process, for example Epic, Feature, and Product Backlog Item for Scrum, with configured field values per level. |
| Required intermediate level | One extra item per parent and required level, identity `<parent>--<type slug>`, with the parent's title and a body that names the grouped plan items. It is in the preview, so the approval binds it. |
| Parent edge | A Parent link (`System.LinkTypes.Hierarchy-Reverse`) on the child. |
| Dependency edge | A Predecessor link (`System.LinkTypes.Dependency-Reverse`) on the dependent item to the item that must be done first. The other item shows it as a Successor. |
| Tags | Exactly one: `factory-plan:<session>/<plan revision>/<node>`. Never `agent-ready` or another factory state tag. |
| State | Not set; Azure DevOps applies the type's initial state. A configured `System.State` must be in the Proposed category. |

### Preflight

Preflight only reads, before approval and again before the first write. The
same plan against an unchanged project renders the same bytes.

1. Local checks: every plan kind is mapped, every parent is on a higher level
   than its child, required intermediate items are added, and every title,
   description, and tag fits Azure DevOps. The encoded create request of each
   item must also fit one `az rest --body` argument (at most 131,071 bytes,
   the Linux per-argument limit); a larger item is a typed `invalid_mapping`
   error before approval.
2. Permissions: View and Edit work items in the root area, and Create tag
   definition in the project.
3. Process: the mapped types exist and are enabled
   (`wit/workitemtypes`), they are on backlog levels in the configured order
   (`work/backlogconfiguration`), the required fields have values and the
   configured values are allowed (`wit/workitemtypes/{type}/fields`), every
   configured value fits its field type and the field is not read-only
   (`wit/fields`: an integer, double, boolean, date, or GUID value must
   parse, and a string value has at most 255 characters; an area or
   iteration path is exempt from the read-only flag, which Azure DevOps sets
   on those paths although a create accepts them), a configured state is a
   Proposed state, and the link types are enabled
   (`wit/workitemrelationtypes`). A failed check is a typed `invalid_mapping`
   error before approval.

### Correlation and reconciliation

`FindPlanItems` runs a WIQL query for the correlation tag in the project, in
every state, and compares the tags of each match exactly. Publication looks
up every item before it creates it: one match is adopted, more than one stops
the operation as `ambiguous`, and no match after an unknown outcome also stops
it as `ambiguous` until an operator reconciles. Every link is verified on the
From work item before it is added.

### Failure classification

| az rest result | Planning error | Meaning |
| --- | --- | --- |
| `Unauthorized(...)`, `Forbidden(...)` | `permission` | No change. |
| `Bad Request(...)`, `Not Found(...)` | `invalid_mapping` | No change, for example a rule error or an unknown type. |
| Host-command timeout | `ambiguous` | The write may have been applied. |
| Any other failure | unclassified | Unknown outcome; reconcile before a new create. |

## Disposable-project acceptance procedure

Scripted coverage (not live evidence): `internal/azuredevops/plan_test.go`
(Agile mapping, PBI mapping, required intermediate items, unsupported
hierarchy, invalid types, fields, and states, permission failures),
`planwrite_test.go` (exact create content, refused factory tags, failure
classification, correlation lookup, link direction), and
`plan_publication_test.go` (the real publication service against a stateful
scripted project: valid order, recorded IDs, lost create response across a
restart, unknown outcome, duplicate candidates, permission refusal, lost link
response). `internal/config/azure_planning_test.go` covers the configuration.

**Live evidence: not run.** Creating work items in an Azure DevOps
organization is an outward action that this change was not authorized to
perform. Record the live result here when an operator runs the procedure.

### Preconditions

- A disposable Azure DevOps Services project that holds no real backlog, for
  example `factory-plan-acceptance`, on the Agile process, and a second one
  on the Scrum process for step 5.
- `az login` as the identity that supervises the registration, with View and
  Edit work items and Create tag definition in the project.
- A Factory build that contains #266, #267, #271, #272, and #274, registered
  against the disposable project.

### Steps and expected artifacts

1. **Item bodies.** Make a session whose plan has a parent `P` with children
   `A` and `B`, where `B` depends on `A`. Preview and approve it with the
   default mapping. Publish.
   - Expected: one Epic and two User Stories, created in the order P, A, B.
     Each description renders as Markdown with the acceptance criteria and the
     `Traceability` section. Each item has exactly one tag,
     `factory-plan:<session>/<revision>/<node>`.
2. **Hierarchy and dependency direction.** Open B.
   - Expected: B has the Parent P and the Predecessor A; A shows B as a
     Successor; P shows A and B as children.
3. **Required intermediate level.** Set `required: true` on the Feature level
   and preview the step 1 plan in a new session.
   - Expected: the preview shows a Feature `P--feature` with P's title that
     names A and B. After publication, A and B are children of the Feature,
     and the Feature is a child of P.
4. **Retry behavior.** Repeat step 1 in a new session. Stop the coordinator
   directly after the first create request (the ledger shows `attempted`),
   start it again, and publish again.
   - Expected: no second work item for that node. The ledger adopts the item
     by its tag, or stops as `ambiguous` until reconciliation; publication then
     finishes with exactly three items and three links.
5. **Configured PBI mapping and rejection.** On the Scrum project, preview
   with the default mapping.
   - Expected: preview fails with `invalid_mapping` that names User Story, and
     nothing is created. Configure Epic, Feature, and Product Backlog Item and
     preview again: the tasks render as Product Backlog Items.
6. **No execution authorization.** For every created item, read its tags and
   state.
   - Expected: no `agent-ready` and no other factory state tag; every item is
     in its initial state; no run starts (`factory status`).

Record for each step: date, Factory revision, `az --version`, the work item
URLs, the ledger states, and whether the result matches.

### Cleanup

1. Delete every created work item (Boards > the item > Delete), then empty
   the Recycle Bin, or delete the disposable project.
2. Delete the `factory-plan:` tag definitions from Project settings > Tags.
