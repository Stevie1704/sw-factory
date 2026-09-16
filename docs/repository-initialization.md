# Repository initialization

This document is the ordered procedure that prepares one target repository for
factory runs, written so an agent can follow it end to end. [Configuration and
local operation](configuration.md) owns the field contract and [Worker
runtime](worker-runtime.md) owns the isolation contract; this document states
what an author needs while writing the file, so an agent working in the target
repository does not have to hold either contract in context. Where this summary
and `docs/configuration.md` disagree, `docs/configuration.md` is correct and
this document is a defect.

The procedure produces four checked-in artifacts in the target repository and
one host registration:

| Artifact | Owner | Why it is needed |
| --- | --- | --- |
| `factory.yaml` | target repository | Declares the target branch, setup, gates, role policy, timeouts, and the pinned worker image |
| `worker/Dockerfile` | target repository | Adds the repository toolchain and the worker skills to the factory base image |
| `worker/skills/` | target repository | The curated skill set the role prompts require, baked into the image |
| `worker/skill-smoke.json` | target repository | Recorded proof that each harness loaded the mandated skills at the pinned digest |
| Host registration | operator host | Created by `factory init` and `factory register`; never checked in |

## The prompt

Paste this into an agent that has shell access to both checkouts. Replace the
two bracketed values first.

~~~text
Prepare the repository at <TARGET_REPO_PATH> for Software Factory runs.

The Software Factory checkout is at <SW_FACTORY_CHECKOUT>. Read
<SW_FACTORY_CHECKOUT>/docs/repository-initialization.md in full and follow its
ordered procedure. Use its field reference rather than another repository's
factory.yaml; where the two disagree, docs/configuration.md decides.

Stop and ask me before:
- writing any file in the target repository,
- running factory init, factory register, or factory bootstrap-labels,
- running scripts/smoke-skills.sh,
- committing anything.

Never invent a credential path and never read a credential file. If one is
missing, stop and ask. Never commit an absolute host path into factory.yaml.
~~~

The prompt is deliberately short. The procedure lives in this document, so
repeating it in the prompt only creates a second copy that can drift.

## Boundaries the agent must not cross alone

Four actions in this procedure are the operator's, not the agent's:

- `factory init` and `factory register` write host configuration and create the
  operational store.
- `factory bootstrap-labels` creates six labels in the GitHub repository.
- `scripts/smoke-skills.sh` streams a host harness credential into a container
  and makes one real, paid model call per mandated skill per harness.
- Any commit or pull request in the target repository.

The agent prepares each command and asks. A missing credential file is a
question for the operator, never a search.

## Step 1: survey the target repository

Collect, from the checkout itself:

- the default branch, which becomes `target_branch`;
- the dependency manifests and lockfiles, which become `setup_files`;
- the command that installs dependencies offline from those lockfiles, which
  becomes `setup`;
- the verification commands the repository already uses, which become `gates`.

An existing CI definition is the best source for the gate list, because those
commands are already known to run without a developer workstation.

## Step 2: write the worker image definition

The worker runs every setup command, every gate, and every harness session. It
supplies a fixed non-login environment, so the toolchain must be reachable on
this exact `PATH`:

~~~text
/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
~~~

A toolchain installed anywhere else is invisible to a gate, whatever a login
shell would resolve. Expose each required command at a stable `PATH` location
instead of relying on a profile.

The factory base image `ghcr.io/stevie1704/sw-factory-base:v1` is built from
`worker/base.Dockerfile` in the Software Factory checkout. It carries the pinned
Codex and Claude Code releases, `factory-report`, `factory-worker-headless`,
Git, the `factory` user with uid `10001`, and the stable worker paths. It is
built on `node:22-bookworm-slim`, so `node` and `npm` are already on the fixed
`PATH`; no other language runtime is.

The base image does not carry the worker skills. Each repository worker image
bakes them, so they are pinned by the digest the repository records. Vendor them
from the same Software Factory checkout that builds the base image:

~~~sh
mkdir -p <TARGET_REPO_PATH>/worker
rm -rf <TARGET_REPO_PATH>/worker/skills
cp -R <SW_FACTORY_CHECKOUT>/worker/skills <TARGET_REPO_PATH>/worker/skills
~~~

The removal is part of the command, not a precaution. Copying a directory onto
an existing directory of the same name nests it as `worker/skills/skills` and
leaves the skills the image actually bakes untouched.

