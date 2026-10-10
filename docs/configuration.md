# Configuration and local operation

Issues #3 and #4 establish two configuration documents and one local operational store.

The host configuration is created with `factory init`. Its path is selected by `--config`, then `FACTORY_CONFIG`, then the operating system's user configuration directory at `factory/config.yaml`. The repository registration is intentionally limited to one repository in version one.

## Quick start

`factory register` infers the values it can from the checkout you run it in.
Inside a Git checkout whose `origin` remote points at GitHub, with the GitHub
CLI authenticated, the complete first-run sequence is:

```sh
factory init
factory register \
  --codex-auth /Users/me/.codex/auth.json \
  --claude-auth /Users/me/.claude/.credentials.json
factory bootstrap-labels
factory doctor
factory start
```

`factory init` is still required. Registration never creates the host
configuration: it fails with an instruction to run `factory init` when the
configuration is missing, so host state is only ever created by an explicit
command.

Registration does not provision cache paths. When the checked-in repository
configuration declares any cache, add `cache_root` and the `caches` mapping to
the registration by hand before `factory doctor`. Until that edit lands, the
`caches` check blocks startup with the name it cannot resolve.

### Inferred registration values

`factory register` reports every value it inferred as an `inferred --<flag>`
line before the registration summary. It infers only these values:

| Flag | Inferred from | Limitation |
| --- | --- | --- |
| `--repository` | `git rev-parse --show-toplevel` in the current directory | Fails when the directory is not inside a Git checkout |
| `--github-owner`, `--github-repository` | the `origin` remote's fetch and push URLs | Only the `origin` remote, only `github.com`, and only when the fetch and push URLs name the same repository |
| `--authorized-user` | the login of the authenticated `gh` account | One user; repeat `--authorized-user` to register more |

Inference is read-only and fails closed. It never changes a Git reference or
remote, never reads or copies a credential file, and never prints command
output. When a value cannot be resolved safely, registration reports the
problem and the flag that overrides it, and writes neither the registration nor
the operational store.

The remaining registration values keep their existing defaults: the host
configuration path, a host-local operational data path, `factory.yaml` in the
checkout, and the polling interval and backoff. The authentication options are
never inferred: `--codex-auth` and `--claude-auth` stay explicit. Omit either
option when that harness has no host-side credential file, such as a Claude
Code credential kept in the macOS login Keychain.

### Explicit fallback flags

Every registration flag remains an override, and each one replaces exactly the
value it names. Supply `--github-owner` alone, for example, and the repository
name and authorized user are still inferred. Use the full option set when the
checkout, remote, or account does not match the registration you want:

```sh
factory init --config /Users/me/.config/factory/config.yaml
factory register \
  --config /Users/me/.config/factory/config.yaml \
  --repository /Users/me/src/project \
  --github-owner example \
  --github-repository project \
  --authorized-user alice \
  --codex-auth /Users/me/.codex/auth.json \
  --claude-auth /Users/me/.claude/.credentials.json \
  --operational-data /Users/me/.local/share/factory/factory.db
factory status --config /Users/me/.config/factory/config.yaml
```

If a repository is already registered, keep create-only behavior by default and
use `--update` to change its credential sources. The requested checkout must
match the registered repository; omitted credential options keep their existing
sources:

```sh
factory register \
  --config /Users/me/.config/factory/config.yaml \
  --update \
  --repository /Users/me/src/project \
  --codex-auth /Users/me/.codex/auth.json \
  --claude-auth /Users/me/.claude/.credentials.json
```

`factory register` creates the SQLite store before it writes a new
registration. An update changes only the supplied authentication paths and
does not recreate the store. Apart from the read-only account lookup used to
infer `--authorized-user`, registration does not contact GitHub, create labels,
or write into the registered repository.

Before claiming an issue, run the complete startup diagnosis:

```sh
factory doctor --config /Users/me/.config/factory/config.yaml
```

The doctor reports configuration, GitHub authentication and permissions, the
factory labels, the checkout's Git checks (remote, hook suppression, worktree support), Docker, the
pinned worker image, both supported harness executables, harness capabilities,
the headless worker helper, harness authentication sources, and SQLite. It runs every
contributor even after a failure and returns a nonzero exit status when any
blocking prerequisite remains. Each failure includes a bounded problem and a
corrective action; command output and credential contents are never rendered.
When the checked-in repository configuration declares `role_craft`, the
doctor also checks every declared role-craft file independently against the
configured target branch head and reports each failure.
The SQLite check opens the existing store read-only; it does not create,
migrate, back up, chmod, or initialize store state.
Missing optional host credential files are warnings because a harness may be
authenticated during its first worker session.

When the checked-in `factory.yaml` fails validation, the `configuration` check
names the first invalid field and the reason:

```text
doctor: configuration: failed
problem: checked-in repository configuration is invalid: worker_build.digest: must be a sha256 digest with 64 hexadecimal characters
action: repair the repository factory.yaml and its declared workflow policy
```

Repair that field and run `factory doctor --config …` again. Repeat until the
check passes. Each run reports only the first invalid field.

- A repository field that declares factory-owned workflow policy, such as
  `roles`, reports `checked-in repository policy is rejected` and the field.
- A newer `schema_version` reports the supplied and the supported version.
- An unknown role key in `role_craft`, `role_harness_defaults`,
  `model_options`, or `reasoning_effort_options` reports only the parent map.
- The finding never shows the supplied value.

YAML syntax errors, type errors, unknown fields, and read errors keep the
generic problem `checked-in repository configuration is missing or invalid`.
The decoder text of these errors can contain repository content, so the doctor
does not show it.

