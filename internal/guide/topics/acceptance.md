# Agent-journey acceptance

An acceptance run measures whether a fresh operator agent can use this
release, from setup through diagnosis and maintenance, with supported
interfaces only. Use it to produce the release evidence report. It is
optional, and it can spend money and create external artifacts. Start it only
when the operator asks for it.

## Read the scenarios

```sh
factory acceptance scenarios --json
```

Each scenario names its starting fixture, the user objective, the authority
and constraints, the interfaces the agent may use, the injected condition,
the observable success and failure criteria, and the cleanup. `gaps` names a
capability that this release does not ship. `deterministic_tests` names the
CI tests with fake adapters that guard the scenario's semantics. A
deterministic pass is not evidence that an agent can use the release.

## Agree the run with the operator

Before the first scenario, get these values from the operator and record
them in the session:

- The target repository. Use a small disposable repository, never a
  production repository.
- The host harness, Claude Code or Codex. Do one run for each harness, with
  a fresh agent session and a new results file each time.
- The spending limit, and the scenarios that may make paid calls.
- The external destinations that may receive artifacts, for example the
  disposable repository's issue tracker. A defect report goes to a
  test destination only, never to the Software Factory repository.
- A run marker, for example `acceptance-<date>-<harness>`. Put it in every
  issue, branch, and directory that the run creates. Clean up only artifacts
  that carry the marker.

Apply `factory guide authorization` to every action. The run itself does not
authorize a paid call, an external effect, or a human disposition.

## Run a scenario

1. Start from the scenario's fixture. Use only the scenario's interfaces and
   the guidance that the installed release ships.
2. Inject the scenario's condition.
3. Observe the success and failure criteria. Record what you observed, not
   what you expected.
4. Do the scenario's cleanup. Remove only artifacts that carry the run
   marker.

After an interruption, find the artifacts that carry the run marker before
you act again. Do not repeat an external effect whose outcome is unknown;
use the recovery that the scenario's interfaces supply.

## Record the outcome

Write one results file per run. Do not add fields that the schema does not
name; the report refuses them.

```json
{
  "schema_version": 1,
  "host_harness": {"name": "claude", "version": "<claude --version>"},
  "worker_digest": "sha256:<digest of the worker image the run used>",
  "worker_harnesses": [{"name": "codex", "version": "<version in the worker>"}],
  "results": [
    {
      "scenario": "inspect-history",
      "evidence": "live",
      "outcome": "pass",
      "duration_seconds": 420,
      "interventions": [{"cause": "authorization", "summary": "operator approved the gate retry"}],
      "workarounds": [],
      "usage": {"measured": false, "reason": "the harness reports no cost"}
    }
  ]
}
```

- `evidence` is `live` for an agent run, or `deterministic` for a CI result.
- `outcome` is `pass`, `fail`, `blocked`, or `not-run`. Every outcome except
  `pass` needs a one-line `reason`. A skipped paid step is `not-run` or
  `blocked`, never `pass`.
- A live result that ran needs `duration_seconds` and `usage`. Record
  measured tokens or cost. When the harness does not measure them, set
  `measured` to false and give the reason. Do not estimate.
- `interventions` lists each human step with its cause: `authorization`,
  `human-disposition`, `credential`, `environment`, `factory-defect`,
  `guidance-gap`, or `other`.
- `workarounds` lists each action outside the supported interfaces:
  `operational-store-edit`, `run-branch-edit`, `hidden-checkout-knowledge`,
  `weakened-gate`, or `other`. A result with a workaround is never a pass.
- Omit a value you did not observe. A scenario without a result is reported
  as `not-run`.

Every text value is one line of at most 300 characters. Do not put
repository content, credentials, prompts, transcripts, or home-directory
paths into the results file. Keep raw evidence local; the debug and
reporting topics decide what may leave the host.

## Write the report

```sh
factory acceptance report --results <results file> --output <new directory> --json
```

The command adds the release, revision, and platform of this build. It
writes `acceptance-report.json` and `acceptance-report.md` with owner-only
permissions, and it never replaces an earlier report. The report shows live
and deterministic outcomes in separate columns, the recorded gaps, the
interventions by cause, the unsupported workarounds, and the usage. Give the
report to the operator. The operator decides whether to publish it.
