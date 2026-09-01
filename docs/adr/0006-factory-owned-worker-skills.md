---
status: accepted
---

# ADR 0006: Factory-owned worker skills

## Context

Both harnesses read a skill set from their role home and advertise every
installed skill, with its name and description, in the model-visible prompt of
every session. The worker's role home is a private Docker volume that Docker
seeds from the worker image, so whatever the image places at
`$HOME/.codex/skills` and `$HOME/.claude/skills` becomes part of the agent's
visible instructions for the run.

Reusable craft guidance is worth giving a role agent. A personal host skill
directory is not the way to deliver it: it is outside the worker image digest
recorded in `factory.yaml`, so two runs on the same digest could receive
different instructions, and a later replay could not reconstruct what the agent
was told. The runtime already refuses a repository cache whose host path names
`.codex` or `.claude` for the same reason.

Skill content also carries authority claims. A published skill can instruct an
agent to open a pull request, move a ticket, or rewrite history, which the
coordinator owns exclusively.

## Decision

A curated skill set is vendored under `worker/skills` and copied into both role
homes by the worker image definition. The worker image digest therefore pins
the skills exactly as it pins the harness versions. No host skill directory is
ever mounted or seeded into a worker, so a session still inherits no personal
plugin, hook, MCP server, or setting.

A skill is admitted only when it is craft guidance that applies inside the
mounted worktree. A skill that mutates the issue tracker, opens a pull request,
or moves a workflow stage is refused.

The embedded role prompts scope the set per role, because the harnesses cannot:
the implementation body names `tdd`, `codebase-design`, `diagnosing-bugs`, and
`resolving-merge-conflicts` and refuses `domain-modeling`, and the architecture
body claims `domain-modeling` for terminology and decision records. Both bodies
state that a skill advises craft only and that the factory rules decide any
disagreement.

## Consequences

Adding, updating, or removing a skill is an image change: rebuild the worker
image and record the new digest in `factory.yaml`. A run already holding role
storage keeps the skill set that storage was created from.

Scoping lives in the prompts, so a skill-set change that shifts role ownership
also bumps a prompt version and its content identity, as ADR 0005 requires.

Every installed skill spends context in every session through its advertised
description, which keeps the admitted set deliberately small.

The skills are copied into a writable role home, so the factory pins what a
session starts with rather than guaranteeing the bytes stay unmodified for the
whole run.
