# Debugging

Use this procedure when a `factory` command fails, a run stops progressing,
or a result looks wrong. Read and apply `factory guide authorization` before
acting; it owns the operator-agent contract for inspection and recovery.

## Procedure

1. **Collect identity and state.** Record the output of these commands. They
   are read-only.

   ```sh
   factory version --json
   factory status --json
   ```

   `factory status` shows the supervisor heartbeat and the active run, or the
   latest terminal run. For a run it shows the stage, the status, any pending
   effect, any discrepancy, and the safe operator actions.

2. **Run the diagnosis.**

   ```sh
   factory doctor --json
   ```

   It runs every startup check, also after a failure. Without `--json`, each
   check that does not pass prints `problem:` and `action:` lines. The next
   section describes the JSON documents.

3. **Identify the failing subsystem.** Use the first source that names it:
   a failed `doctor` check, a `status` discrepancy or pending effect, or the
   error line of the failed command. Subsystems are host configuration,
   repository configuration, GitHub, Docker and the worker image, harness,
   harness authentication, worker skills, and the operational store. At run
   level, they are the stage and the status of the run.

4. **Inspect only the relevant evidence.** Read the output of the failing
   check or command, the issue status comment, and the gate or review result
   of the named checkpoint. `factory start --verbose` writes a progress log of
   the coordinator to standard error. Do not read credential files. Do not
   collect unrelated logs or files.

5. **Classify the cause.** Use the table in the next section. When the
   evidence does not show the cause, record the cause as unknown.

6. **Use one documented recovery action.** Select it from
   `recovery.actions` or from the recovery table. Confirm that each entry of
   its `requires` list is true. Apply `factory guide authorization` to the
   exact command, effects, and human input. Run it one time with `--json`.

7. **Verify the result.** Run the `verify` command of the action, normally
   `factory status --json`, and `factory doctor --json` again.
   Confirm the `postcondition` of the action. When it did not occur, do not
   repeat the action without new evidence. Go back to step 3.

## Read the JSON documents

`factory doctor --json` and `factory status --json` write one JSON object to
standard output and nothing else. The text output and the JSON document show
the same result. Use the JSON document. Do not parse the text output.

### Contract

- `schema_version` is `1`. Factory can add fields in the same schema
  version. Ignore fields that you do not know. Factory increments the schema
  version when it removes or renames a field, or changes the meaning of a
  field.
- `command` is `doctor` or `status`. `outcome` is `ready`, `blocked`, `ok`,
  or `error`.
- A `code` is a stable identifier with the form
  `<subsystem>.<check>.<condition>`. A code does not change when the wording
  of a problem changes. Match codes, not text.
- `likely_origin` is `repository`, `environment`, `factory`, or `unknown`. It
  is a classification from the evidence of the check. It is not proof. A
  failed gate or a failed command alone is not a Factory defect.
- `factory doctor --json` shows a configuration problem as a diagnostic.
  `factory status --json` cannot continue without the configuration and the
  operational store. When it cannot read them, the document shows
  `outcome: "error"`. The `error` object has `code`, `likely_origin`, and
  `message`.
- An invalid flag or argument is the only exception. Factory writes it to
  standard error as text and exits with status 2.

| Command | Exit 0 | Exit 1 | Exit 2 |
| --- | --- | --- | --- |
| `factory doctor --json` | `outcome` is `ready` | `outcome` is `blocked` or `error` | invalid argument |
| `factory status --json` | `outcome` is `ok`, also for a paused or failed run | `outcome` is `error` | invalid argument |
| `factory config validate --file <path> --json` | `valid` is true | `valid` is false | invalid argument |
| `factory onboard verify --repository <path> --json` | `result` is `passed` | any other `result` | invalid argument |
| `factory onboard plan` or `onboard status --plan <file> --json` | the status document is written, whatever its `result` | the plan cannot be read or written | invalid argument |
| `factory onboard record --plan <file> <step> --json` | the recorded step passed, or the live proof was started, declined, or abandoned | the step did not pass, or nothing was recorded | invalid argument |

