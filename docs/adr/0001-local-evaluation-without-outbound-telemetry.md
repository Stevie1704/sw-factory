---
status: accepted
---

# Keep local evaluation summaries without outbound telemetry

The factory stores content-free local evaluation summaries because retry ceilings, review policy, and workflow cost cannot be responsibly tuned from anecdotes alone. This does not relax the privacy boundary: the factory sends no outbound telemetry and evaluation summaries contain no prompts, transcripts, diffs, source contents, logs, or credentials.

Local evaluation summaries are distinct from current operational state and disposable run artifacts. They may retain stage durations, invocation and attempt counts, harness and version identifiers, exhaustion and escalation reasons, available usage estimates, and explicit human dispositions. Their retention and deletion are locally controlled and documented.

This accepts a small amount of durable local metadata in exchange for an evidence-based rollout. The measured pilot must use these summaries before the objection cycle, concurrent dual review, and full review-repair loop are treated as justified product complexity.
