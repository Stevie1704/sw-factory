# Production readiness review — 2026-10-01

Reviewed commit: `824b6e4d305950461ca603855dbffdade42614aa`.

## Assessment and scope

The architecture is a credible foundation for a supervised company tool. The immediate target is a single-repository POC; containment and supported workflow reliability still need work before using it with company code. A shared service across repositories remains a later expansion.

After the initial assessment, the user narrowed the first milestone to **a single-repository POC**. The POC can use one registered repository, a dedicated operator-controlled host, GitHub supervision, and a foreground coordinator. Continuous managed-service operation and shared multi-repository supervision are later milestones. The operating system remains unspecified.

The current product supports one registered repository: `internal/config/config.go:463` rejects multiple registrations, and coordinator operations select `host.Repositories[0]`. This fits the POC, so #108 is deferred. Automatic merging remains outside the product contract.

The review examined repository guidance, current domain documentation and ADRs, the CLI and coordinator, configuration, Git/GitHub adapters, workflow and report acceptance, durable effects and SQLite, gates and repair, worker isolation, harness adapters, builds and CI. It read all seven open GitHub issues, their available comments, and the wider issue inventory. This is a targeted production review, not a line-by-line audit of every source file.

## What is already strong

- Workflow authority stays in the coordinator. Structured agent reports are validated proposals rather than commands to mutate workflow state.
- Specification packets, image identities, checkpoint gates, and independent review preserve explicit identities. Repository guidance and craft cannot redefine factory authority.
- Test protection, finite repair budgets, authorized GitHub commands, and human merge ownership provide useful supervision boundaries.
- Review workers mount the checkout read-only and use separate invocation state.
- Workers run as non-root, drop capabilities, use `no-new-privileges`, and avoid mounting the Docker socket, host home, SSH agent, and operational database.
- SQLite has schema versioning, compare-and-set updates, pending-effect reservations, migration backups, and fail-closed handling of newer schemas.
- CI now checks formatting, vet, tests, builds, race detection, and the pinned worker's Docker lifecycle.

These strengths reduce risk, but passing tests do not resolve the defects below.

## New findings and published tasks

These tasks were not represented by the seven open issues at initial review time. At the user's request, they have now been published as comprehensive GitHub issues #208–#212, each labelled `bug` and `ready-for-agent`, with evidence, required behavior, scope, acceptance criteria, and related work. They can be implemented independently and do not depend on #108.

### P1 — Prevent Git hooks from executing repository code on the coordinator host

