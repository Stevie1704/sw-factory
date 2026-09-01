Implementation craft:

- The accepted test-stage handoff is the seam. Do not re-negotiate it or widen it. On a contract-first route, use the seam supplied by the accepted test-stage handoff. On the implementation-owned TDD path, agree the seam from the frozen acceptance criteria as part of this run.
- Work in vertical slices: implement one behavior with the smallest useful change, then repeat. Do not write the full test surface up front and implement against it afterward.
- Prefer the smallest change that satisfies the frozen criteria. Abstraction, parameters, or extension points the specification does not ask for are out of scope.
- Run focused checks continuously and run the full declared gate suite before proposing a checkpoint.
- Report the focused commands you actually ran and the observable evidence they produced. Never use terminal appearance as evidence.
- When a check-repair packet is supplied, read the coordinator-supplied packet in `/invocation/specification.json` before changing the implementation.

<!-- implementation-owned-tdd:start -->
Implementation-owned TDD:

- When no independent test-stage handoff is required, implementation owns the complete red/green/refactor loop for the frozen acceptance criteria.
- Write or update a focused behavioral test before production behavior when practical, observe the expected red result, make the smallest implementation change, and observe green before moving to the next vertical slice.
- Revise the initial test design when implementation evidence requires it, including essential test infrastructure when needed; do not weaken deterministic gate meaning to make the implementation pass.
- Include the red/green evidence and the commands actually run in the structured implementation handoff.
<!-- implementation-owned-tdd:end -->

Repair and handoff ownership:

<!-- independent-test-handoff:start -->
- For a required independent test stage, repair implementation behavior in the mounted worktree; do not edit tests or gates merely to make them pass.
<!-- independent-test-handoff:end -->
<!-- implementation-owned-tdd-repair:start -->
- For implementation-owned TDD, revise behavioral tests or essential test infrastructure when needed, while preserving deterministic gate meaning.
<!-- implementation-owned-tdd-repair:end -->
- During review repair, address implementation-owned findings without editing protected tests. When a finding names the test role as suggested owner, submit a structured test objection through the existing report protocol; never edit that test directly.
- Preserve the frozen specification and deterministic gates. Do not dismiss a finding without observable evidence.
- Include the focused commands and observable behavioral evidence in the structured implementation handoff.
- On a contract-first route, do not edit any protected test path or change its recorded content. If a protected test is incorrect, submit a structured objection with --test-objection test|claim|evidence; never edit the protected test.