The doctor is not a standalone `factory.yaml` validator. It needs a registered
repository in the host configuration, and it runs the full startup diagnosis.

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
renews one coordinator lease on every polling pass. The GitHub adapter
publishes it as a closed milestone titled
`factory coordinator lease`, visible on the repository's closed-milestones
page. The first renewal creates it. Later renewals find it again and edit its
description. This is also true after a restart or a lost GitHub response.
Thus there is always one lease resource, however long the coordinator polls. The
coordinator rewrites only the block between `<!-- factory-lease:begin -->` and
`<!-- factory-lease:end -->`. That block shows the full coordinator identity,
the active run (or `none`), and the complete renewal and expiry times. A past
expiry means the coordinator stopped renewing. Text outside the block is kept.
The coordinator adopts the milestone only when the authenticated `gh` account
created it and the block is present. A milestone with that title which fails
this check stops renewal with an actionable error; rename or delete it. If you
change the `gh` account, the old account's milestone fails this check, so
delete it once. Lease
failures use the bounded lease backoff and do not change workflow state or
retry budgets. The host lock remains the ownership authority; the coordinator
lease does not give distributed fencing or failover. The lease is optional per
adapter: with a tracker adapter that has no lease, the coordinator polls
without one (ADR 0018).

Installations from before this change published the lease as a pending
`factory/lease` Commit Status, and GitHub accepts at most 1,000 statuses for
each commit and context. The coordinator no longer reads or writes that
context, so an installation whose target commit is already at the cap starts
normally. GitHub cannot delete Commit Statuses. The last legacy status stays
pending on its old commit and stops showing on the branch head after the next
commit. If branch protection requires `factory/lease`, remove that
requirement. The coordinator also renews a host-local heartbeat
in the operational store. `factory status` and `factory doctor` report that
heartbeat as live, expired, or missing; live additionally requires the
kernel-backed coordinator lock, so a clean stop or process death is visible
immediately. When an active run has no live heartbeat, status names the
supervisor as the reason progress is stalled.
`factory stop` signals the locked
coordinator and leaves any active run, branch, worktree, worker, and session
artifacts in place. Polling never creates factory labels; use
`factory bootstrap-labels` explicitly.

After each observation that claimed or found a run, the coordinator drives that
run through baseline, the stages its frozen route and test policy select, the
checkpoint gate suite, the bounded check-repair loop, the branch push, one
draft pull request, and both independent reviews. Blocking findings are
combined into one bounded review-repair packet; an accepted repair reruns the
gates and both reviews against a new checkpoint. It stops in a defined waiting
state for clarification, a policy rejection, an exhausted budget, a repeated
review blocker, a harness limit, an authentication failure, or an ambiguous
recovery, and publishes the reason in the editable status comment.
`factory issue`, `factory agent`, `factory agent-report`, and
`factory draft-pr` stay available for diagnosis and deliberate manual
operation; they are not part of routine unattended progression.

## Host configuration

The generated host file uses schema version 2 and contains the repository path,
GitHub or Azure DevOps identity, authorized maintainers, polling settings, credential sources,
the checked-in repository configuration path, and the operational-data path.

```yaml
schema_version: 2
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
    authentication:
      codex_auth_path: /Users/me/.codex/auth.json
      claude_auth_path: /Users/me/.claude/.credentials.json
    review:
      concurrency: 2
      authorized_units: 8
    operational_data_path: /Users/me/.local/share/factory/factory.db
    repository_config_path: /Users/me/src/project/factory.yaml
    cache_root: /Users/me/.local/share/factory/caches
    caches:
      go-build: /Users/me/.local/share/factory/caches/go-build
    worker_limits:
      memory: 8g
      cpus: 4
      pids: 4096
      log_max_size: 10m
      log_max_files: 3
```

`cache_root` is the operator's data root for repository caches, and `caches`
maps each cache name the repository declares to its host directory. Every
mapped directory must resolve inside `cache_root`, so a repository commit can
never request an arbitrary writable host mount. Both keys are optional and are
omitted when the repository declares no cache.

`worker_limits` bounds every worker container the coordinator starts for the
repository. The limits are host-owned: the checked-in `factory.yaml` cannot
declare them, so a repository commit cannot raise its own limits. Every key is
optional, and an omitted key takes its default:

| Key | Default | Docker setting | Accepted values |
| --- | --- | --- | --- |
| `memory` | `8g` | `--memory`, and `--memory-swap` with the same value | A size with an optional `b`, `k`, `m`, or `g` unit, at least `6m` |
| `cpus` | `4` | `--cpus` | A CPU count of at least `0.01` with at most nine decimal places, such as `4` or `1.5` |
| `pids` | `4096` | `--pids-limit` | A positive whole number |
| `log_max_size` | `10m` | `--log-opt max-size` of the `json-file` log driver | A positive size with an optional `b`, `k`, `m`, or `g` unit |
| `log_max_files` | `3` | `--log-opt max-file` of the `json-file` log driver | A positive whole number |

Swap always equals memory, so a worker cannot extend its memory with swap.
Configuration validation rejects a zero, negative, or unparseable value with a
field-level error, for example `repositories[0].worker_limits.pids`.
`factory doctor` reports the effective limits as
`doctor: worker limits: passed (memory 8g, swap 8g, cpus 4, pids 4096, log 3 x 10m)`.
The limits apply when the coordinator creates a worker container. A worker
that already exists keeps the limits it was created with.
`docs/worker-runtime.md` describes how a command that reaches a limit fails.

### Azure DevOps registration

