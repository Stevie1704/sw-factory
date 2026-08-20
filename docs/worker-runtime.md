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

The Docker adapter uses these stable paths regardless of the host checkout:

| Worker path | Access | Contents |
| --- | --- | --- |
| `/work` | read-write | The run worktree |
| `/git` | read-only | A credential-free projection of Git metadata needed for history and diffs |
| `/cache/<name>` | declared per cache | A repository cache |

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

The coordinator prepares `/git` from the repository's objects, refs, and
worktree state. Git configuration, remote definitions, hooks, submodules, and
host-specific worktree indirection are omitted, so repository history and diff
operations remain available without exposing remote credentials.

Setup and the selected repository-declared gate run with `env -i` plus an
explicit worker baseline. Role-policy commands additionally receive the
coordinator-defined role identity; clean-policy commands do not. The gate
runner publishes one final Commit Status at the run's exact checkpoint SHA
under the stable context `factory/gate/<gate-name>`; command output is not used
to decide success.

The contract tests use a controlled Docker executable. Live Docker, harness,
and terminal checks remain environment checks and are not ordinary unit-test
dependencies.
