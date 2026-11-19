# Upgrade a Factory installation

Use this procedure to move one registered installation to a new Factory
release. The new `factory` binary is the target. Run every command in this
procedure with the target binary, unless a step names the old binary.

## First-version policy

- Upgrade only an installation that passes the migration gate: no process
  holds the coordinator lock, no run is non-terminal, no effect is pending,
  no invocation, session-role invocation, or session command is in
  progress, every ended session-role invocation has released its worker, and
  Docker proves that no harness process runs in a worker of this store. Docker
  must be reachable; an unknown worker state never passes. Finish every active run, or cancel it deliberately, with the old
  binary before you start. The upgrade never moves a claimed run to a new
  worker or policy, and never resumes an old harness session with a new
  image. The migration gate is narrower than the full quiescence that a
  maintenance checkpoint needs.
- The coordinator runs in the foreground. A supervised service is not
  supported yet. Stop the coordinator with Ctrl-C in its terminal, or with
  `factory stop --config <path>`. A coordinator, a store migration, a
  reset, a checkpoint, and a restore write one lock record with their PID, kind, and release, and the plan
  reads that record. A lock record without that identity is invalid.
- There is no automatic update and no live-run migration. A restore puts
  back only a maintenance checkpoint of the same release (see below).

## 1. Plan

Download the target release and verify its checksum as the installation
instructions in the release archive describe. Do not install it on `PATH`
yet. Run:

```sh
<target factory> upgrade plan --config <host config> --onboarding-plan <plan file> --resources <prepared directory> --json
```

`--onboarding-plan` names the onboarding plan of the repository, for example
`$HOME/.cache/sw-factory/onboarding-<repository name>.json`. Omit it when no
plan exists; the plan then contains an operation that creates one. The plan
must belong to the host configuration that `--config` selects; a plan of
another configuration is a blocker, and its evidence is not used.
`--resources` names the directory that `onboard prepare` of the target
filled; it defaults to the onboarding plan's directory. `--checkpoints`
names a directory whose direct subdirectories may be maintenance
checkpoints. The plan lists the newest one that verifies and matches this
registration and the schema of the live store: the checkpoint that the
installed release created before the migration. Only the release named in
its manifest restores it, so keep that release until the upgrade is done.
It searches only one level deep and keeps no registry, so a checkpoint
outside that directory is not listed.

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
| `migration_gate` | Each condition of the migration gate and whether it holds |
| `operations` | The ordered steps with `state` (`done`, `pending`, `blocked`), `command`, `effect`, `paid`, `paid_calls`, `authorization`, and `expected_effects` |
| `verification` | The checks of the final state and whether each holds |
| `recovery_limits` | What the upgrade cannot undo |
| `store_backups` | The existing pre-migration backups, as provenance; each has a `.json` provenance file beside it |
| `checkpoint` | The newest matching maintenance checkpoint with `path` and `manifest`, or `null` when `--checkpoints` is not given or no checkpoint matches |

`command` is always an executable shell command, or empty for a manual step
that `description` explains. Show the plan to the operator. Apply `factory
guide authorization` before each operation whose `effect` is
`local_mutation` or `external_mutation`, and before each operation with
`paid: true`; `paid_calls` lists each paid call, and `authorization` names
the contract to apply.

## 2. Resolve the blockers

Do the `action` of each blocker, then plan again. Never edit the operational
store, a lock file, or the onboarding plan to remove a blocker.

| Code | Meaning |
| --- | --- |
| `run.active`, `invocation.active`, `effect.pending`, `session_invocation.active`, `session_command.queued` | Active work fails the migration gate. Finish or cancel it with the binary that reads the store schema. |
| `session_invocation.unreleased` | An ended session role has not released its worker. Start the coordinator that owns the store until its reconcile pass releases it, then stop it. |
| `worker.active` | A harness process still runs in a worker of this store, with no active invocation. Let the owning coordinator finish or cancel it; do not stop the worker by hand while its process writes. |
| `docker.unreachable`, `worker.unobserved` | Docker cannot be reached, or the helper state of a running worker cannot be read. Repair the access to Docker, then plan again. |
| `coordinator.worker_mismatch` | The lock record names a coordinator of another release. Stop it. Start the target coordinator only at `start_coordinator`. |
| `coordinator.unobserved` | The lock or its record cannot be read, for example because the record is invalid. Do not edit the lock; follow `factory guide debug`. |
| `coordinator.running` | A coordinator of the target runs before the upgrade is done. Stop it. |
| `maintenance.running` | A store migration, a reset, or a checkpoint holds the repository lock. Wait until it ends. |
| `worker.release_unreadable` | The release label of the pinned worker image cannot be read. Check that this account can inspect the image. |
| `store.schema_newer` | A newer release migrated the store. Use that release or a newer one as the target. |
| `store.schema_unsupported`, `store.unreadable`, `store.missing` | The target cannot prove the store state. Keep the current binary and follow `factory guide debug` or `factory guide reporting`. |
| `store.interrupted_write` | A write stopped before its commit. The store migration operation rolls it back first. |
| `worker.base_image_missing` | Pull the named target base image. The plan never pulls. |
| `configuration.invalid` | Repair `factory.yaml` for the target release. |
| `registration.missing`, `onboarding.plan_unreadable`, `onboarding.plan_registration_mismatch` | Name the correct host configuration, or the onboarding plan of that configuration. |