Re-vendor whenever the base image is rebuilt. `scripts/smoke-skills.sh` reads
its expected skill text from the Software Factory checkout and its actual text
from the image, so vendored skills that have drifted from their source produce a
smoke result that proves nothing.

A repository whose toolchain is already in the base image needs only the skill
layer:

~~~dockerfile
# worker/Dockerfile — Node repository.
ARG FACTORY_BASE_IMAGE=ghcr.io/stevie1704/sw-factory-base:v1

FROM ${FACTORY_BASE_IMAGE}

# Both harnesses read a skill set from their own role home. Baking the set pins
# the agent-visible instructions to this image digest.
COPY --chown=factory:factory skills /home/factory/.codex/skills
COPY --chown=factory:factory skills /home/factory/.claude/skills

USER factory
~~~

A repository that needs another toolchain copies it from an official image in a
second build stage and links the commands its gates call into `/usr/local/bin`.
Copy everything that toolchain loads at runtime, not only its entry binary: a
shared-library interpreter and its package manager are easy to leave behind, and
the image contract check in Step 3 is where that shows up.

`worker/Dockerfile` in the Software Factory checkout is the worked example for
a compiled toolchain. Keep the build context at the `worker` directory, so the
`skills` source path stays stable.

## Step 3: build and verify the image

The base image builds from the Software Factory checkout, because it compiles
that repository's reporting binaries:

~~~sh
cd <SW_FACTORY_CHECKOUT>
docker build \
  --build-arg FACTORY_BASE_VERSION=1 \
  --file worker/base.Dockerfile \
  --tag ghcr.io/stevie1704/sw-factory-base:v1 \
  .
~~~

The repository worker image then builds from the target checkout without
pulling that base again:

~~~sh
cd <TARGET_REPO_PATH>
docker build --pull=false \
  --build-arg FACTORY_BASE_IMAGE=ghcr.io/stevie1704/sw-factory-base:v1 \
  --file worker/Dockerfile \
  --tag <IMAGE_NAME>:v1 \
  worker
~~~

`<IMAGE_NAME>` is the name the repository builds under locally, for example
`ghcr.io/example/project-worker`. It becomes `worker_build.image`.

Read the digest from the built image:

~~~sh
docker image inspect <IMAGE_NAME>:v1 --format '{{.Id}}'
~~~

That value is Docker's local content-addressable image id, not a registry
manifest digest. The worker runtime never pulls, so the image must exist locally
under the recorded name at the recorded digest. Publishing to a registry later
means replacing the recorded value with the registry's manifest digest.

Verify the image contract before recording anything:

~~~sh
docker run --rm --pull=never \
  --cap-drop ALL --security-opt no-new-privileges \
  <IMAGE_NAME>@<DIGEST> /bin/sh -c '
  test "$(id -u)" = 10001
  test "$HOME" = /home/factory
  test -d /work && test -d /git && test -d /cache && test -d /invocation && test -d /results
  test -x /usr/local/bin/factory-report
  test -x /usr/local/bin/factory-worker-headless
  command -v codex >/dev/null
  command -v claude >/dev/null
  command -v git >/dev/null
  test "$PATH" = /usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
'
~~~

Extend the command list with every toolchain command the gates call. The
headless lifecycle of the new image is verified independently, from the Software
Factory checkout:

~~~sh
cd <SW_FACTORY_CHECKOUT>
scripts/verify-headless-worker.sh <IMAGE_NAME>@<DIGEST>
~~~

## Step 4: write factory.yaml

`factory.yaml` sits at the target repository root. It is parsed with strict
field checking: an unknown field is an error, and so is a field the factory
owns. `roles`, `stages`, `prompts`, and `transitions` are declared by the
factory in `internal/workflow` and are rejected in repository configuration.

### Required fields

| Field | Rule |
| --- | --- |
| `schema_version` | Exactly `1`. A newer value fails closed. |
| `target_branch` | The branch runs start from. Nonempty, no line breaks. |
| `setup` | The dependency command. Required and nonempty; use `true` when the repository needs no setup step. |
| `setup_environment_policy` | `clean` or `role`. |
| `gates` | At least one gate. |
| `role_harness_defaults` | At least one factory-declared role. |
| `model_options` | Exactly the same role set as `role_harness_defaults`. |
| `timeouts` | `setup`, `agent`, `gate`, and `review`, each a positive Go duration. |
| `retry_limits` | `check_repair`, `review_repair`, and `test_revision`, each at least `1`. |
| `test_policy.mode` | `required` or `advisory`. |
| `worker_build` | `image`, `digest` (`sha256:` and 64 hexadecimal characters), and `definition`. |
| `base_synchronization.mode` | `never` or `before_ready`. `before_ready` also needs `branch`. |

