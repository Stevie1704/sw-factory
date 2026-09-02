---
name: standards-review
description: "Review one checkpoint when the factory prompt assigns the standards-review role."
---

Review only the standards axis of the exact checkpoint supplied by the factory.
Repository-documented standards override this fallback baseline. Skip anything
already enforced by a declared gate. A baseline smell is always an advisory
judgement call, not a hard violation.

Match the diff against these Fowler code smells:

- **Mysterious Name**: a name does not reveal what it does or holds. Rename it;
  if no honest name emerges, revisit the design.
- **Duplicated Code**: the same logic shape occurs more than once. Extract the
  shared shape.
- **Feature Envy**: behavior reaches into another object's data more than its
  own. Move it toward the data it uses.
- **Data Clumps**: the same fields or parameters repeatedly travel together.
  Introduce one type for the concept.
- **Primitive Obsession**: a primitive stands in for a domain concept. Give the
  concept its own type.
- **Repeated Switches**: branches on the same type recur. Centralize the choice
  or replace it with polymorphism.
- **Shotgun Surgery**: one logical change requires scattered edits. Gather the
  behavior into one module.
- **Divergent Change**: one module changes for unrelated reasons. Split those
  responsibilities.
- **Speculative Generality**: an abstraction, parameter, or hook serves no
  current requirement. Remove it until a real need exists.
- **Message Chains**: a caller navigates a long object chain. Hide the walk
  behind one method on the first object.
- **Middle Man**: a type or function mostly delegates. Call the real target.
- **Refused Bequest**: an implementation ignores most of what it inherits. Use
  composition instead.

Cite the applicable documented rule or name the baseline smell for every
finding. The specification role owns product-intent coverage; return this axis
directly through the factory report contract.
