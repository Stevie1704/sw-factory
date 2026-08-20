# Spike 0002 — Nested harness TTYs through cmux and Docker

- **Issue**: #2 (parent spec #1, user story 91)
- **Date**: 2026-08-20
- **Status**: in progress — the central risk is answered; five criteria still need a person at a keyboard
- **Reproduction**: `spike/issue-2/` (throwaway; not merged into product packages)

## Question

Can Codex and Claude Code be identified, stopped, recreated, and resumed
robustly through two layers of terminal indirection — a Docker pseudo-terminal
inside a cmux surface?

## Answer so far

Yes, for both harnesses. Each one survived the destruction of its container.
Each one resumed from the same pinned image and recalled a token given before
the container was destroyed: Codex `SPIKE-TOKEN-7731`, Claude Code
`SPIKE-TOKEN-4402`. The nested pseudo-terminal carries window size, `SIGWINCH`,
ANSI colour, and `SIGINT` correctly.

Five findings change the architecture:

1. The Codex sandbox cannot start inside a hardened worker.
2. The two harnesses answer Ctrl+C differently.
3. A macOS host cannot seed a Claude Code credential as a file.
4. Claude Code keeps state outside the directory that an obvious mount holds.
5. The cmux control socket refuses callers that cmux did not start.

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
see failure mode 4. The worktree and the invocation result directory are bind
mounts. Destroying the container therefore destroys no session state.

## Verdict per acceptance criterion

Issue #2 says a person must verify resizing, approval prompts, interrupt
handling, and correct rendering. Automated evidence can show that the mechanism
works. It cannot show that the result looks right. The table separates the two.

| # | Criterion | Verdict | Evidence |
| --- | --- | --- | --- |
| 1 | Interactive terminal app in a container pseudo-terminal via cmux, explicit terminal type | Proven for both | Both TUIs read out of their own cmux surfaces, with the returned surface id checked against the requested one. Claude Code v2.1.232 and Codex v0.148.0, both at `/work`. See `spike/issue-2/evidence/` |
| 2 | Colours and layout render correctly | Mechanism proven; appearance needs a person | Claude surface: 408 box-drawing characters, palette `#D77757/#888888/#999999`. Codex surface: 108 box-drawing characters, 10 colours. The grids contain no broken rows. Whether the result looks correct is a human judgement |
| 3 | Human keystrokes reach the harness | Proven through Docker; cmux layer needs a person | The operator typed into both TUIs in the container and both answered |
| 4 | Resize without corruption | Mechanism proven at both layers; appearance needs a person | `tty-probe` moved 24×80 to 40×132 inside the container and `SIGWINCH` arrived. `harness-probe` resized both live TUIs and each redrew wider. Neither test can judge corruption |
| 5 | Approval prompt raised and answered | Not proven | Claude Code raised its folder-trust prompt and accepted an answer, but the criterion asks for a human answering a tool approval |
| 6 | Ctrl+C interrupts the harness, container survives | Proven for both | `harness-probe` sends Ctrl+C to the live TUIs. The container stays `Running` in both cases. The two harnesses behave differently — see failure mode 2 |
| 7 | Stage completes by writing schema-versioned JSON, no screen parsing | Proven for Codex; not proven for Claude Code | Codex ran `factory-report`. The result is committed at `spike/issue-2/evidence/inv-codex-1.json` |
| 8 | Container destroyed and recreated from the same pinned image, session resumes with context | Proven for both headless; proven interactively for Claude Code only | Codex returned `SPIKE-TOKEN-7731` and Claude Code returned `SPIKE-TOKEN-4402`. After a later recreate the Claude TUI started with no theme, login, or trust prompt. Codex interactive resume after a recreate was not tested |
| 9 | cmux restarted, surfaces recoverable | Not proven | — |
| 10 | Native session identifier observed and documented | Proven for both | See below |
| 11 | Findings written up, spike code not merged into product packages | Done | This document. Code stays in `spike/issue-2/` |

Criteria 2, 3, 4, 5, and 9 still need a person at a keyboard. Criterion 7 needs
one more interactive check for Claude Code, and criterion 8 needs one more
interactive check for Codex.

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

### 2. The two harnesses answer Ctrl+C differently

Both harnesses keep the container alive, which is what the criterion requires.
They do not treat the keystroke the same way.

| Harness | Effect of Ctrl+C | Consequence for the coordinator |
| --- | --- | --- |
| Claude Code | Interrupts the current work. The process keeps running | The surface stays usable. No resume is needed |
| Codex | The process exits | The coordinator must resume the native session to continue |

Measured by `spike/issue-2/bin/harness-probe`, which sends Ctrl+C to each live
TUI and then looks for the process inside the container.

The spec gives an unexpected harness exit one automatic native-resume attempt.
A human pressing Ctrl+C in a Codex surface produces exactly that state. The
coordinator must not count an operator interrupt against a retry budget.

### 3. Claude Code cannot be credential seeded from a macOS host

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

### 4. Claude Code keeps state outside `~/.claude`

The obvious mount — a volume at `~/.claude` — silently loses state. Claude Code
writes `~/.claude.json` **beside** that directory, not inside it, so it lands in
the container's writable layer and dies with the container. It holds project
trust, MCP configuration, and onboarding state.

The first recreate produced:

