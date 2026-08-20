# Spike 0002 — Nested harness TTYs through cmux and Docker

- **Issue**: #2 (parent spec #1, user story 91)
- **Date**: 2026-08-20
- **Status**: in progress — automated evidence complete, keyboard checks pending
- **Reproduction**: `spike/issue-2/` (throwaway; not merged into product packages)

## Question

Can Codex and Claude Code be identified, stopped, recreated, and resumed
robustly through two layers of terminal indirection — a Docker pseudo-terminal
inside a cmux surface?

## Answer so far

Yes for Codex, end to end. A Codex session survived destruction of its
container, was resumed from the same pinned image, and recalled a token given
before the container was destroyed. The nested pseudo-terminal carries window
size, `SIGWINCH`, ANSI colour, and `SIGINT` correctly.

Three findings change the architecture and are described below: Codex's own
sandbox cannot run inside a hardened worker, Claude Code cannot be credential
seeded from a macOS host the way Codex can, and the cmux control socket refuses
callers that cmux did not start.

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

Harness home directories are named volumes (`swf-spike-claude-home`,
`swf-spike-codex-home`). The worktree and the invocation result directory are
bind mounts. Destroying the container therefore destroys no session state.

## Verdict per acceptance criterion

| # | Criterion | Verdict | Evidence |
| --- | --- | --- | --- |
| 1 | Interactive terminal app in a container pseudo-terminal via cmux, explicit terminal type | Codex proven headlessly; cmux surfacing pending | `TERM=xterm-256color`, `tput colors` = 256, `[ -t 1 ]` true under `docker exec -t` |
| 2 | Colours and layout render correctly | Partly proven | ANSI SGR sequences arrive intact through both layers; final judgement is visual |
| 3 | Human keystrokes reach the harness | Pending keyboard | pty probe proves byte delivery into the container |
| 4 | Resize without corruption | Proven | `tty-probe`: 24×80 → 40×132 observed inside the container, `SIGWINCH` raised |
| 5 | Approval prompt raised and answered | Pending keyboard | — |
| 6 | Ctrl+C interrupts the harness, container survives | Proven | `SIGINT` reaches the foreground group, the process dies, container stays `Running` |
| 7 | Stage completes by writing schema-versioned JSON, no screen parsing | Proven for Codex | Codex ran `factory-report`; `.run/results/inv-codex-1.json` has `schema_version: 1` |
| 8 | Container destroyed and recreated from the same pinned image, session resumes with context | **Proven for Codex** | New container id, same image digest, `codex exec resume` returned `SPIKE-TOKEN-7731` |
| 9 | cmux restarted, surfaces recoverable | Pending keyboard | — |
| 10 | Native session identifier observed and documented | Proven for Codex; interface known for Claude | See below |
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
`~/.claude/.credentials.json` to copy. On Linux it does use a plain file, so the
credential exists as a file *inside* the container once logged in there.

**Workaround**: either log in once inside the container, which persists in the
harness home volume, or use `claude setup-token` to mint a long-lived token and
inject it as `CLAUDE_CODE_OAUTH_TOKEN`. Ticket #73 must not assume the two
harnesses seed the same way.

### 3. `docker cp` breaks non-root workers

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

### 4. Bind mounts carry host ownership

`/work` and `/results` are bind mounts and appear inside the container with the
host uid, so uid 10001 cannot write to them. The spike chmods them `0777`. The
product should either run the worker with a uid matching the host user or use
named volumes for the result directory.

### 5. Codex global flags must precede the `resume` subcommand

`codex exec resume <uuid> -s danger-full-access` fails with `unexpected argument
'-s' found`. `codex exec -s danger-full-access resume <uuid>` works. The Codex
adapter must build argv in that order. Likewise `codex exec --full-auto` does
not exist; `--full-auto` is an interactive-only flag.

### 6. The cmux control socket rejects outside callers

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

### 7. cmux workspace layout is the right surfacing primitive

`cmux new-workspace --layout <json>` creates a workspace whose panes each run
their own command, which maps directly onto the spec's run surfaces (status,
test agent, implementation agent, checks, spec review, standards review). See
`spike/issue-2/cmux-surfaces`.

### 8. Terminal echo defeats naive output matching

Any automated driver that types a command into a pty and waits for a marker will
match the terminal's **echo** of the command before the command has produced
output. The `WorkerRuntime` contract tests must not scrape terminal output for
control flow — which is exactly why the spec routes stage results through
`factory-report` and forbids parsing screen content. This spike confirms that
rule empirically.

## Recommendations for downstream tickets

- **#14 / #73 (harness adapters, auth)**: model credential seeding per harness.
  Codex is a file copy; Claude Code is not on macOS.
- **#69 (startup capability check)**: assert `codex login status` and the Claude
  equivalent inside the worker, not on the host.
- **#72 (worker isolation)**: keep `--cap-drop ALL` and default seccomp; disable
  the harness's own sandbox rather than weakening the container.
- **#84 (`factory doctor`)**: check cmux socket reachability from the
  coordinator's own process, since that is the failure that silently disables
  every terminal operation.
- **`TerminalRuntime`**: specify how the coordinator obtains cmux socket access.
