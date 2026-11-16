# Azure DevOps extension route acceptance evidence

This record is the evidence of issue #307 for specification #302: Azure
DevOps through the azure-devops CLI extension with `az devops login` and a
personal access token (PAT), Epic/PBI plan publication, and Azure Repos pull
requests (ADR 0030). It follows the conventions of the
[agent-journey baseline](baseline.md): deterministic evidence and live
evidence stay apart, and a scenario that did not run is `not-run`, never
`pass`. The scenarios are `azure-extension-publish-and-execute` and
`azure-extension-repository-assignment`; read them with
`factory acceptance scenarios --json`.

- [Results](azure-devops-extension-route-results.json): the recorded
  outcomes of the two scenarios.
- [Report](azure-devops-extension-route/acceptance-report.md) and its
  [JSON form](azure-devops-extension-route/acceptance-report.json), written
  by `factory acceptance report`. The scenarios outside #307 are `not-run`
  in this report because this run did not record them.
- The `az rest` route keeps its own record:
  [Azure DevOps plan publication](azure-devops-plan-publication.md).

**Live evidence: not run.** No command of this route has run against Azure
DevOps. An isolated Azure CLI installation was used only for offline helper
dispatch with mocked SDK responses; no disposable project was authorized. Scripted adapters are not live-provider
proof.

## Identities

| Item | Value |
| --- | --- |
| Platform | darwin/arm64, Go 1.27.4 (go.mod toolchain) |
| Release | Development build of branch `feat/302-azure-devops-pat`; the report names the exact revision |
| Azure CLI and azure-devops extension | Azure CLI 2.93.0 and azure-devops 1.0.11, isolated temporary installation for offline helper dispatch only |
| Pinned worker image | Not rebuilt for #307. No worker or report contract changed; no live invocation ran. |
| Host harness | Not observed: no live scenario ran. The implementing agent was Claude Code (Claude Opus 5.5); it is not product evidence. |

## What the workflow is

| Concept | On this route |
| --- | --- |
| Route selection | `azure_devops.route: devops_extension` in the host registration. An omitted route stays `az_rest` (ADR 0019). |
| Authentication | `az devops login --organization https://dev.azure.com/<org>`; the operator types the PAT at the `Token:` prompt. Factory never reads, asks for, or stores the PAT. |
| Git | Separate. Push and fetch use the host's credential helper or SSH agent; `az devops login` does not configure Git. |
| Plan mapping | Explicit `azure_devops.planning`: `parent` is an Epic, `task` and `investigation` are Product Backlog Items (PBIs), each PBI links directly to its Epic with a Parent link. No Feature is inserted and no User Story is substituted. |
| Ownership | Epics and PBIs belong to the project. A pull request belongs to one repository and links its PBI natively. |
| Repository assignment | Each executable PBI carries exactly one `factory-repo:<repository GUID>` tag. The GUID is the `id` of `az repos show --project <project> --repository <name>`. Area Paths stay team and board organization only. |
| Existing PBIs | Need an explicit `factory-repo` tag before execution. Adding the tag never authorizes a run; `agent-ready` from an authorized user does. |
| One repository per PBI | A PBI executes in one repository. Split work that changes two repositories into one PBI for each. One Run never spans repositories. |
| Two repositories | Two installations: each has its own host configuration, store, and `factory start --config`, both naming the same organization and project. The publishing installation lists the other repository in `plan_repositories`. |
| Diagnosis | `factory doctor --json` runs only read commands. |