### Optional fields

| Field | Rule |
| --- | --- |
| `setup_files` | Repository-relative, unique manifest and lockfile paths. An empty list is valid. The contents identify the dependency graph that triggers a setup rerun. |
| `role_craft` | Repository-relative Markdown per declared role. No absolute path, no `..` segment, no backslash. Each file is read from the frozen base checkpoint at claim time; a missing file fails the claim. |
| `reasoning_effort_options` | Per declared role. A role that declares none accepts no reasoning-effort selection. |
| `allowed_overrides` | Unique values from `model`, `reasoning_effort`, and `harness`. An empty list disables issue-level overrides. |
| `review_units` | `max_unit_bytes` defaults to 65536 and is capped at 1048576. `max_units` defaults to 4 and is capped at 8. |
| `test_policy.test_paths`, `test_policy.infrastructure_paths` | Extra repository-relative prefixes the test role may edit. `*_test.go`, `test/`, `tests/`, `test-support/`, and `__tests__/` are allowed by default. |
| `test_policy.allow_human_exemption`, `test_policy.allow_technical_exemption`, `test_policy.allow_automated_objections` | Booleans, each defaulting to `false`. See the test policy below. |
| `caches` | A list of `name`, `path`, `read_only` entries. Names are unique and paths are nonempty. See the caveat below before declaring one. |
| `evaluation.retention` | A positive Go duration, or omitted. |

### Gates

Each gate declares `name`, `command`, `timeout`, `blocking`, and
`environment_policy`. Names are unique. `depends_on` may reference earlier gates
only, never the gate itself, so the list is an ordered, acyclic sequence. A
blocking gate stops the checkpoint; a non-blocking gate reports and continues.
Independent gates still run after an earlier failure.

### Roles

The factory declares five roles: `test`, `implementation`, `architecture`,
`spec_review`, and `standards_review`. Supported harnesses are `codex` and
`claude`.

- `role_harness_defaults` and `model_options` must cover the same roles.
- `test_policy.mode: required` makes the `test` role mandatory in both maps.
- An issue that selects the `acceptance` route needs the `test` role, and
  `design-acceptance` needs `architecture` and `test`. A claim naming an
  undeclared role is refused with `route_unavailable`.

Start with `implementation`, `spec_review`, and `standards_review` in advisory
test mode. Add `test` when the repository is ready to make the independent test
role mandatory, and `architecture` when issues should be able to select the
design route.

`reasoning_effort_options` values are harness-specific and are validated against
the harness the role declares. Claude Code accepts `low`, `medium`, `high`,
`xhigh`, and `max`. Codex accepts its own effort names. Deriving the values from
the wrong harness fails validation before launch.

### Test policy

`test_policy.mode` decides whether the independent test role runs. `required`
enters the test stage, verifies coordinator-rerun red evidence, creates a
separate test checkpoint, and protects the accepted test paths before
implementation. `advisory` leaves the red/green loop inside the implementation
role.

Three booleans govern how a required-mode run may be relaxed. Each defaults to
`false`, and each opens exactly one door:

| Field | What `true` permits |
| --- | --- |
| `allow_human_exemption` | A human skips the test stage for one issue. The run additionally needs the frozen issue marker `<!-- factory-test-exemption: human \| justification -->`. |
| `allow_technical_exemption` | The test role records a provisional technical skip when the change is not testable by the declared means. The skip stays policy-controlled and visible. |
| `allow_automated_objections` | The implementation role may object to a test automatically, which resumes the test session for up to `retry_limits.test_revision` revisions. |

`allow_automated_objections` is evidence-gated in this factory: it stays closed
until the measured pilot in issue #26 records a proceed decision, and the
coordinator verifies that decision comment independently. Declare it `false`.

Both exemption switches are inert under `advisory` mode. Declare the three
fields explicitly anyway, so the policy a run applies is readable in the file
rather than inferred from an absent key.

### Caches

`caches` maps a name to a host path that the worker mounts writable. Repository
configuration currently has no way to express that path portably, so a declared
cache pins one operator's home directory into a committed file. Omit `caches`
until issue #180 splits the name from the host path.

