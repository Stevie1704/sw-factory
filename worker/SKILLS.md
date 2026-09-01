# Worker skill set

The worker image ships a curated skill set at `$HOME/.codex/skills` and
`$HOME/.claude/skills`. Both harnesses read those directories, so a role agent
sees the same craft references whichever adapter runs it.

The set is factory-owned and pinned by the worker image digest recorded in
`factory.yaml`. A run never reads a personal skill directory from the host, so
an agent's visible instructions stay reproducible for the run's digest. See
`docs/adr/0006-factory-owned-worker-skills.md` for the boundary this preserves.

## Curation rule

A skill belongs here only when it is craft guidance that applies inside the
mounted worktree. A skill that mutates the issue tracker, opens a pull request,
or moves a workflow stage is refused: the coordinator owns those decisions, and
a skill that claims them would contradict the role prompt.

## Vendored skills

Sources are `https://github.com/mattpocock/skills.git`, vendored from the host
`~/.agents` installation on 2026-09-01.

| Skill | Upstream path | Folder hash |
| --- | --- | --- |
| `codebase-design` | skills/engineering/codebase-design/SKILL.md | `344e3efc88ee60663b2e989555c2efdb548fbd42` |
| `diagnosing-bugs` | skills/engineering/diagnosing-bugs/SKILL.md | `a66e40b2643dd0477d37c94f2369353c2a966c6a` |
| `domain-modeling` | skills/engineering/domain-modeling/SKILL.md | `388c9822641805ca2dcd5038e68a1d5282437ee5` |
| `resolving-merge-conflicts` | skills/engineering/resolving-merge-conflicts/SKILL.md | `ebd182bec2c67bd44aaf46675a4f7daa422645cc` |
| `tdd` | skills/engineering/tdd/SKILL.md | `2b2d405a37108a5d137c33ca20701bb4df3dfdc1` |

Updating a skill means replacing its directory, rebuilding the worker image, and
recording the new digest in `factory.yaml`. In-flight runs keep the skill set
their role storage was created from.