A registration can name an Azure DevOps Services repository in place of the
GitHub owner and repository. Azure Boards work items of the project are then
the work tracker, and Azure Repos is the code host (ADR 0019). A registration
names exactly one of `github` and `azure_devops`; validation rejects both, and
each `azure_devops` field is required and must be a plain name without `/`,
`\`, `?`, `#`, or `%`.

```yaml
schema_version: 2
repositories:
  - path: /Users/me/src/service
    azure_devops:
      organization: contoso
      project: Factory Pilot
      repository: service
    authorized_users:
      - alice@contoso.com
    polling:
      interval: 30s
      backoff: 5m
    operational_data_path: /Users/me/.local/share/factory/factory.db
    repository_config_path: /Users/me/src/service/factory.yaml
```

`factory register` infers only a GitHub identity. For Azure DevOps, write the
registration into the host file by hand, then run `factory doctor`.

- **Credential.** The factory calls the Azure DevOps REST API through
  `az rest`. Install the Azure CLI and sign in with `az login` as the
  company-managed identity that supervises the repository, for example
  `az login --service-principal` or `az login --identity`. The factory never
  reads or stores the credential.
- **Authorized users.** `authorized_users` holds account names (user principal
  names) such as `alice@contoso.com`, because comment, vote, and work item
  update authors carry that name.
- **Intake.** A work item is eligible when it is open and has the
  `agent-ready` tag, and the last update that added that tag came from an
  authorized user. Any editor can add a tag, so the tag alone is not enough.
  A work item is open unless its state is in the Completed or Removed category.
- **Run state.** The factory labels are work item tags. The factory never
  changes the work item state, so board rules stay intact. A tag is created
  on first use, which needs the Create tag definition permission until every
  factory tag exists; `factory bootstrap-labels` changes nothing.
- **Text.** HTML descriptions and comments are read as text with HTML comments
  kept, so `<!-- factory-route: ... -->` works whether it is typed as text or
  stored as a comment. The factory writes its own comments as Markdown.
- **Pull requests.** Commands and clarification questions on the pull request
  are thread comments. A factory comment opens a closed thread, so a "comments
  must be resolved" branch policy does not block the merge. A reviewer vote of
  "waiting for author" (-5) or "rejected" (-10) is a change request; the
  voter's open threads up to the vote are its findings.
- **Limits.** Azure Repos accepts at most 4,000 characters in a pull-request
  description. A longer generated body fails at the draft pull request instead
  of being truncated. There is no coordinator lease, because Azure DevOps has
  no milestones.
- **Remote.** The checkout's `origin` remote must be the registered Azure Repos
  repository in the HTTPS (`https://dev.azure.com/contoso/Factory%20Pilot/_git/service`)
  or SSH (`git@ssh.dev.azure.com:v3/contoso/Factory%20Pilot/service`) form.
  Host Git uses its own credential helper or SSH agent for push and fetch.

`factory doctor` reports three Azure DevOps checks:

| Check | Fails when |
| --- | --- |
| `azure devops authentication` | `az` has no usable identity for the organization |
| `azure devops repository` | the organization, project, or repository cannot be read |
| `azure devops permissions` | the identity lacks Contribute, Create branch, or Contribute to pull requests on the repository, or View and Edit work items in the project's root area, or a factory tag does not exist yet and the identity lacks Create tag definition |

All paths persisted in a repository registration are absolute. The coordinator does not infer macOS-specific paths in its domain or deep modules; only the command's default host-config resolver uses the host operating system's standard user configuration directory.

Host schema version 1 is deliberately not migrated in place. Before installing
this binary, finish or cancel every non-terminal run with the previous binary,
stop the previous coordinator, and re-register. Repository configuration keeps
its independent schema version 1 authority.

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

[Repository initialization](repository-initialization.md) is the ordered
procedure that produces this file for a repository that has none, together with
its worker image and pinned digest. This section owns the contract the procedure
writes against.

The default checked-in file is `factory.yaml` at the repository root. It is parsed with strict field checking and validated before a run can be claimed. Unknown schema versions fail closed.

