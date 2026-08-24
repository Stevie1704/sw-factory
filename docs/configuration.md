# Configuration and local operation

Issues #3 and #4 establish two configuration documents and one local operational store.

The host configuration is created with `factory init`. Its path is selected by `--config`, then `FACTORY_CONFIG`, then the operating system's user configuration directory at `factory/config.yaml`. The repository registration is intentionally limited to one repository in version one.

```sh
factory init --config /Users/me/.config/factory/config.yaml
factory register \
  --config /Users/me/.config/factory/config.yaml \
  --repository /Users/me/src/project \
  --github-owner example \
  --github-repository project \
  --authorized-user alice \
  --operational-data /Users/me/.local/share/factory/factory.db
factory status --config /Users/me/.config/factory/config.yaml
```

`factory register` creates the SQLite store before it writes the registration. It does not contact GitHub, create labels, or write into the registered repository.

## Host configuration

The generated host file contains the repository path, GitHub identity, authorized maintainers, polling settings, cmux settings, the checked-in repository configuration path, and the operational-data path.

```yaml
schema_version: 1
repositories:
  - path: /Users/me/src/project
    github:
      owner: example
      repository: project
    authorized_users:
      - alice
    polling:
      interval: 30s
      backoff: 5m
    cmux:
      socket_path: ''
      control_workspace: factory-control
    authentication:
      codex_auth_path: /Users/me/.codex/auth.json
    operational_data_path: /Users/me/.local/share/factory/factory.db
    repository_config_path: /Users/me/src/project/factory.yaml
```

All paths persisted in a repository registration are absolute. The coordinator does not infer macOS-specific paths in its domain or deep modules; only the command's default host-config resolver uses the host operating system's standard user configuration directory.

`cmux.socket_path` is optional and is passed to the cmux adapter as its
connection endpoint; the coordinator still keeps cmux identifiers behind the
`TerminalRuntime` seam. `authentication.codex_auth_path` is optional and names one host-side Codex
`auth.json` file. The factory stores only this path. During an implementation
invocation the worker adapter streams the file into a separate,
factory-managed credential volume and links that copy into the role home; it
never mounts the host harness directory or writes back to the host source.

## Checked-in repository configuration

The default checked-in file is `factory.yaml` at the repository root. It is parsed with strict field checking and validated before a run can be claimed. Unknown schema versions fail closed.

```yaml
schema_version: 1
target_branch: main
setup: scripts/worker-go.sh mod download
setup_files: [go.mod, go.sum]
setup_environment_policy: clean
gates:
  - name: format
    command: gofmt -l .
    timeout: 30s
    blocking: true
    environment_policy: clean
  - name: test
    command: scripts/worker-go.sh test ./...
    timeout: 2m
    blocking: true
    depends_on: [format]
    environment_policy: clean
role_harness_defaults:
  test: codex
  implementation: codex
model_options:
  test: [gpt-5]
  implementation: [gpt-5]
timeouts:
  setup: 5m
  agent: 30m
  gate: 5m
  review: 10m
retry_limits:
  check_repair: 3
  review_repair: 2
  test_revision: 2
test_policy:
  mode: required
  allow_human_exemption: true
  allow_technical_exemption: true
allowed_overrides: [model, reasoning_effort]
caches:
  - name: go-build
    path: /tmp/factory-cache
    read_only: false
worker_build:
  image: ghcr.io/stevie1704/sw-factory-worker
  digest: sha256:db586fccdc3c75fcb083a3ff0fc63c700008b0b1eb919e11e67592919ed3ccb5
  definition: worker/Dockerfile
base_synchronization:
  mode: before_ready
  branch: main
```

The validator checks the schema version, target branch, setup, optional repository-relative `setup_files`, setup environment policy, ordered unique gates and earlier dependencies, matching role harness/model policies, positive durations, positive retry limits, test policy, supported unique overrides (`model`, `reasoning_effort`, or `harness`), caches, worker image, and base-synchronization mode. `setup_files` names the checked-in manifests and lockfiles whose contents identify the dependency graph; an empty list is valid. An empty `allowed_overrides` list is valid and means that issue-level overrides are disabled. Validation errors are typed and identify the offending field, including `schema_version` for an unsupported newer schema.

## Worker image build and digest pinning

This repository owns a two-layer worker image definition:

- `worker/base.Dockerfile` defines the versioned factory base image. It pins
  the Codex and Claude Code npm packages through `CODEX_VERSION` and
  `CLAUDE_VERSION` build arguments, installs Git and the basic worker
  utilities, and builds the repository's `factory-report` binary into
  `/usr/local/bin/factory-report`.
