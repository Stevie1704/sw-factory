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

Before claiming an issue, run the complete startup diagnosis:

```sh
factory doctor --config /Users/me/.config/factory/config.yaml
```

The doctor reports configuration, GitHub authentication and permissions, the
factory labels, the checkout's remote/hooks/worktree support, cmux, Docker,
the pinned worker image, both supported harness executables, interactive-resume
capabilities, harness authentication sources, and SQLite. It runs every
contributor even after a failure and returns a nonzero exit status when any
blocking prerequisite remains. Each failure includes a bounded problem and a
corrective action; command output and credential contents are never rendered.
The SQLite check opens the existing store read-only; it does not create,
migrate, back up, chmod, or initialize store state.
Missing optional host credential files are warnings because a harness may be
authenticated during its first visible worker session.

Start the persistent coordinator after diagnosis is ready:

```sh
factory start --config /Users/me/.config/factory/config.yaml
factory stop --config /Users/me/.config/factory/config.yaml
```

`factory start` runs the complete startup diagnosis before taking a private
host lock. It polls immediately and then at the configured interval, claims
only the oldest open issue carrying `agent-ready`, and skips queue claims while
any non-terminal run exists. GitHub transport failures use the configured
backoff and do not change workflow state or retry budgets. The coordinator
publishes a renewable `factory/lease` Commit Status on the target branch; its
description includes the coordinator, active run, heartbeat, and expiry so a
stale owner remains diagnosable in GitHub. `factory stop` signals the locked
coordinator and leaves any active run, branch, worktree, worker, and session
artifacts in place. Polling never creates factory labels; use
`factory bootstrap-labels` explicitly.

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
      claude_auth_path: /Users/me/.claude/.credentials.json
    operational_data_path: /Users/me/.local/share/factory/factory.db
    repository_config_path: /Users/me/src/project/factory.yaml
```

All paths persisted in a repository registration are absolute. The coordinator does not infer macOS-specific paths in its domain or deep modules; only the command's default host-config resolver uses the host operating system's standard user configuration directory.

`cmux.socket_path` is optional and is passed to the cmux adapter as its
connection endpoint; the coordinator still keeps cmux identifiers behind the
`TerminalRuntime` seam.

`authentication.codex_auth_path` and `authentication.claude_auth_path` are both
optional and each names one host-side harness credential file. The factory
stores only these paths. When an invocation selects a harness that has a
registered source, the worker adapter streams that one file into a separate,
factory-managed credential volume and links the copy into the role home. It
never mounts the host harness directory and never writes back to the host
source. Each harness has its own source because a host can hold one harness
credential as a file without holding the other: macOS keeps the Claude Code
credential in the login Keychain, so `claude_auth_path` is declared only where
a credential file exists. A harness with no registered source keeps the
credential the worker itself persisted in its role volume.

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
  architecture: codex
  spec_review: codex
  standards_review: codex
model_options:
  test: [gpt-5.6-luna]
  implementation: [gpt-5.6-luna]
  architecture: [gpt-5.6-luna]
  spec_review: [gpt-5.6-luna]
  standards_review: [gpt-5.6-luna]
# Optional, and harness-specific: these are Codex effort names because the
# test role runs on Codex. A role that declares no values accepts no
# reasoning-effort selection at all.
reasoning_effort_options:
  test: [medium, high]
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
  # Optional prefixes for essential test infrastructure.
  test_paths: []
  infrastructure_paths: []
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
evaluation:
  retention: 720h
```

The validator checks the schema version, target branch, setup, optional repository-relative `setup_files`, setup environment policy, ordered unique gates and earlier dependencies, matching role harness/model policies (including the mandatory `test` role), optional `reasoning_effort_options` for declared roles, positive durations, positive retry limits, test policy, supported test-role prefixes, supported unique overrides (`model`, `reasoning_effort`, or `harness`), caches, worker image, base-synchronization mode, and optional positive `evaluation.retention`. `setup_files` names the checked-in manifests and lockfiles whose contents identify the dependency graph; an empty list is valid. `test_policy.test_paths` and `test_policy.infrastructure_paths` authorize additional repository-relative paths for the test role; conventional `*_test.go`, `test/`, `tests/`, `test-support/`, and `__tests__/` paths are allowed by default. An empty `allowed_overrides` list is valid and means that issue-level overrides are disabled. Validation errors are typed and identify the offending field, including `schema_version` for an unsupported newer schema.

## Per-role harness, model, and reasoning effort

`role_harness_defaults`, `model_options`, and `reasoning_effort_options` are
selected independently for each role, so the best harness can be used for each
stage. The supported harness values are `codex` and `claude`.

A request selects one declared option per setting. When it selects nothing, the
role uses its declared harness, its first declared model, and its first
declared reasoning effort. A selection outside the declared options needs the
matching `allowed_overrides` entry; without it the coordinator refuses the
launch as a typed policy rejection with the code `harness_override`,
`model_override`, or `reasoning_effort_override`.

