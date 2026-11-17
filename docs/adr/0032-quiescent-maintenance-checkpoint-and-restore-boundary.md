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
   installation is **quiescent**. Restore puts that snapshot back only on the
   same host, registration, and release.
2. **Active-run recovery fails closed.** Checkpoint creation refuses an
   installation with active work, and restore refuses when it cannot prove
   that no external effect happened after the snapshot. There is no override
   flag in the first version.
3. A checkpoint never copies a live database file. It takes a SQLite online
   snapshot (`VACUUM INTO`) inside one read transaction while it holds the
   coordinator lock.
4. A checkpoint contains no credential, credential path content, session
   volume, or harness transcript. It does contain private issue and
   specification text, so it is private data, never a shareable artifact.
5. Restore preserves the original store until the replacement is verified,
   and an interrupted restore is always diagnosable from a restore journal.
6. The pre-migration `.bak-*` copy of the operational store stays migration
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

A checkpoint must exclude coordinator, CLI, and worker writes:

1. Acquire the coordinator lock without waiting. A held lock refuses with
   `checkpoint.coordinator_running`. The lock stays held until the manifest
   is written, so no coordinator can start during the checkpoint.
2. Open the store through the normal read-write opener. This rolls back a hot
   `-journal` from an interrupted writer. The opener migrates an older schema,
   so the command first reads the schema version through the read-only path,
   and refuses an older schema with `checkpoint.migration_required`. A
   checkpoint never migrates.
3. Begin one read transaction. Inside it, check quiescence (below) and run
   `VACUUM INTO '<staging file>'`. A CLI or UI writer that is not under the
   coordinator lock, such as a session command append, either committed
   before the transaction or is not in the snapshot. The snapshot is
   transactionally consistent either way.
4. `VACUUM INTO` creates the file with the process umask. Set its mode to
   `0600` at once; the staging directory is `0700`, so no other user can read
   it before that. Check the staging file with `PRAGMA integrity_check`, read
   its schema version, and compute its SHA-256. Then write the manifest and rename the
   staging directory into place.

Copying the database file is not supported, also not after closing the
connection, because another process can write between the close and the copy.
The current pre-migration backup copies the file; follow-up D moves it to the
same online primitive.

**Quiescent** means all of these are true inside the snapshot transaction:

- no run has a non-terminal status;
- the pending-effect journal is empty;
- no invocation or session-role invocation is active;
- no ideation session is publishing or has an incomplete publication;
- the command inbox has no unapplied command;
- the coordinator lock was free when the command acquired it.

Any other result refuses with `checkpoint.not_quiescent` and names each
condition. The upgrade store migration (`factory upgrade migrate-store`)
checks only the first three conditions and the lock, because it does not
replace the store and keeps its pre-migration copy. A checkpoint needs the
complete list, because restore replaces the store. The operator finishes or cancels the work with the current
binary first.

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
| `watermarks` | The highest run, invocation, publication-ledger, and local-issue identities, and the factory-owned pull-request and branch names that the snapshot knows |
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
6. The live store, when it is readable, has no pending effect, no
   non-terminal run, and nothing newer than the watermarks: no run,
   invocation, ledger entry, or local issue identity that the snapshot does
   not have.
7. The work tracker and code host show no factory-owned effect after the
   snapshot: no `factory/*` branch or pull request that the snapshot does not
   know, no issue with a factory state label whose run the snapshot does not
   know, and no factory status comment that is newer than `created_at`.

An older snapshot therefore cannot authorize a duplicate comment, pull
request, push, worker launch, or harness resume. A source that restore cannot
read (for example, the tracker is not reachable) refuses with
`restore.divergence_unverified`. Unknown evidence is never success.

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
| External effect after the snapshot | Not applicable | Refused: `restore.external_divergence`, with each unexplained item |
| Divergence source not readable | Not applicable | Refused: `restore.divergence_unverified` |
| Missing checkpoint file or manifest | Not applicable | Refused: `restore.checkpoint_incomplete` |
| Checksum, integrity, or manifest-schema failure | Not applicable | Refused: `restore.checkpoint_corrupt`; nothing is written |
| Missing store directory | Refused: `checkpoint.store_missing` | Proceeds only after the operator recreates the registration; the preview names it |
| Missing pinned worker image | Created; the manifest records the pin | Proceeds; `factory doctor` reports the image afterwards |

## Restore plan and interruption

Restore writes a restore journal `<store>.restore.json` (mode `0600`) before
each step and replaces it atomically. The steps are:

1. `staged`: copy `store.db` to `<store>.restore-<checkpoint_id>`, then
   verify its checksum and integrity.
2. `original_preserved`: rename the live store and each existing sidecar to
   `<store>.pre-restore-<checkpoint_id>[-suffix]`. Never delete it.
3. `replaced`: rename the staging file to `<store>`, then sync the directory.
4. `verified`: open the store read-only, compare the schema version and the
   watermarks with the manifest, and run the store and supervisor doctor
   checks.
5. `complete`: release the coordinator lock.

A later restore command or `factory doctor` reads the journal. Before
`replaced`, continuation removes the staging file and leaves the original in
place. At or after `replaced`, continuation repeats verification. A failed
verification renames the preserved original back. The preserved original is
removed only by the operator. Restore never starts the coordinator, resumes a
harness, or replays a pending effect. The operator runs `factory doctor` and
then `factory start`.

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

`internal/store/checkpoint_feasibility_test.go` is a test-only prototype. It
shows that a `VACUUM INTO` snapshot taken while another connection holds an
uncommitted write contains only committed state, has no sidecar, and passes
the integrity check. It also shows that `VACUUM INTO` writes a file that the
read-only store opener refuses until its mode is `0600`. It shows that a divergence check refuses a snapshot when a later run or
factory-owned pull request exists, or when a pending effect is journaled. It
adds no production code.

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
  - [ ] Each quiescence condition refuses with `checkpoint.not_quiescent` and
        names that condition; nothing is written.
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
  - [ ] The divergence check uses fake tracker and code-host adapters; an
        unreadable source refuses with `restore.divergence_unverified`.
  - [ ] A confirm with a stale plan digest is refused.
  - [ ] After success, the preserved original exists, and `factory doctor`
        reports the store ready.
- Failure injection: interrupt after each journal step; continuation reaches
  either the original store or a verified replacement, never a mix.

### D. Pre-migration backup provenance

- Replace the file copy in `backupDatabase` with the online snapshot
  primitive, and write `<store>.bak-<timestamp>.json` with the source and
  target schema versions, the release that migrated, and the SHA-256.
- Acceptance:
  - [ ] A migration while another connection holds an uncommitted write
        produces a backup that passes the integrity check.
  - [ ] `factory reset` still removes these files as backups of that exact
        database.
  - [ ] `factory upgrade migrate-store --json` reports the provenance file.

### E. Upgrade guidance

- When A to C ship, `factory guide maintenance` adds an optional
  "checkpoint before the store migration" step, and the upgrade plan lists
  the newest matching checkpoint. Until then the guide states that no
  supported restore exists.