## 3. Do the operations in order

Do only the first operation with `state: "pending"`. Then run the plan again.
A `done` operation has valid evidence; do not do it again.

1. `pass_migration_gate`: every condition of the migration gate holds.
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

Optional, before `migrate_store`: a checkpoint before the store migration.
Create it with the installed release before `install_binary`, once the
migration gate holds, as the Maintenance checkpoint section below describes.
The target binary refuses a checkpoint of an older store schema. The
checkpoint adds no operation and no blocker, so the plan never requires it.
Plan again with `--checkpoints <directory>` to list it. To restore it before
`migrate_store`, run `factory checkpoint restore` of the release that its
manifest names.

`migrate_store` holds the repository lock, rolls back an interrupted write,
and refuses work that fails the migration gate, a lock that another process
holds, and an unsupported schema.
Then it takes an online snapshot of the store to a private
`<store>.bak-<timestamp>` file, writes `<store>.bak-<timestamp>.json` beside
it with the source and target schema versions, the migrating release, and the
SHA-256, and migrates the schema in one transaction. Its document has
`outcome` (`migrated`, `current`, `refused`, or `error`), `migration` with
`from_version`, `to_version`, `backup`, and `provenance`, and `refusals`. A release
accepts the store schemas from its oldest accepted schema to its own. This
release accepts only its own schema, so `migrate_store` reports `current` for
it and refuses an older schema as `store.schema_unsupported`. No other command
migrates an existing store: `factory start`, `factory doctor`, `factory
status`, and every other command refuse an older schema, and the doctor
reports `store.sqlite.migration_required`.

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
current store, a recorded doctor of the target, and a coordinator whose lock
record names the target release and revision. `factory status --config
<path> --json` shows the supervisor heartbeat.

## Maintenance checkpoint

A maintenance checkpoint is a consistent snapshot of the operational store
with a manifest. It needs full quiescence, which is stricter than the migration
gate. Full quiescence is the migration gate plus one extra condition: no
ideation session publishes or has an incomplete publication
(`publication.in_progress`). Run:

```sh
factory checkpoint create --config <host config> --output <new directory> --json
```

`--output` names a directory that does not exist yet. Its parent must be
private to the operator (no group or other permissions), and it must not lie
inside the registered checkout or the store directory. The command holds the
repository lock for its whole duration, takes the snapshot while it holds the
SQLite write lock, and reads the state of every branch, pull request, issue,
and status comment that the store knows. The checkpoint directory has mode
`0700` and holds `store.db` and `manifest.json`, each with mode `0600`. The
manifest records the checksums, the release, the registration identity, and
that external state. It holds no credential and no credential path, but it
holds issue and specification text: keep the checkpoint private, and never
attach it to a bug report.

The document has `schema_version: 1`, `outcome` (`created`, `refused`, or
`error`), `checkpoint` with `path` and `manifest`, and `refusals` with `code`
and `detail`. A refusal writes nothing. It exits 0 only for `created`.

To check a checkpoint without a configuration, run:

```sh
factory checkpoint inspect --path <checkpoint directory> --json
```

`inspect` verifies the manifest, every checksum, the exact file set, and the
store snapshot (`PRAGMA integrity_check` on a temporary read-only copy). It
changes no file. Its document has `schema_version: 1`, `outcome` (`verified`,
`refused`, or `error`), `verified` (true only for `verified`), `checkpoint`
with `path` and `manifest` when it verifies, and `refusals` with `code` and
`detail`. It exits 0 only for `verified`. A damaged checkpoint refuses with
`restore.checkpoint_corrupt` or `restore.checkpoint_incomplete`.

| Code | Meaning |
| --- | --- |
| `checkpoint.not_quiescent` | One full-quiescence condition fails; `detail` names it. Finish or cancel that work first. |
| `checkpoint.coordinator_running` | A coordinator or another maintenance command holds the repository lock. Stop it or wait. |
| `checkpoint.migration_required` | The store schema is older than this release. Checkpoint with the release that wrote it. |
| `checkpoint.external_unverified` | A known external object, or the target branch, cannot be read; `detail` names it. |
| `checkpoint.store_missing` | No operational store exists. |
| `checkpoint.destination_refused` | `--output` exists, is not private, or lies inside the checkout or the store directory. |