- `worker/Dockerfile` extends that base with the Go toolchain required by this
  repository's setup and gates. Go and `gofmt` are exposed through
  `/usr/local/bin`, because the worker adapter deliberately supplies the fixed
  non-login `PATH` `/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`.

Build and verify both images with Docker from the repository root:

```sh
make worker-build
```

The command builds the versioned base image locally, builds the repository
worker without pulling that base again, verifies the worker as uid `10001`
with `HOME=/home/factory`, checks the stable worker paths and required tools,
and verifies that the digest-qualified reference resolves locally with
`--pull=never`. The digest is Docker's local content-addressable image ID, so
the configured image must be built locally under the same image name; this is
deliberate because the worker runtime never pulls. A registry publish workflow
should replace it with the registry's manifest digest. The command prints the
exact `worker_build` block to copy into `factory.yaml`. The defaults pin Codex
`0.148.0`, Claude Code `2.1.232`, and
Go `1.25.0`; override those build arguments explicitly when producing a new
versioned image. Set `WORKER_PLATFORM=linux/amd64` (or another target) when
building for a platform different from the Docker daemon.

After the image smoke checks, the same command mounts this checkout and runs
the configured setup, format, vet, test, and build gates under the worker's
clean baseline environment.

The checked-in `worker_build.image` is intentionally an image name without a
mutable tag. The reported SHA-256 digest is appended by the worker adapter as
`image@digest`, so a run never starts from a mutable tag and never pulls an
image implicitly.

The Go setup and gates in this repository invoke `scripts/worker-go.sh`. The
wrapper removes only the coordinator's Git routing and disabled-origin
configuration variables, keeps Git prompts disabled, and turns off Go VCS
stamping; this lets local Git fixtures in `go test` run without touching the
worker's read-only `/git` projection.

Issue #3 establishes and validates this repository-declared gate contract. Issue #5 adds the worker runtime and the coordinator path that runs setup plus one selected gate in the pinned worker and publishes its result to the exact checkpoint SHA. Issue #7 uses that same frozen gate contract to run every declared gate before the first branch push. The issue #9 gate model runs one baseline suite before the first agent invocation, runs setup once per suite, continues independent gates after a failure, records dependency skips, reruns setup for every new dependency-input fingerprint, and retains results by phase and exact checkpoint SHA. The commands declared by this repository's `factory.yaml` are also run as part of the repository verification suite.

An issue may explicitly accept a known pre-existing blocking baseline failure by
including one exact marker in its frozen body, for example
`<!-- factory-baseline-target: test -->`. The special `all` target accepts all
blocking baseline failures, and `setup` targets a setup failure. Without a
matching marker, the baseline moves the run to failed preflight and no agent
invocation is permitted.

Worker execution uses stable in-container paths (`/work`, `/git`, and
`/cache/<name>`), a non-root uid, dropped capabilities, disabled privilege
escalation, and no Docker socket. The coordinator prepares `/git` as a
credential-free projection of Git history, refs, and run worktree state while
omitting Git configuration, remotes, and hooks. Setup and gates receive their
configured clean or role environment explicitly; they do not inherit the coordinator's host environment
or credentials. Explicit worker mounts receive owner-plus-group permissions,
and the runtime passes their existing non-root host groups to Docker as
supplemental groups; other-user access is removed. See [Worker runtime](worker-runtime.md)
for the runtime seam and its isolation contract.

## Claiming an issue

Factory-owned GitHub labels are created only by the explicit bootstrap command. Run it after registration and before claiming the first issue:

```sh
factory bootstrap-labels --config /Users/me/.config/factory/config.yaml
```

The command is idempotent and manages exactly these six labels: `agent-ready`, `agent-running`, `agent-needs-input`, `agent-failed`, `agent-cancelled`, and `agent-complete`. Claiming an issue never creates labels implicitly. `agent-ready` is the product trigger label; the tracker label `ready-for-agent` is unrelated.

The one-shot claim command accepts either a positional issue number or `--issue`:

```sh
factory issue --config /Users/me/.config/factory/config.yaml 42
```

It refuses closed issues, issues without `agent-ready`, and a repository that already has an active run. Before changing GitHub, it freezes the issue snapshot and the resolved `factory.yaml` as specification packet version one in the operational store. The packet contains no GitHub credentials.

