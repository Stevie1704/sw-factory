# Factory acceptance report

- Release: development (development), revision 711704928bc7fa4dca255583445efd9fa03a530e, modified false
- Platform: darwin/arm64
- Host harness: not observed
- Worker digest: not observed
- Generated: 2026-10-15T13:13:59Z
- Recorded duration: 0s

A deterministic pass uses fake adapters. It is not evidence that an agent can use the release; only a live pass is.

| Evidence | pass | fail | blocked | not-run |
| --- | --- | --- | --- | --- |
| live | 0 | 0 | 0 | 10 |
| deterministic | 10 | 0 | 0 | 0 |

## Scenarios

| Scenario | Live | Deterministic | Gaps |
| --- | --- | --- | --- |
| `onboard-from-release` | not-run: no live run yet; needs an operator-approved target repository, host harness, and spending limit | pass | none |
| `onboard-instructions-only` | not-run: no live run yet; needs an operator-approved target repository, host harness, and spending limit | pass | none |
| `resume-interrupted-onboarding` | not-run: no live run yet; needs an operator-approved target repository, host harness, and spending limit | pass | none |
| `diagnose-failures` | not-run: no live run yet; needs an operator-approved target repository, host harness, and spending limit | pass | none |
| `recover-operations` | not-run: no live run yet; needs an operator-approved target repository, host harness, and spending limit | pass: covers recovery semantics only; durable-effect failure injection stays with #181 | #181 |
| `inspect-history` | not-run: no live run yet; needs an operator-approved target repository, host harness, and spending limit | pass | none |
| `upgrade-maintenance` | not-run: no live run yet; needs an operator-approved target repository, host harness, and spending limit | pass: covers only the worker release check; no upgrade preflight, resume, or restore contract ships | #257, #258, #204 |
| `defect-report` | not-run: no live run yet; needs an operator-approved target repository, host harness, and spending limit | pass | none |
| `scoped-authority` | not-run: no live run yet; needs an operator-approved target repository, host harness, and spending limit | pass | none |
| `contributor-maintenance` | not-run: no live run yet; needs an operator-approved target repository, host harness, and spending limit | pass | none |

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

- Reason: covers recovery semantics only; durable-effect failure injection stays with #181

### `inspect-history` (live): not-run

- Reason: no live run yet; needs an operator-approved target repository, host harness, and spending limit

### `inspect-history` (deterministic): pass


### `upgrade-maintenance` (live): not-run

- Reason: no live run yet; needs an operator-approved target repository, host harness, and spending limit

### `upgrade-maintenance` (deterministic): pass

- Reason: covers only the worker release check; no upgrade preflight, resume, or restore contract ships

### `defect-report` (live): not-run

- Reason: no live run yet; needs an operator-approved target repository, host harness, and spending limit

### `defect-report` (deterministic): pass


### `scoped-authority` (live): not-run

- Reason: no live run yet; needs an operator-approved target repository, host harness, and spending limit

### `scoped-authority` (deterministic): pass


### `contributor-maintenance` (live): not-run

- Reason: no live run yet; needs an operator-approved target repository, host harness, and spending limit

### `contributor-maintenance` (deterministic): pass

