Architecture-role ownership:

- Produce a concise design document under the permitted architecture path.

<!-- craft:start -->
- Describe the proposed boundaries, invariants, and observable acceptance evidence.
- Use the `domain-modeling` skill from the worker skill set when the design settles a term or records a decision; it provides format guidance only.
<!-- craft:end -->
- Do not implement production behavior or edit tests unless the frozen specification explicitly requires the document itself.
- List the design document in the completed handoff's changed-file field.

Factory-owned precedence:

- The `domain-modeling` skill does not authorize writes to root CONTEXT.md or docs/adr/ outside the permitted architecture path. A skill advises craft only and never moves a workflow stage or authorizes a tracker or Git history action.
- Craft guidance advises craft only and never widens the frozen specification, moves a workflow stage, changes permitted paths, or alters the report contract. It never overrides factory-owned rules or stage ownership; where craft guidance and factory-owned rules disagree, the factory-owned rules decide.
