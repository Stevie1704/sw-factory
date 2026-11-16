# Factory acceptance report

- Release: development (development), revision cca8dcfa015904aa61982a8bc67263f22f4be4ab, modified false
- Platform: darwin/arm64
- Host harness: not observed
- Worker digest: not observed
- Generated: 2026-11-16T15:49:31Z
- Recorded duration: 0s

A deterministic pass uses fake adapters. It is not evidence that an agent can use the release; only a live pass is.

| Evidence | pass | fail | blocked | not-run |
| --- | --- | --- | --- | --- |
| live | 0 | 0 | 0 | 21 |
| deterministic | 2 | 0 | 0 | 19 |

## Scenarios

| Scenario | Live | Deterministic | Gaps |
| --- | --- | --- | --- |
| `onboard-from-release` | not-run | not-run | none |
| `onboard-instructions-only` | not-run | not-run | none |
| `resume-interrupted-onboarding` | not-run | not-run | none |
| `diagnose-failures` | not-run | not-run | none |
| `recover-operations` | not-run | not-run | #181 |
| `inspect-history` | not-run | not-run | none |
| `upgrade-maintenance` | not-run | not-run | #257, #258, #204 |
| `defect-report` | not-run | not-run | none |
| `scoped-authority` | not-run | not-run | none |
| `contributor-maintenance` | not-run | not-run | none |
| `ideation-publish-github` | not-run | not-run | none |
| `ideation-publish-azure-devops` | not-run | not-run | none |
| `ideation-without-backlog` | not-run | not-run | none |
| `ideation-recovery` | not-run | not-run | #265 |
| `copilot-mixed-harness-recovery` | not-run | not-run | #281 |
| `local-backlog-publish-and-start` | not-run | not-run | none |
| `local-backlog-authorize-committed` | not-run | not-run | none |
| `local-backlog-recovery` | not-run | not-run | none |
| `local-backlog-code-host-access` | not-run | not-run | #300, #301 |
| `azure-extension-publish-and-execute` | not-run: not run in the #307 acceptance; no authorized disposable Azure DevOps project, and az is not installed on the development host | pass: scripted az only; the implementation turn and the Gate are not driven on this route: the journey records the implementation stage through the store, and review feedback and readiness run through the real client, not a coordinator pass | #302, #302 |
| `azure-extension-repository-assignment` | not-run: not run in the #307 acceptance; no authorized disposable Azure DevOps project, and az is not installed on the development host | pass: scripted az only; two installations share one scripted project | #302 |

## Interventions by cause

None recorded.

## Details

### `azure-extension-publish-and-execute` (live): not-run

- Reason: not run in the #307 acceptance; no authorized disposable Azure DevOps project, and az is not installed on the development host

### `azure-extension-publish-and-execute` (deterministic): pass

- Reason: scripted az only; the implementation turn and the Gate are not driven on this route: the journey records the implementation stage through the store, and review feedback and readiness run through the real client, not a coordinator pass

### `azure-extension-repository-assignment` (live): not-run

- Reason: not run in the #307 acceptance; no authorized disposable Azure DevOps project, and az is not installed on the development host

### `azure-extension-repository-assignment` (deterministic): pass

- Reason: scripted az only; two installations share one scripted project
