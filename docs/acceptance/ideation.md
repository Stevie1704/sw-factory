# Ideation acceptance evidence

This record is the evidence of issue #275 for specification #265 (from an
idea to a reviewed, linked backlog). It follows the conventions of the
[agent-journey baseline](baseline.md): deterministic evidence and live
evidence stay apart, and a scenario that did not run is `not-run`, never
`pass`. Read the scenarios with `factory acceptance scenarios --json`; the
ideation scenarios are `ideation-publish-github`,
`ideation-publish-azure-devops`, `ideation-without-backlog`, and
`ideation-recovery`.

- [Results](ideation-results.json): the recorded outcomes.
- [Report](ideation/acceptance-report.md) and its
  [JSON form](ideation/acceptance-report.json), written by
  `factory acceptance report`.

Mocked providers are not live-provider proof. Specification #265 stays open
until the live GitHub and Azure DevOps scenarios below have run.

## Identities

| Item | Value |
| --- | --- |
| Platform | darwin/arm64, macOS 27.0.1, Go 1.27.2 (go.mod toolchain) |
| Release | Development build, revision `783a70888554c5ff89722ecdf2e604dd25cc96c0` (branch `feat/275-acceptance-docs` on the integration branch at `85195e9`) |
| Pinned worker image | `factory.yaml` pins `ghcr.io/stevie1704/sw-factory-worker@sha256:20bad8e4d840d27599c64f047eebba1084fa22ca04112a8dde908a7420bda7b6`. It predates the session roles, so it is not the image a session role needs. |
| Local worker build | `make worker-build` on colima (linux/arm64) passed in 176 s: digest `sha256:cd4fcd3209e470252d1e01a8eaa31257db54697c69e50bedbf7dc5b6176a01b1`, headless lifecycle verification passed for the Codex and Claude Code adapters, `factory-report` has `--ideation-file` and `--plan-file`, and the `grilling` skill is installed for both harnesses. The image was not pushed and the pin was not changed. |
| Worker harnesses | Claude Code 2.1.232 and Codex 0.148.0 (build defaults of the local image); no live invocation ran. |
| Host harness | Not observed: no live scenario ran. The implementing agent was Claude Code (Claude Opus 5.5) under an orchestrator; it is not product evidence. |

## Scenarios that ran (deterministic)

The integrated scenarios are in `internal/factory/ideation_acceptance_test.go`.
Each drives the real UI handler, the command inbox, one coordinator inbox
pass per step, session roles over the real Codex headless adapter with
report fixtures, plan validation, the publication ledger, and the real
GitHub or Azure DevOps planning adapter over a stateful scripted `gh` or
`az` (`internal/github/githubtest`, `internal/azuredevops/azuredevopstest`).

| Test | GitHub | Azure DevOps | What it shows |
| --- | --- | --- | --- |
| `TestIdeationJourneyPublishesTheReviewedPlanToEachTracker` | pass | pass | Idea, two question rounds, an edited brief, a planning draft, a human plan edit, a preview, one approval, and three linked items with the edited title. The parent has both tasks as sub-issues (GitHub) or children (Azure, Epic with User Stories); the documentation task is blocked by the export task (GitHub) or has it as its predecessor (Azure). Every item URL is readable after a browser reconnect. |
| `TestIdeationJourneyPublishesASingleItem` | pass | pass | A one-task plan publishes as one item without a link. |
| `TestIdeationJourneyEndsWithoutBacklogWork` | pass | pass | Defer, resume, and no-build make zero tracker writes and no plan. |
| `TestIdeationJourneyPublishesUnresolvedRequirementsAsInvestigationWork` | pass | pass | A handoff that does not name the open question is refused; the explicit deferral leads to an investigation item and a dependent task. |
| `TestIdeationJourneySurvivesRestartsAndDuplicateSubmissions` | pass | pass | Two coordinator restarts and browser reconnects keep the turns and the reviewed plan revision. A double-sent turn launches one role; a double-sent approval and a resent approval after publication make no second write and leave one receipt. |
| `TestIdeationJourneyRefusesStaleApprovalAndChangedMapping` | pass | pass | An approval of a replaced preview is refused. A mapping change after approval (GitHub: native links no longer offered; Azure: a required Feature level) refuses publication with zero tracker writes. Saving the plan again, a new preview, and a new approval publish under the new mapping. |
| `TestIdeationJourneyRefusesAMalformedPlanGraph` | pass | (adapter-independent) | A planning draft with a dependency cycle saves no plan and names the cycle; a malformed user edit is refused and the earlier revision stays publishable. |
| `TestIdeationJourneyCancellationLeavesNoTrackerEffect` | pass | (adapter-independent) | Cancel stops the active role process, its late report is ignored, and publication is refused with zero tracker writes. |
| `TestIdeationJourneyRecoversRemoteSuccessAfterACoordinatorStop` | pass | pass | The coordinator stops while a create is in flight after the tracker made the item. The receipt is incomplete with that create's outcome unknown; after a restart, a retry adopts the item by its correlation key; each item exists once. |

