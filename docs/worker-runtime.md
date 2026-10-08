# Worker runtime

Issue #5 introduces the `WorkerRuntime` seam. The coordinator addresses a
worker by the run identifier only; Docker container names, process tracking,
container paths, and invocation/result storage remain inside the adapter.

The interface has five operations:

- `start` creates a per-run worker from `image@digest`. An existing stopped
  worker is reused only when its frozen image and mount contract (worktree,
  Git metadata, and cache paths) match the request.
- `resume` restarts the existing worker without changing its frozen image.
- `run-command` runs one shell command and returns its exit result.
- `stop` stops the worker while retaining it for resume.
- `inspect` reports existence and running state without returning a Docker
  identifier.

The optional `HeadlessProcessRuntime` extension adds `start-headless`,
`inspect-headless`, `cancel-headless`, and `finish-headless`. It receives only
the logical run, worker, and invocation identities, the command argv, and the
explicit worker environment. It never returns a container name, PID, PTY, or
host path. The Docker adapter runs the command through
`/usr/local/bin/factory-worker-headless` with `docker exec -d`; no `-i` or `-t`
flag is used.

The checked-in worker build workflow is local-only: Docker's content-addressable
local image ID is emitted as the `sha256` digest and verified as
`image@digest` with `--pull=never`. A published copy must use its registry
manifest digest instead; the worker runtime itself never turns a digest into a
pull.

The Docker adapter uses these stable paths regardless of the host checkout:

| Worker path | Access | Contents |
| --- | --- | --- |
| `/work` | read-write | The run worktree |
| `/git` | read-only | A credential-free projection of Git metadata needed for history and diffs |
| `/cache/<name>` | `read_only` declared per cache | A repository cache, whose host directory the registration maps by name |
| `/invocation` | read-only | The frozen invocation packet for the active role |
| `/results` | read-write | The invocation-scoped `report.json` result directory |
| `/run/factory-auth` | managed; written only by the adapter, read-only for the role | A factory-managed Codex credential volume, separate from role session state |

Headless process state is kept in the role volume at
`/home/factory/.factory-headless/<invocation-id>`. The helper atomically records
`starting`, `running`, `exited`, `cancelled`, or `lost`, retains bounded
head-and-tail native stdout/stderr, and can be inspected or cancelled after the
coordinator restarts. A state lock makes fresh launch and cancellation
idempotent across concurrent Docker exec calls. Exact native resume may replace
only an exited, cancelled, or lost process whose recorded PID is no longer
alive. Cancellation waits through SIGTERM and SIGKILL escalation before it
publishes `cancelled`; the role volume survives worker recreation, while the
worker image remains pinned by the existing `image@digest` contract.

Workers run as uid/gid `10001:10001`, drop all capabilities, disable privilege
escalation, and use the ordinary bridge network for public research access.
The adapter does not mount the Docker socket, host home, SSH agent, Keychain,
operational store, or arbitrary repository paths. It starts only from the
configured image digest. Git prompting and the ordinary `origin` push URL are
disabled in the worker environment, so an in-worker push cannot use the
coordinator's GitHub credentials.

Before starting Docker, the adapter applies a worker-specific group strategy to
every explicit bind mount. It preserves the owner bits, adds group
read/execute (and group write for writable mounts), removes other-user access,
and passes each mount entry's existing non-root host group with Docker's
`--group-add`. Writable directories/files therefore use group `rwx`/`rw`, while
read-only Git projections and read-only caches use group `rx`/`r`; Docker's
read-only mount flag prevents container writes. The fixed worker UID can access
the declared paths without changing ownership or relying on world permissions.

The worker's role home is a private Docker volume derived from the run and role.
It is not a host-home mount. When a Codex auth source is configured, the adapter
reads only the explicit `auth.json` file and streams it into a separate,
factory-managed credential volume. Codex sees a link to that copy from the role
home, while session files remain in the role volume. The host harness directory
is never mounted and the host source is never written back; fresh roles can
reuse credentials without inheriting another role's session context. Docker
seeds a fresh role volume from the worker image, so the role home starts with
the curated skill set the image installs and with nothing personal from the
host.

The coordinator prepares `/git` from the repository's objects, refs, and
worktree state. Git configuration, remote definitions, hooks, submodules, and
host-specific worktree indirection are omitted, so repository history and diff
operations remain available without exposing remote credentials.

