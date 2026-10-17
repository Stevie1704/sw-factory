---
status: accepted
---

# Plan publication uses a per-operation ledger and a tracker planning port

Publishing an approved plan (#272) creates several tracker items and the
relationships between them. The pending-effect journal of ADR 0018 holds one
row per run, and its replay handlers return a Run, but a session is not a Run
(ADR 0021) and one publication is many writes. A provider can also apply a
create and lose the response, and neither GitHub nor Azure DevOps offers
idempotency keys for creates. This record defines how publication stays
recoverable without duplicates and without a Run.

## Decision

1. **A narrow planning port.** `tracker.PlanPublisher` is an optional tracker
   capability with five methods: `PreflightPlan` (read-only: resolves types,
   required fields, and relationship capabilities, and renders every item),
   `CreatePlanItem`, `FindPlanItems` (correlation lookup), and
   `CreatePlanRelationship` and `FindPlanRelationship` (create and verify one
   link). Adapters own provider commands and credentials; the coordinator,
   agents, and the browser never issue them. Package `internal/publication`
   reaches adapters only through this port, and the port-boundary test guards
   it.
2. **The approval binds the rendered preview, and publication creates only
   that.** The preview of `PreflightPlan` is encoded as canonical JSON with
   explicit tags and a schema version: the items are the approval payload, the
   relationships with their native or textual mode are the relationship
   mapping, and the provider target is the approval target. Before the first
   write, publication runs preflight again. If the result does not match the
   approval digests, for example because a type mapping, label, or
   relationship capability changed, publication fails with a stale-approval
   error and makes no tracker write. It then creates the stored approved bytes,
   not a fresh rendering.
3. **A per-session, per-operation ledger.** Schema version 43 adds
   `ideation_publication_operations`: one row per item create and per
   relationship of one approval, in execution order (each item after its
   parent and dependencies, then every relationship). `pending_effects` is not
   widened. Each row moves through `pending`, `attempted`, `failed`,
   `ambiguous`, and `succeeded`. The row is set to `attempted` before its write
   is sent, and a created item's external identity is recorded before any
   dependent operation starts. A recorded identity and a succeeded row are
   final. The ledger is planned once per approval; a retry or a restart
   resumes it and never regenerates the plan.
4. **Reconcile before every create; stop on doubt.** Every item carries a
   correlation key `<session>/<plan revision>/<node>` that the adapter renders
   into the item, for example a hidden body marker or a namespaced tag. Before
   each create, publication looks the key up. One match is adopted. Several
   matches stop the operation as `ambiguous`. No match after an unknown outcome
   (an `attempted` row at startup, a timeout, or any error that is not a typed
   no-change error) also stops it as `ambiguous`. The operator then adopts one
   of the found items or confirms that none exists; both are checked against a
   fresh lookup. An adapter whose lookup reads only part of the tracker
   also implements the optional `tracker.BoundedPlanLookup`, and the ledger
   detail of the stopped create names that bound before the operator
   confirms absence. Only typed permission, unsupported-capability, and
   invalid-mapping errors prove that a create made no change. Relationships are
   verified before every create, so a retry never adds a duplicate link.
5. **Partial results are evidence, never rolled back.** Each attempt records a
   publication receipt: published, or incomplete with the operation that
   stopped and why. The ledger alone decides the outcome. Interruption and
   cancellation stop before the next write; created items stay in the
   tracker and in the ledger. A cancelled attempt also records an incomplete
   receipt that names every operation that did not succeed, and a
   reconciliation after cancellation records its ledger update and its
   receipt in one transaction. Publication never adds
   a label or tag from the factory state labels, refuses a preview that
   contains one, copies no conversation turns into the payload, and never
   starts a run.

## Consequences

The GitHub (#273) and Azure DevOps (#274) adapters implement
`tracker.PlanPublisher` and must keep `PreflightPlan` deterministic, or every
retry after approval reports a stale approval. They choose their own
correlation encoding and must make `FindPlanItems` a read that sees a
just-created item where the provider offers one. The coordinator command path
(#267) and the review surface (#271) call `publication.Publisher`: `Preview`
for the binding that `store.ApprovePlan` records, `Publish` with the command
request ID as the attempt ID, and `Reconcile` for an ambiguous operation. The
in-memory fake in `internal/tracker/trackertest` supports failure injection
for their tests.

## Considered options

- **Widen `pending_effects` to many rows per owner.** Rejected: every reader
  of the one-row journal, recovery, cleanup, and doctor would change, and the
  replay handlers still return a Run.
- **Retry a create after an unknown outcome when the lookup finds nothing.**
  Rejected: provider listings can lag a create that is still in flight, and the
  cost of a duplicate backlog item is higher than one operator confirmation.
- **Render again at publication time.** Rejected: the human approved exact
  bytes; a fresh rendering could differ silently.