### Worked example

A Node repository with three gates, Claude Code in every role, and advisory
test policy:

~~~yaml
schema_version: 1
target_branch: main
setup: npm ci
setup_files: [package.json, package-lock.json]
setup_environment_policy: clean
gates:
  - name: lint
    command: npm run lint
    timeout: 5m
    blocking: true
    environment_policy: clean
  - name: test
    command: npm test
    timeout: 15m
    blocking: true
    depends_on: [lint]
    environment_policy: clean
  - name: build
    command: npm run build
    timeout: 10m
    blocking: true
    depends_on: [test]
    environment_policy: clean
role_harness_defaults:
  implementation: claude
  spec_review: claude
  standards_review: claude
model_options:
  implementation: [claude-opus-5]
  spec_review: [claude-opus-5]
  standards_review: [claude-opus-5]
reasoning_effort_options:
  implementation: [max]
  spec_review: [high]
  standards_review: [high]
timeouts:
  setup: 10m
  agent: 90m
  gate: 15m
  review: 30m
review_units:
  max_unit_bytes: 65536
  max_units: 4
retry_limits:
  check_repair: 3
  review_repair: 2
  test_revision: 2
test_policy:
  mode: advisory
  allow_human_exemption: true
  allow_technical_exemption: true
  allow_automated_objections: false
allowed_overrides: []
worker_build:
  image: ghcr.io/example/project-worker
  digest: sha256:0000000000000000000000000000000000000000000000000000000000000000
  definition: worker/Dockerfile
base_synchronization:
  mode: never
~~~

Replace the placeholder digest with the value Step 3 produced. The checked-in
[factory.yaml](../factory.yaml) of this repository is the worked example for a
compiled toolchain with a mandatory setup step.

## Step 5: prove every command in the worker

A gate is written into `factory.yaml` only after it has run inside the built
image under the environment the worker actually supplies. Run each command the
way the worker does: against a sanitized copy of the checkout, with an empty
environment and the fixed `PATH`. The copy matters. The worker runs as uid
`10001` against a projection that carries no Git metadata, so mounting the live
checkout both writes build output the operator may not own and hides a command
that depends on `.git`.

The copy is created inside the checkout, because a Docker daemon can bind-mount
only the host paths it shares. A macOS daemon in a virtual machine commonly
shares `$HOME` and nothing else, so a system temporary directory is refused.

~~~sh
cd <TARGET_REPO_PATH>
worktree="$(mktemp -d "$(pwd)/.factory-proof.XXXXXX")"
git archive HEAD | tar -x -C "$worktree"
mkdir "$worktree.git"
# The container writes as uid 10001, which does not own this copy.
chmod -R a+rwX "$worktree"
docker run --rm --pull=never \
  --cap-drop ALL --security-opt no-new-privileges \
  --mount "type=bind,src=$worktree,dst=/work" \
  --mount "type=bind,src=$worktree.git,dst=/git,readonly" \
  --workdir /work \
  <IMAGE_NAME>@<DIGEST> /usr/bin/env -i \
    HOME=/home/factory \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    TERM=xterm-256color \
    LANG=C.UTF-8 \
    LC_ALL=C.UTF-8 \
    FACTORY_RUN_ID=gate-proof \
    GIT_DIR=/git \
    GIT_WORK_TREE=/work \
    GIT_CONFIG_NOSYSTEM=1 \
    GIT_CONFIG_GLOBAL=/dev/null \
    GIT_CONFIG_SYSTEM=/dev/null \
    GIT_CONFIG_COUNT=3 \
    GIT_CONFIG_KEY_0=remote.origin.url \
    GIT_CONFIG_VALUE_0=disabled://factory \
    GIT_CONFIG_KEY_1=remote.origin.pushurl \
    GIT_CONFIG_VALUE_1=disabled://factory \
    GIT_CONFIG_KEY_2=credential.helper \
    GIT_CONFIG_VALUE_2= \
    GIT_TERMINAL_PROMPT=0 \
    GIT_ASKPASS=/bin/false \
    SSH_ASKPASS=/bin/false \
    FACTORY_INVOCATION_DIR=/invocation \
    FACTORY_RESULT_DIR=/results \
    FACTORY_REPORT_COMMAND=/usr/local/bin/factory-report \
    /bin/sh -c 'npm ci && npm run lint && npm test && npm run build'
rm -rf "$worktree" "$worktree.git"
~~~