```yaml
schema_version: 1
target_branch: main
setup: scripts/worker-go.sh mod download
setup_files: [go.mod, go.sum]
setup_environment_policy: clean
gates:
  - name: format
    command: test -z "$(gofmt -l .)"
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
# Optional role-specific craft selected from the target branch at claim time.
role_craft:
  implementation: docs/factory/craft/implementation.md
  standards_review: docs/factory/craft/standards-review.md
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
review_units:
  max_unit_bytes: 65536
  max_units: 4
retry_limits:
  check_repair: 3
  review_repair: 2
  test_revision: 2
test_policy:
  mode: required
  allow_human_exemption: true
  allow_technical_exemption: true
  # Repository-owned authorization for the bounded objection cycle.
  allow_automated_objections: false
  # Optional prefixes for essential test infrastructure.
  test_paths: []
  infrastructure_paths: []
allowed_overrides: [model, reasoning_effort]
caches:
  - name: go-build
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

The validator checks the schema version, target branch, setup, optional repository-relative `setup_files`, setup environment policy, ordered unique gates and earlier dependencies, matching role harness/model policies, the mandatory `test` role when `test_policy.mode` is `required`, optional `reasoning_effort_options` for declared roles, optional `role_craft` entries for declared roles, positive durations, positive retry limits, test policy, supported test-role prefixes, supported unique overrides (`model`, `reasoning_effort`, or `harness`), caches, worker image, base-synchronization mode, bounded `review_units` values, and optional positive `evaluation.retention`. `review_units.max_unit_bytes` defaults to 65,536 bytes and is capped at 1 MiB by factory policy; `max_units` defaults to four and is capped at eight. The factory owns the ten-line context-overlap policy. A large exact checkpoint is persisted as one deterministic review manifest; the specification and standards axes receive separate fresh invocations over those shared units. The host `review.concurrency` defaults to two simultaneous invocations and `review.authorized_units` defaults to eight; a manifest above the normal repository fan-out pauses until an authorized maintainer comments `/factory authorize-review <units>`. `role_craft` paths must be nonempty, repository-relative Markdown paths without control characters, backslashes, absolute paths, or any `..` path segment. At claim time each selected file is read from the exact immutable base checkpoint; a missing file fails the claim. `setup_files` names the checked-in manifests and lockfiles whose contents identify the dependency graph; an empty list is valid. `test_policy.test_paths` and `test_policy.infrastructure_paths` authorize additional repository-relative paths for the independent test role; conventional `*_test.go`, `test/`, `tests/`, `test-support/`, and `__tests__/` paths are allowed by default. An empty `allowed_overrides` list is valid and means that issue-level overrides are disabled. A cache entry declares only a unique `name` and `read_only`. The name becomes the in-worker mount point `/cache/<name>`. A declared `path` is rejected, because host paths belong to host configuration. Validation errors are typed and identify the offending field, including `schema_version` for an unsupported newer schema.

`test_policy.allow_automated_objections` is the repository-owned switch for the
implementation-to-test objection cycle. The value frozen in the specification
packet is the sole authority. While disabled, a structured objection is
persisted and the run waits for a human. When enabled, the coordinator resumes
the original test session, permits the repository's
`retry_limits.test_revision` revision attempts, and reruns the revised focused
command independently before implementation can continue.

## Host Git boundary

The coordinator runs Git on the host only for factory-owned operations:
worktree creation, checkpoints, base synchronization, push, inspection, and
startup diagnosis. Each of these commands starts with the policy
`-c core.hooksPath=/dev/null -c core.fsmonitor=false` (ADR 0015). Command-line
configuration outranks repository, global, system, and ambient `GIT_CONFIG_*`
configuration. Thus no repository hook runs on the host through a factory
operation. This is also true when `core.hooksPath` points at a tracked
directory that a worker can edit. The factory does not change the checkout's Git configuration.
Your manual Git commands in the checkout continue to run your hooks.
`factory doctor` reports the `git hooks` check as blocking when factory Git
invocations do not resolve the disabled hooks path.

| Mechanism | Factory policy | Reason |
| --- | --- | --- |
| Client hooks (`pre-commit`, `commit-msg`, `post-checkout`, `post-merge`, `pre-push`, `reference-transaction`, and all others) | Suppressed | A configured hooks path can point at tracked, worker-editable scripts. |
| `core.fsmonitor` hook command | Suppressed | Status reads would run the configured command. The factory does not need it. |
| Clean, smudge, and process filters; merge drivers | Supported | Only host configuration defines their commands. The worker gets read-only Git metadata without configuration, so it cannot define one. Tracked `.gitattributes` can only select a defined driver. Git LFS needs them. |
| External diff and text conversion | Not reached | Checkpoint validation uses the built-in `diff --check` format. The review diff passes `--no-ext-diff --no-textconv`. |
| Commit signing (`commit.gpgSign`, `gpg.program`) | Supported | The operator configures the program. A repository may require signed commits. |
| Transport authentication (credential helpers, `core.sshCommand`, ssh agent) | Supported | Fetch and push need it. No hook runs, so no repository script can inherit it. |
| Terminal prompts and askpass programs | Disabled | The coordinator runs unattended. A prompt would block it. |
| Server-side hooks of the remote | Not in scope | They run on the remote. The production remote is GitHub. |

### Deadlines and non-interactive mode

Each host `git` and `gh` call has a fixed deadline:

| Operation | Deadline |
| --- | --- |
| `git fetch`, `git push` | 10 minutes |
| All other host `git` commands, including `git ls-remote` and the review diff | 2 minutes |
| All `gh` calls | 2 minutes |

When a deadline expires, the factory stops the command and all processes that
it started. The call then fails with a retryable transport error. The
supervisor does not stop. It backs off and tries again on its next pass, so
lifecycle commands such as `/factory cancel` continue to work. A timeout does
not change the run state: the run stays active, and a GitHub or Git change
that the factory reserved stays in the effect journal. The next pass completes
or recognizes that change. A claim whose label change timed out stays a
claim; it does not become `agent-failed`. `factory doctor`
reports an unresponsive GitHub or Git remote as a failed check that names the
deadline.

Host commands cannot ask for input:

- `git` runs with `GIT_TERMINAL_PROMPT=0`, `GIT_ASKPASS=`, and `SSH_ASKPASS=`.
  Git does not prompt on the terminal and starts no askpass program. Supply
  credentials through a credential helper (for example `gh auth setup-git`) or
  an ssh agent.
- `gh` runs with `GH_PROMPT_DISABLED=1`.

The factory keeps at most 32 MiB of stdout and 32 MiB of stderr for each
host command. A command that writes more fails with an output-limit error. The
factory does not use partial output.

### Move hook checks into gates

A check that a repository runs as a Git hook does not run for factory
checkpoints. If the check is required, declare it as a gate in `factory.yaml`.
The worker runs it against the exact checkpoint. For example, replace a
pre-commit hook that runs a formatter and the unit tests with:

```yaml
gates:
  - name: format
    command: test -z "$(gofmt -l .)"
    timeout: 30s
    blocking: true
    environment_policy: clean
  - name: test
    command: scripts/worker-go.sh test ./...
    timeout: 2m
    blocking: true
    depends_on: [format]
    environment_policy: clean