Declared values must match what the role's harness accepts. Codex takes its own
effort names through `model_reasoning_effort`; Claude Code takes `low`,
`medium`, `high`, `xhigh`, or `max` through `--effort`. `ValidateRepository`
rejects Claude Code `reasoning_effort_options` with unsupported values before
launch, so an invalid declaration never reaches the harness. Declare
`reasoning_effort_options` for every role, and keep `reasoning_effort` out of
`allowed_overrides` unless an operator is meant to bypass that check.

`model_options` and `reasoning_effort_options` are declared per role, not per
harness. A repository that adds `harness` to `allowed_overrides` therefore accepts
responsibility for declaring model and effort options that every permitted
harness accepts; otherwise an authorized override can pair one harness with
another harness's option names.

## Factory-owned role, prompt, and stage registry

Roles, invocation stages, prompt versions, default permitted paths, visible
surface ownership, and report-outcome transitions are declared by the factory
in `internal/workflow`. Repository configuration may select harness and model
policy for a declared role, but `factory.yaml` cannot add or redefine
`roles`, `stages`, `prompts`, or `transitions`. Such fields are rejected as
typed repository-policy errors.

The optional architecture role is launched explicitly with the
factory-declared architecture stage. Its default permitted path is
`docs/architecture`, and its prompt requires a concise design document plus a
normal structured handoff. The role gets a fresh role-owned visible surface;
it does not reuse the implementation surface. Its accepted completed handoff
returns the run to the implementation stage.

An authorized maintainer selects a harness for a later invocation with one
structured comment:

```text
/factory config harness=claude
```

The comment grammar accepts nothing else. A recognized command with an extra
word, a flag, or an unknown key is refused as a typed malformed-command
rejection, so a comment cannot inject a process argument into a launched
harness. The recorded choice is still validated against the frozen repository
policy when the next invocation starts.

The test stage is default-on for both supported `test_policy.mode` values.
A human skip requires `allow_human_exemption: true` and the frozen issue marker
`<!-- factory-test-exemption: human | justification -->`. The test checkpoint
and implementation checkpoint are separate commits; the operational store
retains the test handoff and protected-path hashes between them.

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

When no effect is pending, the lifecycle and supervisor entry points first
observe a tracked issue or pull request, so an already-merged or closed target
can enter its terminal state even when GitHub has deleted the run branch or
only historical invocation infrastructure remains. Before an unchanged
non-terminal run or any other coordinator command can progress, the new process
reconciles the durable effect journal and compares the run identifier,
registered repository, worktree, branch, checkpoint SHA, issue number and
factory state label, marked status-comment identity, and persisted pull-request
identity when present. A missing or mismatched projection is reported alongside
every other discovered discrepancy and pauses the run for human disposition. A
remote run branch whose head is an ancestor of the
persisted checkpoint is not a discrepancy: a checkpoint is committed before the
gate suite runs and pushed only afterward, so an unpushed commit is an
ordinary in-flight state rather than a diverged branch. A journaled effect is
replayed only when its exact intent can be recognized or completed
idempotently; otherwise the run remains waiting with the pending effect visible
to `factory status`. A retried attempt refreshes the payload of its own
reservation, because the effect identity rather than the payload is the
reservation. `factory status` reports
the run, agreement state, pending effect, discrepancies, and safe operator
actions. After inspecting an ambiguous mutation, an operator can explicitly
discard it with `factory reconcile --abandon-effect <effect-id> --reason <reason>`;
the run remains paused. Legacy stores without the journal retain the typed
`recovery-required` refusal.

## Authorized GitHub commands

The coordinator recognizes a command only when the complete trimmed comment is
one structured `/factory` command. Ordinary discussion is ignored, so a casual
mention of “retry” or “refresh” cannot control a run:

```text
/factory status
/factory refresh
/factory answer clarification-1 use the existing JSON format
/factory retry
/factory cancel
/factory config harness=codex
```

Run one comment poll with `factory poll --config /Users/me/.config/factory/config.yaml`;
repeating the command is safe because the persisted watermark filters old
comments.

The author must be present in the registered `authorized_users` list. A status
or refresh command re-renders the existing supervision comment; an answer
command is accepted only from an authorized user while the referenced question
is pending; retry reopens a failed or explicitly cancelled run at its current
stage; cancel stops active worker activity while retaining the run artifacts;
and harness configuration records a later invocation override only when the
frozen repository configuration permits it. A recognized command from an
unauthorized author or a malformed command is a
typed policy rejection. The coordinator leaves workflow stage, status, labels,
and configuration unchanged for that rejection, while recording the rejection
in the same editable status comment.

Answer commands normally use `/factory answer <question-id> <answer>`; the
identifier may also be written as `question=`, `question-id=`, or `id=` for
automation clients, and an answer may begin with `answer=`.

The command grammar and current implementation support both the Codex and Claude
adapters. A selected harness is still validated against the frozen role policy
and the startup capability check before an issue can be claimed.