### Doctor fields

Each entry of `diagnostics` has `code`, `subsystem`, `check`, `status`
(`passed`, `warning`, `failed`, or `skipped`), `blocks_startup`, `observed`,
`problem`, `detail`, `facts`, `likely_origin`, and `action`. The `action`
object has `summary`, `effect` (`inspection`, `local_mutation`, or
`external_mutation`), `preconditions`, and `guide_topic`.

`ready` is true when no check blocks startup. `complete` is false when a
check did not observe its subject: the check is `skipped`, or its `observed`
field is false. An unobserved check is not healthy. Repair the check that
blocks the observation first, then run the diagnosis again.

### Status fields

- `registration.state` is `registered`, `none`, or `unknown`, with
  `provider` (`github` or `azure_devops`) and `repository`.
- `supervisor.state` is `live`, `stale` (the heartbeat expired or the lock
  is not held), `missing` (no heartbeat is recorded), `not_configured`, or
  `unknown`.
- `run.state` is `active`, `terminal`, `none`, or `unavailable`. An active or
  terminal run has `id`, `stage`, `status`, `activity`, `checkpoint_sha`,
  `active_invocation_ids`, `lifecycle_reason`, and `failed_gates`.
- `recovery` has `sources_agree`, `pending_effect`, `discrepancies`,
  `safe_actions`, and `actions`. `safe_actions` is the schema-1 text list.
  `actions` is the typed list. Use only an action from `actions` or from the recovery table in
  this topic.
- `blockers` lists the known blockers, each with `code`, `likely_origin`,
  `problem`, `facts`, and an optional `action`.

`none` means that the value does not exist. `unknown` and `unavailable` mean
that Factory could not read the value. Do not treat them as the same.

### Action fields

Each entry of `recovery.actions`, and each `action` of a blocker or a
diagnostic, has the schema-1 fields `summary`, `effect`, `preconditions`,
and `guide_topic`. The typed fields are:

- `id`: a stable action identity, for example `run.retry_checks`,
  `pending_effect.abandon`, `status.inspect`, or `supervisor.start`.
- `kind`: `executable`, `inspection`, or `human_decision`.
- `authorization`: `none`, `operator`, or `human_review`.
- `command` and `arguments`: the base command and its typed flags. An
  argument with `human_input: true` has no value. A human supplies it.
- `argv`: the complete argument vector. It is present only when no argument
  needs human input. Run it as an argument array. Do not give it to a shell.
- `target`: the `config_path`, `run_id`, `revision`, `checkpoint_sha`,
  `invocation_id`, or `effect_id` that the action applies to. Each action
  that has a command also has the argument `--config` with the
  configuration that the inspection read. Its `argv` and `verify` contain
  it too. Do not remove it.
- `requires`: the structured preconditions, each with `code` and
  `description`. `postcondition` is the expected state after the action.
  `verify` is the inspection command that shows the postcondition.

A doctor action has `id` `<code>.resolve`, the `requires` list of the
check, and the postcondition `<code>.resolved`. Its `verify` is
`factory doctor --json`. Its summary is a description, not a command, so a
doctor repair is a `human_decision` that the operator authorizes, or an
`inspection`.

A recommendation is not an authorization. An `executable` action with
`authorization: operator` follows `factory guide authorization`. A
`human_decision` action never has an `argv`; its `human_input` must come from
the operator's explicit decision under that contract.

Factory checks the target and the preconditions again when the command
runs. For example, `run.retry_checks` gives `factory resume` the flags
`--expect-revision` and `--expect-checkpoint`. When the run changed after
the recommendation, resume refuses and changes nothing. Resume also probes
the worker runtime again; when it is not available, resume refuses with
`check_infrastructure.unavailable` and changes nothing. A run-scoped command
with `--run-id` for another run refuses before it reconciles anything.
`factory auth refresh --expect-invocation <id>` refuses when the run now
recovers another invocation. Run `factory status --json` again to get a
current action.

