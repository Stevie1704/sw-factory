---
status: proposed
---

# ADR 0004: tmux as the single terminal adapter

## Context

The factory's visible supervision is macOS-only because its production
`TerminalRuntime` adapter uses cmux. Workflow code already receives only opaque
`WorkspaceID` and `SurfaceID` handles and never interprets terminal screens, so
the terminal seam can serve macOS, Linux, and WSL without changing workflow
authority.

Keeping cmux beside tmux would preserve cmux notifications and restored
scrollback, but it would require two implementations and two continuing human
verification paths for rendering, resize, input, approval, and interrupt
behaviour. cmux restoration does not reattach a harness, so reconciliation must
recreate the visible surface and resume the native harness session either way.
cmux also restricts which processes may use its control socket.

tmux maps onto the existing seam: a session is a workspace, a pane is a
surface, and server-assigned session and pane identifiers are opaque handles.
Its panes are pseudo-terminals suitable for the nested interactive worker
attachment that the factory already uses.

## Decision

tmux is the factory's single production terminal adapter on macOS, Linux, and
WSL. `TerminalRuntime` remains the portability and testing seam; no adapter kind
is exposed in host configuration. When accepted, this decision supersedes the
cmux-specific and macOS-only terminal constraints in parent specification #1.

The factory owns a dedicated tmux server selected by a required absolute
`terminal.socket_path`. Every adapter command passes that path with `-S`, so the
factory never controls an ambient or default tmux server. The adapter starts the
server on demand, and the control session keeps it alive. `WorkspaceInspector`
reports an absent server or session as a successful negative inspection so
reconciliation can recreate the control and run sessions and resume the native
harness session.

The host-configuration schema advances to version 2 and replaces `cmux:` with
the neutral `terminal:` block; the repository-configuration schema remains at
version 1. Version 1 host configuration receives a typed, actionable
re-registration error; no cmux configuration alias or runtime compatibility is
retained. An operator must drain every non-terminal run before installing the
new binary and migrating the registration.

The adapter keeps text separate from a trailing Enter keystroke and sends
prompt-sized text through bracketed paste. A terminal notification succeeds
only after tmux dispatches it to at least one client attached to the target
session. With no attached client, the adapter returns a typed retryable error
and the coordinator does not persist its notification-sent marker. No separate
desktop-notification dependency is introduced.

The adapter remains a visible-surface mechanism. It owns no workflow
transition, Git history, GitHub mutation, or interpretation of terminal output.

## Consequences

One production terminal path and one human verification path serve every
supported host. cmux-specific runtime, diagnosis, reconciliation vocabulary,
CLI flags, tests, and current documentation are removed.

A tmux server restart loses pane identifiers and scrollback. This costs
operator comfort, not correctness: reconciliation recreates the missing
workspace and surfaces, and native harness state remains in the worker's role
volume.

Terminal notification is available only while at least one client is attached
to the target session and may therefore be retried. GitHub remains the durable
human supervision surface.

The dormant multi-pane layout must be built imperatively if it is enabled,
because tmux splits panes rather than accepting cmux's declared layout tree.
