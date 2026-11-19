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

The baseline comes from a development build at the revision that the report
names, not from a release archive. The next baseline uses a release.

Every scenario's deterministic tests pass in `make check`. The
`upgrade-maintenance` and `recover-operations` deterministic results were
rerun after #257, #258, #181, and #318–#323 and #325 shipped on this branch.
Their named tests cover the upgrade plan, the pre-migration backup, the
checkpoint create, inspect, and restore boundary of ADR 0032, and the effect
failure injection. Restore is supported only under the ADR 0032
preconditions; downgrade and cross-release restore are refused. The tests use fake
adapters, temporary stores, and temporary file systems. They need no paid
model, no personal credential, and no write to a real repository. They prove
that the semantics a scenario depends on hold. They do not prove that an
agent can use the release.

No live scenario ran. A live run needs an operator-approved target
repository, a host harness, and a spending limit. Each live scenario is
therefore `not-run`, not `pass`. Record a live run for Claude Code and a
separate live run for Codex as separate reports.

## Remaining gaps

The walkthrough used the installed binary outside the source checkout:
`factory version`, `factory guide` and its topics, `factory acceptance
scenarios`, and `factory onboard prepare`. The rows below are the gaps that
remain after the rerun. #257, #258, and #181 are closed in the deterministic
results; their rows are removed.

| Scenario | Gap | Tracking |
| --- | --- | --- |
| `upgrade-maintenance` | Service supervision under launchd or systemd is not part of the upgrade. | #204 |
| `contributor-maintenance` | README, AGENTS.md, and `docs/agents/domain.md` pointed a coding agent to a missing `CONTEXT.md`, and AGENTS.md did not name `make check`. Fixed with this baseline; `TestContributorGuidanceLinksResolve` keeps the README and AGENTS.md links resolvable. No fresh agent has run the fix exercise yet. | #259 |

A development build cannot run `factory onboard prepare` without
`--worker-base-image`. That is the documented behavior of a development
build, not a gap; a live run uses a release archive.

## Next measurements

The deterministic rerun of the affected scenarios is done for #257, #258,
#181, and #318–#325. Live runs stay `not-run` until an operator records them
with a release archive, an approved target repository, a host harness, and a
spending limit. Record each live run for Claude Code and for Codex as its own
report. The next live rerun of `upgrade-maintenance` must cover the restore
boundary; `recover-operations` needs its own live run.

Still open: #204 (launchd and systemd supervision). #195 and #218 are not
shipped; the company-repository proof of concept (#218) stays separate, and
synthetic scenarios do not replace it. Record the integrated release report
on the roadmap issue #260.