The coordinator then fetches `origin/<target_branch>`, records that fetched commit SHA, and creates the mutable run branch `factory/<run-id>` from that commit, plus a worktree at the sibling path `.factory-worktrees/<repository-name>/<run-id>`. The ordinary checkout is not checked out onto the run branch. The issue is changed to exactly one factory state label (`agent-running`) while preserving ordinary labels, and one editable status comment records the run identifier, branch, worktree, coordinator, start time, checkpoint, stage, and status. Later coordinator transitions edit that comment by its persisted comment identity; if persistence was interrupted after GitHub created it, the run marker recovers that existing comment rather than creating another. Stage and status remain separate values. The operational store rejects a second non-terminal run for the same repository through its uniqueness constraint. If a claim fails after creating its workspace, the coordinator removes the created run branch and worktree.

The GitHub adapter invokes the locally authenticated `gh` CLI. The coordinator receives issue and mutation results in memory; GitHub credentials are not read into or persisted by the factory. `factory status` reports the active run's stage, status, branch, and worktree, or the latest terminal run when no run is active.

Before any later coordinator command can progress a persisted non-terminal run,
the new process performs a read-only recovery diagnosis. It compares the run
identifier, registered repository, worktree, branch, checkpoint SHA, issue
number and factory state label, marked status-comment identity, and persisted
pull-request identity when present. A missing or mismatched projection is
reported alongside every other discovered discrepancy. Even when all sources
agree, the command returns the typed `recovery-required` result and refuses to
resume: this version performs no Git, GitHub, worker, terminal, harness, or
workflow-state mutation and consumes no workflow budget. `factory status`
remains available and prints the run, agreement state, discrepancies, and safe
operator actions. Issue #21 explicitly supersedes this boundary with complete
reconciliation and idempotent effect recovery.

## Authorized GitHub commands

The coordinator recognizes a command only when the complete trimmed comment is
one structured `/factory` command. Ordinary discussion is ignored, so a casual
mention of “retry” or “refresh” cannot control a run:

```text
/factory status
/factory refresh
/factory retry
/factory cancel
/factory config harness=codex
```

Run one comment poll with `factory poll --config /Users/me/.config/factory/config.yaml`;
repeating the command is safe because the persisted watermark filters old
comments.

The author must be present in the registered `authorized_users` list. A status
or refresh command re-renders the existing supervision comment; retry reopens a
failed or explicitly cancelled run at its current stage; cancel stops active
worker activity while retaining the run artifacts; and harness configuration
records a later invocation override only when the frozen repository
configuration permits it. A recognized command from an unauthorized author or
a malformed command is a
typed policy rejection. The coordinator leaves workflow stage, status, labels,
and configuration unchanged for that rejection, while recording the rejection
in the same editable status comment.

The command grammar also names `claude` for forward-compatible parsing, but the
current implementation role has only the Codex adapter; selecting Claude is a
typed `harness_unavailable` rejection until its later adapter is delivered.

Each comment is processed at most once. The operational store persists a
monotonic run revision, the processed comment ID watermark, and the revision at
which that watermark was written. Polling therefore ignores old comments, and
editing an already processed comment cannot turn it into a new command after a
coordinator restart. Command handling serializes one service's work and uses a
SQLite revision compare-and-set, durably claiming the comment before applying
GitHub effects. The same parser and handler are used for issue comments and
pull-request comments; later commands can extend the registered verb set
without duplicating polling or replay logic.

The poll command also observes the tracked pull request and issue lifecycle.
A merged pull request completes the run and records its merge commit; closing
the issue or an unmerged pull request cancels it. Merge detection takes
precedence over the pull request's closed state. Terminal transitions replace
the factory state label, edit the existing status comment, notify cmux, stop
the worker without deleting retained state, and leave the branch and worktree
available for cleanup or an explicit retry.

After a claim, `factory agent` starts the visible Codex implementation role and
prints the run, invocation, workspace, and surface handles. The role receives a
read-only invocation packet and reports through `factory-report`; use
`factory agent-report --invocation-id <id>` to ask the coordinator to validate
and accept the structured report. Terminal output is never treated as a stage
result. The operational store schema is version 10 and persists invocation
identity, opaque surface handles, prompt version, result directory, native
session identifier, and permitted handoff paths in addition to run state. It
also persists the draft pull-request number and URL so a restarted command can
update the existing pull request instead of creating another one. Terminal runs
retain merge commit, lifecycle reason, and terminal notification-delivery
metadata for status rendering and restart-safe cmux notification retries.

