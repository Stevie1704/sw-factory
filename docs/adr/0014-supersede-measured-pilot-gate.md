---
status: accepted
---

# Supersede the measured-pilot runtime gate with repository policy

The measured pilot in [issue #26](https://github.com/Stevie1704/sw-factory/issues/26) ended `revise and repeat` with zero matched pairs, and its findings document was later removed. Milestone M3 nevertheless shipped the bounded test-objection cycle and concurrent review, so a fixed lookup of issue 26 no longer represents either the delivered product or a reusable authorization mechanism for other repositories.

We supersede the pilot as a runtime prerequisite. The `test_policy.allow_automated_objections` value frozen from each repository's checked-in configuration is the sole authority for automated objection revisions; `false` preserves the objection and waits for a human, while `true` permits only the configured `retry_limits.test_revision` attempts. No GitHub issue number or comment can open or close that gate.

Accepting the shipped M3 behavior without the planned comparison does not claim that the factory outperforms a direct harness. It accepts the feature because its risk is bounded by independent red verification, protected test paths, exact-checkpoint gates and reviews, finite repository-owned budgets, and human fallback on rejection, verification failure, or exhaustion. Automated regression tests and content-free local evaluation summaries from real runs replace the abandoned one-time pilot as ongoing evidence.

This repository now uses `test_policy.mode: required` and enables automated objections for its own runs, providing operational evidence for the independent-test and objection paths. New repositories should still default the switch to `false` and enable it only as an explicit policy decision. Issue #26 remains a historical record and is not reopened; the deleted findings document is not restored.
