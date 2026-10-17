# Spike 0003 — Copilot CLI protocol and credential boundary

- **Issue**: #282 (parent spec #281)
- **Date**: 2026-10-17
- **Status**: offline evidence complete; eight release blockers (RB-1 to RB-8) stay open until live evidence exists
- **Reproduction**: `scripts/probe-copilot-cli.sh OUTPUT_DIRECTORY` (offline by default)
- **Fixtures**: `docs/spikes/copilot-cli-1.0.98/` with `manifest.json`; `make check` validates them

## Question

Can the pinned standalone GitHub Copilot CLI run as a detached, non-TTY
factory harness with a machine-readable identity, an exact resume that fails
closed, an honored model and effort, a dedicated credential without
repository authority, and suppressed executable configuration?

## Answer so far

Partly. No Copilot credential was available (operator decision: offline only).
Every probe ran in a disposable, network-less container with no token. The
offline evidence supports one design and rules out two unsafe options. It does
not prove any behavior that needs a model request. **Copilot must stay gated
until RB-1 to RB-7 have live evidence and RB-8 has an operator decision.**

Evidence labels in this record:

- **DOCUMENTED** — stated in the official docs or the pinned CLI's own help.
- **OBSERVED** — seen in this spike with the pinned binary, and kept as a fixture.
- **UNRESOLVED-LIVE** — needs a credential; the adapter must verify it at run time.

## Environment

| Component | Value |
| --- | --- |
| Package | `@github/copilot@1.0.98` (npm `latest` on 2026-10-14) |
| npm integrity | `sha512-zpXwyI8hLUM/0r9DqSRqxemRBIewLrUzTH+5sQXVH7/VKUaC4SbtbG0YHj1gpjH4YDZOvHZAl9taFQa9Kwl+Pg==` |
| Platform packages | optional dependencies `@github/copilot-linux-arm64` and `@github/copilot-linux-x64` (and others), all `1.0.98` |
| Base image | `node:22-bookworm-slim` (`node@sha256:d649c27dae7ba0137b3cef5dd75baa422c08dc3d9e3fc0c23dfb172dc3cc6436`), the worker base |
| Observed platforms | `linux/arm64` (native, all fixtures); `linux/amd64` under emulation (see below) |
| Runtime | Node `v22.23.2` runs the npm loader; the CLI logs `Node.js version: v24.20.0` for its own bundled runtime |
| Worker shape | uid 10001, `--cap-drop ALL`, `no-new-privileges`, `--network none`, `env -i`, `COPILOT_HOME` on a named volume |
| Container runtime | Colima, Docker server 29.5.2 |

The probe image is rebuilt on every run, so its local image ID is not
reproducible. The npm integrity hash and the Node base digest are the pins.

## Command templates

Fresh (coordinator generates the UUID before launch):

```text
copilot -p <PROMPT> --output-format json --allow-all-tools --no-ask-user
  --no-auto-update --no-remote --no-remote-export
  --disable-builtin-mcps --no-custom-instructions
  --secret-env-vars COPILOT_GITHUB_TOKEN
  [--model <MODEL>] [--reasoning-effort <EFFORT>]
  --session-id <COORDINATOR-UUID>
```

Resume (only the persisted full UUID):

```text
copilot -p <PROMPT> <same options> --resume=<RECORDED-UUID>
```

Environment, set by the worker only: `PATH`, `HOME`, `COPILOT_HOME`,
`COPILOT_AUTO_UPDATE=false`, and `COPILOT_GITHUB_TOKEN` read from the
protected projection inside the worker. Never set `GH_TOKEN`, `GITHUB_TOKEN`,
or `COPILOT_ALLOW_ALL`. stdin is `/dev/null`. Every option above appears in
the pinned `--help` (`TestCopilotCommandTemplatesUseObservedFlags`).

## Verdict per acceptance criterion (#282)

