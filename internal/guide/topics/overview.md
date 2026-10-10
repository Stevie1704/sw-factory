# Software Factory guide

Software Factory is a local, supervised coordinator. It turns an authorized
GitHub issue into a review-ready draft pull request. It runs on the operator's
host. It does not merge pull requests. A human decides the result.

This guide ships inside the `factory` binary. It describes the behavior of
the installed release only. It works offline, outside a Git repository, and
without host configuration.

## Topics

| Command | Use it to |
| --- | --- |
| `factory guide` | Read this orientation. |
| `factory guide setup` | Install prerequisites, configure a host, and prepare a repository. |
| `factory guide debug` | Diagnose a failure and select a supported recovery action. |
| `factory guide reporting` | Report a suspected Factory defect. |

## Discover the installed release

```sh
factory version             # one-line build identity
factory version --json      # schema-versioned build identity
factory doctor              # complete startup diagnosis
factory status              # supervisor and current run state
factory <command> -h        # flags of one command
```

`factory version --json` prints schema version 1 with the fields
`schema_version`, `version` (the release, or `development`), `build`
(`release` or `development`), `revision` (the source commit, or `unknown`),
and `modified` (uncommitted source changes at build time).

Every operational command reads the host configuration from
`$FACTORY_CONFIG`, or from `factory/config.yaml` in the user configuration
directory. Use `--config <path>` to select a different file.

## What Factory coordinates

- **Issue**: a work-tracker item. An authorized user marks it `agent-ready`.
  The coordinator claims it and freezes its text as the specification packet.
- **Run**: one supervised execution for one issue. It owns the frozen
  specification, a `factory/<run-id>` branch, a worktree, and at most one
  pull request. One repository has at most one active run.
- **Invocation**: one harness attempt (Codex or Claude Code) for one role in
  a run. A role is implementation, architecture, test, review, or another
  factory-declared role.
- **Worker**: the isolated Docker container of an invocation. It sees only
  the run worktree, read-only Git metadata, declared caches, and
  factory-managed credential copies.
- **Checkpoint**: an immutable commit that the coordinator accepts at a stage
  boundary. Gates and reviews always name one checkpoint.
- **Gate**: a deterministic command that `factory.yaml` declares. Gates run in
  the worker against a checkpoint. A model cannot pass or fail a gate.
- **Review**: two separate axes at each checkpoint. Specification review checks
  the frozen issue. Standards review checks documented repository standards.
- **Human disposition**: the decision that only an authorized human makes. It
  answers clarification questions, decides blocked reviews, and merges or
  closes the pull request. The coordinator pauses a run when it needs one.

## Three kinds of state

| Kind | Where it lives | Who changes it |
| --- | --- | --- |
| Repository policy | `factory.yaml` and the worker image definition, checked in to the target repository | Repository maintainers, through ordinary commits |
| Host configuration | The host configuration file; never checked in | The operator, with `factory init` and `factory register` |
| Operational state | The SQLite operational store, worktrees, run branches, workers, and credential stores | The coordinator only |

A claimed run uses the policy it froze at claim time. A later commit to
`factory.yaml` does not change it.

## Who may change what

The coordinator owns every workflow, Git, and tracker mutation. It moves
stages, commits checkpoints, pushes branches, changes labels and comments,
and opens the draft pull request.

A worker agent changes files only in the run worktree, and only on the paths
its role permits. It cannot use GitHub, push, or read host credentials. It
proposes a result through `factory-report`. The coordinator validates the
proposal before it acts on it.

An operator agent works on the host for the operator. It runs `factory`
commands, reads diagnosis, and follows this guide. It asks the operator
before it changes host configuration, GitHub, or a repository, and before a
paid model call. It is not a worker. It does not do the work of a run role,
and it does not make a human disposition for the operator.

## Rules for every agent

- Use only the commands in this guide and the flags in `factory <command> -h`.
- Never edit the operational store, a worktree of an active run, a run
  branch, or factory labels by hand.
- Never bypass a checkpoint, a gate, or a review. Never change frozen
  authority to continue a run.
- When the cause of a failure is unknown, say so. Do not guess a cause.

Start with `factory guide setup` on a new host, or with `factory guide debug`
when something fails.
