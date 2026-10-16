---
status: accepted
---

# Ideation readiness is a user decision on the displayed brief

ADR 0021 made the conversation and brief of an ideation session a local
operator surface, ADR 0023 lets the ideation role propose readiness, and ADR
0024 carries browser decisions to the coordinator. This record decides how the
local UI (#269) turns a conversation into a handoff without invented
requirements, and how the ideation role gets its questioning method.

## Decision

1. **The role proposes; the user decides on one brief revision.** The ideation
   role may propose `ready_for_planning`, `investigation_needed`, or
   `no_build`. The proposal changes nothing. The session page shows it with
   the brief revision it was made for and marks it when the brief changed
   since. Only a user command changes the session, and the handoff names the
   brief revision that the page showed.
2. **No unknown reaches planning silently.** A `hand_off` command lists every
   unresolved question of that brief revision in `deferred_questions`; the
   coordinator refuses a handoff that leaves one out or names another. The
   page shows each unresolved question as a required deferral. Planning may
   plan an investigation item for a deferred question but must not answer it.
3. **Investigation needed is an outcome, not a backlog.** The session state
   `investigation_needed` (added to the ADR 0021 lifecycle) records that
   material unknowns must be investigated before planning. It needs at least
   one unresolved question in the current brief; those questions are the
   investigation items. It creates no tracker item and can resume, end as
   no-build, or be cancelled. No-build and deferral also need no backlog.
4. **A changed answer names what it affects.** A user turn can name the
   earlier user answer that it corrects (`ideation_turns.corrects`, schema
   version 46). The session packet marks the correction, the ideation report
   lists the brief decisions it affects in `affected_decisions`, and the page
   shows each of them as revised, withdrawn, unchanged, or added by comparing
   the brief before and after. A correction whose answer names no decision is
   shown as such, never hidden.
5. **The questioning method is factory-owned craft.** Prompt `ideation-v2`
   names the `grilling` worker skill, adapted from upstream with recorded
   provenance in `worker/SKILLS.md`: decision-tree rounds of questions whose
   prerequisites are settled, recommended answers, facts read from the
   snapshot, and revisited decisions after a changed answer. Sub-agent
   orchestration is removed because the factory runtime owns execution.
   Startup diagnosis requires smoke evidence for `grilling` only from the
   harness that can run a declared `ideation` role, so repositories without
   sessions keep their recorded evidence. `ideation-v1` stays buildable for
   persisted invocations.
6. **A reload never repeats a decision.** While a command of the session waits
   for the coordinator, the session page shows it and offers no command form.
   Accepted answers and outstanding questions come from the store: turns, the
   accepted role payloads, and the command inbox.

## Consequences

The handoff needs one more click per unresolved question; this is the
explicit decision the specification asks for. Readiness, corrections, and
pending questions are structured fields, so scenario checks use fixed role
reports and do not depend on model wording. A repository that turns on the
ideation role needs a worker image that ships the `grilling` skill and smoke
evidence for it.

## Considered options

- Let the brief carry separate deferred and investigation lists. Rejected for
  now: the unresolved questions already hold the unknowns, and the handoff
  command and the session state record the user's decision about them without
  a second brief shape for the planning prompt.
- Detect affected decisions only by diffing brief revisions. Rejected: a diff
  cannot show a contradictory decision that the role kept unchanged; the role
  must name what it revisited.
- Require the `grilling` skill for every repository. Rejected: it would block
  startup diagnosis of repositories that never use sessions until a new smoke
  record exists.
