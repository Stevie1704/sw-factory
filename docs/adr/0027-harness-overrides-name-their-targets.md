---
status: accepted
---

# A harness override reaches a new adapter only when the repository names it

Issue #285 adds GitHub Copilot CLI as a third harness. Before it,
`allowed_overrides: [harness]` meant "any harness the factory ships", and the
factory shipped Codex and Claude Code. If that meaning stayed, every existing
repository with the permission would accept Copilot the day the factory
shipped it. Its comment command, `factory agent --harness`, and startup
diagnosis would all start to treat Copilot as selectable. An authorization
that an operator gave for two known adapters would widen to a third without a
change to the checked-in configuration. An old pinned worker image, which has
no `copilot` executable, would also start to fail diagnosis.

A harness override therefore reaches only the harnesses in the new optional
repository field `override_harnesses`. When the field is omitted, the target
set is the original Codex and Claude pair, so an existing repository keeps
its old meaning. A repository opts in to Copilot by override with one
explicit, reviewed line:

```yaml
allowed_overrides: [harness]
override_harnesses: [codex, claude, copilot]
```

The field is valid only together with `harness` in `allowed_overrides`, must
not be empty, and lists known, unique harness names. A role default that
names `copilot` needs no opt-in, because the role default is itself the
explicit selection.

The same frozen configuration decides every surface: launch admission, the
`/factory config harness=` comment, startup diagnosis, and the required skill
smoke evidence. An in-flight run keeps the policy frozen in its packet. A
packet written before the field existed has no target list and keeps the
Codex and Claude meaning.

We considered three other options. A new schema version would force every
repository to migrate to get no behavior change. A per-harness override name
such as `harness:copilot` would add a second grammar for the same list. An
implicit rule such as "Copilot is a target when some role defaults to it"
would make one role's default change what another role may override to.

The rule applies to every adapter that the factory adds later: the omitted
list stays the Codex and Claude pair.

This decision also refines ADR 0002. A `reasoning_effort` or `model` override
still removes the declared-options check, but the factory now also checks the
final harness, model, and effort together before any directory, worker, or
credential effect. Claude Code and Copilot accept closed effort value sets,
and Copilot accepts `auto` as a model that lets it choose a different model.
An override cannot send a value that the selected harness would refuse or
replace silently. Such a request is refused with the typed code
`harness_selection_unsupported`.
