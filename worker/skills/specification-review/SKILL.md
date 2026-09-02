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
smells; return this axis directly through the factory report contract.
