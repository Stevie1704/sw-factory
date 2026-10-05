---
status: accepted
---

# Host Git never runs repository hooks

The coordinator runs Git on the host to create worktrees, create checkpoints, synchronize the base, and push. These commands used the inherited Git configuration. A `core.hooksPath` that points at a tracked directory therefore let a worker edit a script that the coordinator then ran on the host, with the host's Git credentials available. An integration probe proved this with a pre-commit hook during `CreateCheckpoint`.

Every factory Git invocation now starts with one factory-owned policy: `-c core.hooksPath=/dev/null -c core.fsmonitor=false`. Command-line configuration outranks repository, global, system, and ambient `GIT_CONFIG_*` configuration, so no hook and no fsmonitor command can run through a factory operation. The policy applies only to the factory's own commands. The checkout's configuration and a developer's manual Git usage do not change. Startup diagnosis verifies that factory invocations resolve the disabled hooks path.

Checks that a repository needs, such as pre-commit lint, formatting, or tests, belong in `factory.yaml` as Gates. Gates run inside the Worker against an exact checkpoint, which keeps ADR 0010's worker-owned process boundary.

## Considered options

`--no-verify` on commit, merge, and push was rejected because it skips only some hooks: `post-checkout`, `post-commit`, `post-merge`, and `reference-transaction` still run. Scrubbing ambient `GIT_CONFIG_*` variables was rejected because command-line configuration already wins, and scrubbing could remove transport settings that an operator supplies. A host allow-list of repository scripts was rejected because it adds a second checking pipeline next to Gates.

Clean, smudge, and process filters, merge drivers, commit signing, and transport authentication stay supported. Only host configuration can define their commands; tracked content can select a filter or merge driver but cannot define one. Git LFS and signed-commit requirements depend on them. Server-side hooks belong to the remote. The production remote is GitHub, so they do not run on the coordinator host.
