---
status: accepted
---

# Partition exact-checkpoint reviews with one durable manifest

Large exact-SHA diffs can reduce review depth even when the diff remains
readable as a file. The factory will keep one coherent implementation
checkpoint and one review round, but partition that round into bounded review
units described by one durable manifest shared by the specification and
documented-standards axes. Deterministic byte-bounded units improve the chance
of consistent attention while a repository-owned fan-out budget and a
host-owned concurrency ceiling bound paid sessions and machine load.

## Decision

### Preserve the checkpoint and review boundaries

A review unit partitions workload only. It is not an implementation checkpoint,
a review round, a review axis, or a separately published review result. One
review round continues to judge the complete base-to-checkpoint change, and
both review axes retain the separate roles, findings, statuses, and pull-request
sections required by ADR 0005.

Partial implementation checkpoints are rejected. They would require a durable
`implementation incomplete` state and completion signal, teach specification
review to ignore a deliberately absent remainder, promote base checkpoints to
avoid cumulative re-review, preserve review history across several SHAs, and
prevent readiness from accepting a partially implemented issue. Those are new
workflow semantics rather than bounded review workload.

### Derive one deterministic manifest

Before launching any reviewer, the coordinator parses the immutable
`/invocation/review.diff` artifact and creates one ordered manifest. The
manifest records:

- a manifest schema version and factory-owned partition-policy version;
- the run, base SHA, checkpoint SHA, review-diff byte count and SHA-256;
- the effective unit-workload and fan-out limits;
- its own SHA-256 identity; and
- ordered units containing a stable unit ID, measured workload, unit-diff
  SHA-256, primary changed ranges, bounded context ranges, and primary non-text
  file changes.

The partition policy uses the UTF-8 byte size of a unit's self-contained diff
evidence, including file and hunk headers and overlapping context. This is
deterministic and approximates model input better than changed-line count.
Model token counts are rejected because they vary by model and tokenizer.

Partitioning follows the review diff's existing order and never reorders work
to improve packing. The coordinator greedily keeps consecutive complete files
together while the rendered unit fits. An oversized file is split at complete
hunk boundaries; an oversized hunk is split into maximal consecutive line
windows that fit. A split window includes up to the existing ten source-context
lines before and after its primary range. Context, including a changed line
that is primary in a neighbouring unit, may overlap, but every changed line has
exactly one primary assignment. Renames, mode changes, binary summaries, and
other non-text changes also receive exactly one primary unit assignment.

The smallest legal fragment remains a complete diff line with the headers
needed to identify it. If that fragment alone exceeds the workload limit, the
round is unpartitionable and waits for human disposition; the coordinator does
not split a logical line into arbitrary byte fragments or weaken the limit.

The first policy version uses these defaults:

- maximum review workload: 65,536 bytes per unit;
- normal fan-out: four manifest units;
- context overlap: up to ten source lines on each side; and
- maximum findings: 16 per unit and 64 per axis.

The partition algorithm, metric, context width, finding limits, and policy
version are factory-owned. Repository configuration owns `max_unit_bytes` and
`max_units`; their effective values are frozen in the specification packet and
recorded in the manifest. A repository may lower or raise them within
factory-validated bounds but cannot select a different algorithm or metric.

### Persist partial results below the run aggregate

The operational SQLite store keeps normalized durable projections for review
rounds, manifest units, and per-axis unit results. A unit result is uniquely
identified by review round, reviewer role, and unit ID; it also records the
accepted invocation identity and exact checkpoint SHA. Review invocations carry
their assigned unit identity and ranges, and every finding adds that `unit_id`
to the existing location, claim, evidence, severity, suggested resolution, and
suggested owner fields.

Invocation materialisation derives a regular read-only
`/invocation/review-unit.diff` from the persisted manifest and validates its
recorded byte count and SHA-256 before harness activation. The complete
`/invocation/review.diff` remains the exact round evidence required by the
review-diff contract, while the prompt directs the reviewer to its bounded unit
artifact. Frozen specification, repository guidance, and gate-result metadata
remain bounded shared context. A reviewer may inspect nearby checkpoint
worktree content needed to understand its unit, but owns findings only for that
unit's primary assignments.

The existing specification and standards review projections remain the
terminal per-axis aggregates used by workflow decisions and external
publication. They are not used as the only store for in-progress unit results.
The invocation store permits simultaneous review invocations for distinct unit
IDs while continuing to reject two active invocations for the same
round/role/unit identity.

