---
status: accepted
---

# Azure DevOps CLI extension route with PAT login and repository assignment tags

ADR 0019 reaches Azure DevOps through `az rest` with an Entra ID `az login`
and rejects a personal access token (PAT). ADR 0025 publishes plans through
the same route and gives each item exactly one Factory tag. Spec #302
requires a second route for an organization whose only supported
authentication is a PAT entered through `az devops login`: Boards work items
through `az boards`, pull requests through `az repos`, and any operation
without a dedicated command through `az devops invoke`. Two registered
repositories can share one Azure project, so each executable Product Backlog
Item (PBI) also needs an explicit repository owner. This record defines the
route, its authentication boundary, the repository assignment contract, and
the inventory that #304 to #306 implement. It amends ADR 0019 decision 1 and
its PAT option, and ADR 0025 decisions 4 and 6.

## Decision

1. **The host registration selects the route explicitly.**
   `azure_devops.route` is `az_rest` or `devops_extension`. An omitted route
   is `az_rest`, so every existing registration keeps ADR 0019 unchanged. Any
   other value fails validation with a field error. Only the extension route
   accepts `azure_devops.plan_repositories` (decision 8).
2. **Factory consumes the CLI-managed login and never handles the PAT.** The
   operator runs `az devops login --organization https://dev.azure.com/<org>`
   once. Factory never reads, prompts for, collects, logs, or persists a PAT;
   no configuration field, worker payload, report, or store column holds
   one. The worker environment denylist refuses `*_PAT` and every
   `AZURE_DEVOPS_EXT_*` name, because the extension reads its PAT and other
   credentials from such variables.
3. **The extension route never runs `az rest` or requests an Entra token.**
   Every extension call goes through one runner on the existing
   `CommandRunner`/`hostcmd` seam: argv only, an empty standard input, the
   two-minute `CommandTimeout`, the 32 MiB output bound, and
   `AZURE_CORE_ONLY_SHOW_ERRORS=1`, `AZURE_CORE_NO_COLOR=1`,
   `AZURE_CORE_COLLECT_TELEMETRY=0`, `AZURE_CORE_DISABLE_CONFIRM_PROMPT=1`,
   and `AZURE_EXTENSION_USE_DYNAMIC_INSTALL=no`, so a missing extension fails
   as an unknown command instead of installing or prompting. Each command
   carries `--organization https://dev.azure.com/<org>`, `--detect false`,
   `--output json`, and `--only-show-errors`, and `--project` and
   `--repository` wherever the command accepts them, so ambient CLI defaults
   and Git remotes never select the target. A command that accepts no
   `--project` (for example `az boards work-item show` and `az repos pr
   show`) is organization-scoped; the adapter must verify the project and
   repository in its response. An adapter operation that is not ported yet
   fails with `ErrExtensionOperationUnavailable`; it never falls back to
   `az rest`.
