# Setup

This topic prepares a host and one target repository for factory runs. Ask
the operator before each step that writes host configuration, changes GitHub,
or commits to the repository.

## Prerequisites

The host needs:

- the `factory` command on `PATH`, from a release archive (see "Install a
  release" below);
- Git and `jq`;
- a running Docker daemon whose server platform is `linux/arm64` or
  `linux/amd64`, and that uses the containerd image store;
- the GitHub CLI, authenticated for the target repository;
- Codex or Claude Code, installed and signed in.

Check them before any other step. When a check fails, stop and tell the
operator what is missing. Do not install software or sign in for the
operator.

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

Run this command in the Git checkout of the repository to prepare:

```sh
factory onboard
```

It starts an interactive Claude Code session (`--harness codex` selects
Codex). Its first message gives the agent the complete repository
initialization procedure. The procedure covers the field rules of
`factory.yaml`, the worker image definition, the worker skills, the image
build and digest, the gate proof, the host registration, and the skill smoke.

The procedure, the worker skills and their provenance, the worked worker
definition, and the smoke and verification scripts are part of the installed
`factory` binary. `factory onboard` writes them to a new directory below the
user cache directory for the session, and it removes the directory when the
session ends. The worker base image comes from the release: the session pulls
it by its digest. Pulling the image, checking GitHub, and the paid skill smoke
need network access. The procedure asks before each paid or external step.

A development build names no worker base image. Then `factory onboard` stops
and asks for `--worker-base-image <image>@sha256:<digest>`. Ask the operator
for that value, and never use a mutable tag.

Run `factory onboard` again to rebuild the worker image after an upgrade, or
when the toolchain or a harness version changes.

## Register the host

Run these commands in the target checkout, after operator approval:

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
factory ui               # read-only web view on 127.0.0.1
factory stop             # stop the polling coordinator
```

An authorized user adds the `agent-ready` label to an issue. The coordinator
claims it, runs the roles, gates, and reviews, and opens a draft pull request.
Prove a new setup with one disposable issue before real work.

## Agent discovery in a target repository

Agents that open the target repository find Factory through its agent
instruction file, for example `AGENTS.md` or `CLAUDE.md`. Add this section to
the end of the existing file, or create the file when none exists. Do not
replace or reword other content of that file. Ask the operator before the
commit.

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
