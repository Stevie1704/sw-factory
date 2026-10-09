---
status: accepted
---

# The Azure DevOps adapter uses the Azure CLI and keeps work items and pull requests apart

ADR 0018 made the work tracker and the code host factory-owned ports. Issue #222 adds the second adapter: Azure DevOps Services, with Azure Boards work items as the work tracker and Azure Repos as the code host. One adapter implements both sides, as the GitHub adapter does. The operator selects it with an `azure_devops` block in the host registration in place of the `github` block.

## Decisions

1. **The Azure CLI owns the credential.** Every call is one `az rest` command with the Azure DevOps resource id. az resolves the credential from `az login`, which can be a user, a service principal, or a managed identity. The adapter never reads, stores, or prints it, as the GitHub adapter does with `gh`. A personal access token is not used, because the pilot rule requires a company-managed identity.
2. **Azure DevOps Services only.** The adapter calls `dev.azure.com` with REST API 7.1. Azure DevOps Server is out of scope until a user needs it.
3. **Tags carry the run state.** Each factory label is a work item tag. The adapter never changes the work item state field, so team board rules and the process template stay untouched. A tag is created on its first use, so `bootstrap-labels` changes nothing; `factory doctor` requires the Create tag definition permission while a factory tag is missing. A work item is open unless its state is in the Completed or Removed category, which works for every process template.
4. **The adapter checks who added `agent-ready`.** On GitHub, only a user with triage access can add a label. Any editor can add a tag in Azure Boards. A work item is therefore eligible only when the last update that added `agent-ready` came from an authorized user. The queue and every direct issue read apply this check, so a claim cannot admit a work item that an unauthorized user tagged. A claimed work item no longer has the tag, so the history is read only before a claim. The adapter receives the authorized users from the composition root.
5. **One text conversion.** A description or comment in HTML converts to text: block ends and line breaks become new lines, list items start with `- `, other tags go, entities are decoded, and HTML comments stay. The route marker and the command language therefore read the same text as on GitHub. A Markdown field or comment passes unchanged. The factory writes its own comments as Markdown.
6. **Pull-request comments have their own seam.** Work items and pull requests have separate number spaces and separate comment APIs, so work item 7 and pull request 7 are different objects. The code-host port gains two optional interfaces, `PullRequestCommentReader` and `PullRequestCommentClient`. The coordinator reads the run's pull request, and posts clarification questions there, through them when an adapter implements them; a separate code-host adapter supplies them through `factory.Dependencies`. With these seams, the issue and the pull request are two comment streams even when their numbers are equal. GitHub does not implement them, because its pull-request comments are issue comments; its behavior does not change. The clarification payload gains a `PullRequest` flag; a payload stored earlier decodes as false, which is correct for GitHub.
7. **Comment identities sort by creation time.** The coordinator keeps one comment watermark across the issue and the pull request. Azure comment ids are unique only per work item or per thread. Each Azure identity therefore starts with the fixed-width UTC creation time and then names its source, for example `20261009T100000.000000000Z.w42.c7` or `20261009T110500.000000000Z.p17.t6.c1`. The watermark compares identities that are not numbers as text, so they sort in creation order. GitHub identities stay numeric and compare as numbers. The edit methods read the work item, thread, and comment from the identity.
8. **A vote is a review.** Azure Repos has no review event. Every vote change creates a system thread, which gives the review a stable identity and time. The service account authors that thread; its `CodeReviewVotedByTfId` property names the voter by identity id, and the pull request's reviewer list resolves that id to a login. A vote of -5 (waiting for author) or -10 (rejected) is `CHANGES_REQUESTED`; 5 and 10 are `APPROVED`. The voter's open threads published up to the vote are the findings: general threads form the body, file threads the inline comments.
9. **No coordinator lease.** Azure DevOps has no milestones. The adapter does not implement `tracker.LeaseClient`, so the coordinator skips renewal (ADR 0018).
10. **Fail before a limit, never truncate.** Azure Repos accepts at most 4,000 characters in a pull-request description. The adapter rejects a longer body before the call, because truncation could remove a marker or human text. For the same reason, it reads a found pull request by id: the list endpoint truncates descriptions to 400 characters.

## Consequences

- The factory's generated pull-request body includes the complete issue body and review findings, so it can exceed 4,000 characters. Such a run fails at the draft pull request on Azure Repos until a follow-up shortens the generated body for that host.
- `factory register` still infers only a GitHub identity. An Azure DevOps registration is written into the host configuration by hand (see `docs/configuration.md`).
- The mapping is tested against a scripted `az` runner. A run against a disposable Azure DevOps project is still open.

## Considered options

- A personal access token in an environment variable. Rejected: it is a personal credential, and the adapter would have to handle the secret.
- The work item state field for run states. Rejected: it can conflict with board rules and needs a per-template state map.
- A coordinator-wide comment identity type with an explicit order field. Rejected for now: text order of a time-prefixed identity keeps the stored watermark a string and changes no GitHub behavior.
