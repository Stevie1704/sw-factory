# Spike 0002 — Nested harness TTYs through cmux and Docker

- **Issue**: #2 (parent spec #1, user story 91)
- **Date**: 2026-08-20
- **Status**: in progress — both harnesses proven through cmux; three keyboard checks pending
- **Reproduction**: `spike/issue-2/` (throwaway; not merged into product packages)

## Question

Can Codex and Claude Code be identified, stopped, recreated, and resumed
robustly through two layers of terminal indirection — a Docker pseudo-terminal
inside a cmux surface?

## Answer so far

Yes, for both harnesses. Each survived destruction of its container, was
resumed from the same pinned image, and recalled a token given before the
container was destroyed — Codex `SPIKE-TOKEN-7731`, Claude Code
`SPIKE-TOKEN-4402`. The nested pseudo-terminal carries window size, `SIGWINCH`,
ANSI colour, and `SIGINT` correctly.

Four findings change the architecture and are described below: Codex's own
sandbox cannot run inside a hardened worker, Claude Code cannot be credential
seeded from a macOS host the way Codex can, Claude Code keeps state *outside*
the directory an obvious mount would persist, and the cmux control socket
refuses callers that cmux did not start.

## Environment

| Component | Version |
| --- | --- |
| Container runtime | Colima 4 CPU / 8 GiB, Docker server 29.5.2, aarch64 |
| Worker image | `sw-factory-spike:issue-2`, `node:22-bookworm-slim` base |
| Image pin | `sha256:2e3e55fd1b9e292753c7634313a580e328357867f39a2bceda3bfd1187331b50` |
| Claude Code | 2.1.232 |
| Codex | 0.148.0 |
| cmux | 0.64.22 (102) |

The worker runs as uid 10001, `--cap-drop ALL`, `--security-opt
no-new-privileges`, no Docker socket. `TERM=xterm-256color` and
`COLORTERM=truecolor` are set explicitly in the image.

## Worker shape that made this work

PID 1 is `sleep infinity`. Every harness runs through `docker exec -it`. This
is the single most important structural decision: because no harness is PID 1,
Ctrl+C reaches the harness and can never take the container down with it, and a
harness can exit and relaunch without disturbing the container.

The **whole home directory** is a named volume (`swf-spike-home`), with
per-harness volumes nested inside it (`swf-spike-claude-home`,
`swf-spike-codex-home`). Mounting only `~/.claude` and `~/.codex` is not enough —
see failure mode 3. The worktree and the invocation result directory are bind
mounts. Destroying the container therefore destroys no session state.

## Verdict per acceptance criterion

| # | Criterion | Verdict | Evidence |
| --- | --- | --- | --- |
| 1 | Interactive terminal app in a container pseudo-terminal via cmux, explicit terminal type | **Proven for both** | Both TUIs read out of their cmux surfaces: Claude Code v2.1.232 welcome panel at `/work`, Codex v0.148.0 panel at `/work` |
| 2 | Colours and layout render correctly | **Proven for both** | Claude surface: 408 box-drawing characters, palette `#D77757/#888888/#999999`. Codex surface: 108 box-drawing characters, 10-colour palette. No corruption in either grid |
| 3 | Human keystrokes reach the harness | Proven through Docker; cmux layer pending operator | Operator typed into both TUIs in the container and both answered |
| 4 | Resize without corruption | Proven | `tty-probe`: 24×80 → 40×132 observed inside the container, `SIGWINCH` raised |
| 5 | Approval prompt raised and answered | Pending keyboard | Claude Code raised its folder-trust approval and accepted an answer, but the criterion wants a human answering a tool approval |
| 6 | Ctrl+C interrupts the harness, container survives | Proven | `SIGINT` reaches the foreground group, the process dies, container stays `Running` |
| 7 | Stage completes by writing schema-versioned JSON, no screen parsing | Proven for Codex; Claude pending the interactive approval check | Codex ran `factory-report`; `.run/results/inv-codex-1.json` carries `schema_version: 1` |
| 8 | Container destroyed and recreated from the same pinned image, session resumes with context | **Proven for both, headless and interactive** | Codex returned `SPIKE-TOKEN-7731`, Claude Code returned `SPIKE-TOKEN-4402`; after a later recreate the Claude TUI came up with no theme, login, or trust prompt |
| 9 | cmux restarted, surfaces recoverable | Pending keyboard | — |
| 10 | Native session identifier observed and documented | **Proven for both** | See below |
| 11 | Findings written up, spike code not merged into product packages | This document; code confined to `spike/issue-2/` | — |

## Native session identifiers

### Codex

Discovered two ways, and the coordinator should prefer the first:

1. `codex exec` prints `session id: <uuid>` on stdout in its header block.
2. On disk at `~/.codex/sessions/YYYY/MM/DD/rollout-<ISO8601>-<uuid>.jsonl`.

Resume is `codex resume <uuid>` interactively, or `codex exec resume <uuid>`
non-interactively.