4. **Errors stay bounded and secret-free.** The extension's
   missing-credential text and its HTTP 401 text ("requires user
   authentication") become `LoginRequiredError`, whose fixed message is
   `run az devops login --organization <the URL Factory passed>`. Other
   failures return the command group (`az repos show`), never argv values,
   and one line of standard error with the `AZURE_DEVOPS_EXT_PAT` value and
   every opaque run of 40 or more letters, digits, `+`, `=`, `_`, or `-`
   redacted, bounded to 300 characters. `/` and `.` end a run, so URLs and
   paths stay readable.
5. **`az devops invoke` is used only through verified area/resource pairs.**
   The helper takes an area, resource, method, API version, sorted route and
   query parameters, and an optional JSON body. The extension reads a body
   only from `--in-file`, so the body goes into an owner-only temporary file
   that is removed after the call; it never appears in argv or an error. The
   helper refuses an API version with a resource version such as
   `7.1-preview.4`, because the extension's `apiVersionToFloat` cannot parse
   it; `7.1-preview` is accepted. The inventory below names the pair for
   each operation.
6. **Each executable PBI carries one repository assignment tag.** The tag is
   `factory-repo:<repository GUID>`, in lowercase, naming the stable Azure
   Repos repository ID resolved within the registered organization and
   project. It is a Factory convention: Boards work items belong to a
   project, and neither an Area Path nor a linked pull request establishes
   exclusive repository ownership. The registered repository name resolves
   to its ID through `az repos show --project --repository`; the response
   must name the registered project and repository and carry GUIDs. Before a
   claim, on the polling path and on the direct issue and claim paths, exact
   validation accepts only exactly one well-formed tag that names the
   registered repository's GUID. Missing, malformed, multiple, and
   mismatched assignments (an unknown GUID or another installation's
   repository) are refused with a diagnostic that names the tag to set. A
   query filter is only an optimization. An assignment never grants
   `agent-ready` authorization, and an Epic's assignment is never inherited.
   Existing unassigned PBIs need an explicit tag before execution on this
   route.
7. **The validated repository identity is frozen with the Run.**
   `store.Run.TrackerRepository` holds organization, project ID and name,
   repository GUID and name. A store trigger refuses to change or clear it
   once set, so a tag edit cannot redirect active work through any save
   path. A new repository requires a new claim and therefore renewed
   authorization. Runs on other routes keep it nil.
8. **Two repositories in one project are two installations.** Host schema
   version 2 keeps exactly one registration, and every service reads that
   registration. Each repository therefore has its own host configuration,
   operational store, and `factory start` process (`--config`), all naming
   the same organization and project with the extension route. Each
   installation polls the shared project and claims only PBIs whose
   assignment tag names its own repository GUID, so overlapping pollers
   cannot claim each other's work. A plan that one installation publishes
   may assign PBIs to its own repository and to the repositories in its
   `plan_repositories`; each PBI then executes on the installation that
   registers its target. One Run never spans repositories.
9. **The plan contract carries a per-item target.**
   `tracker.PlanGraphItem.TargetRepository` names the registered repository
   a plan node selects; `tracker.RenderedPlanItem.TargetRepository` holds
   the resolved name and GUID that the preview shows. The approval payload
   stores it as `target_repository` and omits it when absent, so payloads
   approved before this record keep their exact bytes, and a changed target
   changes the bound bytes and needs a new approval. #305 fills the target
   and validates it against `PlanTargetRepositories`.
10. **Every published PBI carries correlation and assignment tags.** This
    replaces ADR 0025 decision 4's "exactly one tag": a published item on
    the extension route carries exactly one `factory-plan:` correlation tag
    and, when it is executable, exactly one `factory-repo:` assignment tag.
    Neither is a run-state tag. Every lifecycle tag update must preserve
    both, because the tag write replaces the complete `System.Tags` value.
    Reconciliation of an existing item verifies the assignment as well as
    the correlation and refuses a mismatch instead of adopting or
    retargeting it.
11. **ADR 0025 decision 6 is route-specific.** On the az rest route,
    classification and `az login` stay as they are. On the extension route,
    `LoginRequiredError` is a permission failure that proves no write, and a
    timeout stays ambiguous; #305 maps the remaining extension texts.

## Operation inventory

Evidence: the azure-devops extension's command reference on
learn.microsoft.com (`az devops`, `az devops security permission`,
`az boards`, `az boards work-item`, `az boards work-item relation`,
`az repos`, `az repos ref`, `az repos pr`, `az repos pr reviewer`,
`az repos pr work-item`, page versions of April to October 2026; extension
for Azure CLI 2.30.0 or later), the extension source on
github.com/Azure/azure-devops-cli-extension (`dev/team/invoke.py`,
`dev/common/services.py`, `devops_sdk/client.py`, `dev/boards/work_item.py`,
`dev/boards/relations.py`, `dev/team/security_permission.py`), and the
resource-area and resource-location lists recorded on 10 January 2025 in
that repository's `test_invoke_command_basic.yaml`. `az devops invoke`
matches `--area` against the organization's ResourceAreas names and raises
"--area is not present" otherwise; among locations with the same resource
name it keeps the first one with the highest `maxVersion`, and route
parameters play no part in the choice. "Resolves to" names the winning route
template under that rule. When the URL is built (`client.py`
`_remove_optional_route_parameters`), every whole `{name}` or `{*name}`
segment without a route parameter is dropped, wherever it stands, so for
example `.../comments/{commentId}` lists comments when `commentId` is
omitted, and `{project}/{team}/...` becomes `{project}/...` without `team`;
a `${type}` segment is kept. "Verified command" means the command and
flags appear in the command reference; no command has been run. The recording is one organization at one date; no
route has been run live, and the CLI is not installed on the development
host.

| Current operation (`az rest` path) | Extension route | Status |
| --- | --- | --- |
| Identity: GET `_apis/connectionData` | `invoke --area Location --resource ConnectionData` | **Unresolved.** `Location` is not a ResourceAreas name in the recording, so invoke refuses it. No dedicated command returns the signed-in identity. |
| Repository identity: GET `git/repositories/{repo}` | `az repos show --project --repository` | Verified command; implemented as `ResolveRepository`. |
| Permission checks: GET `_apis/permissions/{ns}/{bit}` | `invoke --area Security --resource Permissions` | **Unresolved.** `Security` is not a ResourceAreas name, so invoke refuses it. Candidate: `az devops security permission show --namespace-id --subject --token` (verified command), whose `--subject` is a user email or group descriptor (blocked by the identity row) and which reports that subject's ACL, not an evaluation for the caller. |
| Root area ID: GET `wit/classificationnodes/Areas` | `invoke --area wit --resource classificationNodes` route `structureGroup=areas` | Resolves to `{project}/_apis/wit/classificationNodes/{structureGroup}/{*path}`. |
| Project tags: GET `wit/tags` | `invoke --area wit --resource tags` | Resolves to `{project}/_apis/wit/tags/{tagIdOrName}`. |
| Work item read: GET `wit/workitems/{id}` | `az boards work-item show --id --expand` | Verified command; organization-scoped (no `--project`). Invoke `wit/workItems` resolves to the create route `${type}`, so it cannot read by ID. |
| Batch read: GET `wit/workitems?ids=` | `invoke --area wit --resource workItemsBatch` POST | Resolves to `{project}/_apis/wit/workItemsBatch` (single location). Request shape from the REST reference "Work Items - Get Work Items Batch"; not run. |
| WIQL intake and correlation: POST `wit/wiql` | `az boards query --wiql --project` | Verified command (flat queries only). Alternative: `invoke wit/wiql`, which resolves to `{project}/{team}/_apis/wit/wiql`. |
| Type states: GET `wit/workitemtypes/{type}/states` | `invoke --area wit --resource workItemTypeStates` | Resolves to `{project}/_apis/wit/workitemtypes/{type}/states`. |
| Update history: GET `wit/workItems/{id}/updates` | `invoke --area wit --resource updates` | Resolves to `{project}/_apis/wit/workItems/{id}/updates/{updateNumber}`. |
| Tag replace: PATCH `wit/workitems/{id}` `/fields/System.Tags` | `az boards work-item update --id --fields System.Tags=...` | Verified command; organization-scoped. The value replaces every tag (decision 10). |
| Work item create: POST `wit/workitems/${type}` | `az boards work-item create --type --title --description --fields --project` | Verified command. **Unresolved:** the command sends only `/fields/...` operations (`work_item.py`), so it cannot set `/multilineFieldsFormat/System.Description=Markdown`; the Markdown description of ADR 0025 decision 5 needs a decision in #305. `invoke wit/workItems` resolves to the create route and could send the full patch, but the spec requires the dedicated command. |
| Relation create: PATCH `wit/workitems/{from}` `/relations/-` | `az boards work-item relation add --id --relation-type <name> --target-id` | Verified command; organization-scoped. `relations.py` maps `<name>` case-insensitively to the service's relation-type `name` (for example `Parent`, `Predecessor`) and sends its reference name; the names come from the organization, not from the CLI. |
| Relation lookup: GET `wit/workitems/{from}?$expand=relations` | `az boards work-item relation show --id` | Verified command. It replaces each `rel` reference name with the friendly name, so a lookup must compare names, not `System.LinkTypes.*`. |
| Comments list, create, edit: `wit/workItems/{id}/comments[/{cid}]` | `invoke --area wit --resource comments`, query `format=markdown`, API `7.1-preview` | Resolves to `{project}/_apis/wit/workItems/{workItemId}/comments/{commentId}` (7.2 over the 5.0 location). The response gains `continuation_token` from the `X-MS-ContinuationToken` header. **Unresolved:** the code uses `7.1-preview.4`, which invoke cannot parse; whether the service accepts `7.1-preview` without a resource version is not verified. `--discussion` writes `System.History` without a comment ID and is not a substitute. |
| Process types: GET `wit/workitemtypes` | `invoke --area wit --resource workItemTypes` | Resolves to `{project}/_apis/wit/workItemTypes/{type}`. |
| Field definitions: GET `wit/fields` | `invoke --area wit --resource fields` | Resolves to `{project}/_apis/wit/fields/{fieldNameOrRefName}`. |
| Type fields: GET `wit/workitemtypes/{type}/fields?$expand=allowedValues` | `invoke --area wit --resource workItemTypesField` | Resolves to `{project}/_apis/wit/workitemtypes/{type}/fields/{field}`. |
| Backlog configuration: GET `work/backlogconfiguration` | `invoke --area work --resource backlogconfiguration` | Resolves to `{project}/{team}/_apis/work/backlogconfiguration`; behavior without a `team` route parameter is not verified. |
| Link types: GET `wit/workitemrelationtypes` | `az boards work-item relation list-type` | Verified command. |
| PR search by branch pair: GET `git/repositories/{repo}/pullrequests?searchCriteria...` | `az repos pr list --project --repository --source-branch --target-branch --status all --top` | Verified command. Invoke `git/pullRequests` resolves to the project-level `{project}/_apis/git/pullRequests/{pullRequestId}` and cannot list one repository. |
| PR read: GET `git/repositories/{repo}/pullrequests/{n}` | `az repos pr show --id` | Verified command; organization-scoped, so the adapter must check the repository in the response. |
| PR create: POST `git/repositories/{repo}/pullrequests` | `az repos pr create --project --repository --source-branch --target-branch --title --description --draft true --work-items <PBI>` | Verified command. `--description` takes one value per line and joins the values with `\n` (`pull_request.py`). Implemented (#306): the title and the whole description each go as one `--flag=value` argument, which argparse binds as exactly one value, so a Markdown line such as `---` is never parsed as an option. Azure CLI replaces a value that starts with `@` (alone or after the first `=`) by a file's content (azure-cli core `_expand_file_prefixed_files`), so such a title, description, or branch is refused before the call. `--repository` takes a name or ID; with a source PBI the adapter passes the frozen GUID after `az repos show` confirms that the registered name still resolves to it. Source-verified, not run live. |
| PR update and readiness: PATCH `pullrequests/{n}` | `az repos pr update --id --title --description` and `--draft false` | Verified command; organization-scoped. Implemented (#306) after a `pr show` repository check. |
| PR work-item link | `az repos pr work-item add --id --work-items` and `az repos pr work-item list --id` | Verified commands; organization-scoped (new for #306). Both return work items with `id` (`pull_request.py`). Implemented: after a `pr show` check against the frozen GUID, list, add when missing, and list again; an unconfirmed add fails. |
| PR threads (reviews and comments): GET/POST `pullRequests/{n}/threads` | `invoke --area git --resource pullRequestThreads` | Resolves to `{project}/_apis/git/repositories/{repositoryId}/pullRequests/{pullRequestId}/threads/{threadId}`. |
| PR thread comment edit: PATCH `threads/{t}/comments/{c}` | `invoke --area git --resource pullRequestThreadComments` | Resolves to `.../threads/{threadId}/comments/{commentId}`. |
| PR reviewers: GET `pullRequests/{n}/reviewers` | `az repos pr reviewer list --id` | Verified command; organization-scoped. Alternative: `invoke git/pullRequestReviewers`, which resolves to the 7.2 project route. Implemented (#306) with the dedicated command after a `pr show` repository check, because its output names no repository. |
| Commit statuses: POST/GET `commits/{sha}/statuses` | `invoke --area git --resource statuses` | Resolves to `{project}/_apis/git/repositories/{repositoryId}/commits/{commitId}/statuses`. `invoke.py` matches the area as well as the resource, so the two `CodeReview` `statuses` locations are not candidates. Query `top`, `skip`, `latestOnly` from the REST reference "Statuses - List" (7.1). |
| Default-branch ref: GET `refs?filter=heads/<branch>` | `az repos ref list --project --repository --filter heads/<branch>` | Verified command. |
| Code-host doctor active PR probe | `az repos pr list --status active --top 1` | Verified command. |

## Consequences

- `internal/azuredevops/extension.go` holds the shared seam:
  `NewClientForRoute`, `Client.Route`, the production `extensionRunner`,
  `Client.extension` and `Client.invoke`, `LoginRequiredError`,
  `ErrExtensionOperationUnavailable`, `RepositoryIdentity`, and
  `Client.ResolveRepository`. `assignment.go` holds
  `RepositoryAssignmentTag`, `IsRepositoryAssignmentTag`, and
  `ValidateRepositoryAssignment`. The composition root passes the selected
  route to the tracker and the independent code host.
- `azuredevopstest.ParseExtensionCall` refuses `az rest`, an unknown group,
  and a call without explicit organization, `--detect false`, and JSON
  output. The stateful `Boards` fake answers `az repos show` for two
  repositories with distinct GUIDs in one project; the route-keyed test fake
  routes extension calls by command and invoke area/resource.
- Until #304 and #306 port them, every Boards and Repos operation on the
  extension route fails closed. The tracker and code-host startup checks
  fail with an `az devops login` repair instead of passing or pointing to
  `az login`, and a plan write reports `unsupported_capability`, which
  proves no write. The az rest route, GitHub, and local Markdown behave as
  before.
- #306 ports the Repos operations: pull requests, threads, comments,
  reviewer reads, commit statuses, the native PBI link, and the code-host
  startup check, whose write permissions stay an unverified warning. Finding
  a coordinator pull-request comment by marker still needs the identity row.
- `store.PlanNode` and `publication.planGraph` carry no target yet; #305
  adds the plan node field, its validation, and the adapter rendering.
- **Residual authentication risk.** The extension tries an `az login` token
  before a PAT (`services.py` `_get_credentials`), and
  `AZURE_DEVOPS_EXT_PAT` takes precedence over the stored `az devops login`
  credential. Factory cannot force PAT-only authentication by argv. An
  operator who must use only the PAT runs Factory as a user without an
  `az login` session. Factory adds no environment isolation it cannot
  verify.
- The unresolved inventory rows (identity, permission evaluation, Markdown
  descriptions, the comment API version) need a decision or live evidence
  in #304 and #305 before those features ship on this route.
- Git push and fetch keep their own credential helper or SSH agent;
  `az devops login` does not configure Git.

## Considered options

- **Several registrations in one host configuration.** Rejected for now:
  host schema version 2 and every service path assume one registration, and
  one Run never needs two repositories. Two installations meet the
  two-repository acceptance without a schema change.
- **Area Path or a linked PR as the repository owner.** Rejected: an Area
  Path organizes teams and boards, and neither establishes exclusive
  ownership.
- **The repository name in the tag.** Rejected: a name can be renamed and
  reused; the GUID is stable.
- **Read or forward the PAT.** Rejected: the CLI already owns the
  credential, and handling it would put a secret in Factory's process,
  configuration, or store.
- **Environment isolation to force PAT-only authentication** (for example a
  separate `AZURE_CONFIG_DIR`). Rejected: it would also hide the stored PAT,
  and its behavior is not verified.