The worker never commits, changes branches, pushes, or calls GitHub. The
host-side `GitWorkspace` validates the accepted worktree, creates the immutable
checkpoint, synchronizes a base branch when policy requires it, pushes the run
branch, and removes the worktree during cleanup.

After the Factory cleanup policy confirms that a terminal run is older than
seven days, the optional `CleanupRuntime` extension removes the run's private
worker container, role-home volumes, and the exact invocation packet/result
directories supplied for that run. It never removes the separate
factory-managed credential volume; the coordinator supplies only the selected
run roles, run identity, and validated output directories to this destructive
adapter operation.

Setup and the selected repository-declared gate run with `env -i` plus an
explicit worker baseline. Role-policy commands additionally receive the
coordinator-defined role identity; clean-policy commands do not. The gate
runner publishes one final Commit Status at the run's exact checkpoint SHA
under the stable context `factory/gate/<gate-name>`; command output is not used
to decide success.

The adapter buffers at most 8 MiB of standard output and 8 MiB of standard
error for every Docker invocation, including worker commands and lifecycle or
inspection calls. Headless inspection retains at most 512 KiB per stream and
preserves both the beginning and end; coordinator-side harness diagnostics are
bounded again to 16 KiB. A stream that writes past the command capture limit
returns a typed output-limit failure instead of a command result.
`docs/agent-runtime.md` records how a role observes that failure.

## Resource limits

Every worker container starts with a memory limit, a CPU limit, a PID limit,
and a bounded container log. The limits come from `worker_limits` in the host
configuration, never from the repository's `factory.yaml`.
`docs/configuration.md` lists the keys and their defaults (`8g` memory with
equal swap, 4 CPUs, 4096 PIDs, and a `json-file` log of 3 files of `10m`).
The adapter applies the defaults for every omitted value, so no worker starts
without a bound. It refuses an invalid limit before it calls Docker.

Workers also start with Docker's `--init` process as PID 1. The init process
reaps orphaned processes. Without it, orphans stay as zombies that hold PIDs,
and a worker that reached its PID limit once would refuse every later fork.

### Out-of-memory kills

The adapter detects an out-of-memory kill from the kernel, never from command
output. It reads the worker cgroup's `oom_kill` count (`memory.events` on
cgroup v2, `memory.oom_control` on cgroup v1) in its own `docker exec`, apart
from the command. No worker process can write that count, so command output
cannot forge an out-of-memory failure.

- **Commands.** `run-command` reads the count before the command starts. When
  the command fails and the count grew, the adapter returns a typed
  `OutOfMemoryError` instead of the exit code. A command that succeeds
  although one of its processes was killed keeps its successful result. The
  gate runner records an execution error, and the check-repair policy pauses
  the run as `check infrastructure unavailable` with a reason that names the
  out-of-memory kill. A focused red-test verification that fails this way
  also names the kill in its pause reason.
- **Detached harness processes.** `start-headless` records the count, as root,
  in `/run/factory-oom/<boot>/<invocation-id>` before the process starts. The
  worker user cannot create, change, or replace entries under `/run`, and the
  adapter trusts only root-owned entries. When `inspect-headless` sees a
  failed exit and the count grew past that baseline, it reports
  `OutOfMemory`. The harness adapter reports an unexpected exit that names
  the out-of-memory kill. The coordinator does not resume it automatically,
  because a resume would meet the same limit. It pauses the run as
  `harness out of memory (<harness>)`, and `/factory resume` continues the
  native session after the operator raises `worker_limits.memory`.

When the kernel exposes no `oom_kill` count, or the baseline could not be
recorded, an out-of-memory kill stays an ordinary failed exit.

A command that reaches the PID limit sees `fork` fail. The worker becomes
usable again when the processes that hold its PIDs end.

Known limits:

- The `oom_kill` count belongs to the whole worker cgroup. A failing command
  that ran while the memory limit killed another process in the same worker
  also reports the out-of-memory failure.
- Limits do not change on a reused worker. A worker created before the limits
  changed keeps its earlier limits until the coordinator recreates it.

## Command timeout and cancellation

The worker, not the host Docker CLI, owns the lifetime of every `run-command`
process. This applies to setup, gates, red-test verification, and headless
state commands alike. A fixed shell supervisor inside the worker starts the
command with `setsid` as the leader of a new process group. Before the command
runs, it atomically records that group under a private random command
identity. The records live in
`/tmp/factory-commands/<kernel-boot-id>-<pid-1-start-time>/`. The directory
name changes when the container or the Docker host restarts, so a process
group number from an earlier boot never names a later process. The
coordinator never sees the identity, the PID, or the path.

