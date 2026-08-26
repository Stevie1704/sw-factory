# Issue 26 — measured pilot findings

- **Parent**: [#1 — Spec: Build a supervised GitHub-to-PR software factory](https://github.com/Stevie1704/sw-factory/issues/1)
- **Issue**: [#26 — Measured pilot against a direct-harness baseline](https://github.com/Stevie1704/sw-factory/issues/26)
- **Version**: 1
- **Protocol frozen**: 2026-08-26
- **Status**: `revise and repeat`; the evidence gate remains open
- **Decision scope**: the minimum test-first factory path only

## Question

Does the minimum supervised factory path produce enough quality and
specification coverage to justify its extra orchestration over a direct
harness, while preserving the visibility, deterministic-gate, and isolation
properties defined by the parent specification?

This pilot is an evidence-gathering exercise, not a claim about final product
suitability. A small corpus cannot establish statistical proof. The result can only
decide whether the next product increment is justified, needs revision and a
repeat pilot, or should stop.

## Decision

`revise and repeat`.

The protocol is frozen below, but no valid matched direct/factory pair was
run for this document. The repository has no pilot runner or already-authorized
disposable issue corpus, and the current documented recovery-required boundary
refuses progression between coordinator processes until the later restart
recovery work is implemented. Starting a live run would therefore require an operator to
provision a real issue corpus and drive the interactive cmux workflow. Recording
measurements that were not collected would invalidate the evidence gate.

This decision deliberately leaves the pilot open. It is not evidence that the
factory is better or worse than the direct harness. The next run must use the
frozen protocol and fill the execution record before the pilot can produce a
`proceed` or `stop` decision.

## Preregistration

The corpus, rubric, thresholds, and handling rules in this section were fixed
before either path was run. Changing any of them requires a new document
version and a new pilot round.

### Corpus

The corpus contains four product-shaped specification packets taken from
completed issue specifications. They cover claim/lifecycle integration, a
bounded repair loop, test-stage ownership, and immutable specification review.
The source issue body is the frozen specification packet; its SHA-256 value and
the base revision are recorded so a future run cannot silently change either
input.

Hash convention: the digest is SHA-256 over the UTF-8 bytes emitted by
`gh api repos/Stevie1704/sw-factory/issues/<number> --jq .body`, including the
single terminal newline emitted by that command.

| ID | Work shape | Frozen specification source | Packet body SHA-256 | Base revision |
| --- | --- | --- | --- | --- |
| `P01` | Claim/lifecycle integration | [issue #4](https://github.com/Stevie1704/sw-factory/issues/4) — claim one issue with labels, branch, worktree, and run identity | `51cb993bb75221d50b5e51ac876a16e0a970c4c1a0269a702df14df676d7e51c` | `26a820aa3d6d5ef8428f8df55786d4ec426610db` |
| `P02` | Bounded stateful repair | [issue #10](https://github.com/Stevie1704/sw-factory/issues/10) — check repair with native session resume | `3d9c149edc18890899af8b437dee8c330914ef80ee0c3d88666551147d356068` | `cb6af2992fad93be97b5b74014f9b674da8b1b3e` |
| `P03` | Test-stage ownership | [issue #12](https://github.com/Stevie1704/sw-factory/issues/12) — verified red tests and protected paths | `6788fd8da1967930cdc98b9d82c14963204f78079d8833deac396f141d33f920` | `e02e78a9715f647ce3e56739db0154aec6c9d3cb` |
| `P04` | Immutable specification review | [issue #15](https://github.com/Stevie1704/sw-factory/issues/15) — one fresh reviewer against an exact checkpoint | `765a3db6cad54bd3480693e62d3fd91d1bb32b36beab3bfa2d6229068d11101b` | `a21d42ae7c231bb8038d13653e0ea777edc3f9b5` |

The four issue bodies include their own acceptance criteria, non-goals, and
expected verification. They are copied byte-for-byte to both arms. The
historical source issues are closed and are not themselves run targets; the
next round must provision disposable run contexts from these frozen
specification packets. Replacing a packet requires a new protocol version.

### Controlled variables

The path is the only intentional independent variable. Each pair uses the same
values for every item below:

| Variable | Frozen value or rule | Repository evidence |
| --- | --- | --- |
| Harness | Codex inside the pinned worker image | `worker/base.Dockerfile` pins Codex `0.148.0`; `factory.yaml` selects Codex for test, implementation, and specification review. |
| Model | `gpt-5.6-luna` | The role model policy in `factory.yaml`. |
| Reasoning | `high`, passed to both paths | The repository allows `reasoning_effort` as a validated override and the Codex adapter passes it as `model_reasoning_effort=high`. |
| Specification packet | The same immutable packet for both paths | The parent specification requires a frozen specification packet; the factory invocation packet carries its version and role identity. |
| Base revision | One recorded target-branch SHA per matched pair | The factory treats the checkpoint SHA as the immutable subject of gates and review. |
| Worker | The same worker image digest, stable paths, clean environment policy, and resource limits | `factory.yaml`, `worker/base.Dockerfile`, and `docs/configuration.md`. |
| Repository state | Fresh disposable worktree from the recorded base, with no prior output | Required before either path starts. |
| Human access | The same operator and assessment rubric; no path label during scoring | Assessment rules below. |

The direct path is one Codex implementation session given only the frozen
specification packet and repository guidance. It may run the same repository
commands a developer normally runs, but it does not receive the factory's
test-stage handoff or specification-review result.

The factory path is the current minimum path:

1. baseline setup and deterministic gates;
2. default-on test stage with verified red evidence;
3. implementation in the same worker with protected test paths;
4. all configured deterministic gates, with up to three bounded check-repair
   attempts;
5. one fresh specification reviewer against the exact implementation
   checkpoint.

The pilot does not enable the automated test-objection cycle, concurrent
standards review, or full review-repair loop. None of the four selected packets
uses a test exemption. A disputed or unverifiable test pauses for human
disposition. It never enters an automated objection cycle.

### Order and blinding

For each corpus entry, the operator selects the first path using a fixed, recorded
random seed, then runs the other path from an independently reset worktree.
Corpus order is also randomized once after preregistration. The assessment
artifacts are renamed to opaque `A` and `B` identifiers before scoring. Branch
names, factory status comments, invocation identifiers, and path-specific
summaries are removed from the scoring packet. The unblinded mapping and raw
operational records are retained separately for the operator, not included in
the blind assessment packet.

If an evaluator can identify a path from an unavoidable artifact, the evaluator
records that unblinding and continues; the affected score is marked
unblinded. No score is silently treated as blind.

## Assessment rubric

The evaluator scores each output independently before seeing the path mapping.
The rubric is fixed for the round.

### Correctness and specification coverage

Each corpus entry receives a 0–4 score in both dimensions:

| Score | Correctness | Specification coverage |
| --- | --- | --- |
| 0 | Does not build, or makes no usable progress | Does not address the requested behavior |
| 1 | Major behavior is incorrect or unsafe | Most acceptance criteria are missing |
| 2 | Partial behavior works but a required case is wrong | Core intent is present but one or more required criteria are missing |
| 3 | Required behavior works and no known regression is found | All required criteria are addressed with credible evidence |
| 4 | Score 3 plus clear handling of relevant edge cases and maintainable integration | Score 3 plus complete, independently convincing verification and no material limitation |

The evaluator also records binary flags for:

- build or test failure caused by the produced change;
- a missed acceptance criterion;
- a security, credential-isolation, or data-boundary violation;
- an unnecessary scope expansion; and
- a finding that required human rework before acceptance.

### Operational measures

The operator records these values for every path, even when zero or
unavailable:

| Measure | Definition |
| --- | --- |
| Correctness | Corpus-entry score and pass/fail against the frozen acceptance criteria. |
| Specification coverage | Corpus-entry score and list of missed criteria. |
| Human rework | Minutes and concrete edits or decisions required after the path reported completion. |
| Escalations | Test disputes, review findings, runtime pauses, and budget escalations, each with a human disposition where applicable. |
| Elapsed time | Wall time from path start to accepted output, plus factory stage durations. |
| Invocation count | Direct sessions, or factory test/implementation/review and resumed repair invocations. |
| Gate and review rounds | Baseline/checkpoint gate-suite executions, check repairs, and specification-review rounds. |
| Usage and cost | Reliable harness-reported tokens and cost only; missing values remain explicitly unavailable. |

Factory operational records come from the content-free local evaluation-summary
projection in the operational store. The summary is never supplemented with issue text, prompts,
transcripts, diffs, source contents, command output, logs, or credentials. The
direct path uses the same content-free measurement schema where a reliable
harness report exists; otherwise the findings table says `unavailable`.

## Decision thresholds

The thresholds are decision rules for this small pilot, not confidence
intervals or statistical claims.

### `proceed`

Recommend `proceed` only when all conditions hold:

- neither path has a security, credential-isolation, or prohibited-data
  violation;
- every corpus entry has a correctness score of at least 3 and no missed mandatory
  acceptance criterion in either path;
- the factory correctness and specification-coverage medians are no more than
  one rubric point below the direct path medians;
- factory human rework is no more than 30 minutes per corpus entry above the
  direct path, and no more than one additional corpus entry requires human
  rework;
- every factory gate and specification-review result is tied to the exact
  checkpoint, and every pause or exemption has a recorded disposition; and
- at least three of four matched pairs have complete elapsed-time and invocation
  measurements. Missing usage or cost alone does not fail the pilot, but it is
  reported as unavailable.

### `revise and repeat`

Recommend `revise and repeat` when no stop condition is present but a proceed
condition is not met because the sample is incomplete, a controlled variable
was not held constant, blinding was lost, operational data is incomplete, or
quality and human-rework results are inconclusive. The evidence gate remains
open, and the next round must name the revision before execution.

### `stop`

Recommend `stop` for any credential or prohibited-data exposure, an
unbounded/automated loop, a failure to preserve the exact-checkpoint or
human-supervision rules, or a factory result that is materially worse than the
direct path (more than one rubric point lower on either median quality measure
or two or more additional failed corpus entries). A `stop` decision requires the
operator to apply the canonical `wontfix` label and close #13 and #16 before
closing the pilot, as required by issue 26.

## Execution record

This version records preregistration and readiness findings only.

| Field | Result |
| --- | --- |
| Protocol version | `1` |
| Protocol freeze | `2026-08-26` |
| Matched pairs completed | `0/4` |
| Direct path outputs assessed | `0/4` |
| Factory path outputs assessed | `0/4` |
| Blind assessments completed | `0/4` |
| Human dispositions recorded | `0` |
| Decision | `revise and repeat` |
| Evidence-gate state | open |

The corpus-entry worksheet is intentionally retained even while the execution
set is empty:

| Corpus entry | Pair status | Direct result | Factory result | Blind score | Notes |
| --- | --- | --- | --- | --- | --- |
| `P01` | not run | — | — | — | — |
| `P02` | not run | — | — | — | — |
| `P03` | not run | — | — | — | — |
| `P04` | not run | — | — | — | — |

The completed ledger must contain one row for each arm of every matched pair. It is
content-free apart from opaque packet and checkpoint identities:

| Pair | Arm | Packet hash | Base SHA | Correctness | Coverage | Rework minutes | Escalations/dispositions | Exemptions | Wall time | Invocations | Gate rounds | Review rounds | Usage/cost | Blind |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `P01` | direct | as above | as above | — | — | — | — | not run | — | — | — | — | unavailable | — |
| `P01` | factory | as above | as above | — | — | — | — | not run | — | — | — | — | unavailable | — |
| `P02` | direct | as above | as above | — | — | — | — | not run | — | — | — | — | unavailable | — |
| `P02` | factory | as above | as above | — | — | — | — | not run | — | — | — | — | unavailable | — |
| `P03` | direct | as above | as above | — | — | — | — | not run | — | — | — | — | unavailable | — |
| `P03` | factory | as above | as above | — | — | — | — | not run | — | — | — | — | unavailable | — |
| `P04` | direct | as above | as above | — | — | — | — | not run | — | — | — | — | unavailable | — |
| `P04` | factory | as above | as above | — | — | — | — | not run | — | — | — | — | unavailable | — |

No corpus-entry quality, elapsed-time, invocation, gate, review, escalation,
exemption, or usage value is inferred from the zero completed pairs. The local
evaluation-summary implementation and its tests prove that the factory can
retain the required content-free operational measures; they are not pilot
observations.

## Findings and limitations

1. The repository contains the minimum workflow pieces needed by the planned
   factory path. The readiness evidence is:

   | Factory-arm requirement | Checked-in evidence |
   | --- | --- |
   | Default-on test stage and verified red evidence | `internal/factory/test_stage.go` and `docs/agent-runtime.md` |
   | Deterministic gates and exact-checkpoint results | `internal/factory/gate.go` and `internal/gate/runner.go` |
   | Bounded check repair | `internal/factory/repair.go` and `factory.yaml` (`check_repair: 3`) |
   | One immutable specification reviewer | `internal/factory/review.go` and `factory.yaml` (`spec_review: codex`) |
   | Content-free local evaluation summaries | `internal/store/evaluation.go` and `factory evaluation` |

   These are implementation facts, not comparative quality results.
2. The current product does not provide a dedicated direct-vs-factory pilot
   driver or a safe disposable issue corpus. The comparison cannot be
   reproduced from this document alone without an operator provisioning those
   inputs.
3. The documented pre-recovery process boundary refuses later progression after
   a coordinator restart. A live execution that crosses process boundaries must
   wait for the restart-recovery work or explicitly
   record the resulting limitation.
4. Four matched pairs are a small convenience sample. Even a successful result
   cannot establish general superiority, a final product suitability guarantee, or
   statistically reliable retry ceilings.
5. Human scoring and rework are judgment-sensitive. Blinding reduces, but
   cannot eliminate, evaluator effects.
6. Cost is comparable only when both paths expose reliable measurements in the
   same currency. No estimate or currency conversion is allowed.

## Ceiling recommendation

`Await more data` for both ceilings. The current three check-repair and two
review-revision values remain provisional safety defaults for the next round,
not evidence-based optima. Issue 26 provides no observations that justify
changing either value. The current minimum path intentionally does not run an
automated review-revision loop, so that ceiling must be measured after the
later capability is enabled; it must not be inferred from this round's zero
count.

## Reproduction checklist for the next round

Before execution, the operator must:

1. record the exact specification-packet hashes, base SHAs, harness/model/reasoning
   values, worker image digest, and a fresh disposable worktree for each pair;
2. run the direct and factory paths in the fixed randomized order using the
   same worker image and clean environment;
3. collect only bounded summaries, gate/review identities, timing, counts, and
   reliable usage metadata in the operational store's local evaluation-summary
   projection;
4. stop for human disposition on disputed or unverifiable tests, without
   revising them automatically;
5. blind and score the two artifacts per corpus entry, recording unavoidable
   unblinding;
6. publish the completed execution table and threshold calculation as version
   2 of this document; and
7. post the decision and this document link to issue #26. If the result is
   `proceed`, close the pilot. If it is `stop`, apply `wontfix` and close #13
   and #16 first. If it is `revise and repeat`, leave the evidence gate open.

## Sources

- [Issue #26](https://github.com/Stevie1704/sw-factory/issues/26) — corpus,
  comparison controls, metrics, decision vocabulary, and closure rules.
- [Issue #4](https://github.com/Stevie1704/sw-factory/issues/4), [issue #10](https://github.com/Stevie1704/sw-factory/issues/10), [issue #12](https://github.com/Stevie1704/sw-factory/issues/12), and [issue #15](https://github.com/Stevie1704/sw-factory/issues/15) — the four frozen specification packets named in the corpus manifest.
- [`CONTEXT.md`](../../CONTEXT.md) — domain definitions for `Pilot`,
  `Checkpoint`, `Specification packet`, `Gate`, `Worker`, and `Local evaluation
  summary`.
- [ADR 0001](../adr/0001-local-evaluation-without-outbound-telemetry.md) —
  privacy boundary and reason for collecting local content-free evidence.
- [`factory.yaml`](../../factory.yaml) — pinned worker digest, role/model
  policy, gate suite, test policy, and retry ceilings.
- [`worker/base.Dockerfile`](../../worker/base.Dockerfile) — worker harness
  versions and clean worker environment.
- [`docs/configuration.md`](../configuration.md) — worker isolation,
  checkpoint, gate, recovery, and evaluation-summary behavior.
- [`docs/agent-runtime.md`](../agent-runtime.md) — test-stage, check-repair,
  and specification-review execution boundaries.
- [`internal/store/evaluation.go`](../../internal/store/evaluation.go) — local
  summary fields, unavailable-usage semantics, and aggregate measures.
- [`internal/cli/cli.go`](../../internal/cli/cli.go) — the current user-facing
  command surface; it has no pilot runner.
