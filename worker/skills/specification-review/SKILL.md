---
name: specification-review
description: "Review one checkpoint when the factory prompt assigns the specification-review role."
---

Review only the specification axis of the exact checkpoint supplied by the
factory. Treat the frozen specification packet as the source of product intent.

Inspect the diff and observable behavior for:

- requirements that are missing or only partially implemented;
- behavior that was not requested;
- requirements that appear present but are implemented incorrectly.

Tie every finding to the applicable frozen requirement and the concrete
checkpoint evidence. The standards role owns repository conventions and code
smells.

## Completion gate

Complete the role by running `/usr/local/bin/factory-report` with exactly one
outcome and the evidence required by the role prompt. The command must succeed
and write the structured result file for this invocation. The coordinator
advances only from that file; review conclusions and terminal prose are
supporting evidence, not completion.
