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

The upstream folder hash identifies the vendored source. It stops matching the
files here for any skill listed under local deviations below.

| Skill | Upstream path | Upstream folder hash | Deviates |
| --- | --- | --- | --- |
| `codebase-design` | skills/engineering/codebase-design/SKILL.md | `344e3efc88ee60663b2e989555c2efdb548fbd42` | no |
| `diagnosing-bugs` | skills/engineering/diagnosing-bugs/SKILL.md | `99bd56983e42bc752c52da67f1112e4121781cd1` | yes |
| `domain-modeling` | skills/engineering/domain-modeling/SKILL.md | `388c9822641805ca2dcd5038e68a1d5282437ee5` | no |
| `resolving-merge-conflicts` | skills/engineering/resolving-merge-conflicts/SKILL.md | `77f0d7de3143abbf03e55a63522d30bff31ae908` | yes |
| `tdd` | skills/engineering/tdd/SKILL.md | `79288be15c67b849f22b6572056601090fd20913` | yes |

## Local deviations

A vendored skill is kept at its upstream text wherever possible, because a
silent fork makes the provenance above unverifiable. Each deviation below is
deliberate and must survive a re-vendor.

- `diagnosing-bugs/scripts/hitl-loop.template.sh`: the capture helper reads one
  line, which truncates a pasted stack trace, and it echoed the captured text
  verbatim. The template now offers a sentinel-terminated multiline capture and
  redacts common credential shapes before printing, because a role agent's
  output is persisted with the run.
- `resolving-merge-conflicts/SKILL.md`: upstream forbids `--abort`
  unconditionally. A worker must be able to restore the pre-merge state instead
  of committing a resolution it cannot justify, so an abort is permitted when no
  safe resolution exists.
- `tdd/SKILL.md`: drops a pointer to the `code-review` skill, which this set
  does not install. The factory owns review through its own roles.

Updating a skill means replacing its directory, rebuilding the worker image, and
recording the new digest in `factory.yaml`. In-flight runs keep the skill set
their role storage was created from.
