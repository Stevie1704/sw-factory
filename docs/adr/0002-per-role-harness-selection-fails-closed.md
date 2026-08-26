---
status: accepted
---

# Select a harness per role and fail closed on settings it cannot honor

Codex and Claude Code are interchangeable so the best harness can be used for
each stage. The coordinator resolves harness, model, and reasoning effort
independently for each role from the frozen repository policy, and adapters
translate that neutral selection into native commands. An adapter owns no
workflow, Git, retry, or terminal-layout decision.

The two harnesses are not feature-identical. Claude Code exposes no
reasoning-effort process argument, and macOS keeps its credential in the login
Keychain rather than in a file that a host can hand over. Two rules follow.

A setting a harness cannot represent is refused, not dropped. Silently dropping
a reasoning effort would run a role at an effort the repository policy never
authorized, and the operator would see a successful launch.

The refusal is enforced at three points, each earlier than the last is cheap.
Configuration validation refuses declaring reasoning-effort options for a role
whose harness cannot honor them. The coordinator reads the adapter's
capabilities and refuses a selection as a typed policy rejection before the
launch creates a directory, a worker, a credential copy, or a surface. The
adapter refuses it once more at launch, as a fail-closed backstop for a frozen
packet that predates the validation rule.

Capability discovery, rather than a harness name in workflow code, is what the
coordinator asks. This keeps the difference between the harnesses in the
adapter that owns it.

Credential seeding is modeled per harness, not once for all of them. Each
harness names its own optional host source, and a harness without one keeps the
credential the worker itself persisted in its role volume. Modelling seeding as
a single shared capability would make a Claude run on macOS look misconfigured
when it is merely authenticated differently.

A native session belongs to the harness that created it. A resume dispatches on
the harness recorded for the session under repair, and refuses to continue when
the resolved adapter reports a different identity. That is what makes
mid-session migration between Codex and Claude impossible.

Model options remain per role rather than per harness. A repository that
permits harness overrides therefore declares model options that every permitted
harness accepts. Scoping models by harness would be the more complete model,
but nothing in the current requirement needs it.

This accepts that a repository must declare per-role policy that matches each
harness's real capabilities, in exchange for refusals that are visible at
launch instead of silent behavioral drift inside a run.
