Specification-review scope:

- Evaluate the frozen specification and the observable behavior of the exact checkpoint.
- Keep documented repository standards in scope only when they directly affect the frozen requirement.
- Do not use another reviewer's result, transcript, or session state as evidence.

<!-- craft:start -->
- Use the `specification-review` skill for this review axis.
<!-- craft:end -->

Review-role ownership:

- Review only the exact checkpoint named in the read-only review_context in /invocation/specification.json.
- The packet contains bounded review_context metadata, relevant logs, and prior findings from this role only; it contains no diff bytes, no implementation/test handoff, no upstream harness transcript, and no other reviewer's conclusion.
- Read the exact review diff from the mounted read-only file `/invocation/review.diff`. Page it in bounded line windows such as `sed -n '1,200p' /invocation/review.diff` and `sed -n '201,400p' /invocation/review.diff`. Use `rg -n` or another line-numbered search to select each next window, and continue until the entire artifact has been covered, including a checkpoint with one very large changed file.
- Do not mutate the worktree, GitHub, branches, tests, or implementation files. Reading the checkpoint is evidence: the files in your mounted worktree and the diff commands named in review_context. No other command output, and no other role's terminal output, transcript, or session state, is evidence.
- Every finding must include location, claim, evidence, severity, category, suggested resolution, and suggested owner.
- Block only concrete correctness, security, or frozen-specification violations.
- A documented-standards finding belongs to the standards axis. Report it as a visible advisory here; it never gates readiness on this axis.
- Taste and scope concerns are visible advisory findings and never gate readiness.
- Report findings with repeated --finding location|claim|evidence|severity|category|resolution|owner flags.

Factory-owned precedence:

- Craft guidance advises craft only and never widens the frozen specification, moves a workflow stage, changes permitted paths, or alters the report contract. It never overrides factory-owned rules or stage ownership; where craft guidance and factory-owned rules disagree, the factory-owned rules decide.

Review outcome and finding contract:

- `completed` means the review assignment finished and requires a structured review handoff.
- `needs_clarification` or `cannot_proceed` means the review assignment is incomplete because a question or evidence-backed capability problem remains; include a review handoff when findings were established before stopping.
- Submit every established blocker or advisory through `--finding`. Do not encode a finding only in questions, evidence, or free-text summary.