For effect abandonment, the effect ID is the identity guard. Factory derives
it from the run, effect kind, and semantic operation identity. It checks the
current reservation and deletes only that exact run/effect pair; a different
effect is never discarded. `--run-id` optionally adds a run-selection guard.
Unlike resume, abandonment may clear a human-reviewed effect on a terminal
run because it does not restart or advance the workflow.

#### Example: a check paused for check infrastructure

1. `factory status --json` shows `run.stage: "check"`,
   `run.status: "waiting_for_human"`, and the action `run.retry_checks`
   with `requires` code `check_infrastructure.available`.
2. `factory doctor --json` shows the blocking diagnostic, for example
   `worker.docker.unreachable`. Its action is a `human_decision`. The
   operator repairs the runtime, for example starts Docker.
3. `factory doctor --json` is `ready`.
4. Within explicit session authority for this retry, run the `argv` of `run.retry_checks`
   with `--json` added. The outcome is `completed`, `result.continuation` is
   `checks`, and `identities.checkpoint_sha` is the target checkpoint.
5. `factory status --json` shows the postcondition `run.check_active`: the
   run is active in the check stage at the same `checkpoint_sha`. The
   check-repair attempts do not change, and no agent starts.

### Results of mutation commands

These commands accept `--json`: `resume`, `reconcile`, `auth refresh`,
`stop`, `init`, `register`, `bootstrap-labels`, `issue`, `agent`,
`agent-report`, `draft-pr`, `poll`, `cleanup`, and `reset`. With `--json`,
the command writes one JSON object to standard output and nothing else. The
exit status is the same as without `--json`.

- `schema_version` is `1`, with the same compatibility rules as above.
  `command` is the command name; `auth refresh` is `auth-refresh`.
- `outcome` is one of these values:

  | `outcome` | `attempted` | Meaning |
  | --- | --- | --- |
  | `completed` | true | The command did its action. |
  | `pending` | true | An external call did not answer. Its outcome is unknown. Run `factory status --json` before you run the command again. For `reconcile`, the effect stays reserved, and the next reconciliation replays it. |
  | `blocked` | true | The command stopped at a known boundary, for example a reconciliation pause. `recovery` shows the diagnosis. |
  | `refused` | false | The command changed nothing, because a target or a precondition was not true. |
  | `error` | true | An operational failure. The `error` object has `code`, `likely_origin`, and a fixed `message`. Standard error has the detail. A mutation can have occurred, so verify before you continue. |

- `reason.code` is a stable code for `pending`, `blocked`, and `refused`:
  `run.none`, `run.mismatch`, `run.terminal`, `run.revision_changed`,
  `run.checkpoint_changed`, `pending_effect.changed`, `invocation.changed`,
  `check_infrastructure.unavailable`,
  `recovery.waiting_for_human`, `recovery.workflow_failure`,
  `external.outcome_unknown`, `cleanup.confirmation_required`,
  `cleanup.plan_changed`, `cleanup.blocked`, `reset.confirmation_required`,
  `reset.blocked`, or `reset.incomplete`.
- `identities` names the resulting state: `run_id`, `revision`,
  `issue_number`, `checkpoint_sha`, `invocation_id`, `effect_id`, and
  `pull_request_number`, when they apply.
- `result` has the command-specific fields. `blockers` lists each blocker
  of a refused `cleanup` or `reset`. `verification` is the inspection action
  that confirms the outcome.

`cleanup` and `reset` without `--confirm` write the plan in `result`, with
`outcome: "refused"` and `reason.code` `cleanup.confirmation_required` or
`reset.confirmation_required`. They exit with status 2, as without `--json`.

An abandonment whose run or pending effect has disappeared is also
`refused`, with `attempted: false` and `reason.code` `run.none` or
`pending_effect.changed`. These already-absent targets keep the existing
text output and exit status 0. A different pending effect still refuses with
exit status 1 and remains reserved. Match `outcome` and `reason.code` to
determine whether abandonment occurred; exit status alone is insufficient.
Refused abandonment omits `result`; its `verification` action inspects the
selected configuration even when the run no longer exists.
Ordinary `reconcile` without `--abandon-effect` still completes when no run
exists.