An identical replay for an already accepted round/role/unit/invocation is an
idempotent no-op. A second result with conflicting content or invocation
identity is a recovery discrepancy and pauses the run. Results are aggregated
in manifest order and report order, never completion order. The coordinator
does not semantically merge, deduplicate, or re-rank reviewer findings.

A unit may finish as `completed`, `needs_clarification`, or `cannot_proceed`.
The role's stable commit status remains pending until all of its scheduled units
are terminal. After that:

- any incomplete unit or axis finding-limit overflow yields an error status and
  `waiting_for_human`;
- otherwise any role-owned blocking finding yields failure; and
- otherwise the role succeeds.

An incomplete unit outranks repair even when another unit has already found a
blocker, because the round does not yet contain complete review evidence. When
both axes have terminal completed evidence, the coordinator either finalizes
readiness or creates one combined repair packet from all role-owned blocking
findings. Specification and standards findings remain separate and retain
their unit attribution in the issue status comment and pull-request sections.
There are no per-unit commit-status contexts. While an axis is pending, those
surfaces may publish only bounded progress such as completed-unit count; they
do not publish partial unit findings as independent results.

If accepted findings exceed the axis limit, all unit results remain durable but
no findings are silently discarded into a repair packet. The run waits for
human disposition and its bounded external projection reports the overflow.

### Bound fan-out and concurrency separately

The normal four-unit fan-out permits at most eight paid review invocations when
both axes are configured. Fan-out counts manifest units; paid invocation count
is fan-out multiplied by the number of configured review axes.

The host configuration owns review concurrency because it describes local
machine and harness capacity rather than review coverage. Its default is two
simultaneous review invocations across both axes. The effective concurrency
ceiling is recorded when the round is created. Recovery may use a lower current
host ceiling but never silently increase the persisted ceiling for that round.

When a valid manifest needs more units than the repository's normal fan-out,
the coordinator persists the complete manifest as `awaiting_authorization` and
launches nothing. An authorized maintainer may approve additional fan-out for
that exact manifest identity. Authorization may increase unit count only: it
cannot enlarge units, change ranges, or cause repartitioning. The host owns a
default installation ceiling of eight authorized units, so a two-axis round
cannot exceed 16 paid review invocations. A manifest above that ceiling, or one
whose authorization is refused, remains paused for human disposition.

### Recover only the persisted work

Recovery validates the manifest's base SHA, checkpoint SHA, review-diff size and
SHA-256, partition-policy version, effective limits, units, and manifest hash.
A changed policy or configuration never repartitions an existing round.

For each axis, recovery resumes an active invocation according to the ordinary
harness policy, schedules only units without an accepted result or live
invocation, and respects the persisted concurrency and fan-out authorization.
A missing or changed diff or manifest, duplicated primary assignment, uncovered
change, conflicting result, or unsupported policy version is a typed recovery
discrepancy rather than a reason to regenerate state silently.

### Evaluate depth without claiming to prove it

The local evaluation summary records content-free review measurements: diff
bytes, changed-line count, unit count, largest-unit workload, review invocation
count and duration, available usage and cost, findings by severity, incomplete
or over-budget dispositions, repair convergence, and later human-review
outcomes. It records no paths, diff content, finding text, prompts, or
transcripts. Historical unpartitioned rounds read as one-unit rounds where the
measurement is available.

Deterministic coverage, workload bounds, fan-out bounds, aggregation, and
recovery are executable acceptance criteria. Consistent review depth remains a
pilot hypothesis assessed from local comparisons and explicit human
dispositions; no unit or acceptance test claims to prove it.

## Consequences

Reviewing a large checkpoint creates more fresh sessions, store rows, worker
lifecycles, and report aggregation than the current two-invocation round. The
normal and authorized fan-out ceilings make that cost explicit. Normalized unit
state is a larger persistence change than embedding a manifest in the run row,
but it permits atomic concurrent completion, exact idempotency, and recovery of
only missing work without overwriting another unit's result.

Byte workload is an intentionally imperfect proxy for cognitive complexity.
Its determinism and model independence are more important for the first pilot;
changing the metric later requires a new partition-policy version and affects
only review rounds whose manifest has not yet been created.
