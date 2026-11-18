# Factory acceptance report

- Release: development (development), revision 5d00e20f8f66ddb86a35e9837246e9242d6fea85, modified false
- Platform: darwin/arm64
- Host harness: not observed
- Worker digest: not observed
- Generated: 2026-11-18T20:04:30Z
- Recorded duration: 0s

A deterministic pass uses fake adapters. It is not evidence that an agent can use the release; only a live pass is.

| Evidence | pass | fail | blocked | not-run |
| --- | --- | --- | --- | --- |
| live | 0 | 0 | 0 | 21 |
| deterministic | 10 | 0 | 0 | 11 |

## Scenarios

| Scenario | Live | Deterministic | Gaps |
| --- | --- | --- | --- |
| `onboard-from-release` | not-run: no live run yet; needs an operator-approved target repository, host harness, and spending limit | pass | none |
| `onboard-instructions-only` | not-run: no live run yet; needs an operator-approved target repository, host harness, and spending limit | pass | none |
| `resume-interrupted-onboarding` | not-run: no live run yet; needs an operator-approved target repository, host harness, and spending limit | pass | none |
| `diagnose-failures` | not-run: no live run yet; needs an operator-approved target repository, host harness, and spending limit | pass | none |
| `recover-operations` | not-run: no live run yet; needs an operator-approved target repository, host harness, and spending limit | pass | none |
| `inspect-history` | not-run: no live run yet; needs an operator-approved target repository, host harness, and spending limit | pass | none |
| `upgrade-maintenance` | not-run: no live run yet; needs an operator-approved target repository, host harness, and spending limit | pass | #204 |
| `defect-report` | not-run: no live run yet; needs an operator-approved target repository, host harness, and spending limit | pass | none |
| `scoped-authority` | not-run: no live run yet; needs an operator-approved target repository, host harness, and spending limit | pass | none |
| `contributor-maintenance` | not-run: no live run yet; needs an operator-approved target repository, host harness, and spending limit | pass | none |
| `ideation-publish-github` | not-run | not-run | none |
| `ideation-publish-azure-devops` | not-run | not-run | none |
| `ideation-without-backlog` | not-run | not-run | none |
| `ideation-recovery` | not-run | not-run | #265 |
| `copilot-mixed-harness-recovery` | not-run | not-run | #281 |
| `local-backlog-publish-and-start` | not-run | not-run | none |
| `local-backlog-authorize-committed` | not-run | not-run | none |
| `local-backlog-recovery` | not-run | not-run | none |
| `local-backlog-code-host-access` | not-run | not-run | #300, #301 |
| `azure-extension-publish-and-execute` | not-run | not-run | none |
| `azure-extension-repository-assignment` | not-run | not-run | #302 |

## Interventions by cause

None recorded.

## Details

### `onboard-from-release` (live): not-run

- Reason: no live run yet; needs an operator-approved target repository, host harness, and spending limit

### `onboard-from-release` (deterministic): pass


### `onboard-instructions-only` (live): not-run

- Reason: no live run yet; needs an operator-approved target repository, host harness, and spending limit

### `onboard-instructions-only` (deterministic): pass


### `resume-interrupted-onboarding` (live): not-run

- Reason: no live run yet; needs an operator-approved target repository, host harness, and spending limit

### `resume-interrupted-onboarding` (deterministic): pass


### `diagnose-failures` (live): not-run

- Reason: no live run yet; needs an operator-approved target repository, host harness, and spending limit

### `diagnose-failures` (deterministic): pass


### `recover-operations` (live): not-run

- Reason: no live run yet; needs an operator-approved target repository, host harness, and spending limit

### `recover-operations` (deterministic): pass


### `inspect-history` (live): not-run

- Reason: no live run yet; needs an operator-approved target repository, host harness, and spending limit

### `inspect-history` (deterministic): pass


### `upgrade-maintenance` (live): not-run

- Reason: no live run yet; needs an operator-approved target repository, host harness, and spending limit

### `upgrade-maintenance` (deterministic): pass


### `defect-report` (live): not-run

- Reason: no live run yet; needs an operator-approved target repository, host harness, and spending limit

### `defect-report` (deterministic): pass


### `scoped-authority` (live): not-run

- Reason: no live run yet; needs an operator-approved target repository, host harness, and spending limit

### `scoped-authority` (deterministic): pass


### `contributor-maintenance` (live): not-run

- Reason: no live run yet; needs an operator-approved target repository, host harness, and spending limit

### `contributor-maintenance` (deterministic): pass