#### Command-specific results

`completed` describes completion of the finite command. Use the following
fields to determine which workflow action occurred, then run `verification`.
It does not mean a launched invocation finished or a supervisor exited.

| Command | Fields to inspect in a completed result |
| --- | --- |
| `resume` | `result.continuation` is `checks` or `agent`; `result.stage`, `result.status`, and `identities` identify the continued checkpoint or invocation. |
| `reconcile` | `result.reconciliation` identifies reconciliation or a waiting state; `recovery` names any remaining discrepancy or effect. After abandonment, a non-terminal run stays paused and a terminal run stays terminal. |
| `auth refresh` | `result.harness` names the harness whose managed credentials were refreshed; `result.resumed` is true only when the native resume succeeded. |
| `stop` | `result.running` is true when a stop was requested for `result.pid`; false means no coordinator was running. Verify the supervisor has stopped. |
| `init` | `result.config_path` identifies the created host configuration. |
| `register` | `result.repository_path`, `result.operational_data_path`, `result.updated`, and `result.inferred` identify the registration and inferred values. |
| `bootstrap-labels` | `result.repository` and `result.labels` identify the bootstrapped label definitions. |
| `issue` | `identities.run_id`, `result.branch`, `result.worktree`, and `result.baseline_gates` identify the claim and baseline evaluation. |
| `agent` | `identities.invocation_id`, `result.role`, and `result.native_session_id` identify the launched invocation; the agent can still be running. |
| `agent-report` | `result.report_outcome` and `result.invocation_status` describe the accepted report; acceptance does not imply that its outcome was successful. |
| `draft-pr` | `identities.pull_request_number` and `result.pull_request_url` identify a PR. When `result.repair` is present, its `outcome` is `started`, `infrastructure_pause`, `waiting_for_human`, or `exhausted`; no PR was produced by this request. Its attempt, budget, and remaining fields retain retry accounting. |
| `poll` | `result.lifecycle` identifies a lifecycle transition; otherwise `result.commands` gives each command's outcome and any rejection code. An empty list means no comments were processed. |
| `cleanup` | `result.runs` is the plan, `result.skipped` names withheld runs, and `result.deleted` names the removed runs. Preview requires confirmation. |
| `reset` | `result.removed` and `result.remaining` identify applied and incomplete removals; `result.retained` names resources outside reset's scope. Preview requires confirmation. |

An invalid flag or argument writes text to standard error and exits with
status 2. It writes nothing to standard output.

`factory start`, `factory ui`, and the interactive launcher run until they
stop. They do not write a result document. Use `factory status --json` to
observe them. `factory start --verbose` writes its progress log to standard
error.

### Examples

| What you see | Meaning |
| --- | --- |
| doctor `worker.docker.missing` or `worker.docker.unreachable` | Environment failure. Install or start Docker on the host. |
| doctor `github.authentication.failed` | Environment failure. The operator runs `gh auth login`. |
| doctor `config.repository.invalid` | Repository failure. Repair `factory.yaml`. |
| doctor `*.adapter_missing` with origin `factory` | Suspected Factory defect. See `factory guide reporting`. |
| doctor `doctor.unclassified` | Unknown cause. Record the evidence. Do not call it a Factory defect. |
| status `run.gate.failed` | Repository failure. The run's repair loop or a human fixes it. |
| status `run.gate.error` | Environment failure. The worker runtime did not run the gate. |
| status `run.gate.setup_failed` | Unknown cause. The setup command or the runtime failed before the gate ran. Read the lifecycle reason. |
| status `run.supervisor.stale` or `run.supervisor.missing` | Environment failure. Run `factory start` after `factory doctor` is ready. |
| status `recovery.sources_disagree` | Unknown cause. Read `recovery.discrepancies` and `recovery.actions`. |

### Observation and mutation

`factory doctor` and `factory status` only read state. They do not claim an
issue, resume a run, repair state, refresh a credential, or create an issue.
An `action` describes what a person can do next. It does not occur until
someone runs it. Before you run an action with the effect `local_mutation` or
`external_mutation`, verify its current preconditions and target, then apply
`factory guide authorization`, including its rules for inspection-only
requests and ambiguous effects.

