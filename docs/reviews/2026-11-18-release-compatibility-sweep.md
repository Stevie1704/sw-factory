# Release compatibility removal (#317)

This records the sweep against starting commit
`a68524d53ff704b814988865cded7a716acfd41c`. There is no installed release to
support. Schema 53 is the first supported operational-store baseline;
`MinimumUpgradeSchemaVersion` is explicitly 53. The upgrade entry point and
migration gate introduced by #316 remain the path for subsequent releases.

This release has no migration between supported existing schemas: the minimum
and current versions are both 53, so the backup-and-migrate branch is dormant.
`migrate` currently creates a store from version zero only. A subsequent 53→54
upgrade must add version dispatch and its explicit migration step there; this
change retains the entry point, gate, and backup boundary rather than a list
of obsolete steps.

## Schema comparison

Before removing the chain, a temporary Go probe called `store.Open` on a fresh
private database at the starting commit and read:

```sql
SELECT type, name, tbl_name, sql
FROM sqlite_schema
WHERE sql IS NOT NULL
ORDER BY type, name;
```

The baseline DDL was taken from this final schema, excluding SQLite's own
`sqlite_sequence` table, which SQLite creates for AUTOINCREMENT tables. The
probe was run again after baseline creation. The ordered records matched after
removing whitespace from each SQL definition: **32 tables (31 application
tables plus `sqlite_sequence`), 19 explicit indexes, and one trigger**. This
compares column order, types, defaults, nullability, keys, checks, indexes,
partial-index predicates, and trigger bodies, not only table names.

SHA-256 of the normalized records, encoded with Python
`json.dumps(records, separators=(',', ':'))`:

```text
11ffcc04bc37dea8d3046bf5839ea3b12089ed35b7ab585a3b55e03269ab6955
```

Both probes produced this digest. Metadata values deliberately differ: the
chain records version 52; the baseline records 53. The baseline creates its
metadata and all application objects in one transaction. No chain or old
migration fixture is shipped for this comparison.

## Removed

| Location | Earlier-release support removed |
| --- | --- |
| `internal/store/store.go`, `store_baseline.sql`, `upgrade.go` | Schema steps 1–52, intermediate-column tolerance, duplicate-run/invocation reconciliation, timestamp normalization, and completion of an existing schema-zero initialization; baseline and minimum upgrade version become 53. |
| `internal/store/ideation*.go`, `local_claims.go`, `local_execution.go`, `local_issue_identity.go`, `local_issues.go` | Additive schema helpers and scattered historical DDL; all final objects are in the baseline. |
| `internal/store/*_test.go` | Old migration fixtures, migration-only tests, and their migration helper. Current creation, round trips, uniqueness, upgrade inspection, and refusal coverage remain. |
| `internal/prompt/prompts/legacy/`, `prompt.go`, `session.go` | Earlier run/session prompt bodies, their version/digest registries, and old craft-rendering branches. Current embedded bytes and hashes are unchanged. |
| `internal/prompt/prompt.go`, `internal/factory/invocation_lifecycle.go`, `review_units.go` | Inline/omitted review-diff fields, commands, inherited review handoffs, and the historical delivery branch. Current reviews use immutable mounted artifacts. |
| `internal/factory/agent.go`, `agent_launch.go`, `invocation_materialisation.go`, `repository_craft.go` | Invocation packet schemas 1–10, validation version guards, and historical review-artifact detection. Only schema 11 is accepted. |
| `internal/report/report.go` | Standards-review-v1 exemption from current evidence provenance validation. |
| `internal/store/store.go`, `internal/factory/human_review.go`, `review.go`, `review_repair.go` | Mirrored `review_id`, inference of maintainer provenance from it, implicit factory repair source, and implicit completed review outcomes. Current records carry their source/event and outcome explicitly. |
| `internal/factory/recovery.go`, `invocation_lifecycle.go` | Old check/waiting-for-harness recovery reasons and cannot-proceed launch rollback without the current launch-void marker. |
| `internal/factory/progression.go` | Missing agent timeout treated as an unlimited deadline. |
| `internal/factory/test_stage.go` | Synthesizing a missing test-revision history entry during recovery. |
| `internal/factory/report_acceptance.go`, `internal/effect/result_acceptance.go` | Reading mutable disk evidence for a journaled acceptance missing its immutable report snapshot. Readers and replay now require the recorded snapshot before mutation. |
| `internal/store/local_claims.go` | Scanning all claims to match old graph-dependent keys; current claims use the exact revision key and readers reject rebound/old keys. |
| `internal/store/cleanup.go` | Using updated_at when an old row lacks terminal_at. |
| `internal/worker/runtime.go`, `internal/factory/reset.go` | Run-ID fallback in credential-volume names and the removal/reset request fields that existed solely for that fallback. Destructive credential removal requires an explicit store identity. |
| `internal/worker/headless.go` | Inferring helper start failure from stderr for old worker helpers. The helper's explicit process state owns classification. |
| `internal/config/config.go` | Special drained-upgrade instructions for host schema 1, and the RepositoryPolicyError alias. All unsupported positive host and repository schema versions, older or newer, use UnsupportedSchemaVersionError; missing/non-positive versions remain validation failures. |
| `internal/store/store.go`, `internal/factory/recovery.go`, `internal/git/worktree.go`, `internal/store/evaluation.go` | ImplementationHandoff, RecoveryInfrastructureError, RecoveryWorkflowError, LocalGitWorkspace, and the unused RecordEvaluationDisposition forwarding alias. |
| `internal/effect/legacy_payload_test.go` | Replay fixtures existing solely to accept earlier serialized payload shapes. Current journal/replay tests remain. |
| `docs/configuration.md`, `agent-runtime.md`, `repository-initialization.md`, `architecture.html` | Active instructions for old registrations, lease statuses, invocation packets, check-wait states, and onboarding copies. |