This is the complete environment the worker supplies under
`environment_policy: clean`, and it is the same shape `scripts/build-worker.sh`
uses to prove this repository's own gates. The `role` policy adds exactly one
entry, `FACTORY_ROLE`, plus any explicit values the invocation passes.

`/git` is mounted because a run always has it, and the Git variables point every
Git command at it. The empty directory used here is the honest lower bound: it
proves a gate does not silently depend on the host's `.git`, and it makes a
Git-reading gate fail in the proof rather than in the first run. A gate that
legitimately reads history needs a projection with real Git metadata, which only
the coordinator builds; treat such a gate as unproven until a disposable issue
has exercised it.

Remove the copy before the next proof. A gate that globs the whole tree would
otherwise walk the dependencies the previous proof installed inside it. Ignore
`.factory-proof.*` in the target repository so an interrupted proof cannot reach
a commit.

A command that needs an environment variable the worker does not supply, or a
tool that is not on the fixed `PATH`, fails here rather than in the first
baseline suite of the first run. Repair the image definition, rebuild, and
record the new digest.

A run uses the ordinary bridge network, so a setup command may install
dependencies from a registry. It reaches Git through a read-only projection at
`/git` that carries history, refs, and the run worktree state but no remotes, no
hooks, and no Git configuration. A command that reads Git configuration, pushes,
or fetches will not behave the same way in a run.

## Step 6: hand the host steps back to the operator

The coordinator binaries come from the Software Factory checkout; `make install`
places them on the operator's Go bin path. Print these commands and let the
operator run them from the target checkout:

~~~sh
factory init
factory register
factory bootstrap-labels
factory doctor
~~~

`factory register` infers the repository path, the GitHub owner and repository
from the `origin` remote, and the authorized user from the authenticated `gh`
account. See [Quick start](configuration.md#quick-start) for the explicit
fallback flags.

The skill smoke runs once per worker digest, from the Software Factory checkout,
writing its evidence into the target repository:

~~~sh
cd <SW_FACTORY_CHECKOUT>
EVIDENCE_FILE=<TARGET_REPO_PATH>/worker/skill-smoke.json \
WORKER_IMAGE=<IMAGE_NAME> \
WORKER_DIGEST=<DIGEST> \
  scripts/smoke-skills.sh
~~~

It reads the operator's Codex and Claude Code credential files, invokes each
harness in the image once per mandated skill, and records the result keyed by
the digest and the harness version. Startup diagnosis reads that record instead
of repeating the paid call, and a new digest invalidates it.

The diagnosis checks both shipped harnesses whatever the repository declares in
`role_harness_defaults`, because a repository may later assign any role to any
supported harness. A Claude-only repository therefore still needs a recorded
Codex result, and the smoke needs both credential files to produce one. When
only one credential exists, the smoke reports the missing harness and exits
nonzero, and the diagnosis keeps blocking that harness.

The smoke looks for `~/.codex/auth.json` and `~/.claude/.credentials.json`, and
`CODEX_AUTH_PATH` and `CLAUDE_AUTH_PATH` override those defaults. A macOS
operator whose Claude Code credential lives in the login Keychain has no file at
the default path, and the smoke reports Claude as unrecorded until one is
supplied.

## Verifying the result

`factory doctor` runs the startup diagnosis, the pre-claim report that decides
whether a run may start. It reports configuration, GitHub authentication
and permissions, the factory labels, the checkout's remote and worktree support,
Docker, the pinned worker image, both harness executables, harness capabilities,
the headless worker helper, harness authentication, and SQLite. It runs every
check even after a failure, so a configuration finding is visible alongside the
others.

The `configuration` check currently reports an invalid `factory.yaml` as
`checked-in repository configuration is missing or invalid` without naming the
field, so the field reference above is the pre-flight audit rather than an
after-the-fact debugging aid. Issue #189 tracks surfacing the typed error.

Once the diagnosis is ready, prove the setup with one disposable issue before
trusting the configuration on real work. Follow the [end-to-end
demonstration](configuration.md#end-to-end-demonstration).

## Keeping it valid

- A changed skill, harness version, or toolchain needs a rebuild, a new digest
  in `factory.yaml`, and a new skill smoke record.
- A changed verification command needs the same proof in Step 5 before the gate
  list changes.
- A new role in `role_harness_defaults` needs the matching `model_options`
  entry, and the reasoning-effort values of that role's harness.