## Classify the cause

| Class | Typical evidence | Who fixes it |
| --- | --- | --- |
| Repository failure | A gate fails at a checkpoint; `factory.yaml` is invalid; the worker image lacks a tool; a review finds a defect | Repository maintainers, through an ordinary change or the run's repair loop |
| Environment or authentication failure | Docker is stopped; `gh` is not authenticated; a harness credential expired; the network or GitHub is unavailable; a run waits for harness capacity | The operator, on the host |
| Suspected Factory defect | A command crashes or panics; Factory breaks a rule this guide states; state disagrees with itself and no action in this guide applies | Factory maintainers; see `factory guide reporting` |
| Unknown | The evidence does not identify the cause | Nobody yet; report what you observed and that the cause is unknown |

A failed gate or a review finding is not a Factory defect. An expired
credential is not a Factory defect. Do not report a cause that you did not
observe.

## Supported recovery actions

| Command | Precondition | Effect |
| --- | --- | --- |
| `factory stop` | `factory start` runs | Stops the polling coordinator. Keeps all run state. |
| `factory start` | `factory doctor` is ready | Starts polling. Reconciles interrupted work before it continues. |
| `factory auth refresh` | A run waits for a refreshed harness credential | Copies the host credential into the factory credential store. Changes no workflow state. |
| `factory auth refresh --resume` | As above | Also reconciles pending effects and continues the paused native session one time. With `--expect-invocation`, it refuses when the run recovers another invocation. |
| `factory resume` | The run is not terminal and `factory status` names `factory resume` as a safe action | Reconciles a pending effect first. Then it does one of the actions in the next table. It does not use the retry budget. With `--expect-revision` and `--expect-checkpoint`, it refuses a run that changed. |
| `factory reconcile` | A pending effect or discrepancy is reported after a restart | Runs one restart reconciliation. Pauses the run when it cannot prove a safe repair. |
| `factory reconcile --abandon-effect <effect-id> --reason <text>` | A human inspected the ambiguous pending effect that `factory status` names | Discards that exact effect. A non-terminal run stays paused; a terminal run retains its outcome. |
| `factory poll` | Authorized `/factory` comments wait on the issue or pull request | Processes them one time. |
| `factory cleanup` then `factory cleanup --confirm` | The runs are terminal | Prints the plan, then removes local artifacts of terminal runs older than seven days. |
| `factory reset --config <path>` then add `--confirm` | The coordinator is stopped; the operator wants to remove the installation | Prints the plan, then removes every local resource of the installation. |

`factory resume` selects its action from the persisted run. Add
`--run-id <id>` to refuse a different active run.

| Persisted state | Action of `factory resume` |
| --- | --- |
| The active invocation has a native session | Continues that exact native session. |
| The active invocation has no native session | Supersedes that invocation and starts a new invocation of the same role. |
| No active invocation; the check stage paused for check infrastructure or restart reconciliation | Runs the gate checks again at the same checkpoint. |
| No active invocation, other cases | Starts a new invocation of the role that the run's stage selects. |

An authorized GitHub user can also comment one command on the issue:
`/factory status`, `/factory refresh`, `/factory resume`,
`/factory answer <question-id> <answer>`, `/factory repair <instruction>`,
`/factory retry`, `/factory cancel`, or `/factory config harness=<name>`.
These are human dispositions. An agent writes them only when the operator
tells it to.

`factory <command> -h` lists the flags of each command.

## Never do this to continue a run

- Edit the SQLite operational store or the host configuration to change run
  state.
- Bypass, skip, or weaken a checkpoint, a gate, or a review.
- Change the frozen specification, the frozen repository policy, or a role
  prompt of a claimed run.
- Edit, reset, or force-push a run branch or a run worktree.
- Add, remove, or change factory labels or the status comment by hand.
- Delete a worker container, a volume, or a credential store by hand.

When no action in this topic applies, stop. Report the evidence to the
operator.