## Kept

| Location | Reason |
| --- | --- |
| `internal/store/upgrade.go`, `internal/factory/store_migration.go`, `internal/upgrade/`, CLI upgrade commands | Explicit upgrade entry point, backups, schema limits, lock ownership, and migration gate required by #316 and this issue. No pre-baseline schema is migrated. |
| `internal/factory/invocation_lifecycle.go`: credential identity adoption | A current invocation can launch without a registered auth source. An operator may register a source while it is active; recovery and auth refresh persist its repository credential identity before worker recreation. This is current registration behavior, not earlier-release data support. |
| `internal/config/config.go`: override_harnesses default | Current authorization policy in ADR 0027: an omitted list stays limited to Codex/Claude when future adapters are added. Removing it would widen authority or change the current optional-field contract. |
| `internal/config/providers.go`, `internal/cli/adapters.go`, `internal/factory/factory.go` | Current provider inference and combined remote adapters still serve ordinary GitHub/Azure registrations; local tracking selects its code host independently. |
| `internal/factory/comment_streams.go`, `internal/store/store.go`: watermark encoding | Current GitHub writes raw shared-namespace IDs; local/Azure adapters need scoped issue/PR/operator IDs. These are concurrent provider contracts. |
| `internal/factory/{adapters,activity,claim,gate,run_persistence,report_acceptance,recovery,readiness,progression,review_units}.go`, `internal/effect/`, `internal/gate/` | Optional capabilities of current dependency-injected stores, workspaces, trackers, gates, and harness adapters. Focused/embedding adapters can omit journals, plural projections, manifests, or PR readiness. Production SQLite uses durable capabilities. These paths do not open old on-disk schemas. |
| `internal/factory/review.go`, `progression.go`, `internal/config/config.go` | Repositories may configure a subset of current roles. Manual/single-role review remains valid independent of release history. |
| `internal/worker/runtime.go`, `internal/factory/workflow_helpers.go`, `internal/effect/payload.go` | Ordinary roles and gate workers still use run-scoped resources; isolated read-only invocations use invocation-scoped workers. Optional runtime mount-inspection capabilities remain current adapter seams. |
| `internal/report/report.go` | Standalone report writers infer report kind from the current role; unit IDs apply to partitioned reviews only. No earlier prompt contract is selected. |
| `internal/publication/publication.go`, `internal/tracker/tracker.go` | Optional per-item repository targets and Azure project IDs are current provider differences. Canonical JSON encoding and omitted optional values remain deterministic. |
| `internal/store/evaluation.go` | Evaluation summaries are optional at focused store seams; content-free historical counters support current cleanup/retry behavior. |
| `internal/github/`, `internal/azuredevops/`, `internal/git/discovery.go` | External GitHub/API capabilities and review anchors, Azure routes, and dev.azure.com/visualstudio.com URL formats are explicitly out of scope. |
| `internal/worker/{doctor,release,runtime}.go`, `internal/harness/`, worker scripts | Pinned Copilot protocol, native base-image variants, Docker/Git behavior, and worker release mismatch checks remain current external/runtime contracts. |
| Configuration paths, environment variables, CLI flags and commands | No accepted renamed/deprecated key, old environment/path alias, or deprecated command/flag was found beyond the removals above. Current defaults and explicit credential fallbacks remain. |
| `docs/adr/`, `docs/spikes/`, `docs/acceptance/`, prior `docs/reviews/` | Historical records remain unchanged as requested. References to older run revisions, snapshots, or publication history concern current lifecycle state, not earlier factory releases. |
