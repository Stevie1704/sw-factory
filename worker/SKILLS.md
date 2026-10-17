# Worker skill set

The worker image ships a curated skill set at `$HOME/.codex/skills`,
`$HOME/.claude/skills`, and `$HOME/.copilot/skills`. Each harness reads its own
directory, so a role agent sees the same craft references whichever adapter
runs it.

The set is factory-owned and pinned by the worker image digest recorded in
`factory.yaml`. A run never reads a personal skill directory from the host, so
an agent's visible instructions stay reproducible for the run's digest. See
`docs/adr/0006-factory-owned-worker-skills.md` for the boundary this preserves.

## Supported harnesses and discovery roots

The worker image installs the harnesses pinned by `worker/base.Dockerfile`.
Each one reads the skill set from its own role home, so the installations
are separate roots holding the same names rather than one shared root.

| Harness | Installed root | Other roots it also reads |
| --- | --- | --- |
| `codex` | `$HOME/.codex/skills` | `$HOME/.agents/skills`, the project `.codex/skills`, the system cache root |
| `claude` | `$HOME/.claude/skills` | the project `.claude/skills`, installed plugins |
| `copilot` | `$HOME/.copilot/skills` (`$COPILOT_HOME/skills`, personal skills) | `$HOME/.agents/skills`; the project `.github/skills`, `.claude/skills`, and `.agents/skills`; installed plugins; built-in skills |

Pinned Codex still discovers the deprecated `$CODEX_HOME/skills` root, so the
`.codex/skills` installation remains the supported location for that version.

The Copilot CLI is the pinned release recorded in
`docs/spikes/0003-copilot-cli-protocol.md`. Its private home
`$HOME/.copilot` is on the role volume. The launcher `factory-copilot`
restores the image-owned settings in that home before every launch:
`ignoredSkillsLocations: ["/work"]` hides every project skill root in the
worktree, and installed plugins are removed. `disabledSkills` names only the
two built-in skills `customize-cloud-agent` and `github-pr-media`, never a
curated skill.

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
| `copilot` | the skill name in `disabledSkills` of the managed `settings.json` | only the two built-in skills are named |

A hidden skill under pinned Codex is reachable only through an explicit
`$skill-name` mention in the prompt text. The factory role prompts name a skill
in backticks as prose, which is not an explicit mention, so a hidden skill
would leave a role mandated to use guidance it cannot reach. No switch
appears in this set: role prompts, not activation metadata, decide which role
uses which skill.

The role-mandated skills are `implement` for the implementation role,
`specification-review` for the specification reviewer, `standards-review`
for the standards reviewer, `pr` for the optional `pr_writer` role, and
`grilling` for the optional `ideation` session role. Every shipped harness
must advertise all five, because a repository may assign any role to any
supported harness. Startup diagnosis requires smoke evidence for `pr` only
when the repository declares `pr_writer`, and for `grilling` only when it
declares `ideation`, so an existing repository without sessions keeps its
recorded evidence.

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
   without one. Set `HARNESSES` to smoke a subset. `HARNESSES` defaults to
   `codex claude`; Copilot is smoked only when named, and only with the
   dedicated token file in `COPILOT_TOKEN_PATH`, which has no default:

   ```sh
   HARNESSES=copilot COPILOT_TOKEN_PATH=<token file> ./scripts/smoke-skills.sh
   ```

   The Copilot call runs through `factory-copilot` with the production
   isolation options. The token reaches the worker only on standard input
   and a private tmpfs at `/run/factory-auth`, never a Docker argument.

3. Startup diagnosis, deterministic and offline:

   ```sh
   factory doctor
   ```

   For every shipped harness it probes the pinned image for the installed,
   unduplicated, model-visible skill set, and then reads the recorded smoke
   result for that exact digest and harness version. It never makes a model
   call of its own. The image contract is checked for every shipped harness,
   while missing smoke evidence blocks only for harnesses selected by
   `role_harness_defaults`, or for each override target when
   `allowed_overrides` contains `harness`. Copilot is checked only when the
   repository can select it. An unselected harness is reported
   as advisory until the repository can select it.