When the caller's deadline or cancellation ends a command first, the adapter
runs a second, fixed terminator in the same worker before it returns:

1. It publishes a cancellation marker for the command identity. A supervisor
   that has not yet recorded its group sees the marker and exits before the
   command runs.
2. It sends SIGTERM to the process group and waits a 5 second grace period.
3. It sends SIGKILL and waits a second grace period.
4. Zombies count as stopped: a worker created without the init process does
   not reap orphans, and a zombie cannot modify the checkout.

The adapter returns the original `context.DeadlineExceeded` or
`context.Canceled` only after the terminator confirms that no group member is
alive. A worker that is missing or stopped also counts as confirmed, because
stopping a container ends all of its processes. In all other cases the adapter
returns a typed `CommandTerminationError` and never a timeout result. These
cases are a group that survives SIGKILL, a failed terminator call, and a lost
response after the bounded terminator timeout. Repeated cancellation of the
same identity is idempotent.

After an unconfirmed termination, the terminator keeps the records. Every later
supervised command in that worker checks for a cancelled record whose group is
still alive. If one exists, the command does not start, and the adapter
returns the same typed error. So no independent gate, repair launch, or
headless state command can overlap the surviving process. Reconciliation is
to stop or recreate the worker. The next boot uses a new records directory.

The gate runner stops a suite at that error. It records the affected gate as
an execution error, even when the gate is advisory, and skips every later
gate. The check-repair policy classifies the error as infrastructure: the run
waits in `waiting_for_harness`, the coordinator stops the worker, and the run
keeps its repair attempts. The lifecycle reason names the discrepancy and the
recovery action. A `factory-baseline-target` marker never accepts the error,
so a baseline with an unconfirmed termination does not advance.

A timeout with a confirmed termination keeps its deterministic repair
classification. Exit codes, the capture limit, and other runtime failures also
keep their classification.

Known limits:

- A process that leaves its process group with its own `setsid` is outside
  this guarantee until the worker stops.
- The adapter terminates a command only when the caller's context ends. If the
  Docker CLI fails for a transport reason while the command runs, the adapter
  reports a runtime failure. The coordinator then stops the worker.
- A command that exits normally but leaves live background processes is not
  terminated.

## Contract tests

The contract tests use a controlled Docker executable. Live Docker and harness
checks remain environment checks and are not ordinary unit-test dependencies.
`scripts/verify-headless-worker.sh` runs the real command-lifetime and
resource-limit checks after the headless lifecycle checks, so `make
worker-build` and the CI worker verification job both run them. To run them
alone against the pinned worker, use:

```sh
FACTORY_DOCKER_WORKER_IMAGE=ghcr.io/stevie1704/sw-factory-worker@sha256:... \
  go test ./internal/worker -run TestRealWorker
```

Every harness publishes completion with `factory-report`, which atomically
writes a schema-versioned JSON report below `/results`; the coordinator
validates that report against the persisted invocation, current worktree,
permitted paths, and stage invariants. Machine events and captured output do
not publish workflow completion.

## Recovery lifecycle

The worker lifecycle is recoverable independently of workflow budget. A stopped
worker is resumed only when its frozen image and complete mount contract still
match the persisted invocation. If the worker is missing or its immutable
invocation mounts are stale, the adapter recreates it from the persisted
`image@digest` request. The bind-mounted worktree, named role volume, and
factory-managed credential volume survive that recreation; the invocation and
result directories are mounted again from their persisted paths.

Harness adapters use worker-owned process-state inspection. If the persisted
native session process exits after launch,
the coordinator records that interruption and applies its bounded resume policy
without using native output or model text as a correctness signal. An exited
process with a regular report is left for normal report acceptance; only a
missing report enters native-resume recovery.

When harness capacity is unavailable, the coordinator stops the worker and
records `waiting_for_harness`; the polling supervisor retries after capacity
returns. An expired credential stops the worker and waits for an explicit
`factory auth refresh`. `factory auth refresh --resume` combines the host
credential projection with one manual continuation; the default refresh still
only projects credentials. The combined form reconciles pending lifecycle
effects before continuation and still retains the credential projection when
its native-session precondition is rejected. A manual `factory resume` restarts the exact
persisted native session without changing the workflow retry budget. No worker
recreation or harness-capacity wait changes that budget either.
