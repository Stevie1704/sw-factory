# Setup

This topic prepares a host and one target repository for factory runs.
Apply `factory guide authorization` before acting: use existing explicit
session scope for covered steps and request only missing decisions.

## Prerequisites

The host needs:

- the `factory` command on `PATH`, from a release archive (see "Install a
  release" below);
- Git and `jq`;
- a running Docker daemon whose server platform is `linux/arm64` or
  `linux/amd64`, and that uses the containerd image store;
- the GitHub CLI, authenticated for the target repository;
- Codex or Claude Code, installed and signed in.

Check them before any other step. For a failed check, name the missing
prerequisite and its supported remedy, such as installing Git, jq, Docker, or
the GitHub CLI, starting Docker, or running `gh auth login`. Perform an
explicitly authorized installation within environment permissions. Interactive
login or consent remains a named human step; continue after it succeeds.

```sh
factory version --json
command -v factory git jq
docker version --format '{{.Server.Os}}/{{.Server.Arch}}'
docker info --format '{{.DriverStatus}}'   # must show io.containerd.snapshotter.v1
gh auth status
```

`factory-report` and `factory-worker-headless` are worker-only helpers. They
ship in the worker base image, so the host does not need them.

## Install a release

A release publishes one archive per supported host platform:
`factory_<version>_<os>_<arch>.tar.gz` for `darwin/arm64`, `darwin/amd64`,
`linux/arm64`, and `linux/amd64`. It also publishes `checksums.txt` and
`release.json`. Verify the archive against `checksums.txt`, extract it to any
directory, and put its `bin` directory on `PATH`. No Go toolchain and no
Software Factory checkout are needed. Windows and other platforms are not
supported.

`factory version --json` names the release in `version` and the worker base
image in `worker_base_image`. That image is pinned by digest and published for
`linux/arm64` and `linux/amd64`. A development build reports
`"build": "development"` and an empty `worker_base_image`.

## Where each setting lives

- **Repository policy** is the checked-in `factory.yaml` in the target
  repository, and the worker image definition that `factory.yaml` pins by
  digest. It declares the target branch, setup command, gates, role harness
  and model policy, budgets, and cache names. It never names a host path.
- **Host configuration** is private to the host. `factory init` creates it,
  and `factory register` adds the repository, the GitHub identity, the
  authorized users, and the credential source paths. Never check it in.
- **Operational state** is the SQLite store and the run artifacts. Only
  `factory` commands change it.

## Prepare a repository with an agent

An agent that already works in the Git checkout of the repository to prepare
does the onboarding itself. It needs no second agent session. Choose a new
directory below the home directory within the requested task, then run:

```sh
factory onboard prepare --output "<directory>" --json
```

The command writes the release resources to that directory and prints a JSON
manifest. It starts no harness, makes no network call, and needs no host
registration. Take every value from the manifest:

- `factory_command` is the exact `factory` executable of this release. Run
  every later factory command as this path. A `factory` on `PATH` can be
  another build.
- `procedure` is the complete repository initialization procedure. Read it
  in full and follow it from the first step to the last. It names
  `<FACTORY_COMMAND>`, `<FACTORY_RESOURCES>`, and `<FACTORY_BASE_IMAGE>`;
  they are `factory_command`, `resources`, and `worker_base_image`.
- `resource_identity` identifies the exact resources.

The procedure covers the field rules of `factory.yaml`, the worker image
definition, the worker skills, the image build and digest, the gate proof, the
host registration, and the skill smoke. Two commands work before host
registration: `factory config validate --file factory.yaml --json` checks the
policy offline, and `factory onboard verify --repository <checkout> --json`
proves setup and the gates in the pinned worker without a paid model call. Pulling the worker base image,
checking GitHub, and the paid skill smoke need network access. Check those
effects against the authorized scope before executing them.

The directory must be missing, empty, or an earlier preparation of the same
release. Prepare refuses every other directory and never overwrites a file. The
directory stays after the session; the operator removes it when the onboarding
is complete. Never refer to it from a committed file.

The release also ships the optional `factory-onboarding` skill below
`operator/skills/` in the prepared directory. The procedure's section
"Starting the onboarding" names where each harness finds it. Install it only
when the operator asks.

A development build names no worker base image. Then `factory onboard
prepare` stops and asks for `--worker-base-image <image>@sha256:<digest>`.
Ask the operator for that value, and never use a mutable tag.

`factory onboard`, without `prepare`, is the alternative for an operator who
has no agent session yet. It prepares the same resources in a temporary
directory and starts an interactive Claude Code session (`--harness codex`
selects Codex) that follows the same procedure.

Prepare a new directory to rebuild the worker image after an upgrade, or when
the toolchain or a harness version changes.

An onboarding can stop at any step. `factory onboard plan --repository
<checkout> --output <file> --json` keeps an onboarding plan outside the
checkout, and `factory onboard status --plan <file> --json` shows, read-only,
which steps are verified, pending, blocked, stale, or skipped. After an
interruption, read the status first and do not repeat a verified step.
`factory onboard record --plan <file> gate-proof|doctor|live-proof --json`
records the evidence that only a supported command can produce. The
procedure's section "Keep the onboarding plan" describes the plan. Its optional
`authorization_note` records the stated scope and limits, following
`factory guide authorization`; it does not grant authority.

## Register the host

Run these commands in the target checkout when registration and label creation
are covered by the session scope:

```sh
factory init
factory register --codex-auth <codex-auth-path> --claude-auth <claude-auth-path>
factory bootstrap-labels
factory doctor
```

`factory register` infers the repository path, the GitHub owner and name from
the `origin` remote, and the authorized user from the `gh` account. It never
infers a credential path. The operator confirms every credential path. Omit
the flag of a harness that has no credential file. Never read a credential
file.

`factory bootstrap-labels` creates the six factory labels in the GitHub
repository. `factory doctor` runs the complete startup diagnosis. Repeat the
named action and `factory doctor` until it prints `doctor: ready`.

## Start supervision

```sh
factory start            # poll GitHub until stopped; add --verbose for a progress log
factory issue <number>   # claim one agent-ready issue now
factory ui               # local web view and session commands on 127.0.0.1
factory stop             # stop the polling coordinator
```

An authorized user adds the `agent-ready` label to an issue. The coordinator
claims it, runs the roles, gates, and reviews, and opens a draft pull request.
Prove a new setup with one disposable issue before real work.

## Agent discovery in a target repository

Agents that open the target repository find Factory through its agent
instruction file, for example `AGENTS.md` or `CLAUDE.md`. Add this section to
the end of the existing file, or create the file when none exists. Do not
replace or reword other content of that file. Prepare the change within the
requested task; commit when the session scope covers it.

```markdown
## Software Factory

This repository is prepared for Software Factory runs. Run `factory guide` for
orientation and `factory version` for the installed release. Factory owns
issue claims, run branches, labels, and pull requests for its runs.
```

## Keep the setup valid

- A changed toolchain, harness version, or worker skill needs a new worker
  image, a new digest in `factory.yaml`, and a new skill smoke record.
- A new Factory release needs a worker image built from its own worker base
  image. Until then, `factory doctor` reports `worker.release.mismatch`.
- A changed gate command needs a proof in the worker before the change.
- Run `factory doctor` after every change to the host or the repository
  policy.
