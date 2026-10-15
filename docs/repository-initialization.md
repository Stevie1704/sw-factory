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

## Starting the onboarding

Before you start, make sure that:

- a Software Factory release is installed (see [Install a Software Factory
  release](installation.md));
- the Docker daemon runs on a supported worker platform, `linux/arm64` or
  `linux/amd64`;
- `jq` is on `PATH`;
- the GitHub CLI is authenticated (`gh auth status`);
- Claude Code or Codex is installed and signed in on the host.

The procedure runs in an agent session in the checkout of the repository to
onboard. There are two ways to start it. Both use the same preparation of the
release resources and this same procedure.

### With the agent that already runs

An agent that already works in the target repository prepares the release
resources itself and then follows this document. It starts no second harness.
Choose a new directory below your home directory, because a VM-backed Docker
daemon can mount only paths there:

~~~sh
factory onboard prepare --output "$HOME/.cache/sw-factory/onboarding-<version>" --json
~~~

The command writes the resources of the installed release to that directory:
this document and its reference documents, the operator onboarding skill, the
curated worker skills, the worked worker definition, and the smoke and
verification scripts. It starts no harness, makes no network call, and needs no
host registration or Factory checkout. It prints this manifest:

| Field | Meaning |
| --- | --- |
| `schema_version` | Version of the manifest shape, now `1` |
| `factory_version`, `factory_revision` | The release and source revision of the build that prepared the directory |
| `factory_command` | Absolute path of that `factory` executable |
| `worker_base_image` | Digest-pinned worker base image of the release |
| `resources` | Absolute path of the prepared directory |
| `procedure` | Absolute path of this document in that directory |
| `resource_identity` | `sha256:` digest of the paths, modes, and contents of all resources |

The agent then reads the file at `procedure` and follows it. In the steps
below, `<FACTORY_COMMAND>`, `<FACTORY_RESOURCES>`, and `<FACTORY_BASE_IMAGE>`
mean `factory_command`, `resources`, and `worker_base_image`.
`<TARGET_REPO_PATH>` means the top level of the target checkout.

The directory must be missing or empty. Prepare accepts a directory again only
when an earlier preparation of the same `resource_identity` completed it and no
resource in it changed. Then it prints the same manifest, for example after an
interrupted agent session. It refuses every other directory, also one with
another release's resources. It never overwrites or removes a file there.
It exports into a hidden `.<name>.partial-*` directory beside the destination
and renames it into place when the export is complete. An interrupted export
leaves the destination unchanged; remove the hidden directory.

The directory belongs to the operator. It stays after the command and after
the agent session end. Remove it when the onboarding is complete, and prepare a
new one for a later rebuild. No committed file may refer to it.

The release also ships the `factory-onboarding` skill at
`<FACTORY_RESOURCES>/operator/skills/factory-onboarding/SKILL.md`. It tells an
agent to find the installed `factory`, prepare the resources, and follow this
document. Install it only when you want it, and only where you choose:

| Host agent | Skill location |
| --- | --- |
| Claude Code | `.claude/skills/factory-onboarding/` in the target repository, or `~/.claude/skills/factory-onboarding/` for all repositories |
| Codex | `~/.codex/skills/factory-onboarding/` |
| Agent without skill support | none: tell the agent to read the `SKILL.md` file and follow it as plain instructions |

Copy the complete `factory-onboarding` directory, for example:

~~~sh
cp -R "<FACTORY_RESOURCES>/operator/skills/factory-onboarding" ~/.claude/skills/
~~~

Factory never installs the skill itself. The skill holds no procedure, model
list, or credential. An older copy of the skill still prepares the resources
with the installed release and uses that release's manifest and procedure.

### With an interactive launcher

`factory onboard`, run from the checkout of the repository to onboard, starts
a new interactive Claude Code session for the procedure. Use `--harness codex`
to start Codex instead:

~~~sh
factory onboard
~~~

The command prepares the resources as `factory onboard prepare` does, in a new
directory below the user cache directory. The first message of the session
names the target repository, that resource directory, the procedure file, and
the worker base image of the release. It also names `<FACTORY_COMMAND>`, the
absolute path of the `factory` executable that started the session. The
placeholders below mean those values. The command removes the directory when
the session ends.

### Rules for both ways