The smoke exits non-zero when any harness it was asked to prove produced no
record, so a skipped harness never reads as a pass.

When a harness call exits non-zero, for example with an expired credential,
the smoke does not report a skill that did not load. It prints the last lines
of the harness diagnosis, records nothing, and exits non-zero.
Renew the credential and run the smoke again.

Rebuilding the image invalidates the recorded evidence, because the digest is
part of the key. Rebuild, record the new digest, re-run the smoke, then run
the startup diagnosis.

### Currently recorded

`worker/skill-smoke.json` holds a `codex` record and a `claude` record for the
pinned digest, so the startup diagnosis passes both `worker skill contract`
checks.

There is no `copilot` record, and none may be written without a live run. The
smoke can drive Copilot, but no dedicated Copilot credential was available,
so no live call has happened (release blocker RB-7 in
[Copilot CLI release evidence](../docs/acceptance/copilot.md)). A repository
that can select Copilot therefore fails `factory doctor` with
`harness.skills.evidence_stale` until an operator runs the Copilot smoke for
its pinned image. Offline, `scripts/verify-headless-worker.sh` runs
`factory-copilot skill list` with the pinned CLI and requires every curated
skill in the listing and no planted project skill. A listing is not
activation evidence.

macOS keeps the Claude Code credential in the login Keychain rather than in a
file. Usually the smoke finds no file at the default `CLAUDE_AUTH_PATH` and
skips the `claude` harness. Claude Code does not renew an old file at that
path, so its session expires and the smoke fails with
`OAuth session expired`. In both cases, export the credential to a temporary
file, point the smoke at it, and delete it again:

```sh
umask 077
security find-generic-password -s "Claude Code-credentials" -w \
  > "$TMPDIR/claude-credentials.json"
HARNESSES=claude CLAUDE_AUTH_PATH="$TMPDIR/claude-credentials.json" \
  ./scripts/smoke-skills.sh
rm -f "$TMPDIR/claude-credentials.json"
```

The export is a live credential. Keep it outside the repository, and delete it
when the smoke is recorded.

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
| `grilling` | skills/productivity/grilling/SKILL.md | `f0732035b8b1b60ae39454e4191caef32fa91903` | yes |
| `diagnosing-bugs` | skills/engineering/diagnosing-bugs/SKILL.md | `99bd56983e42bc752c52da67f1112e4121781cd1` | yes |
| `domain-modeling` | skills/engineering/domain-modeling/SKILL.md | `388c9822641805ca2dcd5038e68a1d5282437ee5` | no |
| `implement` | skills/engineering/implement/SKILL.md | `f07d230f645fc9ac390cf13a450bbff12ad791a3` | yes |
| `pr` | skills/engineering/pr/SKILL.md | `00b896d811adffad747c7ca30078d909d60c7f93` | yes |
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
- `grilling/SKILL.md`: vendored from the host `~/.agents` installation on
  2026-10-16 for the `ideation` session role (prompt `ideation-v2`). It keeps
  the upstream design tree, frontier rounds, and recommended answers, but the
  factory runtime owns execution: the role looks facts up itself in the
  read-only snapshot instead of dispatching sub-agents, and a round ends with
  the role's report instead of a wait inside one session. It adds rules the
  factory needs: keep rounds small, revise or withdraw every decision below a
  changed answer, keep evidence apart from assumptions, never invent an
  answer to an open question, and leave the readiness decision to the user.
  The emoji question format is dropped, because the role prompt sends
  questions as structured report fields.
- `implement/SKILL.md`: keeps the upstream vertical-slice, TDD, focused-check,
  and full-suite workflow, but hands checkpoint and review ownership back to the
  factory. The factory's dedicated reviewers replace the upstream combined
  review step, and the coordinator owns Git history.
- `pr/SKILL.md`: the worker cannot take screenshots and the `pr_writer` role
  must not invent evidence. The Evidence section therefore drops the screenshot
  tier and asks for observed or factory-supplied output only, with "not
  available" for a before that nobody observed. `pr/CREDITS.md` keeps the
  upstream attribution for the Summary visuals.
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