Each comment is processed at most once. The operational store persists a
monotonic run revision, the processed comment ID watermark, and the revision at
which that watermark was written. Polling therefore ignores old comments, and
editing an already processed comment cannot turn it into a new command after a
coordinator restart. Command handling serializes one service's work and uses a
SQLite revision compare-and-set, durably claiming the comment before applying
GitHub effects. The same parser and handler are used for issue comments and
pull-request comments; later commands can extend the registered verb set
without duplicating polling or replay logic.

When an implementation report requests clarification, the coordinator pauses
the run with `agent-needs-input`, renders pending question IDs and prompts in
the editable status comment, posts the questions on the issue (or tracked pull
request), and notifies cmux. The question comment carries a marker scoped to
the run and its specification packet version, so an interrupted publication
repairs that round's comment while an answered round's questions remain
readable. A command such as
`/factory answer clarification-1 use the existing JSON format` records the
authorized answer in a new specification-packet version and resumes a fresh
implementation invocation with that packet. Clarification pauses do not
consume retry budget. `/factory refresh` re-reads the issue into another packet
version, preserves resolved answers, invalidates superseded downstream
invocations and checkpoint results, and resumes implementation against the new
snapshot.

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
result. The operational store schema is version 24 and persists invocation
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
demonstration evidence. Close the disposable pull request and issue, then use
`factory cleanup` after inspection to remove the eligible local run artifacts.

## Operational SQLite store

The operational store contains current workflow state, the active run's frozen specification packet, the status-comment identity needed by later transitions, and the draft pull-request identity needed for idempotent regeneration. Registration and status both reject paths that resolve inside the repository checkout, including symlink aliases; its directory is private (`0700`) and the SQLite file is private (`0600`). A fresh store is initialized directly; an older supported schema is copied to a timestamped `.bak-*` file before its explicit migration runs. Migration backups are not pruned automatically in this foundation; issue #23 owns the visible cleanup and retention policy. A newer or unversioned database refuses to open. There is no silent guessing or destructive migration. GitHub credentials are never columns in this store.

Issue #25 adds a logically separate `evaluation_summaries` projection inside
the same versioned SQLite store, plus isolated usage and disposition tables. It stores
only bounded metadata, counts, durations, fixed categories, hashed human-event
identities, and reliable harness-reported numeric usage. It never copies issue
or specification text, prompts, transcripts, diffs, source contents, command
output, logs, or credentials. Existing stores migrate with the normal private
`.bak-*` backup and newer schemas still fail closed.

The checked-in `evaluation.retention` duration is an explicit operator policy
for choosing a cutoff; the factory never deletes summaries automatically. Use
`factory evaluation` for a local report, `factory evaluation-disposition` to
attach a human classification, and the separately confirmed
`factory evaluation-delete --before <RFC3339> --confirm` command to remove only
selected terminal summaries. Ordinary run-artifact cleanup does not touch this
projection.

## Run-artifact cleanup

Ordinary run data becomes eligible seven days after a run enters its current
terminal state: merged, closed, failed, or cancelled. The coordinator retains
the current baseline and latest output projections while a run is active, and
keeps worktrees and implementation session state while its pull request is
open. It does not select active or waiting runs for cleanup.

`factory cleanup` first prints every exact local worktree, local factory branch,
logical worker target, and generated stored-output directory selected for
removal. For a run with a tracked pull request it also reads the current GitHub
lifecycle and keeps the run when that pull request is still open. It requires
an explicit `--confirm` flag; the command never deletes a remote branch. A
pending external-effect reservation, a malformed run identity, or a path that
cannot be proven run-scoped blocks that run from cleanup. The second
confirmation pass reuses the displayed cutoff and refuses a changed plan, so
the printed targets are the targets being authorized.

```sh
factory cleanup --config /Users/me/.config/factory/config.yaml
factory cleanup --config /Users/me/.config/factory/config.yaml --confirm
factory cleanup --config /Users/me/.config/factory/config.yaml --run-id <run-id> --confirm
```

Cleanup removes operational run rows, invocation history, deterministic gate
results, worker containers, run-scoped role-home volumes, generated invocation
packets/results, and Git metadata projections through the WorkerRuntime and
GitWorkspace adapters. Factory-managed credential volumes and local evaluation
summaries are retained; `factory evaluation-delete` remains the only summary
deletion command.

The high-level `Factory` seam injects configuration, repository checking, GitHub, pull requests, `GitWorkspace`, worker, terminal, harness, clock, run-identity, and operational-store adapters. Foundation tests use a real temporary SQLite store and fake the external repository/configuration boundary; issue #4 adds focused fake-adapter tests for the claim seam and a real temporary Git repository test for worktree isolation. Issue #5 owns the `WorkerRuntime` adapter and coordinator worker ownership; issue #6 adds the portable `TerminalRuntime`, Codex harness, invocation packet, and structured report boundary; issue #7 adds host checkpoint, push, and draft-PR orchestration.
