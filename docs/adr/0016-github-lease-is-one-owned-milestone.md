---
status: accepted
---

# The GitHub lease is one owned milestone

The coordinator renewed its GitHub lease by adding a pending `factory/lease` Commit Status to the target-branch head on every polling pass. GitHub accepts at most 1,000 statuses for each commit and context. With a 30-second interval and no new commits, an idle coordinator reached that cap after about 8 hours 20 minutes. Lease renewal then failed on every pass and the supervisor stopped. A restart did not help, because statuses cannot be deleted.

The lease is now the description of one closed milestone titled `factory coordinator lease`. GitHub enforces unique milestone titles in a repository, so one repository can hold at most one lease projection, and a lost creation response cannot cause a duplicate. Every renewal edits that milestone. The number of resources therefore does not depend on the polling count, on run changes, or on target-branch commits. The coordinator rewrites only a marked block that contains the coordinator, the run, and the complete renewal and expiry times, so long identities do not truncate the expiry. It keeps human text outside the block. It adopts a milestone only when the authenticated account created it and the block is present; a copied marker alone does not prove ownership. The first renewal creates the milestone, the same way the first draft pull request is created by the run that needs it. Write access, which publishing a run branch already requires, is enough to create and edit milestones.

The host lock remains the ownership authority. The GitHub lease is a diagnostic projection, not distributed fencing.

## Considered options

Rotating status contexts or attaching statuses to new commits was rejected because it only delays the cap and leaves an unbounded stream of statuses. A Check Run is editable but needs a GitHub App, which the `gh` user credential is not. A label description is limited to 100 characters, which can truncate long identities. A dedicated lease issue or comment needs a label or stored identity for consistent discovery, and it appears in the issue queue. A repository variable needs administrator access.

Legacy `factory/lease` statuses stay on their commits because GitHub cannot delete them. The coordinator never writes another one, so an installation at the cap needs no migration step.
