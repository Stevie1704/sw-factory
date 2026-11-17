# Upgrade a Factory installation

Use this procedure to move one registered installation to a new Factory
release. The new `factory` binary is the target. Run every command in this
procedure with the target binary, unless a step names the old binary.

## First-version policy

- Upgrade only a quiescent installation. Finish every active run, or cancel
  it deliberately, with the old binary before you start. The upgrade never
  moves a claimed run to a new worker or policy, and never resumes an old
  harness session with a new image.
- The coordinator runs in the foreground. A supervised service is not
  supported yet. Stop the coordinator with Ctrl-C in its terminal, or with
  `factory stop --config <path>` and the binary that started it.
- There is no automatic update, no live-run migration, and no restore
  command.

## 1. Plan

Download the target release and verify its checksum as the installation
instructions in the release archive describe. Do not install it on `PATH`
yet. Run:

```sh
<target factory> upgrade plan --config <host config> --onboarding-plan <plan file> --json
```

`--onboarding-plan` names the onboarding plan of the repository, for example
`$HOME/.cache/sw-factory/onboarding-<repository name>.json`. Omit it when no
plan exists; the plan then contains an operation that creates one.

The command only reads. It does not migrate the store, replace a binary,
pull an image, resume work, or make a paid call. It exits 0 for a `ready` or
`complete` plan and 1 for a `blocked` plan. The document has
`schema_version: 1` and these fields:

| Field | Content |
| --- | --- |
| `target` | Version, build, revision, and worker base image of the target |
| `result` | `blocked`, `ready`, or `complete` |
| `components` | `binary`, `worker`, `policy`, `smoke`, and `store`, each `old`, `new`, `incomplete`, `unsupported`, or `unknown` |
| `blockers` | `code`, `detail`, and `action` of each condition that stops every operation |
| `quiescence` | Each condition of a quiescent installation and whether it holds |
| `operations` | The ordered steps with `state` (`done`, `pending`, `blocked`), `command`, `effect`, `paid`, and `expected_effects` |
| `verification` | The checks of the final state and whether each holds |
| `recovery_limits` | What the upgrade cannot undo |
| `store_backups` | The existing pre-migration backups, as provenance |

Show the plan to the operator. Apply `factory guide authorization` before
each operation whose `effect` is `local_mutation` or `external_mutation`, and
before each operation with `paid: true`.

## 2. Resolve the blockers

Do the `action` of each blocker, then plan again. Never edit the operational
store, a lock file, or the onboarding plan to remove a blocker.

| Code | Meaning |
| --- | --- |
| `run.active`, `invocation.active`, `effect.pending` | Active work exists. Finish or cancel it with the binary that reads the store schema. |
| `coordinator.running` | A coordinator holds the repository lock. Stop it. |
| `store.schema_newer` | A newer release migrated the store. Use that release or a newer one as the target. |
| `store.schema_unsupported`, `store.unreadable`, `store.missing` | The target cannot prove the store state. Keep the current binary and follow `factory guide debug` or `factory guide reporting`. |
| `store.interrupted_write` | A write stopped before its commit. The store migration operation rolls it back first. |
| `worker.base_image_missing` | Pull the named target base image. The plan never pulls. |
| `configuration.invalid` | Repair `factory.yaml` for the target release. |
| `registration.missing`, `onboarding.plan_unreadable` | Name the correct host configuration or onboarding plan. |

## 3. Do the operations in order

Do only the first operation with `state: "pending"`. Then run the plan again.
A `done` operation has valid evidence; do not do it again.

1. `quiesce`: no active work, and no coordinator runs.
2. `install_binary`: install the target archive in place of the old one, so
   that `command -v factory` names the target binary.
3. `prepare_resources`: export the target release resources to a new
   directory.
4. `onboarding_plan`: create or refresh the onboarding plan with the target
   binary.
5. `rebuild_worker`: rebuild the worker image from the target worker base
   image and record its digest in `factory.yaml`.
6. `gate_proof`: prove setup and the gates in the new worker.
7. `skill_smoke`: record the skill smoke for the new worker digest. It makes
   paid calls. The command repeats only the missing skills.
8. `migrate_store`: run `factory upgrade migrate-store --config <path>
   --json`. This is the only supported store migration entry point.
9. `doctor`: record the startup diagnosis of the target.
10. `start_coordinator`: start `factory start --config <path>` in the
    foreground and keep its terminal open.

`migrate_store` holds the repository lock, rolls back an interrupted write,
and refuses active work, a running coordinator, and an unsupported schema.
Then it copies the store to a private `<store>.bak-<timestamp>` file and
migrates the schema in one transaction. Its document has `outcome`
(`migrated`, `current`, `refused`, or `error`), `migration` with
`from_version`, `to_version`, and `backup`, and `refusals`. A command that
only inspects, such as `factory doctor` or `factory status`, never migrates.

## 4. Continue after an interruption

Run the plan again. `components` tells which parts are `old`, `new`, or
`incomplete`, and the first `pending` operation is the supported
continuation. Valid evidence is reused only when its identity matches: the
smoke records of the new worker digest stay valid, so the paid smoke is not
repeated. Gate proof and doctor evidence of the old release are stale and
run again.

After `migrate_store`, the old binary refuses the store because its schema is
newer. Running the old binary is not a rollback. Continue forward with the
target binary.

## 5. Verify the result

The upgrade is complete only when `result` is `complete` and every
`verification` entry has `satisfied: true`: the target binary on `PATH`, the
pinned worker release, a valid configuration, complete smoke evidence, a
current store, a recorded doctor of the target, and a running coordinator.
`factory status --config <path> --json` shows the supervisor.

## Recovery limits

- Factory has no supported restore. A `<store>.bak-*` file is migration
  provenance, not a restore point. Never copy it over the store. Decision
  record ADR 0032 in the source repository sets the future checkpoint and
  restore boundary.
- A database copy does not rewind branches, pull requests, comments, labels,
  workers, or harness sessions. An older store can repeat those effects.
- Never downgrade the binary to escape a blocker. When compatibility or
  consistency with external state cannot be established, stop and give the
  operator the plan document.
