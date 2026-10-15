# Agent-journey acceptance baseline

This baseline records the current release's position against the
agent-journey scenarios of issue #259. Read the scenarios with
`factory acceptance scenarios --json`, and the live procedure with
`factory guide acceptance`.

- [Baseline results](baseline-results.json): the recorded outcomes.
- [Baseline report](baseline/acceptance-report.md) and its
  [JSON form](baseline/acceptance-report.json), written by
  `factory acceptance report`.

## What the baseline proves

Every scenario's deterministic tests pass in `make check`. They use fake
adapters, temporary stores, and temporary file systems. They need no paid
model, no personal credential, and no write to a real repository. They prove
that the semantics a scenario depends on hold. They do not prove that an
agent can use the release.

No live scenario ran. A live run needs an operator-approved target
repository, a host harness, and a spending limit. Each live scenario is
therefore `not-run`, not `pass`. Record a live run for Claude Code and a
separate live run for Codex as separate reports.

## Gaps found through shipped interfaces

The walkthrough used the installed binary outside the source checkout:
`factory version`, `factory guide` and its topics, `factory acceptance
scenarios`, and `factory onboard prepare`.

| Scenario | Gap | Tracking |
| --- | --- | --- |
| `upgrade-maintenance` | The guide has no upgrade procedure. Only `factory guide setup` names the worker rebuild after an upgrade, and the exported installation document names `worker.release.mismatch`. No preflight or resumable maintenance command ships. | #257 |
| `upgrade-maintenance` | No shipped guidance states that restore and downgrade are unsupported, so an agent cannot report the support boundary from the release alone. | #258 |
| `upgrade-maintenance` | Service supervision under launchd or systemd is not part of the upgrade. | #204 |
| `recover-operations` | The deterministic tests cover the recovery contract, not failure injection around every durable effect. | #181 |
| `contributor-maintenance` | README and AGENTS.md pointed a coding agent to a missing `CONTEXT.md`, and AGENTS.md did not name `make check`. Fixed with this baseline; `TestContributorGuidanceLinksResolve` keeps the links resolvable. | #259 |

A development build cannot run `factory onboard prepare` without
`--worker-base-image`. That is the documented behavior of a development
build, not a gap; a live run uses a release archive.

## Next measurements

Rerun the affected scenarios when #257, #258, #204, and #181 ship, and
record the integrated release report on the roadmap issue #260. The
company-repository proof of concept (#218) stays separate; synthetic
scenarios do not replace it.
