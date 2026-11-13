# Local Markdown backlog acceptance evidence

This record is the evidence of issue #298 for specification #291 (local
Markdown issues with UI approval and supervised implementation). It follows
the conventions of the [agent-journey baseline](baseline.md): deterministic
evidence and live evidence stay apart, and a scenario that did not run is
`not-run`, never `pass`. Read the scenarios with
`factory acceptance scenarios --json`; the local scenarios are
`local-backlog-publish-and-start`, `local-backlog-authorize-committed`,
`local-backlog-recovery`, and `local-backlog-code-host-access`.

- [Results](local-markdown-backlog-results.json): the recorded outcomes of
  the four local scenarios.
- [Report](local-markdown-backlog/acceptance-report.md) and its
  [JSON form](local-markdown-backlog/acceptance-report.json), written by
  `factory acceptance report`. The report lists every manifest scenario.
  The scenarios outside #298 are `not-run` in this report because this run
  did not record them; their evidence is in [ideation.md](ideation.md),
  [baseline.md](baseline.md), and [copilot.md](copilot.md).

Scripted adapters are not live-provider proof. The live scenarios below did
not run.

## Identities

| Item | Value |
| --- | --- |
| Platform | darwin/arm64, macOS 27.0.1, Go 1.27.2 (go.mod toolchain) |
| Release | Development build, revision `8a624368e72b03bcf7079c3097d423a737c77846` (branch `feat/291-local-markdown-backlog`), unmodified |
| Pinned worker image | Not rebuilt for #298. No worker or report contract changed; no live invocation ran. |
| Host harness | Not observed: no live scenario ran. The implementing agent was Claude Code (Claude Opus 5.5); it is not product evidence. |

## Integrated journeys (deterministic)

The journeys are in `internal/factory/local_acceptance_test.go`. Each one
drives the real UI handler and the protected command inbox, session roles
over the real Codex headless adapter with report fixtures, the production
local publisher (`internal/localpublication`) and local tracker
(`internal/localtracker`) over a temporary Git repository with a bare origin,
the real Git worktree manager, a SQLite store, the coordinator queue and
lifecycle passes, and a scripted GitHub code host. The human steps are real
Git operations in a separate reviewer clone: a push of the specification
branch, a squash merge, and implementation merges. The code-host double fails
the test on any issue read, label, or comment call.

| Test | Result | What it shows |
| --- | --- | --- |
| `internal/factory:TestLocalBacklogJourneyStartsOneIssueAfterTheHumanSpecificationMerge` | pass | Discussion, exact preview, a double-clicked Approve and start, one specification commit with exactly the previewed bytes and no conversation text, an untouched operator checkout (branch, HEAD, staged, unstaged, and untracked changes), `waiting_for_specification_merge` on the page across a restart and a resent approval, the claim at the human squash commit, review-ready that keeps the slot, completion after the human implementation merge, and one Run in total. |
| `internal/factory:TestLocalBacklogJourneyRunsAParentsTasksInDependencyOrder` | pass | A change request and a revised draft, a decline refused while it leaves a dangling dependency and applied after the repair, one batch commit for the parent and two tasks, a coordinator and browser restart after every durable transition, the dependent `blocked_by_dependencies` while its prerequisite is claimed and review-ready, the dependent claimed at the prerequisite's merge commit, a parent without a Run, and one Run per task. |
| `internal/factory:TestLocalBacklogJourneyAuthorizesCommittedWorkAfterTheSessionEnded` | pass | Accept and commit only: the session is published and terminal, the merged item stays `missing_execution_approval`, a fresh exact execution preview and approval on `/issues` start the work, a replayed approval returns the stored command, and a cancellation from the Run controls ends the Run without a second claim. |

## Traceability to #298

