---
status: accepted
---

# Operator-directed defect reports are not telemetry

ADR 0001 prohibits outbound telemetry. An agent that operates Factory on an unrelated repository can still find a defect in Factory itself, and Factory's maintainers need that evidence. Issue #242 adds `factory bug-report`. This decision records why that command does not break ADR 0001.

## Decision

1. **Preparation and publication are separate.** `factory bug-report` writes a local Markdown draft. It makes no network request and needs no GitHub authentication. `factory bug-report submit <file>` is a separate, non-interactive host command. Its invocation is the publication action, and it publishes exactly the reviewed file.
2. **Only an explicit operator action publishes.** Guide, doctor, status, draft preparation, and worker failure never submit. A worker can only propose a suspected defect in its structured report. The proposal is bound to the Invocation and has bounded fields; `factory-report` refuses a bad proposal, and acceptance ignores the proposal, so it changes no outcome, gate, stage, or recovery rule. A draft names a proposal by Invocation and code only; its worker-authored text is shown on the terminal, not collected into the draft. Workers receive no GitHub credentials and cannot run the host submission path.
3. **Collection uses an allowlist.** The draft collects fixed vocabulary values and Factory-generated identifiers only. It never copies configuration or store objects, error text, paths, repository identities, branch or gate names, prompts, transcripts, source, diffs, or logs. Factory does not claim that a scrubber makes free text safe; operator-written text stays visible in the draft for review.
4. **The destination is fixed.** Reports go to `github.com/Stevie1704/sw-factory`, independent of the current directory, of the registered provider, and of `GH_HOST`. A future override must be explicit host-owned configuration, never worker or repository input. The GitHub adapter implements a `bugreport.Upstream` port, so provider-neutral workflow packages gain no GitHub dependency (ADR 0018).
5. **Retries never create blind duplicates.** The draft carries a report identity, and the issue body carries it as a marker. Local submission state beside the draft records a pending and a created submission. A creation with an unknown outcome blocks a new creation until a search finds the issue or the operator confirms that none exists. An exclusive lock per report identity covers reconciliation and creation, so two concurrent submissions cannot both create the issue. Failure classification reads only the standard error of `gh`, never its arguments, so operator text cannot turn an unknown creation outcome into a known one.

Automatic submission under a standing policy is deferred. It needs a separate agreement on destination, disclosure, frequency, duplicate handling, and authorization.

## Considered options

- A support bundle with raw logs and a regex scrubber. Rejected: a scrubber cannot prove that arbitrary text is free of secrets or private content.
- Submission from the worker. Rejected: workers have no credentials by design, and a worker report must not publish anything.
- An interactive confirmation prompt in `submit`. Rejected: an operator agent runs it non-interactively; the separate command is the confirmation.
