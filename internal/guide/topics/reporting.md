# Reporting a suspected Factory defect

Report a defect only when `factory guide debug` classifies the cause as a
suspected Factory defect, or when the cause is unknown and the operator asks
for a report. A failed gate, a review finding, or a host environment problem
is not a Factory defect.

Factory never sends a report by itself. It has no telemetry. Every report is
a deliberate action of the operator.

## What a report contains

- **Release identity**: the output of `factory version --json`.
- **Expected behavior**: what this guide or the command help says should
  happen. Name the topic or the command.
- **Observed behavior**: what happened, with the exact command, its exit
  status, and the error line.
- **Reproduction**: the shortest sequence of commands that shows the
  behavior, or a statement that you could not reproduce it.
- **Local evidence**: the relevant `factory doctor` and `factory status`
  output, and the relevant lines of a `factory start --verbose` log. Keep only
  the lines that support the observation.
- **Classification**: suspected Factory defect, or unknown cause. State what
  you did not verify.

Remove credentials, tokens, private repository content, and personal host
paths before you share a report. Replace a value with a placeholder such as
`<redacted>`.

## Where a report goes

The upstream destination is the issue tracker of the Software Factory
repository, `Stevie1704/sw-factory` on GitHub.

## Host reporting path

An operator agent writes the report as a local Markdown draft first, and
shows it to the operator. The operator decides whether to submit it. After
the operator approves, submit it with the GitHub CLI, for example:

```sh
gh issue create --repo Stevie1704/sw-factory --title "<summary>" --body-file <draft.md>
```

Do not add workflow labels. Factory maintainers triage new reports. When
there is no network or no GitHub authentication, keep the local draft and
tell the operator.

## Worker reporting path

A worker agent never contacts GitHub and never reads host credentials. When
it cannot continue because of a suspected Factory defect, it returns
`cannot_proceed` with the observed evidence through `factory-report`. The
coordinator validates that outcome and records it as a blocker that a human
must resolve. The operator,
or an operator agent, then decides about a report with this topic.
