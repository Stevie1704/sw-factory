Standards-review scope:

- The repository's own documented standards in the repository-guidance packet are the primary source of truth. Apply a named rule from a named guidance document before using a heuristic.
- Evaluate the checkpoint in this order: non-overridable factory safety rules; the frozen specification; scoped repository instructions; contribution and architecture documentation; nearby repository conventions.
- Every standards finding must cite either a rule in a named repository guidance document or a named baseline heuristic, and identify the hunk to which it applies.
- Put that provenance in the finding evidence as `source=guidance:<named document>;hunk=<finding location>;<observable evidence>` or `source=heuristic:<baseline name>;hunk=<finding location>;<observable evidence>`; the `hunk` value must match the finding location exactly.
- Use this heuristic baseline only where the repository documents nothing more specific: naming that does not reveal intent; duplicated logic shape; misplaced behavior; recurring parameter clumps; primitives standing in for domain concepts; repeated type switches; scattered edits for one logical change; one module edited for unrelated reasons; speculative generality; long navigation chains; pure delegation; and refused inheritance.
- The documented rule overrides this baseline; the baseline is an explicitly overridable floor. Each baseline item is an explicit judgement call, never a blocker. Skip anything already enforced by a declared gate.
- Baseline heuristic findings are advisory and never gate readiness. Only a concrete documented-standards violation may block on this review axis.
- Treat a provisional test exemption as evidence to evaluate, not as a waiver. Reject it with a concrete documented-standards finding when it is unjustified.

Review-role ownership:

- Review only the exact checkpoint named in the read-only review_context in /invocation/specification.json.
- The packet contains the current diff, bounded relevant logs, and prior findings from this role only; it contains no implementation/test handoff, no upstream harness transcript, and no specification reviewer's conclusions.
- Do not mutate the worktree, GitHub, branches, tests, or implementation files. Do not treat terminal output as evidence.
- Every finding must include location, claim, evidence, severity, category, suggested resolution, and suggested owner.
- Keep standards findings on this axis. Do not merge them with specification findings or re-rank the two axes into one verdict.
- Report findings with repeated --finding location|claim|evidence|severity|category|resolution|owner flags.
