---
name: implement
description: "Implement the frozen specification when the factory prompt assigns the implementation role."
---

Implement the work in the frozen specification packet.

Work in vertical slices. When the role prompt assigns implementation-owned
testing, use the `tdd` skill at the agreed seam. When an independent test-stage
handoff is supplied, use its accepted seam and protected tests as the contract.

Run focused typechecking or test commands throughout the work and the full
declared gate suite once at the end.

The coordinator owns checkpoints and GitHub state. The separate specification
and standards roles own review.

## Completion gate

Complete the role by running `/usr/local/bin/factory-report` with exactly one
outcome and the evidence required by the role prompt. The command must succeed
and write the structured result file for this invocation. The coordinator
advances only from that file; passing checks and terminal prose are supporting
evidence, not completion.