```

A gate fails only on a nonzero exit status. Thus a check that only lists
problems, such as `gofmt -l`, must turn that output into a failure.

Developers can keep the hook for manual commits. For factory checkpoints, gates
replace hooks.

## Repository caches

A cache is declared on both sides of the repository-versus-host boundary, and
neither side can supply the other's half.

| Declares | Owner | Fields |
| --- | --- | --- |
| The cache name and its container-side purpose | Checked-in repository configuration | `caches[].name`, `caches[].read_only` |
| The host directory for one declared name | Host configuration | `cache_root`, `caches.<name>` |

The worker always mounts a cache at `/cache/<name>`, so the repository controls
what the cache is for and whether the worker may write to it, and the operator
controls which host directory that cache uses. A repository configuration that
declares `caches[].path` is rejected as a typed validation error.

Host configuration refuses a mapped directory outside `cache_root` when it
loads, so an out-of-root mapping never reaches a run. `factory doctor` reports
the `caches` check as a blocking finding when a declared cache has no host
mapping, when host configuration maps a cache the repository does not declare,
or when a mapped directory resolves outside `cache_root` through a symbolic
link. A worker launch, gate run, or check repair fails closed on an unmapped
name, so a repository commit that adds a cache after registration cannot
receive a defaulted host path.

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

Roles, invocation stages, prompt versions, default permitted paths, active
role ownership and report-outcome transitions are declared by the factory
in `internal/workflow`. Repository configuration may select harness and model
policy for a declared role, but `factory.yaml` cannot add or redefine
`roles`, `stages`, `prompts`, or `transitions`. Such fields are rejected as
typed repository-policy errors.

The optional `role_craft` map selects one repository-relative Markdown file per
declared role. Its content replaces only that role's embedded `craft` section;
the embedded authority, permitted paths, workflow stages, route sections, and
report contract remain in force. The coordinator captures the file from the
claim's exact base checkpoint, stores its content and SHA-256 identity in the
specification and invocation packets, and uses those frozen bytes for every
prompt rebuild or restart. Repository craft cannot declare prompt bodies,
factory sections, `allowed_overrides`, or workflow behavior.

The optional architecture role is launched explicitly with the
factory-declared architecture stage. Its default permitted path is
`docs/architecture`, and its prompt requires a concise design document plus a
normal structured handoff. The role gets a fresh invocation and role home. Its accepted completed handoff
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

`test_policy.mode: required` enables the independent test role. A required-mode
run enters `test/active`, verifies coordinator-rerun red evidence, creates a
separate test checkpoint, and protects the accepted test paths before
implementation. A human skip additionally requires
`allow_human_exemption: true` and the frozen issue marker
`<!-- factory-test-exemption: human | justification -->`; technical skips are
still provisional and policy-controlled.

`test_policy.mode: advisory` is the implementation-owned TDD path. A healthy
baseline moves directly to `implementation/active`; there is no separate test
invocation, handoff, checkpoint, protected-test path, exemption, or test-stage
dispute. The implementation prompt owns the complete red/green/refactor loop,
including focused behavioral tests and essential test infrastructure within its
permitted scope. Deterministic gates and exact-checkpoint specification review
remain unchanged in both modes.

`retry_limits.review_repair` bounds automatic implementation repairs caused by
blocking review findings. The repository owns the value and the factory applies
no upper limit of its own. Findings from the configured specification and
standards reviewers are delivered together. If a materially same blocker
survives an attempted repair, or the budget is exhausted, the run waits for a
human. A successful repair creates a new checkpoint and the coordinator reruns
the complete configured gate suite and both reviewers in fresh sessions.

When `base_synchronization.mode` is `before_ready`, the coordinator fetches the
configured target branch and merges it into the factory branch immediately
before readiness. The merge may create a merge commit; it never rebases or
force-updates the branch. A changed head returns the run to checks and a fresh
review round. Merge conflicts remain for human disposition. `never` skips this
boundary synchronization.

### Optional PR writer

The `pr_writer` role writes a short summary for the human reviewer before the
coordinator hands the pull request over. It is off by default. To turn it on,
declare the role in all policy maps that you use:

```yaml
role_harness_defaults:
  pr_writer: claude
model_options:
  pr_writer: [claude-sonnet-5-5]
reasoning_effort_options:
  pr_writer: [medium]