### Claude Code

Claude Code lets the caller **assign** the identifier with `--session-id <uuid>`
instead of discovering it afterwards. The coordinator should generate the run's
session id and pass it in; that removes an entire class of discovery race.
Verified: a coordinator-generated uuid was accepted and became the session.

On disk the session is at
`~/.claude/projects/<cwd-with-slashes-replaced-by-dashes>/<uuid>.jsonl`. For a
worktree mounted at `/work` that is `~/.claude/projects/-work/<uuid>.jsonl`, so
the path depends on the container mount point, not the host path — an argument
for keeping the worktree mount point stable across runs.

Resume is `-r/--resume <session_id>`, and `--fork-session` produces a new id
when resuming — which is the natural fit for the spec's "fresh reviewer" role.

## Failure modes and workarounds

### 1. Codex's sandbox cannot start inside a hardened container

`codex exec -s workspace-write` fails on every shell command:

```
bwrap: No permissions to create a new namespace, likely because the kernel
does not allow non-privileged user namespaces.
```

The cause is **not** capabilities. Dropping or keeping capabilities makes no
difference; Docker's **default seccomp profile** blocks `unshare(CLONE_NEWUSER)`.
Verified three ways: `unshare --user` fails with `--cap-drop ALL`, fails without
it, and succeeds only under `--security-opt seccomp=unconfined`.

**Workaround**: the container is the security boundary, so Codex's inner sandbox
is redundant. Run Codex with `-s danger-full-access`. This is what OpenAI
documents for externally sandboxed environments.

**Rejected alternative**: `--security-opt seccomp=unconfined`. It weakens the
real boundary to re-enable a redundant inner one. Do not do this.

### 2. Claude Code cannot be credential seeded from a macOS host

Codex keeps its OAuth tokens in `~/.codex/auth.json`, so the spec's "seed a
narrowly scoped copy" works: one file, streamed into the container, and
`codex login status` reports `Logged in using ChatGPT`.

Claude Code on macOS keeps its credential in the **login Keychain**; there is no
`~/.claude/.credentials.json` to copy. The Linux build inside the container does
use a plain file: after an in-container login, `~/.claude/.credentials.json`
exists as a 0600 file owned by the container user, and it survives container
recreation in the harness home volume. Seeding therefore works in one direction
only — a container can keep its own credential, but a macOS host cannot hand one
over as a file.

**Workaround**: either log in once inside the container, which persists in the
harness home volume, or use `claude setup-token` to mint a long-lived token and
inject it as `CLAUDE_CODE_OAUTH_TOKEN`. Ticket #73 must not assume the two
harnesses seed the same way.

### 3. Claude Code keeps state outside `~/.claude`

The obvious mount — a volume at `~/.claude` — silently loses state. Claude Code
writes `~/.claude.json` **beside** that directory, not inside it, so it lands in
the container's writable layer and dies with the container. It holds project
trust, MCP configuration, and onboarding state.

The first recreate produced:

```
Claude configuration file not found at: /home/factory/.claude.json
A backup file exists at: /home/factory/.claude/backups/.claude.json.backup.<ts>
```

Resume still worked, because Claude Code keeps its own backups *inside*
`~/.claude`, which was persisted. That self-healing is what makes this dangerous:
the symptom is a warning, not a failure, so it would have shipped unnoticed and
left every recreated worker re-prompting for directory trust.

**Workaround**: persist the entire home directory as a volume and nest the
per-harness volumes inside it. `CLAUDE_CONFIG_DIR` was not found in the
installed build, so relocating the file is not an option here.

Codex does not have this problem — everything it needs is under `~/.codex`.

**Do not hand-restore `~/.claude.json`.** Recovering the file by copying a saved
copy back in looks like it works and is not enough — see failure mode 4. Mount
the whole home *before* the first login instead.

### 4. Headless authentication and interactive readiness are not the same state

`claude -p` and the interactive TUI gate on different files, and only the first
one is covered by the credential.

After the home directory was persisted and `~/.claude.json` was restored by
hand, `claude -p 'say ok'` answered normally — the credential in
`~/.claude/.credentials.json` was valid and had survived recreation. The
interactive TUI in the same container nevertheless replayed onboarding: theme
picker, then `Select login method`.

The cause is that the restored `~/.claude.json` carried `userID` and
`oauthAccount` but **not `hasCompletedOnboarding`**. The interactive path gates
on onboarding state; the headless path does not.

Two consequences for the product:

- A capability check that runs `claude -p` **does not prove** an interactive
  session will come up authenticated. Ticket #69 must probe the interactive path
  or assert on `hasCompletedOnboarding` directly.
- Reconstructing harness state by copying config files is unreliable. Persist
  the whole role home from the first login and never rebuild it by hand.

This was found the hard way: the automated evidence for failure mode 3 was
gathered through `claude -p` only, and reported the fix as verified. The
operator then had to log in again in the cmux surface.

### 5. `docker cp` breaks non-root workers