| # | Criterion | DOCUMENTED | OBSERVED (offline) | UNRESOLVED-LIVE |
| --- | --- | --- | --- | --- |
| 1 | Version, platform, runtime, fresh event, final/failure shapes, exit semantics | `-p` exits after completion; `--output-format json` emits JSONL, one object per line. The docs name no event type for `-p` JSONL. Only `copilot workflow run` documents a final record (`workflow.result`) and exit codes 0/1/2/130 | Version, platforms, runtime above. Every pre-model failure writes **nothing to stdout**, prose to stderr, and exits **1** (argument errors too, not 2) | **RB-1**: the JSONL event names, the identity-bearing event, the final event, and failure events of a `-p` run |
| 2 | Non-TTY fresh run and resume of the same session; missing/unknown IDs, ambiguity, corruption, recreation | `--session-id <uuid>` resumes a match **or creates a new session**. `--resume` matches ID, 7+ hex prefix, exact name, then auto-generated summary; bare `--resume` errors under `-p` only when several sessions exist. Session state is `session-state/<id>/` with `events.jsonl` | Non-TTY runs work. `--resume=<unknown uuid>` fails closed before auth and creates no directory. `--session-id` with a non-UUID fails and creates nothing. `--resume` with `--session-id` is rejected. A session without events is not resumable (`Cannot create session from empty events array`). A corrupt `workspace.yaml` fails closed. Bare `--resume` with no sessions **created a new random session**. State on a named volume survives worker recreation | **RB-2**: resume of a session with real events after process restart and worker recreation; full-UUID priority over names/summaries; ambiguous prefixes with real sessions; a corrupt `events.jsonl` |
| 3 | Explicit model and effort, unsupported values, fallback, detection | `--model`, `COPILOT_MODEL`, `settings.json` `model`, custom agent model (highest). Managed model policy has a required `fallback` model used "when the active model isn't allowed". Effort levels in docs: `low`…`max` | `--reasoning-effort` accepts `none, minimal, low, medium, high, xhigh, max`; any other value fails at argument parsing (exit 1). `--model` is not validated before auth; an unknown name reaches the auth check | **RB-3**: unknown or unentitled model, effort on a model that does not support it, managed `fallback` substitution, and the machine evidence of the effective model and effort |
| 4 | Dedicated credential, minimal permissions, storage, refresh, org prerequisites, no repository write | Token precedence `COPILOT_GITHUB_TOKEN` > `GH_TOKEN` > `GITHUB_TOKEN` > keychain/`config.json` > `gh auth token`. Supported: `gho_`, `ghu_`, fine-grained `github_pat_` owned by a personal account with the **Copilot Requests** account permission. Classic `ghp_` unsupported. CLI must be enabled by policy in at least one org that grants the seat | `GH_TOKEN` is picked up even with an unknown prefix (`Authentication token found but could not be validated`). A planted `~/.config/gh/hosts.yml` without a `gh` binary is not used. `--secret-env-vars` exists | **RB-4**: that a Copilot-Requests-only fine-grained PAT runs prompts and cannot write repositories or trackers; expiry and refresh behavior |
| 5 | Unattended permissions within enterprise policy; typed failures | `--allow-all-tools` is required for non-interactive mode. Managed `permissions.disableBypassPermissionsMode` can disable bypass | Missing auth: stdout empty, exit 1, stderr `Error: No authentication information found.` Unvalidated token: `Error: Authentication token found but could not be validated.` Argument errors: stderr `error: invalid value …`, exit 1. Session-selection errors: stderr `Error: No session, task, or name matched '<value>'.`, exit 1 | **RB-5**: shapes of expired token, rate limit/capacity, CLI-disabled policy, and unavailable model. **RB-6**: `--allow-all-tools` under a policy that disables bypass |
| 6 | Suppress built-in GitHub MCP, unapproved config, remote/cloud, publishing, self-update, repository guidance; keep curated skills | Flags `--disable-builtin-mcps`, `--no-custom-instructions`, `--no-remote`, `--no-remote-export`, `--no-auto-update`. Repository hooks, workspace MCP, and repository plugins load only for a trusted folder; `COPILOT_ALLOW_ALL=true` (exactly) trusts the working directory. npm installations only report a new version | Untrusted worktree: planted `.mcp.json`/`.github/mcp.json` not listed, planted plugin not installed, planted `sessionStart` hook did not run (the run stopped at auth). `ignoredSkillsLocations: ["/work"]` hides all project skills; the curated skill in `$COPILOT_HOME/skills/` stays listed. `instruction list` still finds `AGENTS.md`, `CLAUDE.md`, `.github/copilot-instructions.md`, `.github/instructions/*`. Two built-in skills (`customize-cloud-agent`, `github-pr-media`) are always listed. The documented `remote` setting is **ignored as an unknown key** in 1.0.98. With a token present, the CLI fetches the GitHub release list even with `COPILOT_AUTO_UPDATE=false` and `--no-auto-update` | **RB-7**: with a credential, prove no GitHub MCP tools, no repository hook/MCP/plugin, no repository instructions with `--no-custom-instructions`, no session export, no update download into `~/.cache/copilot/pkg`, and curated skill activation |
| 7 | Sanitized templates and bounded fixtures; documented vs observed vs unresolved | — | 40 sanitized probe fixtures (no timestamps, no generated UUIDs, no tokens); `make check` validates them | Live probes are listed as `not-run` in `manifest.json` |
| 8 | Document blockers; keep dependent work gated | — | — | RB-1 to RB-8 below |

### linux/amd64