Factory never prunes checkpoints.

### Restore a checkpoint

A restore replaces the operational store with the snapshot of a checkpoint
of the same host, registration, and release. Stop the coordinator first.
Preview the plan:

```sh
factory checkpoint restore --config <host config> --path <checkpoint directory> --json
```

The preview changes nothing. It verifies the checkpoint, holds the repository
lock, checks the live store, Docker, the work tracker, the code host, and the
remote, and prints `plan` with `steps`, `preserved_paths`, and `digest`. To
apply exactly that plan, run the command again with
`--confirm <digest>`. A changed live store or another changed situation gives
another digest, and the confirm refuses with `restore.plan_stale`; preview
again.

The confirm holds the SQLite write lock of the live store from its checks
until the live files are in `original/`. A command that the local UI submits
in that time waits and can fail as a busy store; submit it again after the
restore. When another connection holds the lock too long, the confirm
refuses with `restore.not_quiescent` (`store.write_locked`). When a preserved
original changed after the checks, the restore puts it back and fails.

The document has `schema_version: 1`, `outcome` (`preview`, `restored`,
`refused`, `interrupted`, or `error`), `plan`, `refusals` with `code` and
`detail`, and `journal` for a continued restore. It exits 0 only for
`preview` and `restored`.

The restore writes the restore journal `<store>.restore.json` before each
rename and keeps the original store and its sidecars in
`<store directory>/.restore-<checkpoint_id>/original/`. Remove that directory
yourself after you checked the restored installation. While a journal exists,
every command that opens the store refuses, and `factory doctor` reports
`store.sqlite.restore_in_progress`. Run the same restore command with the
same release: it continues the interrupted restore first, to the unchanged
original or to the verified replacement, and reports `interrupted`. The
restore never starts the coordinator, resumes a harness, or replays an
effect. Afterwards run `factory doctor`, then `factory start`.

| Code | Meaning |
| --- | --- |
| `restore.checkpoint_incomplete` | A checkpoint file or the manifest is missing. |
| `restore.checkpoint_corrupt` | A checksum, the integrity check, the file set, or the manifest schema fails. Nothing is written. |
| `restore.schema_newer` | The snapshot schema is newer than this release. |
| `restore.release_mismatch` | The checkpoint is of another release or revision, or of an older schema; or the interrupted restore was started by another release. Restore with the creating release, then migrate with `factory upgrade migrate-store`. |
| `restore.registration_changed` | The registration or the host configuration path differs from the checkpoint. |
| `restore.coordinator_running` | A coordinator or another maintenance command holds the repository lock. |
| `restore.worker_active` | A harness runs in a worker of this store, a role session has no proved resource release, or Docker cannot prove that none runs. With `worker.unattributed`, the live store is missing or does not open, so it cannot name its workers, and a `factory-worker-*` container runs; restore cannot tell whether it belongs to this store, so stop it first, also when it is idle or of another installation. |
| `restore.not_quiescent` | The live store has a non-terminal run, a pending effect, other active work, or a record that the snapshot does not have; `detail` names it. Resolve a pending effect with `factory reconcile` or effect abandonment first. With `store.unreadable`, the live store exists but does not open, for example because it is busy, not private, or of another schema; repair that first. With `store.write_locked`, another connection holds the write lock of the live store; close it and preview again. Only a missing or damaged (empty, corrupt, or not a database) store is replaced without this check, and the preview names the damage. |
| `restore.external_divergence` | A known branch, pull request, issue, or status comment changed after the checkpoint, or a `factory/*` branch, a pull request from one, or an issue with a run-state label is unknown to it; `detail` names each object. |
| `restore.divergence_unverified` | A known object or a listing cannot be read. Unknown evidence never passes. |
| `restore.plan_stale` | The `--confirm` digest is not the digest of the current plan. |
| `restore.work_directory_exists` | The work directory of an earlier restore of this checkpoint exists. Move it away after you review its original. |

A missing store directory is created when the registration is the one of the
checkpoint; the preview names it. A missing worker image does not stop a
restore; `factory doctor` reports it afterwards. The `agent-ready` label alone
is a human authorization, not a factory effect, so it never diverges. Only
the GitHub adapters list factory-owned objects; with another tracker or code
host, the restore refuses with `restore.divergence_unverified`.

## Recovery limits

- The only supported restore is `factory checkpoint restore` of a
  maintenance checkpoint of the same host, registration, and release. A
  `<store>.bak-*` file is migration provenance, not a restore point. Never
  copy it or any other file over the store. Decision record ADR 0032 in the
  source repository sets the checkpoint and restore boundary.
- A database copy does not rewind branches, pull requests, comments, labels,
  workers, or harness sessions. An older store can repeat those effects.
- Never downgrade the binary to escape a blocker. When compatibility or
  consistency with external state cannot be established, stop and give the
  operator the plan document.