Setup, PAT scopes, permissions, expiry, and re-login are in the
[configuration guide](../configuration.md#cli-extension-route-pat-login).
The scopes there come from the REST reference pages of the operations in the
ADR 0030 inventory; they are not verified in a live run.

## Scripted evidence (deterministic)

The integrated journeys drive the real Azure DevOps client on the
`devops_extension` route over `azuredevopstest.Boards`, a stateful scripted
`az` that fails the test for any extension call without explicit
`--organization`, `--detect false`, and JSON output, and for any argument
Azure CLI would read from a file. The fake also answers `az rest` for the
`az_rest` route, so the journey asserts that it recorded no `az rest` call.
The fake keeps a tag history with authors, replaces the complete
`System.Tags` value on each update, stores Markdown comments, and keeps pull
requests per repository GUID with their linked work items, so the tests read
the repository and the link from the fake's state, not from the request.

| #302 / #307 criterion | Evidence | Result |
| --- | --- | --- |
| Preview and approval through the UI; approval binds content, hierarchy, and target | `internal/factory:TestIdeationJourneyPublishesTheReviewedPlanToEachTracker`, `internal/factory:TestIdeationJourneyRefusesStaleApprovalAndChangedMapping` (subtests `azure-devops-extension`), `internal/factory:TestAzureExtensionJourneyRunsAnEpicAcrossTwoRepositories`, `internal/publication:TestPreviewBindsEachItemsTargetRepository`, `internal/webui:TestPreviewShowsEachItemsTargetRepository` | pass |
| One Epic and two directly linked PBIs, IDs and URLs recorded | `internal/azuredevops:TestExtensionPublishCreatesOneEpicWithTwoDirectPBIs`, `internal/factory:TestAzureExtensionJourneyRunsAnEpicAcrossTwoRepositories` | pass |
| Incompatible process fails before writes, no silent remapping | `internal/azuredevops:TestExtensionPreflightRefusesWithoutWrites`, `internal/azuredevops:TestExtensionPreflightRendersAnEpicWithDirectPBIsAndTheirTargets` | pass |
| Retry and restart reconciliation without duplicates | `internal/factory:TestIdeationJourneyRecoversRemoteSuccessAfterACoordinatorStop`, `internal/factory:TestIdeationJourneySurvivesRestartsAndDuplicateSubmissions` (subtests `azure-devops-extension`), `internal/azuredevops:TestExtensionPublishReconcilesLostResponsesWithoutDuplicates`, `internal/azuredevops:TestExtensionRelationWithALostResponseIsVerifiedNotRepeated`, `internal/azuredevops:TestExtensionPublishStopsOnDuplicateCorrelationMatches` | pass |
| Publication grants no execution authorization | `internal/factory:TestAzureExtensionJourneyRunsAnEpicAcrossTwoRepositories` (both installations poll no work and a direct claim is refused until `agent-ready`), every ideation journey's inert check | pass |
| Claim, frozen repository, and state tags | `internal/factory:TestAzureExtensionJourneyRunsAnEpicAcrossTwoRepositories`, `internal/factory:TestClaimFreezesTheValidatedRepositoryAssignmentWithTheRun`, `internal/store:TestFrozenTrackerRepositoryCannotBeRedirected` | pass |
| Both registrations create a draft PR in their repository with a native PBI link | `internal/factory:TestAzureExtensionJourneyRunsAnEpicAcrossTwoRepositories`, `internal/factory:TestCreateDraftPullRequestLinksTheSourceWorkItemOfAFrozenRepository`, `internal/azuredevops:TestExtensionCreateLinksTheSourceWorkItemInTheFrozenRepository`, `internal/azuredevops:TestExtensionLostCreateResponseRecoversThePullRequestAndItsLink`, `internal/azuredevops:TestEnsurePullRequestWorkItemRecoversAMissingAssociation` | pass |
| Review feedback and readiness | `internal/factory:TestAzureExtensionJourneyRunsAnEpicAcrossTwoRepositories` (the real client reads the maintainer's change request and readies the pull request; another installation cannot change it), `internal/azuredevops:TestExtensionReviewsAndCommentsReadThreadsThroughInvoke`, `internal/azuredevops:TestExtensionCreateUpdateAndReadinessUseDedicatedCommands`, `internal/azuredevops:TestExtensionCommitStatusesUseTheGitStatusesInvokeRoute`. The coordinator's review and readiness passes are route-neutral and covered by the existing review tests in `make check`. | pass, partial (see below) |
| Two installations, one project, Epic with PBIs for both repositories; each claims only its own | `internal/factory:TestAzureExtensionJourneyRunsAnEpicAcrossTwoRepositories`, `internal/azuredevops:TestExtensionQueueListsOnlyWorkItemsAssignedToTheRegisteredRepository` | pass |
| Missing, ambiguous, malformed, unknown, and mismatched tags refused on polling and direct claim | `internal/factory:TestAzureExtensionJourneyRunsAnEpicAcrossTwoRepositories`, `internal/factory:TestClaimRefusesAnInvalidRepositoryAssignmentBeforeAnyEffect`, `internal/azuredevops:TestClaimRepositoryAssignmentRefusesEveryInvalidAssignment`, `internal/azuredevops:TestValidateRepositoryAssignmentRefusesEveryInvalidAssignment`, `internal/azuredevops:TestValidateRepositoryAssignmentFailsClosedWithoutAUsableRepositoryList`, `internal/azuredevops:TestExtensionStartupChecksWarnAboutUnassignedAgentReadyWorkItems` | pass |
| Correlation and assignment tags kept through retry and lifecycle updates | `internal/factory:TestAzureExtensionJourneyRunsAnEpicAcrossTwoRepositories`, `internal/azuredevops:TestExtensionPublishReconcilesLostResponsesWithoutDuplicates`, `internal/azuredevops:TestExtensionReplaceIssueLabelsKeepsAssignmentAndCorrelationTags`, `internal/azuredevops:TestExtensionPublishRefusesAnExistingItemWithAnotherTarget` | pass |
| No redirection after claim | `internal/factory:TestAzureExtensionJourneyRunsAnEpicAcrossTwoRepositories` (a retag after the claim leaves the Run and its pull request in the frozen repository), `internal/azuredevops:TestExtensionCreateRefusesARegistrationThatNoLongerNamesTheFrozenRepository`, `internal/store:TestFrozenTrackerRepositoryCannotBeRedirected` | pass |
| Login failure, command routing, no `az rest` | `internal/azuredevops:TestExtensionRunnerTurnsAMissingOrExpiredLoginIntoARepairInstruction`, `internal/azuredevops:TestExtensionRunnerRedactsSecretsFromCLIErrors`, `internal/azuredevops:TestExtensionRouteNeverFallsBackToAzRest`, `internal/azuredevops:TestExtensionRunnerIsNonInteractiveAndReturnsJSON` | pass |
| Read-only startup diagnosis | `internal/azuredevops:TestExtensionStartupChecksValidateIdentityAndPermissionsReadOnly`, `internal/azuredevops:TestExtensionStartupChecksDiagnoseLoginAndExtensionDistinctly`, `internal/azuredevops:TestExtensionStartupChecksWarnAboutUnassignedAgentReadyWorkItems`, `internal/azuredevops:TestExtensionCodeHostChecksPassReadOnlyProbesWithoutBoards` | pass |
| No PAT in workers | `internal/worker:TestDockerRuntimeRejectsMutableImagesAndCredentialEnvironment` | pass |
| Route selection and compatibility | `internal/config:TestAnOmittedAzureRouteKeepsTheAzRestRoute`, `internal/config:TestValidateHostRejectsAnInvalidAzureRouteSelection`, `internal/cli:TestAdaptersCarryTheSelectedAzureRoute` | pass |
| Review regressions: early frozen-GUID check, retry assignment checks, queue continuation, rendered descriptions, and clarification recovery | `internal/azuredevops:TestExtensionAdoptionRefusesAReplacementBeforeUpdating`, `internal/azuredevops:TestExtensionRetryRefusesAReassignedSuccessfulItem`, `internal/azuredevops:TestExtensionQueueContinuesPastIneligibleItems`, `internal/azuredevops:TestExtensionCreatePlanItemUsesTheDedicatedCommand`, `internal/azuredevops:TestExtensionClarificationLookupRecoversOnlyTheAuthenticatedAuthorsComment` | pass |
| Read-only helper and permission denial | `internal/azuredevops:TestCLIHelperSDKContract`, `internal/azuredevops:TestCLIHelperSourcesArePrivateAndRemoved`, `internal/azuredevops:TestExtensionRunnerLoadsAndRemovesTheEmbeddedHelper`, `internal/azuredevops:TestExtensionPreflightRefusesDeniedPermissions`, `internal/azuredevops:TestExtensionPlanPermissionsRefusesAChildAreaDenial` | pass |

**Partial.** The journey records the end of the implementation turn through
the store before it creates the draft pull request, as
`internal/factory:TestCreateDraftPullRequestLinksTheSourceWorkItemOfAFrozenRepository`
does. It does not drive the implementation turn, the Gate, or a coordinator
review pass on this route; review feedback and readiness run through the real
client on the pull request that the journey created. The journey's commit
statuses go to a recording fake, not to the real client;
`internal/azuredevops:TestExtensionCommitStatusesUseTheGitStatusesInvokeRoute`
covers the client's status route.

## Regression

`make check` (formatting, vet, all tests, build) passed with these
regression guards:

- GitHub: every ideation journey (subtest `github`) and the existing
  issue-to-pull-request tests.
- The `az rest` route: every ideation journey (subtest `azure-devops`),
  `internal/azuredevops:TestAzRestRouteDoesNotRunExtensionCommands`,
  `internal/azuredevops:TestAzRestRouteNeedsNoRepositoryAssignment`,
  `internal/azuredevops:TestAzRestPreflightRefusesRepositoryTargets`, and the
  publication tests of [the az rest record](azure-devops-plan-publication.md).
- Local Markdown tracker with Azure Repos: the journeys of
  [the local backlog record](local-markdown-backlog.md),
  `internal/cli:TestLocalCompositionSuppliesIndependentHostCapabilities`, and
  `internal/effect:TestUpsertPullRequestLinksNothingWithoutASourceWorkItem`
  (a local issue number is never sent as a work-item ID).

## Known limits

These limits hold for this release. A live run must not report them as
passed.

1. **Helper evidence is scripted.** The bundled read-only CLI helper uses
   the installed official extension's connection-data and security SDKs.
   Offline command dispatch, SDK contracts, author-aware comment recovery,
   and denied-permission behavior pass; none has authenticated against a live
   Azure DevOps project. Organization policy and PAT scopes remain live checks.
2. **Description format.** The dedicated create command stores rendered GFM
   HTML. Source Markdown is approval-bound; intake converts HTML to text with
   link destinations, checkbox states, and Factory markers. It does not promise
   reconstruction of Markdown source syntax. Service-side HTML normalization
   needs live evidence (ADR 0030 decision 15).
3. **Plan target control.** A plan node selects another repository only
   through `target_repository` in the stored or draft plan JSON. The plan
   editor has no control for it, and the planning role gets no guidance to
   set it. A node without a target executes on the publishing installation's
   repository.
4. **PAT-only authentication is not enforced.** The extension uses an
   existing `az login` session before the PAT, and an exported
   `AZURE_DEVOPS_EXT_PAT` before the stored login (ADR 0030, residual
   authentication risk). An operator who must use only the PAT runs Factory
   as an account without an `az login` session and without that variable.
5. **One repository per installation.** Host schema version 2 registers one
   repository. Two repositories in one project are two installations; one
   Run never spans repositories.
6. **Scripted only.** Nothing in this record ran against Azure DevOps. The
   scripted project does not model the reverse link ends (Child, Successor)
   that Azure DevOps adds on the target item, the `7.1-preview` comment
   version, or the `multilineFieldsFormat` of the batch read.

## Disposable-project live procedure

Run this procedure only with an authorized disposable target. Otherwise the
live result stays `not-run`. Never record it as `pass` from scripted
evidence.

### Preconditions

- A disposable Azure DevOps Services project on the Scrum process with no
  real backlog, and two Git repositories in it, for example `service` and
  `web`, each with a `main` branch and a `factory.yaml`.
- A PAT for this organization only, with the scopes and permissions of the
  configuration guide and a short expiry.
- The account that runs Factory has no `az login` session
  (`az account show` fails) and `AZURE_DEVOPS_EXT_PAT` is not set
  (`printenv AZURE_DEVOPS_EXT_PAT` prints nothing). Without both, a pass does
  not prove PAT-only authentication.
- Git push and fetch to both repositories work through the host's own Git
  credential helper or SSH agent.
- A Factory build of the revision under test and a worker image of that
  release pinned in each `factory.yaml`.

### Steps

1. **Versions.** Run `az version --output json` and
   `az extension show --name azure-devops --output json`. Record the Azure
   CLI and extension versions and `factory version --json`.
2. **Sign in.** Run
   `az devops login --organization https://dev.azure.com/<org>` and type the
   PAT at the `Token:` prompt. Do not pass it as an argument, an `echo` pipe,
   or a variable.
3. **Repository IDs.** Run
   `az repos show --project <project> --repository service --organization https://dev.azure.com/<org> --output json`
   and the same for `web`. Record both `id` values.
4. **Two installations.** Write one host configuration for each repository
   with its own `operational_data_path` and `route: devops_extension`. The
   `service` configuration has the Epic/PBI `planning` mapping and
   `plan_repositories: [web]`. Run `factory doctor --config <path> --json`
   for each. Expected: login, repository, Boards query, identity, and effective
   permissions pass; no work item, tag, or pull request changes. A denied or
   unobservable permission fails the check.
5. **Plan.** In the `service` installation, run an ideation session to a
   plan with one parent and two tasks. Keep each task short: Azure Repos
   accepts at most 4,000 characters in a pull-request description. Set
   `target_repository: web` on one task: send an `edit_draft` command to the
   session command API (`POST /api/commands` of `factory ui`, with the
   command token of the page) whose draft nodes carry it (Known limits,
   item 3). Preview. Expected: the review page shows each PBI's target name
   and GUID. Approve.
6. **Publication.** Expected: one Epic and two PBIs; each PBI shows the Epic
   as Parent and the Epic shows both children; no Feature. Each PBI has its
   `factory-plan` tag and the `factory-repo` tag of its repository; the Epic
   has no `factory-repo` tag. No item has `agent-ready`, and neither
   installation starts a Run. Record the Epic and PBI URLs and tags.
7. **Retry.** Stop the coordinator during a later publication of a second
   session, restart, and publish again. Expected: no duplicate work item or
   link.
8. **Assignment refusals.** Create PBIs with no, two, a malformed
   (`factory-repo:service`), an unknown, and the other repository's
   `factory-repo` tag, and add `agent-ready` as an authorized user. Run
   `factory issue --config <service config> <ID>` for each. Expected: each
   is refused with the tag to set, and no Run starts. The unknown tag is
   reported as naming no repository of the project; the other repository's
   tag is reported as mismatched and names that repository.
   `factory doctor --config <service config>` warns about the missing,
   malformed, two-tag, and unknown PBIs, not about the web PBI. Add `agent-ready` to a
   correctly assigned PBI as a user who is not in `authorized_users`.
   Expected: no installation claims it.
9. **Execution.** Add `agent-ready` to both published PBIs as an authorized
   user. Start both installations (`factory start --config <path>`).
   Expected: each claims only its own PBI; each PBI keeps both metadata tags
   beside the state tag; the status comment on each PBI exists once and is
   edited in place, not posted again.
   Post a `/factory` command on a PBI and check that the Run reads it.
10. **No redirection.** Change the `factory-repo` tag of the claimed
    `service` PBI to the `web` GUID. Expected: the Run continues in
    `service`; the `web` installation does not claim it.
11. **Pull requests.** Expected: each draft pull request is in its PBI's
    repository and the PBI lists it as a linked pull request. Record the
    pull request URLs and run
    `az repos pr work-item list --id <PR> --organization https://dev.azure.com/<org> --output json`
    for each.
12. **Review and readiness.** Vote "waiting for author" with one comment on
    one pull request. Expected: the vote is read as a change request with
    the comment as its finding, implementation resumes, and the pull request
    returns to draft; after the repair it becomes ready and shows its commit
    statuses. Request clarification on the pull request; verify the questions
    are published and a restarted coordinator recovers the same marked
    comment rather than creating a duplicate.
13. **Expiry.** Run `az devops logout --organization <URL>` and
    `factory doctor --json`. Expected: the login check fails with
    `run az devops login --organization <URL>`, and nothing else changes.
    Sign in again.

### Record

| Item | Value |
| --- | --- |
| Azure CLI version | not run |
| azure-devops extension version | not run |
| Factory revision | not run |
| Organization and project | not run |
| Repository IDs (`service`, `web`) | not run |
| Epic URL | not run |
| PBI URLs and their `factory-plan` and `factory-repo` tags | not run |
| Pull request URLs and linked PBIs | not run |
| Doctor result per installation | not run |
| Result per step | not run |

### Cleanup

Abandon the pull requests and delete their branches. Remove the work items
and the factory tags of the disposable project. Run
`az devops logout --organization <URL>` and revoke the PAT.

## Interventions, time, and usage

| Item | Value |
| --- | --- |
| Human interventions | None in the scripted journeys. |
| Elapsed time | The extension subtests of the ideation journeys and the two-repository journey take less than 2 s together. |
| Usage | No paid model call ran: the journeys use report fixtures. |

## Review-fix scripted rerun

The two-installation journey now creates both the service and web PRs and
checks their separate repository GUIDs and native source-PBI links. Regression
checks also cover a replacement repository before PR mutation, reassigned
items restored from the publication ledger, the queue beyond 1000 older
ineligible items, denied child-area permissions, rendered Markdown/HTML, and
coordinator-owned clarification recovery. The CLI helper is embedded in the
Factory binary and needs no separate installation. Offline dispatch uses
mocked service responses and is not live Azure DevOps evidence.
