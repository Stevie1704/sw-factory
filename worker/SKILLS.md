# Worker skill set

The worker image ships a curated skill set at `$HOME/.codex/skills` and
`$HOME/.claude/skills`. Both harnesses read those directories, so a role agent
sees the same craft references whichever adapter runs it.

The set is factory-owned and pinned by the worker image digest recorded in
`factory.yaml`. A run never reads a personal skill directory from the host, so
an agent's visible instructions stay reproducible for the run's digest. See
`docs/adr/0006-factory-owned-worker-skills.md` for the boundary this preserves.

## Supported harnesses and discovery roots

The worker image installs the harnesses pinned by `worker/base.Dockerfile`.
Each one reads the skill set from its own role home, so the two installations
are separate roots holding the same names rather than one shared root.

| Harness | Installed root | Other roots it also reads |
| --- | --- | --- |
| `codex` | `$HOME/.codex/skills` | `$HOME/.agents/skills`, the project `.codex/skills`, the system cache root |
| `claude` | `$HOME/.claude/skills` | the project `.claude/skills`, installed plugins |

Pinned Codex still discovers the deprecated `$CODEX_HOME/skills` root, so the
`.codex/skills` installation remains the supported location for that version.

`$HOME/.agents/skills` is deliberately left empty and the checks assert it
stays that way. A second root holding the same names would let one skill name
resolve to two installations, and the worker must not ship that ambiguity.
Migrating to `$HOME/.agents/skills` is a separate decision, not a repair for
skill visibility.

The mounted worktree is repository content and can hold its own project skill
directory. That is untrusted input like the rest of the checkout; the factory
never relies on it for a role-mandated skill.

## Activation semantics

Shipping a skill is not the same as advertising it. Each harness has its own
switch that removes a shipped skill from the model-visible catalog:

| Harness | Hidden by | Default |
| --- | --- | --- |
| `codex` | `policy.allow_implicit_invocation: false` in `agents/openai.yaml` | absent, so the skill is advertised |
| `claude` | `disable-model-invocation: true` in the `SKILL.md` front matter | absent, so the skill is advertised |

A hidden skill under pinned Codex is reachable only through an explicit
`$skill-name` mention in the prompt text. The factory role prompts name a skill
in backticks as prose, which is not an explicit mention, so a hidden skill
would leave a role mandated to use guidance it cannot reach. Neither switch
appears in this set: role prompts, not activation metadata, decide which role
uses which skill.

The role-mandated skills are `implement` for the implementation role,
`specification-review` for the specification reviewer, and `standards-review`
for the standards reviewer. Every shipped harness must advertise all three,
because a repository may assign any role to any supported harness.

## Verification

Three layers verify the contract, and they stay separate because only the
middle one costs a model call.

1. Artifact contract, deterministic and offline:

   ```sh
   go test ./worker/...
   ```

   It fails when a role prompt names a skill the image does not install, when a
   mandatory skill is hidden from either harness, or when the image definition
   seeds a skill set into more than one discovery root per harness.

2. Real invocation smoke, once per worker digest and harness version:

   ```sh
   make worker-build                 # prints the new digest
   # record the digest in factory.yaml, then:
   ./scripts/smoke-skills.sh
   ```

   The smoke asks each harness, inside the pinned image and with the exact
   phrasing the role prompts use, to load each mandatory skill and quote a line
   that exists only in that skill's body. It records the result in
   `worker/skill-smoke.json`, keyed by image digest and harness version. It
   needs a host credential for each harness it smokes and skips any harness
   without one. Set `HARNESSES` to smoke a subset.

3. Startup diagnosis, deterministic and offline:

   ```sh
   factory doctor
   ```

   For every shipped harness it probes the pinned image for the installed,
   unduplicated, model-visible skill set, and then reads the recorded smoke
   result for that exact digest and harness version. It never makes a model
   call of its own. Both parts block for every shipped harness, not only for
   the harness `role_harness_defaults` currently selects: a repository may
   assign any role to any supported harness, so an unverified harness is not a
   safe run.

The smoke exits non-zero when any harness it was asked to prove produced no
record, so a skipped harness never reads as a pass.

Rebuilding the image invalidates the recorded evidence, because the digest is
part of the key. Rebuild, record the new digest, re-run the smoke, then run
the startup diagnosis.

### Currently recorded

`worker/skill-smoke.json` holds a `codex` record for the pinned digest only.
The `claude` smoke has not been run, because it needs a Claude credential file
at the configured `claude_auth_path` and none exists on the machine that
recorded the codex result. Until someone runs

```sh
HARNESSES=claude ./scripts/smoke-skills.sh
```

and commits the updated file, the startup diagnosis blocks on `claude worker
skill contract`. That is the contract this document states, not an oversight in
it.

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