```

When the role is not declared, readiness is the same as before and no
`pr_writer` invocation starts.

When the role is declared, the coordinator runs it inside the readiness step,
after all reviews pass and before the draft flag changes:

- The role reads the base-to-checkpoint diff, the frozen specification, the
  gate results for the checkpoint, the findings of both review axes, and
  `GLOSSARY.md` when it exists. The coordinator keeps a bounded log of the
  setup and gate command output for each checkpoint, passed gates included,
  and mounts it at `/invocation/gate-output.log`. Its worktree is read-only and it has no
  code-host credentials, so it cannot change the run branch.
- It returns markdown that follows the `pr` worker skill: **Summary**,
  **Evidence**, and **Merge Danger**. The worker cannot take screenshots, so
  the role must not invent before/after evidence.
- The coordinator saves the text with the checkpoint before it updates the
  pull request. A retry of readiness at the same checkpoint reuses the saved
  text and starts no new invocation.
- The coordinator puts the text above its own generated section, between
  `<!-- factory-pr-writer:start -->` and `<!-- factory-pr-writer:end -->`, with
  a note that an agent wrote it. It replaces only the text between these
  markers. Text that a person wrote stays unchanged.
- Each new hand-off checkpoint, for example after a repair, runs the role again
  and replaces the earlier section.
- Merge Danger is advice. It is not a gate.

A failure never blocks the hand-off. When the role fails to start, returns
`cannot_proceed`, returns an empty or invalid body, or passes `timeouts.agent`,
the coordinator ends the role. A body is invalid when it has no Summary,
Evidence, or Merge Danger heading, or when it contains a coordinator marker.
A failed start can pause the run, for example for harness capacity; the
coordinator then restores the status the run had before the start. In each case
it saves a failed summary, removes an earlier `pr_writer` section,
emits a `pr_summary_warning` event, and makes the pull request ready with the
coordinator section only.

The `pr` skill is in the worker image. `factory doctor` requires smoke evidence
for it only when the repository declares `pr_writer`, so record a smoke result
for the pinned digest before you turn the role on.

Repository configuration cannot declare a workflow route. A route is selected
per issue with a frozen `factory-route` marker before claim, and it needs the
roles it runs to be declared in `role_harness_defaults` and `model_options`:
the `acceptance` route needs `test`, and the `design-acceptance` route needs
`architecture` and `test`. A claim whose route names an undeclared role is
refused with the `route_unavailable` policy code. Required test policy cannot
be downgraded by a route. See the README section on workflow routes.

## Worker image build and digest pinning

This repository owns a two-layer worker image definition:

- `worker/base.Dockerfile` defines the versioned factory base image. It pins
  npm and the Codex and Claude Code npm packages through `NPM_VERSION`,
  `CODEX_VERSION`, and `CLAUDE_VERSION` build arguments, installs Git and the basic worker
  utilities, and builds the repository's `factory-report` binary into
  `/usr/local/bin/factory-report` and the headless process helper into
  `/usr/local/bin/factory-worker-headless`.
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
`0.148.0`, Claude Code `2.1.232`, and npm `11.21.0`; override those build arguments explicitly
when producing a new versioned image. The Go release is not an override: the
script reads the approved release from the go.mod `toolchain` line, refuses a
different `GO_VERSION`, and verifies the build metadata of the shipped helpers
and the worker toolchain in the image. Set `WORKER_PLATFORM=linux/amd64` (or
another target) when building for a platform different from the Docker daemon.

`make worker-publish` scans and publishes a locally built image and prints its
registry manifest digest. [Build toolchain and vulnerability
scanning](supply-chain.md) owns the approved Go toolchain, the scan policy,
and the complete rebuild, publish, repin, and smoke procedure.

After the image smoke checks, the same command mounts this checkout and runs
the configured setup, format, vet, test, and build gates under the worker's
clean baseline environment. After those repository gates, it runs
`scripts/verify-headless-worker.sh` in the newly built image. That offline,
real-Docker check uses a deterministic stand-in for each production harness to
verify the pinned skill roots, exact prompt delivery, report production,
cancellation, fresh coordinator-process inspection, and native resume.

The worker image also carries the curated skill set from `worker/skills`, which
it installs into both harness role homes. The digest recorded here therefore
pins the skills a role agent sees, exactly as it pins the harness versions. See
`worker/SKILLS.md` for the set and its curation rule, and ADR 0006 for the
boundary. Changing a skill needs a rebuild and a new digest.

The recorded worker skill smoke evidence in `worker/skill-smoke.json` is keyed
by this digest and by each harness's version, so a new digest invalidates it.
Re-run `./scripts/smoke-skills.sh` after recording a new digest. Startup
diagnosis blocks missing evidence for harnesses selected by
`role_harness_defaults`, or for both supported harnesses when
`allowed_overrides` contains `harness`; an unselected harness is reported as
an advisory finding.

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

The coordinator uses two factory-owned ports (ADR 0018). The tracker port in `internal/tracker` covers issues, labels, comments, and the lease. The code-host port in `internal/codehost` covers pull requests, reviews, and commit statuses. The CLI selects the GitHub adapter for both ports. The GitHub adapter invokes the locally authenticated `gh` CLI. The coordinator receives issue and mutation results in memory; GitHub credentials are not read into or persisted by the factory. `factory status` reports the supervisor heartbeat and the active run's stage, status, branch, and worktree, or the latest terminal run when no run is active.

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
/factory resume
/factory answer clarification-1 use the existing JSON format
/factory repair validate permitted paths before the adoption return
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
is pending; a repair command supplies one maintainer instruction as an
unbudgeted human repair packet and resumes implementation from the current
checkpoint, and is admitted only while a review waits for human disposition
over an open tracked pull request with a valid checkpoint and no active
invocation; retry reopens a failed or explicitly cancelled run at its current
stage; cancel stops active worker activity while retaining the run artifacts;
and harness configuration records a later invocation override only when the
frozen repository configuration permits it. `/factory resume` is admitted only
for a non-terminal typed harness-capacity pause or the typed authentication and
native-session recovery pauses in `waiting_for_human`; it cannot bypass
pending clarification questions, review disposition, capture-limit, or other
human gates. It retries the paused lifecycle without consuming the automatic
recovery allowance. A recognized command from an
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

For an expired credential, refresh the host source first with
`factory auth refresh`. `factory auth refresh --resume` combines that
credential projection with one explicit native-session continuation; the
default command remains credential-only. The equivalent GitHub sequence is the
same host refresh followed by `/factory resume`, delivered by the live polling
supervisor. The `--resume` path reconciles pending effects before continuing,
and it reports a refresh success separately from a failed native-session
precondition so adding the flag does not discard the credential refresh.

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
request). The question comment carries a marker scoped to
the run and its specification packet version, so an interrupted publication
repairs that round's comment while an answered round's questions remain
readable. A command such as
`/factory answer clarification-1 use the existing JSON format` records the
authorized answer in a new specification-packet version and resumes a fresh
implementation invocation with that packet. Clarification pauses do not
consume retry budget. `/factory refresh` re-reads the issue into another packet
version, preserves resolved answers, invalidates superseded downstream
invocations and checkpoint results, and resumes the role selected by the new
packet. If an accepted implementation checkpoint already exists and the run
worktree is clean, refresh instead treats that checkpoint as the new packet
baseline, reruns the baseline gates there, and restarts the workflow from that
checkpoint, reusing the prior implementation session when native resume is
available. This checkpoint behavior also applies when the refreshed issue text
is unchanged; it does not require fabricating an uncommitted worktree change.
The lifecycle reason names `/factory refresh` while that restart is being
established. `/factory revision` is the authorized ready-PR amendment command: it
creates a new packet version from the current open issue, drafts the tracked PR,
invalidates all prior gate and review results, treats the current clean
checkpoint as the new amendment baseline, preserves the existing
branch/worktree, and restarts the workflow from the current checkpoint. It
never merges or force-updates a remote branch.

The poll command also observes the tracked pull request and issue lifecycle.
A merged pull request completes the run and records its merge commit; closing
the issue or an unmerged pull request cancels it. Merge detection takes
precedence over the pull request's closed state. Terminal transitions replace
the factory state label, edit the existing status comment, stop
the worker without deleting retained state, and leave the branch and worktree
available for cleanup or an explicit retry.

After a claim, `factory agent` starts the selected role. Codex and Claude Code
both run headlessly inside the pinned worker and print only logical invocation
and native session identities. The role receives a read-only invocation packet and reports through `factory-report`; use
`factory agent-report --invocation-id <id>` to ask the coordinator to validate
and accept the structured report. Native output is never treated as a stage
result. The operational store schema is version 40 and persists invocation
identity, prompt version, result directory, native
session identifier, and permitted handoff paths in addition to run state. It
also persists the draft pull-request number and URL so a restarted command can
update the existing pull request instead of creating another one. Terminal runs
retain merge commit and lifecycle reason for status rendering and restart-safe
GitHub projection retries. The store also retains the host-local supervisor
heartbeat used by `factory status` and `factory doctor`.

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
Docker with the configured worker image available, and the host Codex `auth.json`
path registered in the host configuration. Codex and Claude Code repositories,
including mixed per-role selections, use the headless worker path and do not
require no terminal software. Build the local commands from this checkout
(`factory`, `factory-report`, and `factory-worker-headless`) so the worker image
can invoke the pinned report command; the headless worker helper is built into
the image.

Run the path in this order:

```sh
factory bootstrap-labels --config /Users/me/.config/factory/config.yaml
factory issue --config /Users/me/.config/factory/config.yaml <issue-number>
factory agent --config /Users/me/.config/factory/config.yaml --run-id <run-id>