`docker cp` preserves the host uid. A credential copied from the macOS host
lands as uid 501, mode 0600, and the container user (uid 10001) cannot read it —
`codex login status` fails with `Permission denied (os error 13)`. Worse, the
container user cannot overwrite it either.

**Workaround**: stream the file through the container user instead of copying:

```bash
docker exec -i <container> bash -lc 'umask 077; cat > ~/.codex/auth.json' < src
```

Removing a bad copy works because the parent directory is owned by the container
user, so `rm` succeeds where `write` does not.

### 6. Bind mounts carry host ownership

`/work` and `/results` are bind mounts and appear inside the container with the
host uid, so uid 10001 cannot write to them. The spike chmods them `0777`. The
product should either run the worker with a uid matching the host user or use
named volumes for the result directory.

### 7. Codex global flags must precede the `resume` subcommand

`codex exec resume <uuid> -s danger-full-access` fails with `unexpected argument
'-s' found`. `codex exec -s danger-full-access resume <uuid>` works. The Codex
adapter must build argv in that order. Likewise `codex exec --full-auto` does
not exist; `--full-auto` is an interactive-only flag.

### 8. The cmux control socket rejects outside callers

```
Error: ERROR: Access denied - only processes started inside cmux can connect
```

cmux only accepts control connections from processes it started, unless a socket
password is supplied via `--password`, `CMUX_SOCKET_PASSWORD`, or Settings.

This validates the spec's decision that the coordinator runs inside a dedicated
cmux control workspace — but it makes that decision **load bearing**, not
cosmetic. A coordinator launched from an ordinary shell or a `launchd` job
cannot drive cmux at all. `TerminalRuntime` needs an explicit story for
obtaining socket access.

### 9. cmux workspace layout is the right surfacing primitive

`cmux new-workspace --layout <json>` creates a workspace whose panes each run
their own command, which maps directly onto the spec's run surfaces (status,
test agent, implementation agent, checks, spec review, standards review). See
`spike/issue-2/cmux-surfaces`. Both harness TUIs came up correctly this way.

Two ref-resolution traps, both of which produced wrong answers before they were
noticed:

- Short refs such as `pane:6` resolve **relative to the caller's workspace**.
  `cmux focus-pane pane:6` fails with `Pane not found` unless
  `--workspace <id>` names the owning workspace.
- Short refs are positional and **shift as workspaces come and go**. Resolve
  handles to UUIDs once and store those, exactly as `TerminalRuntime` intends.

### 10. `terminal.replay` ignores the surface it is asked for

`cmux rpc terminal.replay '{"surface": "<id>"}'` returns the **active** surface's
render grid regardless of the argument. A deliberately invalid id returns the
caller's own surface rather than an error, and the response's `surface_id` field
then disagrees with the request.

Reading a specific surface requires focusing it first, and focus follows the
caller, so a coordinator cannot passively read a surface it is not sitting in.

This is only a problem for anything that wants to *read* terminals — which the
factory must never do for control flow. It reinforces the spec's rule: stage
results travel through `factory-report`, never through the screen. Always
compare the returned `surface_id` against the requested one before trusting
anything read this way.

### 11. A pinned worker must disable harness auto-update

The Claude Code TUI reported `Auto-update failed: no write permission to npm
prefix`. The failure is benign — the non-root user cannot write the global npm
prefix, which is what keeps the pinned version pinned — but the attempt is noise
and would succeed if the image were ever built with a writable prefix. Set
`DISABLE_AUTOUPDATER=1` for Claude Code so a run cannot change harness version
mid-flight, as the spec requires.

### 12. Terminal echo defeats naive output matching

Any automated driver that types a command into a pty and waits for a marker will
match the terminal's **echo** of the command before the command has produced
output. The `WorkerRuntime` contract tests must not scrape terminal output for
control flow — which is exactly why the spec routes stage results through
`factory-report` and forbids parsing screen content. This spike confirms that
rule empirically.

## Recommendations for downstream tickets

- **#14 / #73 (harness adapters, auth)**: model credential seeding per harness.
  Codex is a file copy; Claude Code is not on macOS.
- **#72 (mounts)**: persist the whole role home, not just the harness config
  directory. Add a contract test asserting `~/.claude.json` survives recreation,
  because its loss degrades silently rather than failing.
- **#69 (startup capability check)**: assert `codex login status` and the Claude
  equivalent inside the worker, not on the host — and probe the **interactive**
  path for Claude Code, because `claude -p` succeeds while the TUI still demands
  login.
- **#72 (worker isolation)**: keep `--cap-drop ALL` and default seccomp; disable
  the harness's own sandbox rather than weakening the container.
- **#84 (`factory doctor`)**: check cmux socket reachability from the
  coordinator's own process, since that is the failure that silently disables
  every terminal operation.
- **`TerminalRuntime`**: specify how the coordinator obtains cmux socket access,
  and store cmux handles as UUIDs rather than short refs, which are positional
  and shift underneath a running coordinator.
