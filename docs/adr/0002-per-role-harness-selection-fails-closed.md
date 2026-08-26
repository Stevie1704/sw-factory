---
status: accepted
---

# Select a harness per role and fail closed on settings it cannot honour

Codex and Claude Code are interchangeable so the best harness can be used for
each stage. The coordinator resolves harness, model, and reasoning effort
independently for each role from the frozen repository policy, and adapters
translate that neutral selection into native commands. An adapter owns no
workflow, Git, retry, or terminal-layout decision.

The two harnesses are not feature-identical. Claude Code exposes no
reasoning-effort process argument, and macOS keeps its credential in the login
Keychain rather than in a file that a host can hand over. Two rules follow.

An adapter refuses a validated repository setting it cannot represent, rather
than dropping it. Silently dropping a reasoning effort would run a role at an
effort the repository policy never authorized, and the operator would see a
successful launch. A role that runs on Claude Code therefore declares no
reasoning-effort options.

Credential seeding is modelled per harness, not once for all of them. Each
harness names its own optional host source, and a harness without one keeps the
credential the worker itself persisted in its role volume. Modelling seeding as
a single shared capability would make a Claude run on macOS look misconfigured
when it is merely authenticated differently.

A native session belongs to the harness that created it. A resume runs in that
harness or not at all, which is what makes mid-session migration between Codex
and Claude impossible. Capability discovery, rather than a hardcoded harness
name in workflow code, is what the resume path checks.

This accepts that a repository must declare per-role policy that matches each
harness's real capabilities, in exchange for refusals that are visible at
launch instead of silent behavioural drift inside a run.