# In the detached implementation invocation, make the requested small change and
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
- the selected role reaches its headless worker process, while its container
  has no Git remote or GitHub credential access;
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

The operational store contains current workflow state, the active run's frozen specification packet, the status-comment identity needed by later transitions, and the draft pull-request identity needed for idempotent regeneration. Registration and status both reject paths that resolve inside the repository checkout, including symlink aliases; its directory is private (`0700`) and the SQLite file is private (`0600`). A fresh store is initialized directly; an older supported schema is copied to a timestamped `.bak-*` file before its explicit migration runs. Migration backups are not pruned automatically in this foundation; issue #23 owns the visible cleanup and retention policy, and `factory reset` removes only the backups whose file name proves they belong to that exact database. A newer or unversioned database refuses to open. There is no silent guessing or destructive migration. GitHub credentials are never columns in this store.

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

## Local web UI

`factory ui` serves a read-only view of the runs in the operational store. Open
the printed URL in a browser on the same host. Stop the server with `Ctrl-C`
(SIGINT) or SIGTERM.

```sh
factory ui --config /Users/me/.config/factory/config.yaml \
  --address 127.0.0.1:8765 \
  --refresh 5s
```

| Flag | Default | Meaning |
|---|---|---|
| `--config` | Host configuration path | Host configuration of the registered repository. |
| `--address` | `127.0.0.1:8765` | Listen address. The host must be exactly `127.0.0.1` and the port must be explicit. Port `0` selects a free port. |
| `--refresh` | `5s` | Interval between two automatic page reads. `0` turns automatic refresh off. It must not be negative. |

The command prints `factory ui listening on http://127.0.0.1:<port>` when it is
ready and `factory ui stopped` when it stops. An invalid address, a negative
interval, or a positional argument stops the command with exit status 2 before
it opens a socket.

Access:

- The UI has no login. For this reason it listens only on `127.0.0.1`. The
  command refuses `0.0.0.0`, `::`, `::1`, `localhost`, and all other hosts.
- The UI answers only requests whose `Host` header is `127.0.0.1` or
  `localhost`. This stops a DNS-rebinding page from reading run data through
  the browser.

Store use:

- Each page request opens the operational store read-only, reads, and closes
  it. The UI never creates, migrates, or changes the store, and it does not
  probe the coordinator lock. Thus the UI works while `factory start` runs and
  also while the coordinator is stopped.
- The read-only open requires the current store schema. After an upgrade, run
  `factory start` or `factory status` once to migrate the store. Until then,
  and when the store is missing or busy, the page shows "Operational store
  unavailable" (HTTP 503). The next refresh tries again.

Data and network:

- The UI sends no request to GitHub, Azure DevOps, a remote Git host, or any
  other service, and it sends no telemetry (ADR 0001). Pages and assets load
  only from the UI server; the Content-Security-Policy blocks all other
  origins. The pull-request link on a run page is the only external URL, and
  the browser opens it only when you click it.
- Run pages show repository content: issue titles, pending questions, the last
  command message, and gate failure diagnostics. This content stays on the
  host; the UI serves it only on the loopback interface.

