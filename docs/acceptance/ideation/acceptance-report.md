# Factory acceptance report

- Release: development (development), revision 783a70888554c5ff89722ecdf2e604dd25cc96c0, modified false
- Platform: darwin/arm64
- Host harness: not observed
- Worker digest: not observed
- Generated: 2026-10-16T14:14:00Z
- Recorded duration: 0s

A deterministic pass uses fake adapters. It is not evidence that an agent can use the release; only a live pass is.

| Evidence | pass | fail | blocked | not-run |
| --- | --- | --- | --- | --- |
| live | 0 | 0 | 0 | 14 |
| deterministic | 13 | 0 | 1 | 0 |

## Scenarios

| Scenario | Live | Deterministic | Gaps |
| --- | --- | --- | --- |
| `onboard-from-release` | not-run: not run in the #275 acceptance; live runs were not authorized for this run | pass | none |
| `onboard-instructions-only` | not-run: not run in the #275 acceptance; live runs were not authorized for this run | pass | none |
| `resume-interrupted-onboarding` | not-run: not run in the #275 acceptance; live runs were not authorized for this run | pass | none |
| `diagnose-failures` | not-run: not run in the #275 acceptance; live runs were not authorized for this run | pass | none |
| `recover-operations` | not-run: not run in the #275 acceptance; live runs were not authorized for this run | pass: covers recovery semantics only; durable-effect failure injection stays with #181 | #181 |
| `inspect-history` | not-run: not run in the #275 acceptance; live runs were not authorized for this run | pass | none |
| `upgrade-maintenance` | not-run: not run in the #275 acceptance; live runs were not authorized for this run | blocked: only the worker release check has coverage; no upgrade preflight, resume, or restore contract ships | #257, #258, #204 |
| `defect-report` | not-run: not run in the #275 acceptance; live runs were not authorized for this run | pass | none |
| `scoped-authority` | not-run: not run in the #275 acceptance; live runs were not authorized for this run | pass | none |
| `contributor-maintenance` | not-run: not run in the #275 acceptance; live runs were not authorized for this run | pass | none |
| `ideation-publish-github` | not-run: not authorized for #275: needs a disposable GitHub repository, a host harness, a spending limit, and a worker image of this release pinned in factory.yaml | pass | none |
| `ideation-publish-azure-devops` | not-run: not authorized for #275: needs a disposable Azure DevOps project, a host harness, a spending limit, and a worker image of this release pinned in factory.yaml | pass | none |
| `ideation-without-backlog` | not-run: not authorized for #275: needs a host harness and a spending limit for the paid ideation turn | pass | none |
| `ideation-recovery` | not-run: not authorized for #275: needs disposable GitHub and Azure DevOps targets, a host harness, and a spending limit | pass: session-role volume and snapshot cleanup by reset stays open (ADR 0023) | #265 |

## Interventions by cause

None recorded.

## Details

### `onboard-from-release` (live): not-run

- Reason: not run in the #275 acceptance; live runs were not authorized for this run

### `onboard-from-release` (deterministic): pass


### `onboard-instructions-only` (live): not-run

- Reason: not run in the #275 acceptance; live runs were not authorized for this run

### `onboard-instructions-only` (deterministic): pass


### `resume-interrupted-onboarding` (live): not-run

- Reason: not run in the #275 acceptance; live runs were not authorized for this run

### `resume-interrupted-onboarding` (deterministic): pass


### `diagnose-failures` (live): not-run

- Reason: not run in the #275 acceptance; live runs were not authorized for this run

### `diagnose-failures` (deterministic): pass


### `recover-operations` (live): not-run

- Reason: not run in the #275 acceptance; live runs were not authorized for this run

### `recover-operations` (deterministic): pass

- Reason: covers recovery semantics only; durable-effect failure injection stays with #181

### `inspect-history` (live): not-run

- Reason: not run in the #275 acceptance; live runs were not authorized for this run

### `inspect-history` (deterministic): pass


### `upgrade-maintenance` (live): not-run

- Reason: not run in the #275 acceptance; live runs were not authorized for this run

### `upgrade-maintenance` (deterministic): blocked

- Reason: only the worker release check has coverage; no upgrade preflight, resume, or restore contract ships

### `defect-report` (live): not-run

- Reason: not run in the #275 acceptance; live runs were not authorized for this run

### `defect-report` (deterministic): pass


### `scoped-authority` (live): not-run

- Reason: not run in the #275 acceptance; live runs were not authorized for this run

### `scoped-authority` (deterministic): pass


### `contributor-maintenance` (live): not-run

- Reason: not run in the #275 acceptance; live runs were not authorized for this run

### `contributor-maintenance` (deterministic): pass


### `ideation-publish-github` (live): not-run

- Reason: not authorized for #275: needs a disposable GitHub repository, a host harness, a spending limit, and a worker image of this release pinned in factory.yaml

### `ideation-publish-github` (deterministic): pass


### `ideation-publish-azure-devops` (live): not-run

- Reason: not authorized for #275: needs a disposable Azure DevOps project, a host harness, a spending limit, and a worker image of this release pinned in factory.yaml

### `ideation-publish-azure-devops` (deterministic): pass


### `ideation-without-backlog` (live): not-run

- Reason: not authorized for #275: needs a host harness and a spending limit for the paid ideation turn

### `ideation-without-backlog` (deterministic): pass


### `ideation-recovery` (live): not-run

- Reason: not authorized for #275: needs disposable GitHub and Azure DevOps targets, a host harness, and a spending limit

### `ideation-recovery` (deterministic): pass

- Reason: session-role volume and snapshot cleanup by reset stays open (ADR 0023)
