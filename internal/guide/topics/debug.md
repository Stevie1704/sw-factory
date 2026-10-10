# Debugging

Use this procedure when a `factory` command fails, a run stops progressing,
or a result looks wrong. Inspection commands change nothing. Recovery
commands change state, so ask the operator before you run one.

## Procedure

1. **Collect identity and state.** Record the output of these commands. They
   are read-only.

   ```sh
   factory version --json
   factory status
   ```

   `factory status` shows the supervisor heartbeat and the active run, or the
   latest terminal run. For a run it shows the stage, the status, any pending
   effect, any discrepancy, and the safe operator actions.

2. **Run the diagnosis.**

   ```sh
   factory doctor
   ```

   It runs every startup check, also after a failure. Each check that does not
   pass prints `problem:` and `action:` lines.

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

7. **Verify the result.** Run `factory status` and `factory doctor` again.
   Confirm the expected change. When it did not occur, do not repeat the
   action without new evidence. Go back to step 3.

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
| `factory resume --run-id <id>` | The run paused for harness capacity, authentication, or a native-session interruption | Continues the exact persisted native session. Does not use the retry budget. |
| `factory reconcile` | A pending effect or discrepancy is reported after a restart | Runs one restart reconciliation. Pauses the run when it cannot prove a safe repair. |
| `factory reconcile --abandon-effect <effect-id> --reason <text>` | A human inspected the ambiguous pending effect that `factory status` names | Discards that effect. The run stays paused. |
| `factory poll` | Authorized `/factory` comments wait on the issue or pull request | Processes them one time. |
| `factory cleanup` then `factory cleanup --confirm` | The runs are terminal | Prints the plan, then removes local artifacts of terminal runs older than seven days. |
| `factory reset --config <path>` then add `--confirm` | The coordinator is stopped; the operator wants to remove the installation | Prints the plan, then removes every local resource of the installation. |

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
