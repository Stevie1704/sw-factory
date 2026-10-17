# Copilot CLI release evidence

This record traces every acceptance criterion of specification #281 (GitHub
Copilot CLI as an isolated factory harness) to deterministic checks or to an
open live release blocker. Issue #287 wrote it. It follows the
[agent-journey baseline](baseline.md) rules: deterministic evidence and live
evidence stay apart, and a check that did not run is `not-run`, never `pass`.

- [Results](copilot-results.json): the recorded outcomes of the
  `copilot-mixed-harness-recovery` scenario
  (`factory acceptance scenarios --json`).
- [Spike 0003](../spikes/0003-copilot-cli-protocol.md): the pinned CLI
  contract and the release blockers RB-1 to RB-8. Only the outputs that tests
  read are committed; `scripts/probe-copilot-cli.sh <dir>` reproduces the full
  output of every probe.
- [Copilot operator guide](../configuration.md#github-copilot-cli): setup,
  smoke, refresh, recovery, and failure actions.

## Release status

**Copilot support is not released.** No Copilot credential was available:
the operator decided on 2026-10-17 to deliver offline only. No live Copilot
invocation, live probe, or live skill smoke ran, and none is recorded as
passed. Specification #281 stays open until the live evidence below exists
and the operator decides RB-8.

| Item | Value |
| --- | --- |
| Pinned CLI | `@github/copilot@1.0.98` (`GitHub Copilot CLI 1.0.98.`), npm integrity in the spike record |
| Pinned worker image | `factory.yaml` still pins `sha256:9eed486a…`, which was built before Copilot support and has no `copilot` executable. A repository that selects Copilot needs a rebuilt image first. |
| Skill smoke | `worker/skill-smoke.json` has no `copilot` record. No live smoke ran, so none was written. |
| Deterministic suite | `make check` passes without network access and without a Copilot credential. |

Consequence for operators: until an operator records a Copilot smoke for a
pinned image that contains the CLI, `factory doctor` blocks every repository
that can select Copilot with `harness.skills.evidence_stale` (or
`harness.skills.evidence_missing` when the evidence file is absent). Its
action names the call: `HARNESSES=copilot COPILOT_TOKEN_PATH=<dedicated
Copilot token file> scripts/smoke-skills.sh`. Codex and Claude Code
repositories are not affected.

## Open live release blockers

Each blocker needs live evidence from a disposable target with a dedicated
token, or an operator decision. A skipped probe is not a pass.

| ID | What is not proved | Live evidence that closes it |
| --- | --- | --- |
| RB-1 | The JSONL event names of a `-p --output-format json` run: identity, final, and failure events. The adapter reads identity provisionally from a top-level `sessionId` or `session_id` field, so a real launch can fail closed at identity discovery. | `COPILOT_PROBE_LIVE=1` probe `fresh`, then `copilotSessionEvidenceFields` set to the observed field. |
| RB-2 | Exact resume of a real session after process restart and worker recreation; full-UUID priority over names, summaries, and prefixes; a corrupt `events.jsonl`. | Live probe `resume` and the live `copilot-mixed-harness-recovery` scenario. |
| RB-3 | That the selected model and effort run, and that a managed `fallback` model cannot replace them silently. | Live probes `model-unknown` and `effort-low`, plus machine evidence of the effective model. |
| RB-4 | That a fine-grained token with only the Copilot Requests permission runs prompts and cannot write a repository or tracker; expiry and refresh behavior. | A non-mutating write check with the token against a disposable repository, and an expired-token run. |
| RB-5 | The output of an expired token, a rate limit or capacity limit, a disabled CLI policy, and an unavailable model. Capacity now settles as an unexpected exit, not a capacity wait. | Observed stderr and JSONL for each case, then a typed mapping in `classifyCopilotOutput`. |
| RB-6 | `--allow-all-tools` under an enterprise policy that disables bypass permissions. | One run in an organization with that policy. |
| RB-7 | With a credential: no built-in GitHub MCP tools, no repository hook, MCP server, plugin, or instruction file, no session export, no update download, and activation of the curated skills. | Live probes and `HARNESSES=copilot scripts/smoke-skills.sh` on the pinned image. |
| RB-8 | Built-in product telemetry. The CLI has no opt-out except `COPILOT_OFFLINE=true`, which needs BYOK (out of scope), and it writes a device ID. | **Open operator decision**: accept this boundary or keep Copilot disabled. This record does not decide it. |

The live smoke, the live invocation, and the continuation after a process and
worker restart (#287) are themselves release blockers. They depend on RB-1,
RB-2, and RB-7.

## Traceability of #281

Each named test exists; `internal/acceptance:TestCopilotReleaseEvidenceNamesExistingTests`
fails when one is renamed or removed. "Met offline" means the deterministic
checks pass in `make check`. It does not mean that the live behavior is
proved.

### 1. Pinned CLI compatibility record

Offline status: met for the documented and observed behavior. Live: blocked
by RB-1 to RB-7.

- `.:TestCopilotFixturesMatchManifest`, `.:TestCopilotFixturesAreNamedByManifest`,
  `.:TestCopilotPinIsConsistent`, `.:TestCopilotCommandTemplatesUseObservedFlags`,
  `.:TestCopilotFixturesAreSanitized`
- `.:TestCopilotLiveEvidenceStaysUnresolved` keeps every live probe recorded
  as `not-run` in the fixture manifest.

### 2. Every configured role through the headless seam, without TTY, editor, or host agent

Offline status: met. Live: blocked by RB-1 and RB-6.

- `internal/harness:TestCopilotHeadlessLaunchesThroughTheWorkerLauncherWithTheAssignedSession`
- `internal/harness:TestHeadlessAdaptersShareOneLifecycleContract`
- `internal/factory:TestCopilotAndMixedHarnessWorkflowsReachReadiness`
- `internal/factory:TestSessionRolesLaunchAcceptAndResumeThroughBothHarnesses`
  (ideation and planning on Codex, Claude Code, and Copilot)

### 3. Copilot-only and mixed selection with frozen policy

Offline status: met. Live: blocked by RB-3.

- `internal/config:TestValidateRepositoryAcceptsCopilotRoleSelection`
- `internal/config:TestValidateRepositoryRejectsUnsupportedCopilotSelections`
- `internal/factory:TestResolveAgentPolicySelectsCopilotForAMixedRepository`
- `internal/factory:TestResolveAgentPolicyKeepsALegacyHarnessOverrideAwayFromCopilot`
- `internal/factory:TestHandleCommandSelectsCopilotOnlyAfterAnExplicitOptIn`
- `internal/factory:TestStartAgentRefusesCopilotSelectionsBeforeAnySideEffect`
- `internal/harness:TestCopilotHeadlessRefusesSelectionsThePinnedCLICannotHonor`
- `internal/harness:TestStartupChecksLeaveCopilotOutOfCodexAndClaudeConfigurations`

### 4. Credential isolation, refresh, and redaction

Offline status: met. Live: blocked by RB-4 and RB-5.

- `worker:TestCopilotLauncherExportsOnlyTheProjectedTokenToCopilot`
- `worker:TestCopilotLauncherRefusesAMissingOrEmptyCredential`
- `worker:TestCopilotLauncherRefusesAnUnreadableCredential`
- `worker:TestHostCopilotStateStaysOutOfWorkerBuilds`
- `internal/worker:TestDockerRuntimeSeedsOnlyTheCopilotTokenIntoTheProjection`
- `internal/worker:TestDockerRuntimeRefusesAnUnsafeCopilotCredentialSource`
- `internal/worker:TestDockerRuntimeRejectsTheHostCopilotHomeAsACache`
- `internal/gateproof:TestProjectLeavesOutAnUntrackedCopilotToken`
- `internal/config:TestValidateHostRequiresAnAbsoluteCopilotAuthSource`
- `internal/cli:TestRunRegisterStoresACopilotTokenSourceAloneOrOnUpdate`
- `internal/factory:TestCopilotCredentialSeedingProjectsTheRegisteredSource`
- `internal/factory:TestRefreshAuthServesCopilotAndKeepsTheRecordedHarness`
- `internal/factory:TestRefreshAuthExplainsHowToRegisterAMissingCopilotSource`
- `internal/harness:TestStartupChecksDiagnoseTheCopilotCredentialSource`

### 5. Restart, recreation, repair, capacity, auth repair, cancellation, and replay keep identity and budgets

Offline status: met, through the deterministic tests of the
`copilot-mixed-harness-recovery` scenario. Live: blocked by RB-2 and RB-5.
Capacity has no observed Copilot shape, so it settles as an unexpected exit
with the bounded resume allowance and never as an endless retry.

- `internal/factory:TestHeadlessStartupRecreatesWorkerAndResumesOnce`
- `internal/factory:TestHeadlessRecoveryBoundsAnUnexpectedCopilotExit`
- `internal/factory:TestHeadlessRecoveryResumesTheSameCopilotSessionAfterAuthRefresh`
- `internal/factory:TestMixedHarnessReviewRoundRepairsInTheRecordedImplementationHarness`
- `internal/factory:TestCheckRepairResumesTheRecordedHarnessAfterAConfigurationChange`
- `internal/factory:TestImplementationObjectionRevisesInTheRecordedTestHarness`
- `internal/factory:TestSessionRolesNeverMigrateANativeSessionToAChangedHarness`
- `internal/factory:TestCancelledCopilotRunStopsItsProcessAndRefusesALateReport`
- `internal/effect:TestAutomaticHarnessResumeDoesNotRepeatAfterResponseLoss`
- `internal/effect:TestResultAcceptanceReplayDoesNotRepeatFinalization`
- `internal/factory:TestLaunchPausesAHarnessMissingFromTheWorkerImageForAnUpgrade`
  and `internal/factory:TestLivenessPausesAHarnessMissingFromTheWorkerImageForAnUpgrade`:
  Copilot selected on an older worker image pauses with the upgrade action and
  does not spend the automatic resume.

### 6. Forged, stale, duplicate, missing, or invalid reports cannot advance

Offline status: met. Report acceptance belongs to the coordinator and does
not depend on the harness. Live: the live scenario confirms it once RB-1 lets
a real launch reach a report.

- `internal/factory:TestCopilotReportAcceptanceAdmitsOnlyTheExactInvocationReport`
- `internal/harness:TestCopilotHeadlessNeverTreatsCLISuccessAsCompletion`
- `internal/harness:TestCopilotHeadlessFailsClosedOnUnusableIdentityEvidence`
- `internal/harness:TestCopilotHeadlessRefusesAFreshStartThatIgnoresTheAssignedSession`

### 7. No bypass through executable configuration, built-in GitHub tools, or cloud and session export

Offline status: the configuration is tested offline: the launch options, the
managed settings, and the launcher. These tests prove what the factory passes
to the CLI, not that the CLI honors it. Live proof that a launch loads no
executable configuration or GitHub tools and exports nothing is release
blocker RB-7 (and RB-6 for enterprise policy). RB-8 is an open operator
decision.

- `.:TestCopilotLiveProbesRunThroughTheProductionLauncher` keeps the RB-7
  live probes on the production launcher and its managed settings.
- `worker:TestCopilotManagedSettingsSuppressUnapprovedBehavior`
- `worker:TestCopilotLauncherRestoresTheManagedConfigurationBeforeEveryLaunch`
- `worker:TestBaseImageInstallsTheCopilotLauncherAndManagedSettings`
- `worker:TestBaseImagePinsTheProvedCopilotRelease`
- `internal/worker:TestDockerRuntimeChecksTheCopilotSkillContractInItsOwnRoot`

Residual passive instruction discovery: the pinned CLI's `instruction list`
finds `AGENTS.md`, `CLAUDE.md`, `.github/copilot-instructions.md`, and
`.github/instructions/*.instructions.md` in the worktree whatever the
settings say. Every launch passes `--no-custom-instructions`, which the docs
say turns off these files. Whether a `-p` run honors that is RB-7. Until it is
proved, a repository's instruction files can reach the Copilot model as
passive guidance. They cannot run code, and they do not change the frozen
role prompt, the role permissions, or report acceptance.

### 8. Deterministic coverage without network or credentials

Offline status: met. Every test above runs in `make check` with stub
executables, a fake Docker, and fixtures. No test reads a host credential.

- `worker:TestSkillSmokeRecordsCopilotThroughTheLauncher`
- `internal/harness:TestCopilotHeadlessMapsObservedFailuresToTypedOutcomes`

### 9. Live smoke evidence for the exact CLI and image, skills, report, and resume

Offline status: the tooling is met. Live: **not run; release blocker**. It
depends on RB-1, RB-2, and RB-7. A missing credential, a skipped harness, a
failed call, and a reply without the skill text record nothing and exit
non-zero:

- `worker:TestSkillSmokeNeverRecordsCopilotWithoutADedicatedCredential`
- `worker:TestSkillSmokeNeverRecordsAFailedCopilotCall`
- `worker:TestSkillSmokeReportsHarnessFailureSeparately`
- `internal/harness:TestStartupChecksBlockCopilotWithoutSmokeEvidence`

### 10. Operator instructions and canonical documents, `make check`

Offline status: met by #287. The [operator
guide](../configuration.md#github-copilot-cli) and the canonical documents
in the README table describe Copilot. `make check` passes.

## Traceability of #287

| Criterion | Status |
| --- | --- |
| Smoke tooling selects Copilot, checks skill content, keys by digest and CLI version | Met offline (criterion 9 tests). No record exists, because no live run happened. |
| Disposable live invocation with a valid report and a continuation after restart | **Blocked**: not run (RB-1, RB-2). |
| Missing credentials, disabled access, unavailable models, and skipped probes never pass; live tests stay out of `make check` | Met offline. The smoke and the live probes run only on an explicit operator call. |
| Production launch suppresses executable configuration and GitHub tools and keeps curated skills; passive discovery recorded | Configuration tested offline (criterion 7). That the CLI honors it is not proved: **blocked** by RB-7. The passive limitation is recorded above. |
| Operator documentation | Met: [operator guide](../configuration.md#github-copilot-cli). |
| Model, harness, and subscription distinction; out-of-scope items | Met: same guide. |
| README and canonical documents; changed decisions | Met: README, configuration, installation, agent runtime, worker runtime, worker skills, repository initialization, guide topics, [ADR 0028](../adr/0028-copilot-cli-joins-the-headless-seam-behind-live-evidence.md). |
| Upgrade steps, existing-run compatibility, failure actions without secrets | Met: same guide. |
| Traceability, `make check`, unperformed live validation stays a blocker | Met: this record. |

## Live procedure

Run these steps with an operator-approved disposable repository and a
dedicated token, then record each result in [the results
file](copilot-results.json) and write its report with `factory acceptance
report` into `docs/acceptance/copilot/`. No report is kept yet, because no
live result exists. Review raw output by hand before recording; never commit
a token, a transcript, or a host path.

1. Rebuild the worker image with `scripts/build-worker.sh`, record the new
   digest in `factory.yaml`, and run `factory doctor`.
2. Run the live probes on the pinned worker image. They launch Copilot only
   through `factory-copilot`, so they observe the managed settings and the
   curated skills of a real launch:
   `COPILOT_PROBE_LIVE=1 COPILOT_PROBE_TOKEN_FILE=<token file> COPILOT_PROBE_WORKER_IMAGE=<image@digest> scripts/probe-copilot-cli.sh <output directory>`.
   Update the spike record for RB-1 to RB-7.
3. Run the skill smoke for the pinned image:
   `HARNESSES=copilot COPILOT_TOKEN_PATH=<token file> ./scripts/smoke-skills.sh`.
4. Run the live `copilot-mixed-harness-recovery` scenario
   (`factory guide acceptance`).
5. Record the RB-8 decision.
