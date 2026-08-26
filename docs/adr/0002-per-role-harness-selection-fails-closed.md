---
status: accepted
---

# Select harness, model, and reasoning effort per role from declared options

Codex and Claude Code are interchangeable so the best harness can be used for
each stage. The coordinator resolves harness, model, and reasoning effort
independently for each role from the frozen repository policy, and adapters
translate that neutral selection into native commands. An adapter owns no
workflow, Git, retry, or terminal-layout decision.

The two harnesses accept the same three settings through different arguments,
and their option names do not match. Codex takes effort through
`model_reasoning_effort`; Claude Code takes `low`, `medium`, `high`, `xhigh`,
or `max` through `--effort`. Neither model names nor effort names transfer.

The repository therefore declares the valid options per role rather than the
factory hardcoding a per-harness table. A per-role declaration is what the
requirement asks for, and it keeps a new harness or a renamed level from
becoming a code change. The cost is that a repository permitting `harness`
overrides must declare options every permitted harness accepts; nothing in the
current requirement needs models and efforts scoped by harness, so they are
not.

Validation happens where it binds. Claude Code warns about an unrecognized
effort level and then silently uses its default, so an invalid value produces a
run at an effort nobody authorized and no error anywhere. The coordinator's
declared-options check is the only thing standing between a typo and that
outcome, and it refuses an undeclared selection as a typed policy rejection
before the launch creates a directory, a worker, a credential copy, or a
surface. Adding `reasoning_effort` to `allowed_overrides` deliberately removes
that protection for an operator who wants it.

Credential seeding is modeled per harness, not once for all of them. Each
harness names its own optional host source, and a harness without one keeps the
credential the worker itself persisted in its role volume. Modeling seeding as
a single shared capability would make a Claude run on macOS look misconfigured
when it is merely authenticated differently: macOS keeps the Claude Code
credential in the login Keychain rather than in a file a host can hand over.

A native session belongs to the harness that created it. A resume dispatches on
the harness recorded for the session under repair, and refuses to continue when
the resolved adapter reports a different identity. That is what makes
mid-session migration between Codex and Claude impossible.