| #298 criterion | Evidence |
| --- | --- |
| End-to-end journey: discussion/plan, revise/decline, exact approval, committed issues, target visibility, dependency-ordered Runs, Gate/review, human merge | The three journeys. They record review-ready through the store and the code-host pull request, as the per-ticket dependency test does; the Gate and review stages themselves are guarded by the existing issue-to-pull-request tests in `make check`. |
| Single issue and parent with dependents, terminal-session reauthorization, input/cancel/recovery, protected-branch/manual publishing waits, restart at each durable transition | The three journeys; input and retry semantics: `TestLocalAnswerRecoveryContinuesWithOneNewPacketThroughNormalProgression`, `TestLocalCancellationRetryAndUnsupportedResumeUseOrdinaryRunSemantics`. |
| Drafts and conversations absent, checkout preserved, no remote issue calls, publication-only never starts, import/reset restores no authority, no duplicate commits or Runs | The journeys; `TestPublicationOnlyItemsRemainVisibleAndNeverStart`, `TestARecreatedStoreImportsContentWithoutLifecycleAuthority`, `TestBatchCommitPreservesCheckoutAndRecovers`, `TestTerminalSessionIndependentExecutionApprovalAndReplay`. |
| Malformed schemas/paths/graphs, missing or changed files, lost commit/push/PR responses, ambiguous effects, partial batch/cancellation, stale authorization, failed/cancelled dependencies, unsupported investigations, unavailable supervisor/code host | The `deterministic_tests` of `local-backlog-recovery` and `local-backlog-authorize-committed` in the scenario manifest, which `TestEveryScenarioNamesExistingDeterministicTests` keeps current. |
| GitHub/Azure regression, report, `make check`, live runs separate | `make check` passed, including every GitHub and Azure DevOps execution and publication test and the ideation journeys. Live scenarios are `not-run` below. |
| Documentation | [Local Markdown issue contract](../local-markdown-issues.md) (operator workflow and code-host limitation), [configuration](../configuration.md#local-tracker-and-independent-code-host), [ideation](../ideation.md#local-markdown-issue-review), [agent runtime](../agent-runtime.md), [repository initialization](../repository-initialization.md), `factory guide` topics, and the README. |
| Azure Repos still needs `az`; diagnosis and an explicit unsupported result | `local-backlog-code-host-access`; doctor conditions `azure_devops.code_host.access.unavailable` and `github.code_host.access.unavailable`. |
| Fully local/manual completion and alternative Azure access are follow-up scope | Recorded as a gap of `local-backlog-code-host-access` and under "Not delivered" below. |

Regression: `make check` (formatting, vet, all tests, build) passed. The three
journeys and the ideation journeys also passed with the race detector.

## Scenarios that did not run (live)

Live runs were not authorized for #298. Each of these is a specific
unverified scenario:

1. The complete local journey with live ideation, planning, and
   implementation turns on Codex or Claude Code, with a worker image of this
   release.
2. Manual publishing of a specification branch to a protected GitHub target
   through a real pull request and merge, including a squash merge.
3. The `push` and `pull_request` publication policies against a real code
   host, including a lost push or pull request response.
4. Implementation pull requests, reviews, and commit statuses on a real
   GitHub repository for a local issue, and their human merge releasing a
   dependent.
5. Azure Repos as the code host of a local tracker, with Boards blocked and
   Repos available, and the doctor result when company policy also blocks
   Azure Repos pull request or status access.

## Not delivered (follow-up scope)

- Completion without any code-host adapter (fully local or manual review and
  merge).
- Browser automation, or an Azure authentication route other than `az`.
- A change to corporate access restrictions.

The local tracker removes Azure Boards and GitHub Issues access only. If
company policy also blocks Azure Repos through `az`, a local tracker alone
cannot complete the issue-to-pull-request flow; doctor reports it before any
Run starts.

## Interventions, time, and usage

| Item | Value |
| --- | --- |
| Human interventions | None in the deterministic journeys. A usage limit stopped the implementing agent of #291; another session continued #298 (cause: `environment`). |
| Elapsed time | The three journeys take about 13 s together. |
| Usage | No paid model call ran: the journeys use report fixtures. |

Conversation content is not telemetry. This record holds counts and
identities only.
