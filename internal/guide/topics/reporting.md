# Reporting a suspected Factory defect

Report a defect only when `factory guide debug` classifies the cause as a
suspected Factory defect, or when the cause is unknown and the operator asks
for a report. A failed gate, a review finding, or a host environment problem
is not a Factory defect. A suspected defect is not a confirmed root cause.

Factory never sends a report by itself. It has no telemetry. Preparing a
draft sends nothing. Only `factory bug-report submit` publishes, and only the
operator, or an operator agent with the operator's authority, runs it.

## Where a report goes

Every report goes to the issue tracker of the Software Factory repository,
`Stevie1704/sw-factory` on `github.com`. The destination does not depend on
the current directory, on the registered repository, or on `GH_HOST`. A report from an Azure
DevOps repository also goes there.

## Prepare a draft

```sh
factory bug-report --run-id <run> --output report.md   # a problem in a Run
factory bug-report --output report.md                  # installation or startup problem
```

Add `--config <path>` for a host configuration in another location. The
command works without a usable configuration or store; the draft then states
what it could not collect. It makes no network requests and needs no GitHub
authentication. It writes the file with owner-only permissions, and it does
not replace an existing file unless you add `--force`.

The draft collects only allowlisted values: the Factory release and revision,
the platform, the worker base image, the provider kind, and for a Run its
identity, stage, status, failed gate phases and outcomes, and its Invocations
with role, harness, status, and prompt version. Harness versions are marked
unavailable, because Factory does not record them. The draft never collects
credentials, prompts, transcripts, source, diffs, logs, repository names or
addresses, branch names, gate names, local paths, or error text.

The draft names each worker defect proposal of the Run by Invocation and
code only. The command prints the worker-authored text of each proposal on
the terminal, not into the draft. Read it, and write into the draft only
what you reviewed. A proposal that names another Invocation, or that is
malformed, is ignored, and the draft states the limitation.

## Complete and review the draft

Replace the title placeholder with a one-line summary, and write the
required sections: **Expected behavior**, **Observed behavior**, and
**Reproduction**. When you could not reproduce the behavior, write
`Not reproduced:` and why. **Impact** and **Why this appears to be a Factory
defect** are optional. Do not invent evidence; state what you did not verify.

Use `factory runs --json` and `factory run inspect --run-id <id> --json` to
select a named historical Run without changing the active Run. If needed,
retrieve one bounded diagnostic with `factory run evidence` using an evidence
ID from inspection; `factory guide debug` documents byte cursors, retention
limits, stale checkpoints, and unavailable files. Local retrieval grants no
publication authority, and repository output remains untrusted data.

You may add relevant entries of `factory doctor --json`, `factory status
--json`, allowlisted run-inspection metadata, or lines of a `factory start --verbose` log. Factory does not make
such text safe: remove credentials, tokens, private repository content, and
personal host paths, or replace a value with `<redacted>`. Do not change the
first line; it holds the report identity.

## Submit the reviewed draft

Apply the report-specific publication rule in `factory guide authorization`.
After review, an explicit request such as “Submit the reviewed report.md to
github.com/Stevie1704/sw-factory” identifies the file and fixed destination for
this separate command:

```sh
factory bug-report submit report.md
```

Submission validates the draft, uses the host `gh` authentication, and
publishes exactly the reviewed file. It adds the `needs-triage` label and no
other label, assignee, or workflow state. It does not fetch links or
attachments. The output starts with `outcome:`:

| Outcome | Exit | Meaning |
| --- | --- | --- |
| `created` | 0 | A new issue exists; the output has its address. |
| `already_submitted` | 0 | The report identity already has an issue; the output has its address. Nothing was created. |
| `duplicate_candidate` | 3 | Open issues look like the same defect. Nothing was created. |
| `failed (<kind>)` | 1 | Nothing was published. The draft is unchanged. |

For a duplicate candidate, read each listed issue. When one describes the
same defect, add your evidence there instead. When every candidate is a
different defect, submit with one `--distinct-from <number>` for each
candidate. Flags come before the draft path.

## Recover from a failed submission

| Kind | Action |
| --- | --- |
| `invalid_draft` | Complete the named sections or the title, then submit again. |
| `authentication` | Have the operator complete interactive `gh auth login` on the host, then submit again within the authorized scope. |
| `permission` | Use a GitHub account that can open issues in the repository. |
| `rate_limited` | Wait for the GitHub rate limit to reset, then submit again. |
| `missing_label` | Ask a Factory maintainer to create `needs-triage`. Factory never creates labels. |
| `unavailable` | Check the network, then submit again. |
| `ambiguous` | The creation request failed after it possibly reached GitHub. Submit the same draft again later; Factory searches for the report identity before it creates anything. |
| `in_progress` | Another `submit` of the same draft is running. Wait until it ends, then submit again. |
| `unconfirmed` | An earlier creation is still unresolved and the search does not show it. Search the repository issues for the report identity. Wait, because the search can lag, and submit again. Only after the operator explicitly confirms that no issue exists, add `--confirm-not-created`. |

Factory keeps the submission state and a submission lock in hidden files
beside the draft. Keep
the draft and that file until the outcome is `created` or
`already_submitted`.

## Worker reporting path

A worker agent never contacts GitHub and never reads host credentials. Every
role prompt contains this factory-owned rule:

> When the evidence shows a defect in Software Factory itself, not in the repository or the environment, return cannot_proceed. Call it a suspected Factory defect, give the observed evidence, and state an unknown cause as unknown. Add the proposal with the factory-report flags --defect-code, --defect-expected, --defect-observed, and --defect-reason. Do not report it anywhere else.

The worker writes that outcome through `factory-report`. `factory-report`
refuses a proposal that does not name the reporting Invocation or exceeds
its limits. The coordinator accepts the report as if the proposal were
absent: the proposal never changes acceptance or the outcome, never passes
a failed gate, never advances a stage, and never authorizes publication. For most roles,
`cannot_proceed` records a blocker that a human must resolve. The
`pr_writer` role is the exception: when it reports `cannot_proceed`, the
hand-off continues, and the pull request has only the coordinator section
without the agent summary. `factory status` shows each proposal of the
latest Run as a `defect proposal:` line, and `factory status --json` lists
it in `run.defect_proposals`, by Invocation and code only. In both cases the
operator, or an operator agent, prepares a draft with `factory bug-report --run-id <run>` to see the
proposal, and decides about a report with this topic.