In every scenario no item carries `agent-ready` or a factory state label or
tag, no item contains conversation text, and no run starts. The scenario
manifest also names the per-issue tests that guard the same semantics in
isolation.

Regression: `make check` (formatting, vet, all tests including the
issue-to-pull-request flows and the read-only run and evaluation
inspection tests, build) passed, as did `make test-race` and `make
vuln-check` (source and the three binaries clean with govulncheck v1.8.0).

## Scenarios that did not run (live)

Live GitHub and Azure DevOps runs were not authorized for #275. Each of these
is a specific unverified scenario:

1. GitHub, native links: publish a three-item plan to a disposable repository
   with sub-issues and blocked-by links
   ([procedure](github-plan-publication.md)).
2. GitHub, textual fallback: the same on a host without sub-issues or
   dependencies.
3. GitHub, retry: a lost create response against the real API and the retry
   that finds the issue by its body marker through the issue listing.
4. Azure DevOps, Agile mapping: Epic and User Stories with parent and
   predecessor links, the Markdown description rendering, and the creation of
   the `factory-plan:` tag definitions
   ([procedure](azure-devops-plan-publication.md)).
5. Azure DevOps, Scrum mapping with a required Feature level and an
   intermediate item.
6. Azure DevOps, retry: a lost create response and the WIQL correlation
   lookup that the retry depends on.
7. Live ideation and planning turns on Codex and on Claude Code with a worker
   image of this release, including the paid `grilling` skill smoke.
8. A live coordinator restart during a role turn and during publication.

## Interventions, time, and usage

| Item | Value |
| --- | --- |
| Human interventions | None in the deterministic scenarios. During the implementation, usage limits stopped the agents several times; the orchestrator resumed them (cause: `environment`). |
| Elapsed time | Integrated journeys 3 s; full `go test ./...` 16 s; `make check` 21 s with a warm cache; `make test-race` 124 s; `make vuln-check` 7 s; `make worker-build` 176 s. |
| Usage | No paid model call ran: the scenarios use report fixtures. The token usage of the implementing agent was not available to it and is not recorded. |

Conversation content is not telemetry. This record holds counts and
identities only.

## Baseline: plan edits and clarification requests

Measure these per published session, from local evidence or with the
operator's consent, and never from conversation text:

- **Human plan edits**: the applied `edit_draft` commands of the session
  before its approval, from `GET /api/sessions/<id>` on the UI (the commands
  list, newest 50), or the plan revisions after the first planning draft.
- **Later implementation clarification requests**: for each run claimed from
  a published item (the item URL in the publication receipt), the
  invocations that ended in `waiting_for_human` in
  `factory run inspect --run-id <id> --json`, which are the clarification
  questions the run asked.

Local evidence on this host: the one operational store (store schema 39,
from an earlier release) holds one run and no ideation session. No session
was published, and no run came from a published item. The baseline is
therefore empty: zero measured sessions and zero measured runs. The
deterministic scenarios make exactly one human plan edit by construction;
that is a fixture, not a measurement. No success threshold is set. Record
the first values after live sessions run, and compare later releases with
them.

## Open items

- Re-pin the worker image: push the rebuilt worker with `make
  worker-publish`, pin its digest in `factory.yaml`, and record a skill smoke
  that includes `grilling`. This is a human release step (registry push and
  paid smoke).
- `factory reset` and `factory cleanup` do not remove session-role Docker
  volumes or the snapshot worktree of a role active at coordinator stop
  (ADR 0023); the guide names the manual removal.
- After a tracker mapping change, an approval can no longer publish; the user
  must save the plan again to return it to review before a new approval. A
  publication that is already incomplete when the mapping changes can only
  be cancelled.
