# Factory acceptance report

- Release: development (development), revision 7a39a566dbb05b6f443cf0ce7b59259463eba5b0, modified false
- Platform: darwin/arm64
- Host harness: not observed
- Worker digest: not observed
- Generated: 2026-11-13T17:39:13Z
- Recorded duration: 0s

A deterministic pass uses fake adapters. It is not evidence that an agent can use the release; only a live pass is.

| Evidence | pass | fail | blocked | not-run |
| --- | --- | --- | --- | --- |
| live | 0 | 0 | 0 | 19 |
| deterministic | 4 | 0 | 0 | 15 |

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
| `local-backlog-publish-and-start` | not-run: not run in the #298 acceptance; live provider, harness, and code-host runs were not authorized | pass | none |
| `local-backlog-authorize-committed` | not-run: not run in the #298 acceptance; live provider, harness, and code-host runs were not authorized | pass | none |
| `local-backlog-recovery` | not-run: not run in the #298 acceptance; live provider, harness, and code-host runs were not authorized | pass | none |
| `local-backlog-code-host-access` | not-run: not run in the #298 acceptance; live provider, harness, and code-host runs were not authorized | pass: covers diagnosis and claim refusal; completion without a code host is follow-up scope | #300, #301 |

## Interventions by cause

None recorded.

## Details

### `local-backlog-publish-and-start` (live): not-run

- Reason: not run in the #298 acceptance; live provider, harness, and code-host runs were not authorized

### `local-backlog-publish-and-start` (deterministic): pass


### `local-backlog-authorize-committed` (live): not-run

- Reason: not run in the #298 acceptance; live provider, harness, and code-host runs were not authorized

### `local-backlog-authorize-committed` (deterministic): pass


### `local-backlog-recovery` (live): not-run

- Reason: not run in the #298 acceptance; live provider, harness, and code-host runs were not authorized

### `local-backlog-recovery` (deterministic): pass


### `local-backlog-code-host-access` (live): not-run

- Reason: not run in the #298 acceptance; live provider, harness, and code-host runs were not authorized

### `local-backlog-code-host-access` (deterministic): pass

- Reason: covers diagnosis and claim refusal; completion without a code host is follow-up scope