Run `PROBE_PLATFORM=linux/amd64 scripts/probe-copilot-cli.sh DIR` for the
same probes under emulation. The result is recorded in
"Emulated linux/amd64 run" at the end of this record. The fixtures are the
native `linux/arm64` run.

## Findings that change the design

1. **`--session-id` is unsafe for resume.** It creates a new session when no
   session matches a valid UUID. Use it only for a fresh launch, where the
   coordinator generates the UUID. Use `--resume=<full-uuid>` for resume: it
   fails closed on an unknown ID (OBSERVED).
2. **Bare `--resume` can create a session.** With no saved sessions it started
   a new one with a random UUID. Never pass `--resume` without a value, and
   never pass `--continue`.
3. **A failed fresh launch leaves an unresumable workspace.** Missing auth
   still writes `session-state/<uuid>/workspace.yaml` (no `events.jsonl`).
   A later `--resume` of that UUID fails closed. A retry of the fresh launch
   with the same `--session-id` is accepted and reaches the auth check.
4. **Session lookup fails before auth.** The adapter can classify a missing
   or corrupt session without a credential: stderr `Error: No session, task,
   or name matched '<id>'.`, exit 1, empty stdout.
5. **Exit 1 is overloaded.** Missing auth, argument errors, and missing
   sessions all exit 1 with empty stdout. The adapter must classify by the
   stderr first line, never by exit status alone, and must keep stderr out
   of persisted diagnostics until it is redacted.
6. **Repository guidance has no observed off switch outside a prompt run.**
   `instruction list` ignores settings and has no flag. Whether
   `--no-custom-instructions` suppresses `AGENTS.md`, `CLAUDE.md`,
   `.github/copilot-instructions.md`, and `.github/instructions/` in a `-p`
   run is RB-7.
7. **Project skill discovery must be cut.** The CLI discovers skills from
   `.github/skills/`, `.claude/skills/`, and `.agents/skills/` in the
   worktree. Set `ignoredSkillsLocations: ["/work"]` in the worker's
   `$COPILOT_HOME/settings.json`, and seed curated skills into
   `$COPILOT_HOME/skills/`. Disable the two built-in skills with
   `disabledSkills` if role policy requires it (their effect is RB-7).
8. **The `remote` setting does not exist in 1.0.98.** Use the flags
   `--no-remote --no-remote-export` and the setting `remoteExport: false`.
9. **The npm loader writes to `$HOME/.cache/copilot/pkg/<platform>/1.0.98`**
   and `$HOME/.cache/Microsoft/DeveloperTools/deviceid` on first run. The
   worker home must be writable; the device ID is product telemetry state.
10. **Update checks happen with a credential.** A token makes the CLI request
    `https://api.github.com/repos/github/copilot-cli/releases` despite both
    update switches. npm installs only report updates (DOCUMENTED); the
    adapter must check that `~/.cache/copilot/pkg/` holds only `1.0.98`.
11. **`gh` fallback needs the `gh` binary.** A planted `hosts.yml` was not
    used. Keep `gh` out of the worker image, and never project the
    coordinator's `gh` configuration.

## Identity and resume mechanism

- Fresh: the coordinator generates a UUID and passes `--session-id`. The
  identity is assigned, not discovered (as for Claude Code in spike 0002).
- Machine evidence for that identity (until RB-1 names a JSONL event):
  `$COPILOT_HOME/session-state/<uuid>/workspace.yaml` has `id: <uuid>` and
  `cwd: /work` (OBSERVED), and `session-state/<uuid>/events.jsonl` is
  non-empty after the run (DOCUMENTED). Model text and the human exit
  summary (`copilot --resume=SESSION-ID` hint) are not evidence.
- Resume: before launch, require `session-state/<uuid>/events.jsonl` to
  exist and be non-empty, and `workspace.yaml` to name the same `id`; then
  launch `--resume=<uuid>`. After the run, verify the same evidence again
  and that no other session directory was created by the process.
- Never pass `-n/--name`, so name matching cannot pick another session.

### Adapter evidence source (#284, PROVISIONAL pending RB-1)

The headless adapter (`internal/harness/headless_copilot.go`) does not use
the on-disk evidence above. The shared headless lifecycle reads identity only
from the output of the current detached process, because role-home files can
belong to an older session and the adapter has no file access through the
worker seam. Until RB-1 names the real event, the one function
`copilotSessionEvidence` reads identity as follows:

- Only complete JSON lines of the current process output count. Prose, the
  `--resume=` hint, partial lines, and lines over 1 MiB do not.
- Only the value of a top-level `sessionId` or `session_id` key of a line is
  identity evidence. A nested key does not count, because tool output or
  event payloads can carry other session identities. No event type name is
  assumed.
- The evidence must be one canonical version-four UUID. A malformed value or
  two different values fail closed as an unexpected exit; absent evidence
  fails closed when the discovery window ends or the process settles.
