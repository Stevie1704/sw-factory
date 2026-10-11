# Debugging

Use this procedure when a `factory` command fails, a run stops progressing,
or a result looks wrong. Inspection commands change nothing. Recovery
commands change state, so ask the operator before you run one.

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

6. **Use one documented recovery action.** Select it from the recovery table.
   Confirm that its precondition is true. Ask the operator. Run it one time.

7. **Verify the result.** Run `factory status --json` and
   `factory doctor --json` again.
   Confirm the expected change. When it did not occur, do not repeat the
   action without new evidence. Go back to step 3.

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
- `recovery` has `sources_agree`, `pending_effect`, `discrepancies`, and
  `safe_actions`. Use only an action from `safe_actions` or from the recovery
  table in this topic.
- `blockers` lists the known blockers, each with `code`, `likely_origin`,
  `problem`, `facts`, and an optional `action`.

`none` means that the value does not exist. `unknown` and `unavailable` mean
that Factory could not read the value. Do not treat them as the same.

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
| status `recovery.sources_disagree` | Unknown cause. Read `recovery.discrepancies` and `recovery.safe_actions`. |

### Observation and mutation

`factory doctor` and `factory status` only read state. They do not claim an
issue, resume a run, repair state, refresh a credential, or create an issue.
An `action` describes what a person can do next. It does not occur until
someone runs it. Before you run an action with the effect `local_mutation` or
`external_mutation`, confirm its preconditions and ask the operator.

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
| `factory auth refresh --resume` | As above | Also reconciles pending effects and continues the paused native session one time. |
| `factory resume` | The run is not terminal and `factory status` names `factory resume` as a safe action | Reconciles a pending effect first. Then it does one of the actions in the next table. It does not use the retry budget. |
| `factory reconcile` | A pending effect or discrepancy is reported after a restart | Runs one restart reconciliation. Pauses the run when it cannot prove a safe repair. |
| `factory reconcile --abandon-effect <effect-id> --reason <text>` | A human inspected the ambiguous pending effect that `factory status` names | Discards that effect. The run stays paused. |
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