## Creating the draft pull request

After the implementation report has been accepted, advance the run through
the host-owned checkpoint, deterministic gates, branch push, and draft PR:

```sh
factory draft-pr \
  --config /Users/me/.config/factory/config.yaml \
  --run-id run-123
```

The coordinator validates the worktree against the stored checkpoint, creates
one commit marked `factory: implementation checkpoint <run-id>`, and records
its full SHA before running every gate from the frozen specification packet.
Only when all gates pass does the host push `factory/<run-id>` and call
GitHub's draft pull-request API. Workers never receive Git remotes, GitHub
credentials, commit authority, or push authority.

The PR body contains one coordinator-owned section between
`<!-- factory-generated:start -->` and `<!-- factory-generated:end -->`.
That section includes the issue and specification summary, checkpoint, stage,
gate results, intervention marker, and control commands. Repeating
`factory draft-pr` finds the existing PR by its exact source and target branch,
regenerates only that marked section, and preserves human-authored text around
it. The issue's single factory state label and editable status comment move to
the `draft_pr` stage at the same transition.

## End-to-end demonstration

The tracer-bullet acceptance path is an operator-run check against a disposable
repository and issue. Do not use a production repository: the path creates a
factory branch, changes the issue's factory label and status comment, pushes
the branch, and opens a draft pull request.

Before starting, prepare a dedicated GitHub repository with a checked-in
`factory.yaml`, a fresh open issue carrying `agent-ready`, valid `gh` login,
Docker with the configured worker image available, a running cmux session, and
the host Codex `auth.json` path registered in the host configuration. Build the
three local commands from this checkout (`factory`, `factory-report`, and
`factory-worker-attach`) so the worker image can invoke the pinned report
command.

Run the path in this order:

```sh
factory bootstrap-labels --config /Users/me/.config/factory/config.yaml
factory issue --config /Users/me/.config/factory/config.yaml <issue-number>
factory agent --config /Users/me/.config/factory/config.yaml --run-id <run-id>

# In the visible implementation surface, make the requested small change and
# submit its structured report with factory-report.
factory agent-report \
  --config /Users/me/.config/factory/config.yaml \
  --run-id <run-id> \
  --invocation-id <invocation-id>
factory draft-pr \
  --config /Users/me/.config/factory/config.yaml \
  --run-id <run-id>
```

Record the command output and verify the demonstration at each boundary:

- the issue has exactly one factory state label and one editable status comment;
- the worker reaches the visible Codex session, while its container has no Git
  remote or GitHub credential access;
- the run worktree contains one `factory: implementation checkpoint <run-id>`
  commit, and `git ls-remote` shows the pushed `factory/<run-id>` branch only
  after every configured gate passes;
- the GitHub pull request is draft, targets the configured base branch, and
  contains the generated factory markers and gate summary;
- repeating `factory draft-pr` updates that same pull request and retains text
  outside the generated markers; and
- `factory status` and the issue's status comment report `draft_pr`.

Keep the resulting pull-request URL and the status-comment URL as the
demonstration evidence. Close the disposable pull request and issue, then
remove the run's local worktree and operational database after inspection.

## Operational SQLite store

The operational store contains current workflow state, the active run's frozen specification packet, the status-comment identity needed by later transitions, and the draft pull-request identity needed for idempotent regeneration. Registration and status both reject paths that resolve inside the repository checkout, including symlink aliases; its directory is private (`0700`) and the SQLite file is private (`0600`). A fresh store is initialized directly; an older supported schema is copied to a timestamped `.bak-*` file before its explicit migration runs. Migration backups are not pruned automatically in this foundation; issue #23 owns the visible cleanup and retention policy. A newer or unversioned database refuses to open. There is no silent guessing or destructive migration. GitHub credentials are never columns in this store.

Issue #25 will add separate content-free local evaluation summaries. Those summaries remain local and are not outbound telemetry.

The high-level `Factory` seam injects configuration, repository checking, GitHub, pull requests, `GitWorkspace`, worker, terminal, harness, clock, run-identity, and operational-store adapters. Foundation tests use a real temporary SQLite store and fake the external repository/configuration boundary; issue #4 adds focused fake-adapter tests for the claim seam and a real temporary Git repository test for worktree isolation. Issue #5 owns the `WorkerRuntime` adapter and coordinator worker ownership; issue #6 adds the portable `TerminalRuntime`, Codex harness, invocation packet, and structured report boundary; issue #7 adds host checkpoint, push, and draft-PR orchestration.