- A resume must report the recorded UUID, else the shared lifecycle cancels
  the process and reports a native session mismatch.

If the live `-p --output-format json` run shows other field names, or no
identity field, change only `copilotSessionEvidenceFields` or
`copilotSessionEvidence`. Until then a real Copilot launch can fail closed
at identity discovery; that is the intended behavior while RB-1 is open.
The resume pre- and postflight checks of `events.jsonl` and `workspace.yaml`
are not implemented; the CLI itself fails closed on an unknown full UUID
(OBSERVED). They stay open under RB-2.

## Credential boundary (proposal, pending RB-4)

Use a fine-grained personal access token owned by a dedicated personal
account, with only the **Copilot Requests** account permission and no
repository access. Store it in factory-managed storage and project it
read-only into the worker (for example `/run/factory-auth/copilot-token`).
Export `COPILOT_GITHUB_TOKEN` only inside the worker process and pass
`--secret-env-vars COPILOT_GITHUB_TOKEN`. A role with shell access can still
read its own token. `copilot login --with-token` would instead write the
token to `$COPILOT_HOME/config.json` in plain text (no keychain in the
worker); its format is undocumented, so the environment projection is
preferred.

### Worker projection observations (#283, OBSERVED offline)

The worker uses `/run/factory-auth/copilot-token` and the launcher
`factory-copilot` (`docs/worker-runtime.md`). Offline runs of the pinned CLI
in the built worker image, with `--network none` and sentinel values only:

| `COPILOT_GITHUB_TOKEN` value | stderr first line (exit 1, empty stdout) |
| --- | --- |
| no type prefix (any text) | `Error: No authentication information found.` |
| `github_pat_…`, `gho_…`, `ghu_…` | `Error: Authentication token found but could not be validated.` |
| `ghp_…` | `Error: Classic Personal Access Tokens (ghp_) are not supported by Copilot.` |
| `ghs_…` | `Error: Failed to load models` |
| `GITHUB_PAT_…` (upper case) | `Error: No authentication information found.` |

So the CLI ignores `COPILOT_GITHUB_TOKEN` without a supported type prefix,
while `GH_TOKEN` with any text counts as a token (`fake-gh-token` fixture).
`--secret-env-vars=COPILOT_GITHUB_TOKEN` does not change the result. The
worker seeding refuses a token without a `github_pat_`, `gho_`, or `ghu_`
prefix. `copilot skill list` through the launcher lists the curated personal
skills, hides a planted `/work/.github/skills` skill, and marks the two
built-in skills `(disabled)`. A listing is not activation evidence (RB-7).

## Release blockers

Each blocker needs live evidence from `COPILOT_PROBE_LIVE=1` (or an operator
decision for RB-8). A skipped live probe is not a pass.

- **RB-1** — JSONL fresh-session identity, final, and failure event shapes
  are not observed. Record the real event types and fields of a `-p
  --output-format json` run; until then the adapter must use the on-disk
  evidence above and treat any stdout event as untrusted.
- **RB-2** — Exact resume of a real session after process restart and worker
  recreation is not observed, nor full-UUID priority over names and
  summaries, ambiguous prefixes with real sessions, or a corrupt
  `events.jsonl`.
- **RB-3** — Model and effort honoring, rejection, and fallback are not
  observed. The adapter must find machine evidence of the effective model
  and effort, and must fail when a managed `fallback` substitutes a model.
- **RB-4** — The dedicated credential's permission boundary and refresh are
  not proved: show with a non-mutating check that the token cannot write a
  disposable repository or tracker, and record expiry behavior.
- **RB-5** — Typed evidence for expired auth, rate limits or capacity,
  policy denial, and unavailable models is not observed.
- **RB-6** — Unattended `--allow-all-tools` under an enterprise policy that
  disables bypass permissions is not observed.
- **RB-7** — With a credential: built-in GitHub MCP absent, repository
  hooks/MCP/plugins and instructions suppressed, no session export, no
  update download, and curated skill activation are not observed.
- **RB-8** — Built-in product telemetry: the docs offer no opt-out except
  `COPILOT_OFFLINE=true`, which requires BYOK (out of scope). The CLI also
  writes a device ID. The operator must accept this boundary or keep Copilot
  disabled.

## Emulated linux/amd64 run

`PROBE_PLATFORM=linux/amd64` on the same arm64 host under QEMU emulation:
`uname -m` is `x86_64`, the version is `1.0.98`, and every probe has the same
exit status and first stderr line as the native fixtures, except one.
`copilot login --help` exited 139 (segmentation fault) under emulation.
Treat this as an emulation artifact until a native amd64 host confirms it;
the factory never runs `copilot login` in a worker. A native amd64 run is
part of the RB-1 live evidence.