Tracking issue: [#208](https://github.com/Stevie1704/sw-factory/issues/208).

`internal/git/worktree.go:186` inherits the host Git configuration and environment. Checkpoint creation invokes ordinary `git add` and `git commit` at `internal/git/worktree.go:493` and `:503`; push similarly invokes ordinary Git. There is no factory-owned hook policy.

A disposable integration probe configured `core.hooksPath=.hooks`, supplied a repository `pre-commit` script, and invoked the production `CreateCheckpoint` method. The hook wrote a marker on the coordinator host. This reproduces a real host execution path without a container escape. It also means normal company pre-commit tooling can unexpectedly execute outside the worker, with host credentials and environment available to it.

Acceptance:

- Factory Git operations disable repository-controlled hooks, including commit, merge, checkout/worktree, and push hooks.
- Required repository checks run through declared worker gates.
- Review adjacent Git execution mechanisms, including configured filters, external diff/text conversion, signing, and credential helpers, and define a narrow host-side policy. Credential helpers needed for authorized transport require deliberate treatment.
- A disposable-repository regression test proves a tracked hook edited by an agent cannot run on the host.

### P1 — Replace the append-only commit-status lease heartbeat

Tracking issue: [#209](https://github.com/Stevie1704/sw-factory/issues/209).

`internal/github/github.go:381` reads the target branch and creates a new `factory/lease` status every time it renews the lease. `internal/factory/polling.go:173` does this on each polling pass, including idle operation. Registration defaults to a 30-second interval.

GitHub permits only 1,000 statuses per SHA and context. An unchanged target commit therefore exhausts the lease status allowance after roughly 8 hours 20 minutes of idle polling, less if statuses already exist. Lease failures skip ordinary progression, and repeated failures eventually terminate the supervisor. Restarting does not remove accumulated statuses. This is inferred from the exact production call path and the documented API limit; the review did not create 1,000 live statuses. [GitHub commit-status documentation](https://docs.github.com/en/rest/commits/statuses#create-a-commit-status).

Acceptance:

- Use a bounded, editable lease projection rather than an indefinite append-only status stream.
- Preserve owner identity and complete expiry information; truncating the current description can also cut off expiry for long identities.
- Simulate more than 1,000 renewals against an unchanged commit and verify continued progression and bounded external state.
- Exercise lease transport failure and restart behavior without consuming workflow repair budgets.

This should be fixed before #204 makes the process continuously available.

### P1 — Terminate timed-out gate and setup processes inside the worker

Tracking issue: [#210](https://github.com/Stevie1704/sw-factory/issues/210).

`internal/gate/runner.go:303` and `:424` create setup/gate deadlines. `internal/worker/runtime.go:465` executes commands through `docker exec`; its command runner cancels the host Docker CLI via `exec.CommandContext`. That does not implement cancellation of the command's process group inside the container.

A probe used the production Docker runtime and pinned worker image, set a one-second deadline, and ran a command that slept three seconds before writing a marker. The API returned `context deadline exceeded`, but the marker was subsequently written. The timed-out command continued executing inside the worker.

Independent gates or a repair invocation can consequently overlap with a command already reported as timed out. The headless harness helper has explicit process-group cancellation; deterministic commands need comparable lifetime ownership.

Acceptance:

- Setup and gates have invocation-specific process identities and bounded TERM/KILL cancellation inside the worker.
- Returning a timeout confirms that the command and its descendants have stopped, or records a typed infrastructure discrepancy and prevents further work on that worker.
- A real Docker regression proves a timed-out command cannot write afterward and cannot overlap a subsequent gate or repair.

### P1 — Honor non-blocking gates at final readiness

Tracking issue: [#211](https://github.com/Stevie1704/sw-factory/issues/211).

The gate runner deliberately allows an independent `blocking: false` command to fail. Existing runner tests cover this behavior. However, `ensureFinalCheckpointGatesPassed` at `internal/factory/gate.go:313` requires every recorded gate outcome to be `passed`, regardless of its blocking flag. Final readiness calls this function at `internal/factory/readiness.go:108`.

A targeted readiness probe supplied a passing required gate and a failing non-blocking gate. The function rejected readiness with `final checkpoint gate "advisory" has outcome "failed"; readiness requires success`.

Acceptance:

- Preserve complete, exact-checkpoint result validation for all gates.
- Permit failed independent non-blocking gates at readiness and report their outcomes visibly.
- Continue blocking when a required gate is failed or skipped because its prerequisite failed.
- Add a Factory-level regression covering claim-to-ready with a failed non-blocking gate.

### P1 — Update and scan the actual Go build artifacts

Tracking issue: [#212](https://github.com/Stevie1704/sw-factory/issues/212).

Both worker Dockerfiles default to Go `1.25.0`. The locally built coordinator uses Go `1.25.5`. Binary-mode `govulncheck` flagged three standard-library advisories in the coordinator: [GO-2026-4602](https://pkg.go.dev/vuln/GO-2026-4602), [GO-2026-4601](https://pkg.go.dev/vuln/GO-2026-4601), and [GO-2026-4341](https://pkg.go.dev/vuln/GO-2026-4341). A source scan explicitly targeting Go `1.25.5` also reported these symbols, with paths through reset directory listing, Git remote URL parsing, and SQLite DSN formatting.

This establishes vulnerable toolchain/symbol presence, not application exploitability. For example, the `os` advisory requires specific Root-related circumstances not established here. The versions containing the identified fixes include Go `1.25.8` for the first two and `1.25.6` for the third; choose a currently supported patched release rather than stopping at those minimum fixes.

An initial source scan automatically switched to Go `1.26.8` to run the latest scanner and found no vulnerabilities. That result does not clear artifacts built with the older toolchain. Binary scanning exposed the difference.

Acceptance:

- Align coordinator builds, CI, worker toolchains, and in-image helper builds on an approved patched Go release.
- Rebuild and repin the worker image; regenerate digest-specific skill smoke evidence as required.
- Scan built coordinator/helper binaries and the final container contents, including Node, harness packages, and OS packages.
- Add recurring dependency/image review and security scanning to release validation.

## Existing GitHub issues

| Priority for the POC and subsequent rollout | Issue | Disposition |
| --- | --- | --- |
| P1 | [#199: setup failure strands check runs](https://github.com/Stevie1704/sw-factory/issues/199) | Fix before rollout. The current classification still sends setup failure to check/waiting_for_harness without a runnable check-stage agent. Separate non-zero repository setup commands from worker/runtime failures and provide gate-suite re-drive. |
| P1 for broader rollout | [#181: durable-effect failure injection](https://github.com/Stevie1704/sw-factory/issues/181) | Complete handler-specific crash/replay tests before broader/unattended use; the POC must at least exercise its supported interruption/recovery paths. Effect coverage remains 44.1%; the important criterion is replay safety around actual mutations, not a coverage percentage alone. |
| Later for foreground POC; P1 for continuous operation | [#204: launchd/systemd supervisor](https://github.com/Stevie1704/sw-factory/issues/204) | Its existing single-repository scope fits the immediate target. A supervised foreground coordinator is sufficient for the POC. Before managed operation, verify service environment, Docker availability, ownership lock identity, clean stop semantics, logs, and restart behavior. |
| P2 | [#189: field-level configuration diagnosis](https://github.com/Stevie1704/sw-factory/issues/189) | Small, high-value onboarding fix. Render safe field/reason diagnostics while preserving credential redaction. |
| P2 | [#195: recover typed lifecycle pauses automatically](https://github.com/Stevie1704/sw-factory/issues/195) | Decide the deterministic allow-list after #199. Manual resume now exists through closed #194. Keep real discrepancies behind human reconciliation; do not use an LLM to override state agreement. |
| P2 maintenance | [#182: remove workflow/prompt dependence on store](https://github.com/Stevie1704/sw-factory/issues/182) | Useful architecture work, but separate from the POC unless it becomes a concrete dependency. Move shared domain types once; avoid expanding the refactor into a rewrite. |
| Deferred | [#108: multi-repository supervisor](https://github.com/Stevie1704/sw-factory/issues/108) | Outside the single-repository POC. Its independent coordinators, per-repository stores/authorization, and fair host capacity remain the appropriate later direction. Reconcile historical references to a TerminalRuntime with ADR 0010 before implementation. |

Closed issues #179 and #200 have delivered useful CI and diagnostics improvements. They should not be presented as missing work. ADR 0014 explicitly supersedes the abandoned measured-pilot runtime prerequisite; this review does not recommend reopening historical #26. Current-product acceptance evidence is still needed.

## Additional company deployment work

These are hardening or operating requirements rather than reproduced workflow defects.

1. **Bound worker resource use.** `DockerRuntime.Start` declares no memory, CPU, PID, or disk/log limits. A runaway test or dependency can exhaust the worker host/VM. Add host-owned limits, OOM classification, and a documented disk/log retention policy. Docker does not set memory constraints by default. [Docker resource constraints](https://docs.docker.com/engine/containers/resource_constraints/).
2. **Define and enforce network/data boundaries.** `--network bridge` provides connectivity, not a public-internet-only policy. Decide which provider and package/research endpoints are allowed, block access to host/internal company services as appropriate, and verify this on the actual Docker backend and company network. Start on a dedicated host/account with repository-scoped GitHub access and company-approved harness credentials. These controls can be deployment-owned; they need not all become application configuration. [Docker bridge networking](https://docs.docker.com/engine/network/drivers/bridge/).
3. **Bound host adapter calls.** Git and GitHub runners use unconstrained buffers and inherit caller contexts; the supervisor context lasts for the service lifetime. Add operation deadlines, non-interactive Git behavior, bounded output, and rate-limit-aware retries. Prove a hung transport cannot block lifecycle commands forever. Doctor should finish with actionable findings when dependencies hang.
4. **Provide backup, restore, upgrade, and rollback procedures.** Migration-time database copies are not a regular disaster-recovery policy. Recovery depends on the database, worktree/Git state, packet/diff artifacts, and role/session volumes together. Demonstrate a restore that reconciles against GitHub and refuses stale/divergent effects safely. Document retention of migration backups and content-bearing artifacts. Avoid silently changing the ADR decision against a full audit/transcript archive.
5. **Create a traceable release.** There are no GitHub releases or Git tags in the inspected checkout. Ship an identifiable coordinator build with the corresponding worker digest, checksums/build metadata, supported-platform matrix, vulnerability results, and migration notes. Add a version/build-info CLI surface. A company rollout needs an exact deployable revision even if package-manager distribution remains deferred.
6. **Run current-product acceptance and soak tests.** The worker verification exercises Docker and controlled harness lifecycle/protocol behavior, not the full GitHub/LLM workflow on company policy. In an explicitly authorized disposable repository, verify both configured harness paths, required/advisory workflows, clarification, repair, setup failure, cancellation, credential refresh, restart around effects, final readiness, and queue continuation. Then soak through an unchanged target branch, transport outage, Docker restart, host reboot, and cleanup. Record exact versions/digests and operator interventions locally.

## Later shared-service requirements — outside the POC

The following requirements are retained for a future shared service, not as POC prerequisites. Issue #108 already captures much of the needed design. Implement it through reviewable slices rather than simply removing the registration limit:

1. **Catalog and repository selection:** stable identities, canonical path and GitHub identity uniqueness, explicit `--repo`/`--all` selection, per-repository stores and authorization, and safe migration of an existing single-repository installation.
2. **Independent repository coordinators:** one active run per repository, separate polling/backoff/recovery, and failure isolation. A blocked, paused, or unhealthy repository must not stop another repository's commands, cancellation, or progression.
3. **Host-wide capacity:** bound the total active workers and harness/review invocations across repositories, allocate capacity fairly, release it during waits, and make capacity queues observable. Per-container resource limits supplement this scheduler. Quotas and usage should be visible per repository; provider-side spending limits can bound cost where exact local usage is unavailable.
4. **Managed service and aggregate operations:** integrate #204 with the host supervisor, expose repository-specific and aggregate health, and document adding/removing a registration by stopping and restarting under #108's fixed startup scope. Prove lock ownership when foreground CLI and service-manager processes coexist.

Use a company-managed GitHub service identity and approved harness accounts rather than deriving deployment authority from an operator's personal CLI login. Explicitly specify repository permissions and authorized maintainers. Test that a maintainer authorized for repository A cannot cancel, amend, refresh credentials, or trigger repair in B. Keep registration and authorization host-owned so a repository commit cannot grant its own access.

Test cross-repository isolation of operational databases, worktrees, review artifacts, Docker names/volumes, credential projections, and cleanup/reset targets. Sharing dependency caches or provider credentials must be a deliberate host policy, not an incidental consequence of adding registrations. An outage or disk-pressure condition in A must not destroy B's persisted state.

Shared service operation also needs an operator escalation path, bounded log retention, an aggregate unhealthy-service signal, and a named owner for restart, credential rotation, and recovery. This can use local/service-manager logs and GitHub supervision; it does not require transmitting evaluation data or adding a full transcript archive. If company policy requires a historical authorization audit, settle its minimal metadata contract explicitly because the current ADR intentionally rejects a full event/transcript journal.

Confirm the intended host OS/architecture and publish matching worker artifacts. Current worker CI verifies an ARM image, while local checks in this review ran on macOS/arm64. Those results alone do not establish acceptance on a centrally hosted Linux/amd64 deployment.

## Suggested delivery order and release gate

1. Fix host Git execution policy, lease accumulation, remote command cancellation, stranded setup recovery, non-blocking readiness, and patched builds.
2. Add the boundary regressions discovered in this review, deliver #189, and define the POC's company account, repository permissions, network/data boundaries, and resource limits.
3. Run an explicitly authorized single-repository POC through claim, route-selected tests, implementation, Gates, draft Pull Request, independent reviews, repair, readiness, and human merge. Include cancellation, credential refresh, and coordinator/Worker interruption recovery. Keep a supervised foreground coordinator and record exact versions/digests and operator interventions.
4. Before broader continuous use, complete #181, deliver #204, establish monitoring and backup/restore/upgrade runbooks, and perform a multi-day soak on the intended host.
5. Consider #108 and the shared-service requirements only after the single-repository POC succeeds. Expand automation under #195 when operational evidence supports it.

The POC gate should require no known unrecoverable supported state on its intended route, confirmed process cancellation, no repository hook execution on the host, patched build artifacts, tested interruption/recovery paths, a documented company data boundary, and human review before merge. Broader continuous deployment additionally requires complete effect replay coverage, managed-service/lease operation, monitoring, and a successful restore rehearsal. Cross-repository authorization, failure isolation, and fair host scheduling belong to the later #108 milestone. Passing unit tests alone is insufficient.

## Verification performed and limits

- `make check`: passed locally on macOS/arm64, Go `1.25.5`.
- `go test -race -coverprofile=/tmp/sw-factory-coverage.out ./...`: passed. Effect coverage 44.1%, factory 62.3%, store 62.1%, worker 71.5%.
- Current commit's GitHub [Checks run](https://github.com/Stevie1704/sw-factory/actions/runs/35976065053): succeeded.
- `scripts/verify-headless-worker.sh` against configured image digest `sha256:a6e2e078f170cf449a60660a494ea08e25cd293bee0ce9026b0662c188fb01fb`: passed on local Docker `29.5.2`.
- Disposable production-adapter hook and gate-timeout probes, plus a focused readiness probe: reproduced the three described defects. Temporary probe source files, repositories, workers, and volumes were removed.
- Binary and explicitly version-targeted source vulnerability scans: flagged the three standard-library advisories above. The final worker OS/npm dependency tree was not vulnerability-scanned in this review.
- During the initial review, no new live GitHub workflow was started, no paid implementation run was launched, and no issues/PRs/comments/labels were changed. Existing company installation state and credentials were not altered. The subsequent authorized publication created issues #208–#212 with `bug` and `ready-for-agent`; it did not add the runtime trigger label `agent-ready`, start a Factory Run, or modify existing issues.

This review does not certify malicious-container escape resistance, multi-tenant operation, company-specific provider approval, or a real end-to-end rollout that has not been executed.
