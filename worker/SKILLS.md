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
`~/.agents` installation on 2026-09-02.

The upstream folder hash identifies the vendored source. It stops matching the
files here for any skill listed under local deviations below.

| Skill | Upstream path | Upstream folder hash | Deviates |
| --- | --- | --- | --- |
| `codebase-design` | skills/engineering/codebase-design/SKILL.md | `344e3efc88ee60663b2e989555c2efdb548fbd42` | no |
| `diagnosing-bugs` | skills/engineering/diagnosing-bugs/SKILL.md | `99bd56983e42bc752c52da67f1112e4121781cd1` | yes |
| `domain-modeling` | skills/engineering/domain-modeling/SKILL.md | `388c9822641805ca2dcd5038e68a1d5282437ee5` | no |
| `implement` | skills/engineering/implement/SKILL.md | `f07d230f645fc9ac390cf13a450bbff12ad791a3` | yes |
| `resolving-merge-conflicts` | skills/engineering/resolving-merge-conflicts/SKILL.md | `77f0d7de3143abbf03e55a63522d30bff31ae908` | yes |
| `specification-review` | skills/engineering/code-review/SKILL.md | `d8e341cee7980127dddda05159bedf25dc853615` | yes |
| `standards-review` | skills/engineering/code-review/SKILL.md | `d8e341cee7980127dddda05159bedf25dc853615` | yes |
| `tdd` | skills/engineering/tdd/SKILL.md | `79288be15c67b849f22b6572056601090fd20913` | yes |

## Local deviations

A vendored skill is kept at its upstream text wherever possible, because a
silent fork makes the provenance above unverifiable. Each deviation below is
deliberate and must survive a re-vendor.

The factory-adapted `implement`, `specification-review`, and `standards-review`
skills make a successful `factory-report` result-file write their explicit
completion gate. The coordinator cannot advance from terminal prose alone.

- `diagnosing-bugs/scripts/hitl-loop.template.sh`: the capture helper reads one
  line, which truncates a pasted stack trace, and it echoed the captured text
  verbatim. The template now offers a sentinel-terminated multiline capture and
  redacts common credential shapes before printing, because a role agent's
  output is persisted with the run.
- `implement/SKILL.md`: keeps the upstream vertical-slice, TDD, focused-check,
  and full-suite workflow, but hands checkpoint and review ownership back to the
  factory. The factory's dedicated reviewers replace the upstream combined
  review step, and the coordinator owns Git history.
- `resolving-merge-conflicts/SKILL.md`: upstream forbids `--abort`
  unconditionally. A worker must be able to restore the pre-merge state instead
  of committing a resolution it cannot justify, so an abort is permitted when no
  safe resolution exists.
- `specification-review/SKILL.md`: extracts only the specification axis from
  upstream `code-review`, using the exact checkpoint and frozen specification
  already supplied by the coordinator. It does not discover a fixed point,
  spawn reviewers, aggregate axes, or mutate the issue tracker.
- `standards-review/SKILL.md`: extracts only the documented-standards axis and
  Fowler smell baseline from upstream `code-review`, using the checkpoint and
  repository guidance already supplied by the coordinator. It does not spawn
  reviewers or aggregate the specification axis.
- `tdd/SKILL.md`: drops a pointer to the `code-review` skill, which this set
  does not install. The factory owns review through its own roles.

Updating a skill means replacing its directory, rebuilding the worker image, and
recording the new digest in `factory.yaml`. In-flight runs keep the skill set
their role storage was created from.
