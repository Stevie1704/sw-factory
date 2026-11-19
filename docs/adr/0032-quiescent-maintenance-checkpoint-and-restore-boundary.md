---
status: proposed — needs human approval of the support boundary
---

# A quiescent maintenance checkpoint is the only supported backup, and restore fails closed

Issue #258. An installation is more than its SQLite file. It also has host
configuration, branches and worktrees, workers, credential and session volumes,
pending effects, and the issue, comment, label, branch, and pull-request
projections on the work tracker and code host. An older database alone can
disagree with that external work. If Factory restores it and continues, it can
repeat an effect: a second status comment, a second pull request, a push, a
worker launch, or a harness resume.

This ADR sets the support boundary before a restore command exists. It does
not add a backup or a restore command. The approval of this boundary is a
human decision; the pull request that adds this ADR is that approval step.
Until the follow-up specifications below are implemented, Factory has no
supported restore, and the [upgrade procedure](../installation.md#upgrade)
states that limit.

## Decision

1. The first supported version is a **maintenance checkpoint**: a consistent
   snapshot of the operational store with a manifest, taken on the same host,
   for the same registration, by the same Factory release, while the
   installation is in **full quiescence**. Restore puts that snapshot back
   only on the same host, registration, and release.
2. **Active-run recovery fails closed.** Checkpoint creation refuses an
   installation with active work, and restore refuses when it cannot prove
   that no external effect happened after the snapshot. There is no override
   flag in the first version.
3. A checkpoint never copies a live database file. One connection holds the
   SQLite write lock (`BEGIN IMMEDIATE`) and checks full quiescence; a second
   connection takes the online snapshot (`VACUUM INTO`) while that lock is
   held. The coordinator lock is held for the whole operation.
4. A checkpoint contains no credential, credential path content, session
   volume, or harness transcript. It does contain private issue and
   specification text, so it is private data, never a shareable artifact.
5. Restore preserves the original store until the replacement is verified.
   A restore journal is written before every rename, so each crash point
   leaves a state that a continuation recovers without loss.
6. The manifest records the observable state of every known external object.
   Restore compares each one and refuses on any difference.
7. The pre-migration `.bak-*` snapshot of the operational store stays migration
   provenance. It is not a checkpoint, and no procedure restores it.

## State and ownership inventory

Classes:

- **Authoritative**: Factory owns it, and no other source can rebuild it.
- **Reconstructible**: a supported command rebuilds it from authoritative or
  Git-owned input.
- **External**: another system owns it (Git remote, work tracker, code host,
  Docker, the operator's checkout).
- **Secret**: credential or session material.
- **Disposable**: it has no value after the operation that made it.

| Item | Location | Class | First-version checkpoint |
| --- | --- | --- | --- |
| Operational store: runs, invocations, pending-effect journal, ideation sessions, publication ledger, local issue identities and authorizations, command inbox, supervisor heartbeat | `operational_data_path` (SQLite) | Authoritative | Captured, as one online snapshot |
| Local evaluation-summary projection | Inside the operational store | Authoritative | Captured with the store |
| SQLite sidecars (`-journal`, `-wal`, `-shm`) | Beside the store | Disposable | Excluded; the snapshot is one self-contained file |
| Pre-migration backups `<store>.bak-*` | Beside the store | Disposable (migration provenance) | Excluded |
| Host configuration and registration | `$FACTORY_CONFIG` or the user configuration directory | Authoritative, private | Excluded; the manifest records its identity digest and a non-secret summary |
| Credential source paths inside the registration | Host configuration | Private | Excluded; never copied |
| Host credential sources, Keychain items, `gh` and `az` login | Host | Secret, external | Excluded |
| Factory-managed credential and role session volumes | Docker volumes | Secret | Excluded |
| Coordinator lock | `$TMPDIR/sw-factory/coordinators` | Disposable | Excluded |
| Run worktrees, local `factory/*` branches, specification worktrees | Next to the store and in the checkout's Git directory | Reconstructible or disposable | Excluded; quiescence means no active run owns one |
| Workers and their containers | Docker | Disposable | Excluded; quiescence means none runs |
| Generated run outputs | Below the store directory | Disposable | Excluded |
| Worker images, worker base images | Docker | Reconstructible (from the worker definition and the release) | Excluded; the manifest records the pinned digest |
| Repository checkout, `factory.yaml`, `worker/skill-smoke.json`, `docs/issues/` | The registered checkout | External (Git) | Excluded; the manifest records the target branch head |
| Repository caches | Host cache root | Disposable | Excluded |
| Onboarding plan and prepared release resources | Operator cache directory | Reconstructible | Excluded |
| Remote branches, issues, labels, comments, pull requests, commit statuses | Work tracker and code host | External | Excluded; restore compares against them |
| Installed binaries | Operator install directory | External | Excluded; the manifest records the release identity |

## Consistent snapshot

A checkpoint must exclude coordinator, CLI, and worker writes between the
quiescence check and the snapshot:

1. Acquire the coordinator lock without waiting. A held lock refuses with
   `checkpoint.coordinator_running`. The lock stays held until the manifest
   is written, so no coordinator can start during the checkpoint.
2. Read the schema version through the read-only path. An older schema
   refuses with `checkpoint.migration_required`; a checkpoint never migrates.
   Then open connection A read-write without migration. This rolls back a hot
   `-journal` from an interrupted writer.
3. On connection A, run `BEGIN IMMEDIATE`. A now holds the SQLite write lock,
   so no other connection can commit a write. Check full quiescence (below)
   on A.
4. On a second connection B, run `VACUUM INTO '<staging file>'`. SQLite
   refuses `VACUUM INTO` inside A's own transaction, so it must run on B. B
   reads the committed state, which is the state that A checked, because no
   write can commit while A holds the lock. A CLI or UI writer that is not
   under the coordinator lock, such as a session command append, waits for
   its busy timeout or fails.
5. End A with `ROLLBACK`. A wrote nothing.
6. `VACUUM INTO` creates the file with the process umask. Set its mode to
   `0600` at once; the staging directory is `0700`, so no other user can read
   it before that. Check the staging file with `PRAGMA integrity_check`, read
   its schema version, and compute its SHA-256. Then write the manifest and
   rename the staging directory into place.

The sequence works in the store's rollback-journal mode and in WAL mode; the
feasibility test runs it in both. The store does not need to change its
journal mode.

Copying the database file is not supported, also not after closing the
connection, because another process can write between the close and the copy.
The pre-migration backup uses the same online sequence (follow-up D, #321).

**Full quiescence** means all of these are true on connection A:

- no run has a non-terminal status;
- the pending-effect journal is empty;
- no invocation or session-role invocation is active;
- every ended session-role invocation has a recorded resource release
  (`resources_released_at` is set), so its worker is proved stopped;
- the command inbox has no session command that is not completed;
- Docker proves that no harness process of this store runs: no worker
  container named from a worker of this store runs a starting or running
  harness helper (`worker.active`);
- no ideation session is publishing or has an incomplete publication;
- the coordinator lock was free when the command acquired it.

The Docker condition reads only the containers whose names derive from the
workers of the invocations in this store, so a container of another
registration never blocks. Its evidence is the harness helper state, not a
running container: a terminal run can leave its idle worker running, for
example after an accepted report that does not stop the worker. Docker that
cannot be reached gives `docker.unreachable`, and a helper state that cannot
be read gives `worker.unobserved`; neither is a pass. A store that never
launched a worker needs no Docker. Restore precondition 5 uses the same
check when the live store is readable.

Any other result refuses with `checkpoint.not_quiescent` and names each
condition. The operator finishes or cancels the work with the current binary
first.

The upgrade store migration (`factory upgrade migrate-store`) uses the
narrower **migration gate**: every condition except the publication
condition, so the first six conditions and the free coordinator lock. It does not check publications, because a migration keeps
every row and its pre-migration copy, while restore replaces the store.

## Manifest, identity, and integrity

A checkpoint is one directory with mode `0700`. It holds `store.db` and
`manifest.json`, each with mode `0600`. The command refuses a destination
inside the registered checkout, inside the store directory, or in a directory
that is readable by group or other.

`manifest.json` (schema version 1) records:

| Field | Content |
| --- | --- |
| `schema_version` | `1`; an unknown version is refused |
| `kind` | `maintenance_checkpoint` |
| `checkpoint_id` | A random identifier |
| `created_at` | RFC 3339 instant of the snapshot transaction |
| `factory` | `version`, `build`, `revision`, and `modified` of the creating build |
| `store_schema_version` | The schema version read from the snapshot |
| `registration_identity` | The SHA-256 identity of the host configuration path and the registration, as the onboarding plan computes it |
| `registration_summary` | Repository path, tracker and code host providers, owner, project, and repository names, and the local installation ID; no credential path |
| `worker_image` | The pinned `image@digest` |
| `target_branch_head` | The commit of the target branch on the remote at checkpoint time |
| `watermarks` | The highest run, invocation, publication-ledger, and local-issue identities that the snapshot knows |
| `external_state` | The observable state of every external object that the snapshot knows: for each known run branch, the remote head SHA; for each known pull request, its head SHA, state (open, closed, merged), and draft flag; for each known issue, its open state and factory state labels; for each status comment that the store identifies, its ID and update time; and the target branch head. A value that cannot be read at checkpoint time refuses the checkpoint with `checkpoint.external_unverified` |
| `files` | For each file: relative path, size, mode, and SHA-256 |
| `private_data` | Always `true`, with the reason: issue and specification text |

`factory checkpoint inspect` verifies the manifest, every checksum, and
`PRAGMA integrity_check` on a temporary read-only copy, and prints the
manifest. It does not need the installation and changes nothing.

Retention belongs to the operator. Factory never prunes checkpoints, and a
checkpoint never goes into a bug report or acceptance evidence.

## Restore preconditions and divergence

Restore is a preview by default. `--confirm` binds the digest of the
previewed plan, as reset does, so a changed situation needs a new preview.
Restore refuses unless every precondition is proved:

1. The manifest and every checksum verify, and the snapshot passes its
   integrity check.
2. The running build has the same release and revision as the manifest, and
   its `CurrentSchemaVersion` equals `store_schema_version`.
3. The current registration identity equals `registration_identity`.
4. Restore acquires the coordinator lock and holds it to the end.
5. No worker container or role session of this installation runs.
   When the live store is missing or does not open, it cannot name its
   workers, and a worker launched after the checkpoint can still run. A
   worker container name (`factory-worker-` and a hash of the worker ID)
   does not name its installation, so restore then always asks Docker and
   refuses on any running `factory-worker-*` container
   (`worker.unattributed`), also an idle one or one of another installation.
   This is conservative; the operator stops those containers first. Docker
   that cannot be reached refuses with `docker.unreachable`.
6. The live store has no pending effect, no non-terminal run, and nothing
   newer than the watermarks: no run, invocation, ledger entry, or local
   issue identity that the snapshot does not have. Only a missing store or a
   damaged one (empty, corrupt, or not a database) skips this precondition,
   and the preview names the damage. A store that exists but does not open
   for another reason, for example busy, not private, or of another schema,
   refuses with `restore.not_quiescent` (`store.unreadable`).
7. Every known external object in `external_state` has exactly its recorded
   state: each known branch has the recorded remote head SHA, each known pull
   request has the recorded head SHA, state, and draft flag, each known issue
   has the recorded open state and factory labels, and each known status
   comment has the recorded update time. A known branch that advanced without
   a new name, pull request, or comment is therefore a divergence.
8. The work tracker and code host show no unknown factory-owned object: no
   `factory/*` branch or pull request that the snapshot does not know, and no
   issue with a factory state label whose run the snapshot does not know.

A confirmed restore takes the SQLite write lock of the live store
(`BEGIN IMMEDIATE`, no write) before it checks preconditions 5 to 8 and the
plan digest, and holds it through the preserve renames. So no writer that is
already open, such as a command inbox, can commit between the checks and the
renames. The plan digest records the size and SHA-256 of each live file;
restore keeps each handle that it reads them with open until it releases the
lock, because a close of any handle of the store file drops the POSIX lock
of SQLite for the whole process. A store that another connection holds past
the busy timeout refuses with `restore.not_quiescent` (`store.write_locked`).
After the release, restore compares each preserved original with the size
and SHA-256 of the plan and rolls back on a difference. A missing or damaged
store gets no lock, because no writer can commit into it. The preview takes
no lock.

An older snapshot therefore cannot authorize a duplicate comment, pull
request, push, worker launch, or harness resume. Any difference refuses with
`restore.external_divergence` and names each object. An object or a source
that restore cannot read (for example, the tracker is not reachable) refuses
with `restore.divergence_unverified`. Unknown evidence is never success.

## Specified outcomes

| Situation | Checkpoint | Restore |
| --- | --- | --- |
| Healthy, quiescent, same release | Created | Proceeds after the preview and `--confirm` |
| Snapshot schema newer than the build | Not applicable | Refused: `restore.schema_newer` |
| Snapshot schema or release older than the build | Not applicable | Refused: `restore.release_mismatch`; restore with the creating release, then migrate with the normal entry point |
| Store schema older than the build | Refused: `checkpoint.migration_required` | Not applicable |
| Changed registration | Not applicable | Refused: `restore.registration_changed` |
| Live coordinator | Refused: `checkpoint.coordinator_running` | Refused: `restore.coordinator_running` |
| Active worker or role session | Refused: `checkpoint.not_quiescent` | Refused: `restore.worker_active` |
| Non-terminal run | Refused: `checkpoint.not_quiescent` | Refused: `restore.not_quiescent` |
| Pending or ambiguous effect | Refused: `checkpoint.not_quiescent` | Refused: `restore.not_quiescent`; resolve it with `factory reconcile` or effect abandonment first |
| External effect after the snapshot, including a moved known branch or a changed known pull request | Not applicable | Refused: `restore.external_divergence`, with each object |
| Restore journal of an earlier restore exists | Not applicable | The command continues that restore first (see below); every other store opener refuses |
| Divergence source or known object not readable | Refused: `checkpoint.external_unverified` | Refused: `restore.divergence_unverified` |
| Missing checkpoint file or manifest | Not applicable | Refused: `restore.checkpoint_incomplete` |
| Checksum, integrity, or manifest-schema failure | Not applicable | Refused: `restore.checkpoint_corrupt`; nothing is written |
| Missing store directory | Refused: `checkpoint.store_missing` | Proceeds only after the operator recreates the registration; the preview names it |
| Missing pinned worker image | Created; the manifest records the pin | Proceeds; `factory doctor` reports the image afterwards |

## Restore plan and interruption

All restore files live in one work directory `<store directory>/.restore-<checkpoint_id>/`
(mode `0700`) on the same file system as the store, so every rename is atomic.
It holds `new/` for the staged replacement and `original/` for the preserved
live files. The restore journal `<store>.restore.json` (mode `0600`) is
written to a temporary file, synced, and renamed into place, and the
directory is synced, **before** each rename. The journal lists the live file
set: the store and each of its `-journal`, `-wal`, and `-shm` files that
existed when the restore started.

While a restore journal exists, every store opener, including the read-only
diagnosis opener, refuses with a typed error that names the restore command.
So no process can open a store whose files are split between the live path
and `original/`, also after the coordinator lock is released by a crash.

| Journal state (written before the step) | Step | State after a crash in this step | Continuation |
| --- | --- | --- | --- |
| `planned` | Create the work directory; copy `store.db` to `new/` and verify its checksum and integrity | Live files untouched; `new/` partial or complete | Remove the work directory and the journal |
| `preserving` | Rename each live file into `original/`, the store first, then each sidecar, syncing the directory after each rename; then release the store write lock and compare each original with the plan | Each listed file is either at its live path or in `original/` | Rename every listed file that is in `original/` back to its live path, then remove `new/` and the journal |
| `replacing` | Rename `new/store.db` to the live store path | Live path empty or holds the replacement; originals complete in `original/` | Remove a replacement at the live path (the checkpoint still holds it), write the journal state `preserving`, then continue as for `preserving` |
| `verifying` | Open the replacement read-only; compare schema version, watermarks, and checksum; run the store doctor checks | Replacement at the live path; originals in `original/` | Repeat the verification; on failure, roll back as for `replacing` |
| `verified` | Write the final journal state, then remove the journal | Replacement verified; originals in `original/` | Remove the journal |

The replacement is one file without sidecars. Before the rename into place
and in each verification, restore refuses a `-journal`, `-wal`, or `-shm`
file at the live path, so a stale sidecar never applies to the replacement;
restore then rolls back and does not remove an unlisted sidecar.

Every continuation before `verified` ends with the original live file set
back in place and unchanged, so it is lossless. After `verified`, the
original stays in `original/` until the operator removes it. Continuation
needs the coordinator lock and runs only from the same release. Restore never
starts the coordinator, resumes a harness, or replays a pending effect. The
operator runs `factory doctor` and then `factory start`.

## Unsupported scenarios

The first version does not support: a restore to another host, another
registration, or another release; recovery of an active run; restore of
credentials, session volumes, worker images, worktrees, branches, or remote
tracker and code-host state; a partial or table-level restore; merging a
snapshot with a newer store; a binary downgrade; and an automatic or scheduled
checkpoint. Each one is refused, not approximated.

## Considered options

| Option | Result |
| --- | --- |
| Copy the database file and its sidecars | Rejected. A copy is not consistent while another process writes, and a copied hot journal can apply to the wrong file. |
| Back up the whole installation, credentials included | Rejected. It makes a secret-bearing artifact, and it still cannot rewind remote state. |
| Allow a restore with active runs and let reconciliation repair it | Rejected. Reconciliation compares one store with the remote state. It cannot tell that an effect it does not know happened after the snapshot, so it can repeat it. |
| Restore across releases and migrate after | Rejected for the first version. It combines two risky steps, and the creating release can do the restore. |
| Promote the pre-migration `.bak-*` copy to a restore point | Rejected. It has no manifest, no quiescence proof, and no divergence check. |

## Feasibility evidence

The production tests are the evidence. `internal/store/snapshot_test.go`
runs the snapshot sequence in rollback-journal mode and in WAL mode:
connection A holds `BEGIN IMMEDIATE` and runs the check; a third writer
cannot change the store; connection B takes the snapshot; A ends. The
snapshot holds the checked state, has mode `0600`, and passes the integrity
check. `internal/store/checkpoint_feasibility_test.go` keeps the one fact
that production does not exercise: `VACUUM INTO` inside A fails, so the
snapshot runs on B. `internal/checkpoint/divergence_test.go` and
`internal/factory/checkpoint_restore_test.go` show that restore refuses a
later run, a later pull request, a pending effect, and a known branch that
moved, and that an unreadable known object refuses as unverified.

## Follow-up implementation specifications

These are drafts. They become issues only after this boundary is approved.
None of them may claim backup or restore delivery for roadmap #260 alone.

### A. `factory checkpoint create`

- CLI: `factory checkpoint create --config <path> --output <new directory> --json`.
- JSON (schema 1): `command`, `outcome` (`created`, `refused`, `error`),
  `checkpoint` (path and manifest), `refusals[]` (`code`, `detail`).
- Acceptance:
  - [ ] A quiescent installation produces a directory with `0700`,
        `store.db` and `manifest.json` with `0600`, and checksums that verify.
  - [ ] Each full-quiescence condition refuses with
        `checkpoint.not_quiescent` and names that condition; nothing is
        written.
  - [ ] The snapshot runs on a second connection while the first holds
        `BEGIN IMMEDIATE`; a test proves that a concurrent writer cannot
        commit between the check and the snapshot.
  - [ ] The manifest records `external_state` for every known branch, pull
        request, issue, and status comment; an unreadable object refuses
        with `checkpoint.external_unverified`.
  - [ ] A held coordinator lock refuses; the lock is held during the snapshot.
  - [ ] An older schema refuses with `checkpoint.migration_required`, and the
        store bytes do not change.
  - [ ] A destination inside the checkout, the store directory, or a
        group-readable directory is refused.
  - [ ] The manifest contains no credential path and no secret (a test scans
        it for each registration credential path).
- Failure injection: kill between `VACUUM INTO` and the manifest write (no
  directory at the final path), disk full during the snapshot, a concurrent
  inbox append during the snapshot.

### B. `factory checkpoint inspect`

- CLI: `factory checkpoint inspect --path <directory> --json`. Needs no
  registration.
- Acceptance:
  - [ ] A valid checkpoint prints its manifest and `verified: true`.
  - [ ] One changed byte, a missing file, an extra file, and an unknown
        manifest schema each give `restore.checkpoint_corrupt` or
        `restore.checkpoint_incomplete`.
  - [ ] Inspection changes no file (bytes and modification times compared).

### C. `factory checkpoint restore`

- CLI: `factory checkpoint restore --config <path> --path <directory> --json`
  (preview), and `--confirm <plan digest>` to apply.
- JSON (schema 1): `command`, `outcome` (`preview`, `restored`, `refused`,
  `interrupted`, `error`), `plan` (steps, preserved paths, digest),
  `refusals[]`, `journal`.
- Acceptance:
  - [ ] Every row of "Specified outcomes" has a fixture test with the exact
        refusal code.
  - [ ] The divergence check uses fake tracker and code-host adapters. A
        moved known branch, a changed known pull request head or state, a
        changed known issue label, and an unknown `factory/*` branch each
        refuse with `restore.external_divergence`; an unreadable source or
        object refuses with `restore.divergence_unverified`.
  - [ ] While a restore journal exists, `store.Open`, `store.OpenReadOnly`,
        and `factory doctor` refuse with a typed error.
  - [ ] A confirm with a stale plan digest is refused.
  - [ ] After success, the preserved original exists, and `factory doctor`
        reports the store ready.
- Failure injection: crash before and after every rename of the table in
  "Restore plan and interruption", including between two sidecar renames.
  Continuation reaches either the unchanged original file set (byte compare)
  or a verified replacement, never a mix.

### D. Pre-migration backup provenance

- Replace the file copy in `backupDatabase` with the online snapshot
  primitive, and write `<store>.bak-<timestamp>.json` with the source and
  target schema versions, the release that migrated, and the SHA-256.
- Acceptance:
  - [ ] The backup uses the snapshot sequence of this ADR, and a migration
        while another connection holds an uncommitted write produces a
        backup that passes the integrity check.
  - [ ] `factory reset` still removes these files as backups of that exact
        database.
  - [ ] `factory upgrade migrate-store --json` reports the provenance file.

### E. Upgrade guidance

- When A to C ship, `factory guide maintenance` adds an optional
  "checkpoint before the store migration" step, and the upgrade plan lists
  the newest matching checkpoint. A checkpoint needs full quiescence, which
  is stricter than the migration gate, so the guide names the extra
  conditions. Until then the guide states that no
  supported restore exists.