```
Claude configuration file not found at: /home/factory/.claude.json
A backup file exists at: /home/factory/.claude/backups/.claude.json.backup.<ts>
```

Resume still worked. Claude Code writes its own backups inside `~/.claude`,
which the volume held.

The recovery hides the fault. The symptom is a warning, not an error. The
defect can therefore reach production, where every recreated worker asks the
operator to trust the directory again.

**Workaround**: persist the entire home directory as a volume and nest the
per-harness volumes inside it. `CLAUDE_CONFIG_DIR` was not found in the
installed build, so relocating the file is not an option here.

Codex does not have this problem. All of its state is under `~/.codex`.

**Do not restore `~/.claude.json` by hand.** Copying a saved copy back appears
to work, but it is not sufficient. See failure mode 5. Mount the whole home
directory before the first login.

### 5. Headless authentication and interactive readiness are not the same state

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

The automated evidence for failure mode 4 used `claude -p` only. It reported
the fix as verified. The operator then had to log in again in the cmux surface.

### 6. `docker cp` breaks non-root workers

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

### 7. Bind mounts carry host ownership

`/work` and `/results` are bind mounts and appear inside the container with the
host uid, so uid 10001 cannot write to them. The spike chmods them `0777`. The
product should either run the worker with a uid matching the host user or use
named volumes for the result directory.

### 8. Codex global flags must precede the `resume` subcommand

`codex exec resume <uuid> -s danger-full-access` fails with `unexpected argument
'-s' found`. `codex exec -s danger-full-access resume <uuid>` works. The Codex
adapter must build argv in that order. Likewise `codex exec --full-auto` does
not exist; `--full-auto` is an interactive-only flag.

### 9. The cmux control socket rejects outside callers

```
Error: ERROR: Access denied - only processes started inside cmux can connect
```

cmux only accepts control connections from processes it started, unless a socket
password is supplied via `--password`, `CMUX_SOCKET_PASSWORD`, or Settings.

This confirms the spec's decision to run the coordinator inside a dedicated
cmux control workspace. The decision is a requirement, not a preference. A coordinator launched from an ordinary shell or a `launchd` job
cannot drive cmux at all. `TerminalRuntime` needs an explicit story for
obtaining socket access.

### 10. cmux workspace layout is the right surfacing primitive

`cmux new-workspace --layout <json>` creates a workspace whose panes each run
their own command, which maps directly onto the spec's run surfaces (status,
test agent, implementation agent, checks, spec review, standards review). See
`spike/issue-2/cmux-surfaces`. Both harness TUIs came up correctly this way.

Two ref-resolution traps, both of which produced wrong answers before they were
noticed:

- Short refs such as `pane:6` resolve **relative to the caller's workspace**.
  `cmux focus-pane pane:6` fails with `Pane not found` unless
  `--workspace <id>` names the owning workspace.
- Short refs are positional and change when workspaces open or close. Resolve
  each handle to a UUID once and store the UUID.

### 11. `terminal.replay` ignores the surface it is asked for

`cmux rpc terminal.replay '{"surface": "<id>"}'` returns the **active** surface's
render grid regardless of the argument. A deliberately invalid id returns the
caller's own surface rather than an error, and the response's `surface_id` field
then disagrees with the request.

To read a specific surface, focus it first. Focus follows the caller. A
coordinator therefore cannot read a surface that it does not occupy.

This is only a problem for anything that wants to *read* terminals — which the
factory must never do for control flow. It reinforces the spec's rule: stage
results travel through `factory-report`, never through the screen. Always
compare the returned `surface_id` against the requested one before trusting
anything read this way.

### 12. A pinned worker must disable harness auto-update

The Claude Code TUI reported `Auto-update failed: no write permission to npm
prefix`. The failure is benign — the non-root user cannot write the global npm
prefix, which is what keeps the pinned version pinned — but the attempt is noise
and would succeed if the image were ever built with a writable prefix. Set
`DISABLE_AUTOUPDATER=1` for Claude Code so a run cannot change harness version
mid-flight, as the spec requires.

### 13. Terminal echo defeats naive output matching

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
  and store cmux handles as UUIDs. Short refs are positional and change while
  the coordinator runs.

## Outstanding work before the spike can close

A person must do these at a keyboard, in the cmux workspace created by
`spike/issue-2/cmux-surfaces`:

1. Type into both surfaces and confirm the harness receives the keystrokes.
2. Confirm that colours and layout look correct in both surfaces.
3. Resize a surface and confirm the redraw has no corruption.
4. Ask Claude Code in its surface to run `factory-report`. Approve the prompt
   that the harness raises. This closes criteria 5 and 7.
5. Quit cmux, start it again, and record whether `cmux restore-session` returns
   the surfaces. This closes criterion 9.
6. Resume a Codex session interactively after `./worker recreate`. This closes
   the remaining half of criterion 8.

## Notes on scope

`CONTEXT.md` and `docs/adr/0001-local-evaluation-without-outbound-telemetry.md`
were committed alongside this spike. They are not part of issue #2.

The glossary in `CONTEXT.md` does not yet define `harness`, `worker`, `surface`,
`invocation`, `coordinator`, or `role`. This spike uses all six terms. The gap
is recorded here for `/domain-modeling` rather than resolved in a spike.
