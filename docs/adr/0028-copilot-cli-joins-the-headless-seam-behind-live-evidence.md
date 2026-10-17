---
status: accepted
---

# Copilot CLI joins the headless seam with a dedicated token and a live-evidence release gate

Specification #281 adds the standalone GitHub Copilot CLI as a third
`HeadlessRuntime` adapter. Spike 0003 proved its command contract offline,
but no Copilot credential was available, so no live run took place. Three
earlier decisions do not fit Copilot unchanged.

**Credential.** ADR 0002 lets a harness without a registered host source keep
the login its own role volume holds. Copilot does not get that fallback. Its
only credential is a dedicated token file (`authentication.copilot_auth_path`)
that the coordinator copies into the factory-managed credential volume. The
in-worker launcher `factory-copilot` exports it as `COPILOT_GITHUB_TOKEN` to
the Copilot process only. The role volume keeps no Copilot login, and the
worker image has no `gh` CLI. The CLI reads `GH_TOKEN`, `GITHUB_TOKEN`, a
`gh` login, and its own `config.json` as credentials, and any of these would
let the coordinator's tracker or code-host authority become the model
credential. A role with shell access can still read its own token, so the
token's narrow permission (Copilot Requests only) is the boundary.

**Identity.** Codex and Claude Code each name a documented machine event that
confirms the native session. For a Copilot `-p` run, no such event was
observed (RB-1). The adapter assigns the UUID with `--session-id`, resumes only
with `--resume=<full UUID>`, and reads identity provisionally from a
`sessionId` or `session_id` field of the current process output. It fails
closed when that evidence is absent, malformed, or ambiguous. A real launch can
therefore stop at identity discovery until RB-1 is closed. The one function
that decides this is `copilotSessionEvidence`.

**Release evidence.** For Codex and Claude Code, a recorded live skill smoke
followed the adapter. For Copilot, the adapter, configuration, and
documentation ship before any live evidence. We therefore separate the code
from the release: a repository can select Copilot only after an operator
records a live skill smoke for an image that contains the pinned CLI. Until
then, `factory doctor` blocks that repository. The smoke never records
evidence for a missing credential, a skipped harness, a failed call, or a
reply without the skill text, and no copilot record is written without a live
run. Specification #281 stays open until the release blockers RB-1 to RB-7
in `docs/acceptance/copilot.md` have live evidence.

**Telemetry.** ADR 0001 keeps local evaluation free of outbound telemetry.
Copilot inference uses GitHub's service, and the CLI has built-in product
telemetry with no opt-out except an offline mode that needs BYOK, which is out
of scope. This is RB-8. It is an open operator decision: accept that boundary
for repositories that select Copilot, or keep Copilot disabled. This record
does not decide it.

We considered two other options. Keeping Copilot code on a branch until live
evidence exists would hide the offline-proved contract from review and let it
drift from the other adapters. Recording a smoke result from an offline
`skill list` would make a listing look like activation evidence, which spike
0003 showed it is not.
