---
status: accepted
---

# The GitHub lease is one owned milestone

The coordinator renewed its GitHub lease by adding a pending `factory/lease` Commit Status to the target-branch head on every polling pass. GitHub accepts at most 1,000 statuses for each commit and context. With a 30-second interval and no new commits, an idle coordinator reached that cap after about 8 hours 20 minutes. Lease renewal then failed on every pass and the supervisor stopped. A restart did not help, because statuses cannot be deleted.

The lease is now the description of one closed milestone titled `factory coordinator lease`. GitHub enforces unique milestone titles in a repository, so one repository can hold at most one lease projection, and a lost creation response cannot cause a duplicate. Every renewal edits that milestone. The number of resources therefore does not depend on the polling count, on run changes, or on target-branch commits. The coordinator rewrites only a marked block. The block contains the coordinator, the run, and the complete renewal and expiry times. Long identities therefore do not truncate the expiry. The coordinator keeps human text outside the block. It adopts a milestone only when the authenticated account created it and the block is present; a copied marker alone does not prove ownership. The first renewal creates the milestone, as the run that needs a draft pull request creates it. Milestones need repository write access and, for a token, issue write permission. The factory already needs both to publish run branches and to edit labels and comments.

The host lock remains the ownership authority. The GitHub lease is a diagnostic projection, not distributed fencing.

ADR 0018 makes the coordinator lease optional per adapter. This decision applies to the GitHub adapter only.

## Considered options

- Rotate status contexts, or attach statuses to new commits. Rejected: this only delays the cap and leaves an unbounded stream of statuses.
- Check Run. Rejected: it is editable, but it needs a GitHub App. The `gh` user credential is not an App.
- Label description. Rejected: the 100-character limit can truncate long identities.
- Dedicated lease issue or comment. Rejected: consistent discovery needs a label or a stored identity, and the issue appears in the issue queue.
- Repository variable. Rejected: it needs administrator access.

Legacy `factory/lease` statuses stay on their commits because GitHub cannot delete them. The coordinator never writes another one, so an installation at the cap needs no migration step.
