# Setup

This topic prepares a host and one target repository for factory runs. Ask
the operator before each step that writes host configuration, changes GitHub,
or commits to the repository.

## Prerequisites

The host needs:

- the `factory`, `factory-report`, and `factory-worker-headless` commands on
  `PATH`;
- Git;
- a running Docker daemon;
- the GitHub CLI, authenticated for the target repository;
- Codex or Claude Code, installed and signed in.

Check them before any other step. When a check fails, stop and tell the
operator what is missing. Do not install software or sign in for the
operator.

```sh
factory version
command -v factory factory-report factory-worker-headless
docker info
gh auth status
```

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

In this release, that procedure, the worker skills, the worker image
definitions, and the build scripts come from the Software Factory source
checkout. A binary built with `make install` records that checkout. Otherwise
pass `--factory-checkout <path>`. When neither is available, stop and ask the
operator for the checkout.

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
- A changed gate command needs a proof in the worker before the change.
- Run `factory doctor` after every change to the host or the repository
  policy.