Pages:

| Path | Content |
|---|---|
| `/` | All runs, newest update first: stage, status, workflow route, and activity. |
| `/runs/{id}` | One run: waiting state and reason, checkpoints, gate results, invocations, pending effect, pull request, and gate failure diagnostics. |
| `/runs/{id}/evaluation` | The local evaluation summary of one run: outcome, wall time per stage, attempts, invocation versions, and reported usage. The summary is content-free (ADR 0001). When cleanup removed the run row, the page shows only the summary. |

Every page shows a supervisor banner. The banner tells if the coordinator
heartbeat is live, expired, or not recorded, and it shows when the page read
the store. When a run is active and the supervisor is not live, the banner
shows a warning that names the run; run `factory start` to continue it. While the browser tab is visible, the page reads the run data and
the banner again after each `--refresh` interval; with `--refresh 0` the page
changes only when you reload it. When the UI server does not answer, the page
shows that the server is unreachable.

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

## Complete local reset

`factory reset` is the separate, deliberately destructive operation that
returns one registered installation to its pre-`init` local state. It is not a
retention policy: it has no cutoff, it selects every persisted run, and it
removes the installation itself.

| Operation | Scope | Keeps the installation |
| --- | --- | --- |
| `factory cleanup` | Terminal run artifacts older than seven days | Yes |
| `factory evaluation-delete` | Selected terminal evaluation summaries | Yes |
| `factory register` | Creates one registration, or updates its credential sources with `--update` | Yes |
| `factory bootstrap-labels` | Creates the factory-owned GitHub labels | Yes |
| `factory reset` | Every local resource of one registered installation | No |

Reset requires an explicit `--config` path. A command that destroys a whole
installation must never default to the operator's real configuration, so the
usual default-path behavior is deliberately not applied here.

```sh
factory reset --config /Users/me/.config/factory/config.yaml
factory reset --config /Users/me/.config/factory/config.yaml --confirm
```

The preview is read-only: it performs no filesystem, Git, Docker,
store, configuration, or GitHub mutation, and it prints the removable targets
separately from the deliberately retained resources. It opens the operational
store through the read-only entry point, so previewing an installation never
creates an absent database, initializes its metadata, or backs up and migrates
an older schema. An absent database is reported as an already-removed target
rather than recreated. Confirmation re-observes current state and never treats
the earlier preview as authority.

Reset removes the whole host configuration, so it refuses a configuration
holding more than one registration: it cannot prove it owns the resources of a
registration it did not plan for. Version one registers exactly one repository,
so this guards a hand-edited configuration rather than a supported mode.

Before deleting anything, reset proves that no coordinator owns the registered
checkout's lock and refuses while `factory start` is running, directing the
operator to `factory stop`; it never signals the coordinator itself. It then
validates every persisted run, invocation, output, worker, role, and
credential-store identity, refusing malformed identities, unsafe paths,
symlinks that could redirect removal, pending effects, ownership ambiguity, and
known required-adapter unavailability.

Reset reads the GitHub lifecycle of every non-terminal run before discarding
its state, using the same coordinator-owned rules as ordinary polling. A merged
pull request completes the run and an unmerged closed pull request or closed
issue cancels it, publishing the normal final label and status comment first.
A genuinely live issue or pull request blocks reset with the run identity and
the supervised cancellation instruction; GitHub transport or authorization
failure also blocks reset while a non-terminal run exists. This step is
essential: deleting the SQLite database first would strand a closed or merged
run with `agent-running` on GitHub, because the coordinator loses the
status-comment and run identities needed to project its final outcome.

The lifecycle transition is committed to the operational store before the final
deletion plan is built. The confirmed sequence removes worker containers,
role volumes, and factory-managed credential
volumes; removes generated invocation and result directories; removes run
worktrees, local run branches, and private Git projections; removes the
operational database with its SQLite sidecars and only the migration backups
proven to belong to that exact database; removes the coordinator lock; and
removes the selected host configuration last. Reset holds that lock for the
whole confirmed pass. Once the store is gone, a concurrent
`factory start` cannot pass its read-only startup diagnosis, so unlinking the
lock before the configuration preserves the configuration if unlinking fails.
The operational
store is the cleanup manifest, so it and the configuration survive until every
resource whose identity depends on them is gone.

The evaluation projection stored in that database disappears with it. Ordinary
evaluation retention outside reset is unchanged and remains owned by
`factory evaluation-delete`.

An already-absent target is a success, so a partially completed reset is safely
retryable. A failure reports what was removed and what remains, without
credentials, credential paths, database content, issue text, prompts, diffs, or
command output, and retains the store and configuration whenever either is
still needed to retry remaining work.

Reset retains the source checkout, its tracked files, `factory.yaml`, and
ordinary local branches; the installed `factory`, `factory-report`, and
`factory-worker-headless` binaries; Docker worker images; repository-declared
cache directories; host Codex and Claude credential sources and host harness
state; GitHub label definitions, issues, pull requests, reviews, comments,
commit statuses, and merged history; and remote `factory/*` branches. Retained
labels are deliberate: `factory bootstrap-labels` is idempotent, while deleting
a label definition would strip it from historical issues. Installation and
worker-image construction stay owned by the build and install commands, so
reset never uninstalls its own binary.

After a successful reset the selected configuration path does not exist, its
operational database does not exist, no planned local runtime artifact remains,
and the ordinary fresh-host journey works again.

The high-level `Factory` seam injects configuration, repository checking,
the tracker adapter, pull requests, `GitWorkspace`, worker, headless harness, clock,
run-identity, and operational-store adapters. Contract tests exercise Docker
and both detached adapters through controlled seams.