`<FACTORY_BASE_IMAGE>` is always pinned as `image@sha256:digest`.

Run every `factory` command of this procedure as `<FACTORY_COMMAND>`. The
agent shell can load a login profile that puts another `factory` build first
on `PATH`, for example a development build from `make install`.

The session must be able to use the resource directory, because the procedure
copies skills and runs scripts from it. Docker and network commands can need
access outside the harness sandbox. The harness then asks you to approve them.

A development build from source names no released worker base image. Pass the
base image that `make worker-build` built, pinned by digest, with
`--worker-base-image` to `factory onboard prepare` or `factory onboard`; see [Build from
source](installation.md#build-from-source).

The agent runs every step of this procedure itself. You approve the actions
listed in the next section and answer its questions.

The launcher prompt and the `factory-onboarding` skill are deliberately short.
This document owns the steps and the approval list, so they do not repeat
them. A second copy could drift.

## Keep the onboarding plan

An onboarding can stop at any step, for example when the session ends after
the image build. A persisted plan lets the next agent find what is done, what
is still valid, and what remains, without a new image build, a new paid
smoke call, or a second proof issue. Create the plan after the preparation,
outside the target checkout:

~~~sh
<FACTORY_COMMAND> onboard plan \
  --repository "<TARGET_REPO_PATH>" \
  --output "$HOME/.cache/sw-factory/onboarding-<repository name>.json" \
  --resources "<FACTORY_RESOURCES>" \
  --json
~~~

Use this path, so that a later agent finds the plan. Add `--config <path>`
when the registration uses a separate host
configuration. The plan keeps the repository, the host configuration path,
the resource directory, and the recorded evidence. It never keeps a
credential, a credential path, or a harness transcript. The command refuses a
plan file inside the checkout and a plan of another repository. Run it again
to refresh the plan; a value that you do not give again stays.

After an interruption, read the plan before any other step. The command is
read-only:

~~~sh
<FACTORY_COMMAND> onboard status --plan "<PLAN>" --json
~~~

The plan describes work. It does not run steps and it does not replace the
coordinator's operational store. Each step still runs with the command that
this procedure names. The status document lists the steps in procedure order:

| Step | Evidence | Depends on |
| --- | --- | --- |
| `prepare` | the resource directory still holds the resources of this release | — |
| `configuration` | `factory.yaml` passes `config validate` | `prepare` |
| `image` | the pinned image exists locally, and its build context did not change since it was first observed | `configuration` |
| `gate_proof` | `onboard record gate-proof` | `image` |
| `registration` | the host configuration registers this checkout | `configuration` |
| `skill_smoke` | `worker/skill-smoke.json` covers every required skill of each harness the policy can select, for the pinned digest | `image` |
| `doctor` | `onboard record doctor` | `gate_proof`, `registration`, `skill_smoke` |
| `live_proof` | `onboard record live-proof` | `doctor` |

Each step has a `state`:

- `verified`: valid evidence proves the step. Do not do it again.
- `pending`: do the step now. `reason` tells why and `action` names the
  command.
- `blocked`: the step waits for a dependency, for an observation that is not
  possible, such as a stopped Docker daemon, or for an unknown outcome.
- `stale`: evidence exists, but an input it depends on changed. `reason`
  names each changed input.
- `skipped`: the operator declined the optional live proof.

`operator_choice` names the decision that a step needs from the operator.
`result` is `incomplete`, `doctor_ready`, `live_proof_passed`, or
`live_proof_declined`. `artifacts` names the resource directory, the
directory of interrupted gate proofs, and the marker of a started live
proof.

Evidence records the identity of each input it depends on: the release, the
`factory.yaml` content, the pinned image, the worker build context, which
holds the worker definition and the vendored skills, the candidate files, the
host registration, and the smoke records of the pinned digest. A change makes
only the evidence that depends on that input stale. A recorded skill smoke
stays valid while the pinned digest is the same. When some skills are missing,
the `skill_smoke` action repeats only those paid calls.

Only supported commands record evidence. You cannot mark a step complete with
text. Record the steps that a cheap observation cannot prove:

~~~sh
<FACTORY_COMMAND> onboard record --plan "<PLAN>" gate-proof --json
<FACTORY_COMMAND> onboard record --plan "<PLAN>" doctor --json
~~~

`gate-proof` runs the Step 5 proof and records it. It does not run again
while its evidence is valid; then the document shows `"reused": true`.
`doctor` runs the startup diagnosis with the plan's host configuration and
records the codes of the blocking findings. Each command writes one JSON
document with the `report` that produced the evidence and the new `status`.
It exits `0` only when the recorded step passed.

The live proof of Step 8 creates an issue. Record its identity before the
issue exists, so a lost response cannot create a second proof issue:

~~~sh
<FACTORY_COMMAND> onboard record --plan "<PLAN>" live-proof --start --json
~~~

The command needs a verified `doctor` and prints a `marker`. It records the
inputs at the start, and a passed proof is valid only for these inputs. Put
the marker in the body of the proof issue. Then record the issue:

~~~sh
<FACTORY_COMMAND> onboard record --plan "<PLAN>" live-proof --issue <NUMBER> --json
~~~

The command reads the issue and its newest run. It refuses an issue whose body
does not carry the marker and records nothing when it cannot observe the
issue. The proof passes when the run reaches `draft_pr`. Until then, the
command exits `1`; run it again to observe the run. After a start, the
outcome is unknown until you record the issue. Find the issue that carries
the marker; never create another one. Only when the operator confirms that no
issue carries the marker, remove the start with `--abandon`. When the
operator declines the proof, record it with `--decline`.

Plan writes are atomic and serialized by a lock file beside the plan. A
second writer waits and then starts from the latest plan. A record command
observes the inputs again while it holds the lock. It records nothing, and
still prints its report, when an input changed or could not be observed
while it ran, or when another writer recorded newer evidence of the same
step. So no writer overwrites another writer's progress.

## Actions that need operator approval

These actions change the host configuration or GitHub, or cost money:

- `factory init` and `factory register` write host configuration and create the
  operational store.
- `factory bootstrap-labels` creates six labels in the GitHub repository.
- `<FACTORY_RESOURCES>/scripts/smoke-skills.sh` streams a host harness credential into a container
  and makes one real, paid model call per mandated skill per harness.
- The export of a Keychain credential to a temporary file for the smoke.
- The end-to-end demonstration in Step 8. It creates an issue, a branch, and a
  draft pull request, and it starts paid agent sessions.
- Any commit, push, issue change, or pull request in the target repository.

The agent shows each command, asks, and runs it after the operator approves.

Docker builds, image checks, and gate proofs in Steps 3 and 5 change only local
images, containers, and disposable copies. They need no approval from this procedure. The
harness can still ask for them under its own permission settings.

The agent never reads a credential file. It can check whether a default
credential path exists. The operator confirms every credential path.

The agent also stops before it writes any file. That is a review point
rather than a boundary: the gate list and the role policy are decisions about
how the repository is verified, and they are cheaper to correct before the file
exists.

## Step 0: check the prerequisites

Run these checks before any other step:

~~~sh
<FACTORY_COMMAND> version --json
docker info
docker version --format '{{.Server.Os}}/{{.Server.Arch}}'
gh auth status
command -v git jq
~~~

`<FACTORY_COMMAND> version --json` must report a `worker_base_image` that
equals `<FACTORY_BASE_IMAGE>`. Only when the operator passed
`--worker-base-image` to a development build can it be empty. In all other
cases, an empty or different value means that the command is not the build
that prepared the resources: stop and tell the operator. The Docker server platform must be `linux/arm64` or
`linux/amd64`. The release publishes no worker base image for another
platform, so stop and tell the operator that the platform is not supported.
The host does not need `factory-report` or `factory-worker-headless`. Both
ship in the worker base image.

`factory guide` prints the installed release's orientation, and `factory guide
setup` its setup topic. Read both before Step 1.

The harness that runs this session needs no check. The worker image carries
both harnesses, and `factory doctor` checks them inside the image, not on the
host. When a check fails, stop and tell the operator what is missing. Do not
install or sign in on the operator's behalf.

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

The factory base image `<FACTORY_BASE_IMAGE>` is the base image of the
installed release, pinned by digest. The release built it from
`worker/base.Dockerfile` of the same source revision, for `linux/arm64` and
`linux/amd64`. It carries the pinned Codex and Claude Code releases,
`factory-report`, `factory-worker-headless`, Git, the `factory` user with uid
`10001`, and the stable worker paths. It is built on `node:22-bookworm-slim`,
so `node` and `npm` are already on the fixed `PATH`; no other language runtime
is. Its label `io.github.stevie1704.sw-factory.release` names the release, and
the repository worker image inherits it.

The base image does not carry the worker skills. Each repository worker image
bakes them, so they are pinned by the digest the repository records. Vendor them
from the resources of the same release:

~~~sh
mkdir -p "<TARGET_REPO_PATH>/worker"
rm -rf "<TARGET_REPO_PATH>/worker/skills"
cp -R "<FACTORY_RESOURCES>/worker/skills" "<TARGET_REPO_PATH>/worker/skills"
~~~

Keep the quotes: the resource directory or the target path can contain
spaces.

The removal is part of the command, not a precaution. Copying a directory onto
an existing directory of the same name nests it as `worker/skills/skills` and
leaves the skills the image actually bakes untouched.

Re-vendor whenever the base image changes, which includes every Factory
upgrade. The smoke script reads its expected skill text from
`<FACTORY_RESOURCES>` and its actual text from the image, so vendored skills
from another release produce a smoke result that proves nothing.
`<FACTORY_RESOURCES>/worker/SKILLS.md` records the provenance of each skill.

A repository whose toolchain is already in the base image needs only the skill
layer:

~~~dockerfile
# worker/Dockerfile — Node repository.
ARG FACTORY_BASE_IMAGE=<FACTORY_BASE_IMAGE>

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

Write the `<FACTORY_BASE_IMAGE>` value, with its digest, as the `ARG` default.
Never use a mutable tag such as `:v1` or `:latest`: the repository image must
build from the base image of the installed release.

`<FACTORY_RESOURCES>/worker/Dockerfile` is the worked example for a compiled
toolchain. It is Software Factory's own worker definition, and its `ARG`
default is the development base tag; do not copy that default. Keep the build
context at the `worker` directory, so the `skills` source path stays stable.

## Step 3: build and verify the image

Pull the base image of the release by its digest. The pull needs network
access to `ghcr.io`. Docker selects the manifest of the daemon's platform:

~~~sh
docker pull <FACTORY_BASE_IMAGE>
~~~

A development base image that `make worker-build` built is already local;
skip the pull for it.

The repository worker image then builds from the target checkout without
pulling that base again:

~~~sh
cd "<TARGET_REPO_PATH>"
docker build --pull=false \
  --build-arg FACTORY_BASE_IMAGE=<FACTORY_BASE_IMAGE> \
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

Extend the command list with every toolchain command the gates call. Then
verify the headless lifecycle of the new image with the release's script:

~~~sh
"<FACTORY_RESOURCES>/scripts/verify-headless-worker.sh" <IMAGE_NAME>@<DIGEST>
~~~

The script runs offline fake harnesses through the real worker helpers. Its
Go command lifetime and resource limit checks need the Factory source, so it
skips them here; the release ran them against the same base image.

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
| `caches` | A list of `name` and `read_only` entries with unique names. A `path` is rejected: host configuration supplies it. See the caches section below. |
| `evaluation.retention` | A positive Go duration, or omitted. |

### Gates

Each gate declares `name`, `command`, `timeout`, `blocking`, and
`environment_policy`. Names and commands are nonempty, names are unique,
`timeout` is a positive Go duration, and `environment_policy` is `clean` or
`role`. `depends_on` may reference earlier gates
only, never the gate itself, so the list is an ordered, acyclic sequence. A
blocking gate stops the checkpoint; a non-blocking gate reports and continues.
Independent gates still run after an earlier failure.

A non-blocking (`blocking: false`) gate is advisory at two points:

- Suite evaluation: a failing or timed-out non-blocking command does not fail
  the suite, so it does not start check repair or spend the repair budget.
- Final readiness: a failed non-blocking gate command does not prevent
  readiness. Every blocking gate must pass.

The advisory failure stays visible. The factory keeps the `failure` status on
the exact checkpoint, the failed outcome in the stored result, and the gate in
the generated pull-request summary. These rules continue to apply:

- A blocking gate that depends on a failed non-blocking gate is skipped, and
  that skip blocks the suite and readiness.
- A setup failure, a worker execution error, an unconfirmed command
  termination, or a skipped non-blocking gate is not an advisory command
  failure. Each still blocks readiness.
- Readiness rejects a missing, duplicate, or mismatched result for every gate,
  blocking or not. The result must match the run, checkpoint, phase, declared
  order, blocking policy, and setup fingerprint.

### Roles

The factory declares six roles. `implementation`, `spec_review`, and
`standards_review` are the base set. `test`, `architecture`, and `pr_writer`
are optional. Supported harnesses are `codex` and `claude`.

- `role_harness_defaults` and `model_options` must cover the same roles.
- `test_policy.mode: required` makes the `test` role mandatory in both maps.
- An issue that selects the `acceptance` route needs the `test` role, and
  `design-acceptance` needs `architecture` and `test`. A claim naming an
  undeclared role is refused with `route_unavailable`.

Start with `implementation`, `spec_review`, and `standards_review` in advisory
test mode. Add `test` when the repository is ready to make the independent test
role mandatory, and `architecture` when issues should be able to select the
design route. Add `pr_writer` when an agent should write a short summary for
the human reviewer into each pull request; see [Optional PR
writer](configuration.md#optional-pr-writer).

Before you write `factory.yaml`, ask the operator which optional roles to
declare, and which harness, models, and reasoning effort each declared role
uses. Do not decide this for the operator. A declared `pr_writer` adds the
`pr` skill to the smoke in Step 7 for the harness that runs it.

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

Three independent booleans sit beside the mode. Each defaults to `false`, and
each opens exactly one door:

| Field | What `true` permits | When it is consulted |
| --- | --- | --- |
| `allow_human_exemption` | A human skips the test stage for one issue, with the frozen issue marker `<!-- factory-test-exemption: human \| justification -->`. | Only in `required` mode, and never when the issue selects a route that runs the test role. A selected route is a deliberate request for independent evidence and overrides the marker. |
| `allow_technical_exemption` | The test role reports a technical exemption instead of a test handoff. | Whenever a test-role report arrives, so also under `advisory` mode when a route runs the test stage. Without it the report is refused. |
| `allow_automated_objections` | The implementation role objects to a test automatically, which resumes the test session for up to `retry_limits.test_revision` revisions. | Whenever an objection is reported. While `false`, the objection is persisted and the run waits for a human. |

`allow_automated_objections` is the repository-owned authorization for the
bounded implementation-to-test revision cycle. Declare it `false` by default;
changing it is a deliberate repository-policy decision. The frozen value in the
specification packet controls the run, and a disabled cycle preserves the
objection for human disposition.

Declare all three explicitly, so the policy a run applies is readable in the
file rather than inferred from an absent key.

### Caches

`caches` declares a cache by name and container-side access mode only. The
worker mounts it at `/cache/<name>`, and `read_only` states whether the worker
may update it. The host directory is not repository policy: the registration
maps each declared name to a directory under its `cache_root`, so a committed
file never pins one operator's home directory.

Every declared cache needs that host mapping before a run can start. Add
`cache_root` and the `caches` mapping to the host registration in the same
change that declares the cache, or `factory doctor` blocks on the unmapped
name. See [Configuration](configuration.md) for the host-side shape.

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

Replace the placeholder digest with the value Step 3 produced. Software
Factory's own `factory.yaml`, in its source repository, is the worked example
for a compiled toolchain with a mandatory setup step.

## Step 5: prove every command in the worker

A gate is written into `factory.yaml` only after it has run inside the built
image under the environment the worker actually supplies. First validate the
file. The command needs no host registration, Docker, or network:

~~~sh
<FACTORY_COMMAND> config validate --file <TARGET_REPO_PATH>/factory.yaml --json
~~~

It uses the parser and validator of a real run. It exits `0` for a valid file
and `1` for an invalid file. Each entry of `diagnostics` has these fields:

| Field | Meaning |
| --- | --- |
| `code` | `file_unreadable`, `syntax_error`, `unknown_field`, `type_mismatch`, `unsupported_schema`, `repository_policy`, or `invalid_value` |
| `field` | The field path, when it is known |
| `line` | The line in the file, when the parser reports it |
| `message` | The problem, in fixed text |

No diagnostic repeats a value from the file. `completeness` tells how far the
validation got:

- `complete`: the file is valid.
- `not_run`: the file did not parse, so no field rule ran.
- `first_error`: validation stopped at the first failed field rule. Fix it and
  validate again, because the later fields were not checked.

Then prove setup and every gate in the pinned worker:

~~~sh
<FACTORY_COMMAND> onboard verify --repository <TARGET_REPO_PATH> --json
~~~

The command copies the candidate files to a disposable workspace below the
user cache directory. Then it starts the worker that `worker_build` pins. It
runs setup and the gates with the worker runtime and the gate runner of a run.
These are the same as in a run:

- the worker isolation and the fixed `PATH`;
- the `clean` or `role` environment;
- the gate order and the `depends_on` skips;
- the timeouts, the cancellation, and the output capture limit.

The resource limits are the defaults, because the proof reads no host
registration. A tool that is only on the host cannot make the proof pass. The
command claims no issue, starts no agent, and publishes no status. It makes no
paid model call. Setup can need network access. Docker pulls the pinned image
when it is not local.

The candidate files are the tracked files in their working-tree state and the
untracked files that Git does not ignore, so uncommitted onboarding changes are
tested and `factory.yaml` is read from that copy. These files stay out:

- `.git` and every other ignored file, such as dependency and build output;
- an untracked credential file, such as `.env`, `.npmrc`, `*.pem`, or
  `auth.json`;
- a top-level `.factory-proof*` entry that an earlier manual proof left;
- an entry that is not a file or a symbolic link, such as a submodule;
- a path below a directory that is a symbolic link, because its content can
  lie outside the checkout.

`candidate.excluded` lists each excluded path and its reason. The proof never
writes into the checkout. Each declared cache is an empty directory that the
proof removes afterwards, so the proof runs with cold caches.

The report fields:

| Field | Meaning |
| --- | --- |
| `schema_version` | Version of the report shape, now `1` |
| `result` | `passed`, `failed`, `unavailable`, or `interrupted` |
| `image_reference` | The worker `image@digest` the proof ran in |
| `candidate` | `fingerprint` of the projected paths, modes, and contents; `files`; `excluded` |
| `git_head` | The checkpoint commit of the candidate that the worker saw at `/git` |
| `evidence_id` | Changes when a candidate file, a gate, the history, the worker digest, or the Factory build changes |
| `setup`, `gates[]` | `outcome`, `exit_code`, and the last 2 KiB of `stdout_tail` and `stderr_tail`; each gate also has `name`, `blocking`, `accepted`, and `skip_reason` |
| `diagnostics` | Why no proof ran, when `result` is `unavailable` |
| `cleanup` | `status` is `removed`, or `incomplete` with the remaining `workspace` |

An outcome is `passed`, `failed`, `timed_out`, `output_limit_exceeded`,
`out_of_memory`, `termination_unconfirmed`, `error`, `interrupted`, `skipped`,
or `setup_failed`. `accepted` applies the final-readiness rule of a run: a gate
passes, or an advisory gate fails or times out. The result is `failed` when
setup does not pass or a gate is not accepted. The command exits `0` only for
`passed`.

The worker sees Git metadata at `/git` that has the same shape as in a run. It
holds the history and the refs of the checkout, but no Git configuration, no
remotes, and no hooks. The proof adds one checkpoint commit of the candidate
files on top of `HEAD`, so `git status` is clean, as at a run checkpoint. The
commit exists only in the workspace. Host Git configuration is not read for
it, so no host-defined filter runs on candidate files. As in a run, a gate
cannot fetch, push, or read the Git configuration of the checkout.

A command that needs an environment variable the worker does not supply, or a
tool that is not on the fixed `PATH`, fails here rather than in the first
baseline suite of the first run. Repair the image definition, rebuild, record
the new digest, and prove again. A proof is valid only for its `evidence_id`.

The command removes its worker, its role storage, and its workspace when it
ends, also after an interrupt. Do this when the process was killed or when
`cleanup` is `incomplete`. It removes the leftovers of every proof that no
longer runs:

~~~sh
<FACTORY_COMMAND> onboard verify --cleanup --json
~~~

A successful proof does not complete the onboarding. `factory doctor` after
Step 6 is still the final check of the host, the providers, and the runtime.

With an onboarding plan, run the proof with `<FACTORY_COMMAND> onboard record
--plan "<PLAN>" gate-proof --json` instead. It runs the same proof and records
the result.

## Step 6: register the host

The coordinator is `<FACTORY_COMMAND>`. A host configuration registers one
repository. First check whether the default configuration already exists:

~~~sh
<FACTORY_COMMAND> init --help    # prints the default configuration path
~~~

When a file exists at that path, it already registers another repository. Do
not change it. Ask the operator for a separate configuration path, for example
`~/.config/factory-<repository>/config.yaml`, and an operational data path
next to it. Export the path as `FACTORY_CONFIG` before the commands below, and
pass `--operational-data` to `factory register`. Every later `factory` command
of this repository then needs the same `FACTORY_CONFIG` or `--config`. Tell the
operator so.

Ask the operator for approval, then run these commands from the target
checkout:

~~~sh
<FACTORY_COMMAND> init
<FACTORY_COMMAND> register \
  --codex-auth <CODEX_AUTH_PATH> \
  --claude-auth <CLAUDE_AUTH_PATH>
<FACTORY_COMMAND> bootstrap-labels
~~~

`factory register` infers the repository path, the GitHub owner and repository
from the `origin` remote, and the authorized user from the authenticated `gh`
account. The authentication paths are never inferred. Replace each credential
placeholder with a credential file that the operator confirmed. Omit the option
when that harness has no host-side file, for example a Claude Code credential
in the macOS login Keychain. See [Quick start](configuration.md#quick-start)
for the explicit fallback flags and the `--update` command for an existing
registration.

`factory doctor` checks a registered credential file offline. It cannot find
out whether the provider still accepts the credential. On macOS, Claude Code
keeps its current credential in the login Keychain, and
`~/.claude/.credentials.json` can be an old copy whose refresh token no longer
works. Register that file only when the operator confirms that it is current.
A Step 7 smoke that fails with an expired or unrefreshable session shows that
the registered file will fail in a run too.

## Step 7: record the skill smoke

The skill smoke runs once per worker digest, with the release's script,
writing its evidence into the target repository:

~~~sh
cd "<TARGET_REPO_PATH>"
EVIDENCE_FILE="<TARGET_REPO_PATH>/worker/skill-smoke.json" \
WORKER_IMAGE=<IMAGE_NAME> \
WORKER_DIGEST=<DIGEST> \
  "<FACTORY_RESOURCES>/scripts/smoke-skills.sh"
~~~

Always set `WORKER_IMAGE` and `WORKER_DIGEST`. The script reads the expected
skill text from `<FACTORY_RESOURCES>`, the resources of the release that built
the base image.

It reads the operator's Codex and Claude Code credential files, invokes each
harness in the image once per mandated skill, and records the result keyed by
the digest and the harness version. Startup diagnosis reads that record instead
of repeating the paid call, and a new digest invalidates it.

The diagnosis checks both shipped harnesses whatever the repository declares in
`role_harness_defaults`, because the worker image must keep both skill
contracts valid. Recorded smoke evidence is blocking only for harnesses named
by `role_harness_defaults`, or for both supported harnesses when
`allowed_overrides` contains `harness`; an unselected harness with no record is
reported as an advisory finding. A single-harness repository can therefore
smoke only its selected harness by setting `HARNESSES` before running the
script. When only one credential exists, the smoke reports any harness it was
asked to prove without a credential and exits nonzero, while startup blocks
only if that harness is eligible under the repository policy.

The smoke looks for `~/.codex/auth.json` and `~/.claude/.credentials.json`, and
`CODEX_AUTH_PATH` and `CLAUDE_AUTH_PATH` override those defaults. On macOS,
Claude Code keeps its credential in the login Keychain, and a file at the
default path can be old and expired. In that case, export the Keychain
credential to a temporary file, point `CLAUDE_AUTH_PATH` at it, and delete the
file after the smoke. The commands are in [worker
skills](../worker/SKILLS.md), also at `<FACTORY_RESOURCES>/worker/SKILLS.md`. The export needs operator approval, and the agent
does not read the exported file.

The smoke records every skill that loaded, also when another skill fails, and
shows what the harness answered for a skill that did not load. To retry, set
`MANDATORY_SKILLS` to the skills that did not load and `HARNESSES` to the
harness. The retry adds them to the same record, so it needs one paid call per
skill and not one per mandated skill.

Then run `<FACTORY_COMMAND> doctor` from the target checkout, as the next
section describes, until it reports ready. With an onboarding plan, record the
ready diagnosis with `<FACTORY_COMMAND> onboard record --plan "<PLAN>" doctor
--json`.

## Verifying the result

`factory doctor` runs the startup diagnosis, the pre-claim report that decides
whether a run may start. It reports configuration, GitHub authentication
and permissions, the factory labels, the checkout's remote and worktree support,
Docker, the pinned worker image, the release of the worker image, both harness
executables, harness capabilities,
the headless worker helper, harness authentication, and SQLite. It runs every
check even after a failure, so a configuration finding is visible alongside the
others.

Run it from the target checkout. It reads the default host configuration that
`factory init` created; pass `--config` only when the registration lives
somewhere else:

```sh
factory doctor
```

An invalid `factory.yaml` makes the `configuration` check name the first
invalid field and the reason, for example:

```text
doctor: configuration: failed
problem: checked-in repository configuration is invalid: model_options.test: must declare at least one model
action: repair the repository factory.yaml and its declared workflow policy
```

Repair the named field, then run the doctor again. Repeat until the check
passes. Each run reports only the first invalid field. The finding never shows
the supplied value. An unknown role key reports only its parent map.

YAML syntax errors, type errors, unknown fields, and read errors keep the
generic problem `checked-in repository configuration is missing or invalid`.
For these errors, compare the file with the field reference above.

The doctor does not validate `factory.yaml` alone. It needs the host
registration, and it runs the full startup diagnosis.

## Step 8: prove the setup with a disposable issue

Once the diagnosis is ready, prove the setup with one disposable issue before
trusting the configuration on real work. The proof runs the installed
coordinator on the registration from Step 6. It creates a branch and a draft
pull request in the target repository, and it starts paid agent sessions. Ask
the operator before you start it. When the operator declines, for example
because the target is a production repository, skip the proof.

After approval, with an onboarding plan, record the start with `onboard record
live-proof --start` first and put its marker in the issue body. Record a
declined proof with `--decline`. Then:

1. Create an issue with a small, verifiable change, for example a new test
   case. An authorized user adds the `agent-ready` label.
2. Start the coordinator in its own terminal and keep it running:

   ~~~sh
   <FACTORY_COMMAND> start --verbose
   ~~~

3. Follow the run with `<FACTORY_COMMAND> status` or `<FACTORY_COMMAND> ui`
   until the issue reaches `draft_pr` or a waiting state. A waiting state
   names its reason in the status comment of the issue.
4. Check the result: the draft pull request targets `target_branch`, contains
   the generated factory section with the gate summary, and, when `pr_writer`
   is declared, its summary. The issue has one factory state label and one
   status comment.
5. Stop the coordinator with `<FACTORY_COMMAND> stop`.

Report the pull-request URL and the status-comment URL to the operator. Ask
before you close the pull request or the issue.

The onboarding is complete when this proof succeeds, or when the operator
declines it and `factory doctor` reports ready. With an onboarding plan,
record the issue with `onboard record live-proof --issue <NUMBER>`; the plan
`result` is then `live_proof_passed` or `live_proof_declined`.

## Agent discovery

Point later agents in the target repository at the installed guide. Append the
section that `factory guide setup` shows under "Agent discovery in a target
repository" to the repository's agent instruction file. Keep every other line
of that file unchanged. The commit needs operator approval.

## Keeping it valid

- A changed skill, harness version, or toolchain needs a rebuild, a new digest
  in `factory.yaml`, and a new skill smoke record.
- A new Factory release needs the same rebuild from its own base image, with
  freshly vendored skills. A release coordinator refuses a worker image of
  another release: `factory doctor` reports `worker.release.mismatch` until the
  rebuild. Prepare the new release's resources in a new directory with
  `factory onboard prepare`, or run `factory onboard` again.
- A changed verification command needs the same proof in Step 5 before the gate
  list changes.
- A new role in `role_harness_defaults` needs the matching `model_options`
  entry, and the reasoning-effort values of that role's harness.
