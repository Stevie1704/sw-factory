---
status: accepted
---

# Browser session commands go through a durable inbox

ADR 0021 decided that the browser reaches the coordinator through a durable
command inbox, not a control socket, and that the coordinator is the only
writer of session state. `factory ui` and `factory start` are separate
processes that share only the operational store (ADR 0010). This record
defines the inbox contract (#267): identity, acknowledgement, recovery,
availability, and the protection of the mutation route.

## Decision

1. **One inbox row per browser request.** The UI parses a request strictly
   and with bounds (`internal/sessioncommand`), then appends one row to
   `ideation_commands` (schema version 45) through `store.CommandInbox`. That
   handle opens only an existing, current store; it never creates, migrates,
   or backs up the store, and its only write is the append. A command has a
   browser-chosen request ID (unique), a session ID, a kind, the session
   revision the page showed, and a canonical JSON payload.
2. **Acceptance is not completion.** A new command is `accepted`; the answer
   is HTTP 202. The coordinator moves it to `applying` and then to
   `completed` with one outcome: `applied`, `conflict`, `refused`,
   `unsupported`, or `failed`. The first outcome is final. The browser reads
   outcomes and session activity from the store, so a reload or reconnect
   shows accepted results without a live connection.
3. **Idempotency and stale tabs.** A repeated request ID with the same content
   returns the stored command and its outcome, also while the coordinator is
   stopped. The same ID with other content is refused. A command whose
   expected revision is not current is refused before the append (HTTP 409
   with both revisions), and the coordinator checks the revision again before
   it applies the command. The request ID is also the turn, approval, and
   publication receipt ID, so a repeated command records each of them once.
4. **Exactly-once application.** A session write can name its command
   (`IdeationWrite.CommandID`); the store then completes the command in the
   same transaction. A command interrupted by a coordinator stop stays
   `applying` and the next coordinator resumes it. Handlers must be safe to
   run again: approval and publication continue by request ID, and session
   creation accepts its own earlier result. A command that a coordinator
   started three times without an outcome completes as `failed`.
5. **The coordinator is the only consumer.** `factory start` drains the inbox
   at startup and then every second, beside the run loop. Commands that call
   the tracker (preview, approve, publish, reconcile) run in the background,
   one at a time per session, so a cancel is applied while publication runs.
   Each command kind has one handler; session roles and review pages replace
   or extend handlers instead of adding a second command path. Without a
   tracker that implements `tracker.PlanPublisher`, publication commands
   complete as `unsupported`.
6. **An unavailable coordinator gives an explicit result.** When the persisted
   supervisor heartbeat is missing or expired, a new command gets HTTP 503
   with the outcome `unavailable` and nothing is appended. The UI never starts
   a writer. The heartbeat can stay live for up to one poll interval plus
   backoff after a coordinator dies; a command accepted in that window stays
   durable and completes when a coordinator starts again.
7. **The mutation route accepts only this UI's pages.** `POST /api/commands`
   requires an `Origin` equal to the loopback origin of the request, rejects a
   `Sec-Fetch-Site` other than `same-origin`, requires `application/json`, and
   requires the per-process token that the layout renders into each page. A
   page on another origin cannot read that token, and the existing Host check
   stops DNS rebinding. The body is bounded and decoded with unknown fields
   refused. Other methods get HTTP 405. The Content-Security-Policy keeps
   `form-action 'none'`; pages submit with `fetch` from
   `/assets/commands.js`.
8. **Progress is structured state.** Session pages read the session, its
   revisions, approvals, receipts, publication ledger, and command outcomes.
   Harness output and role text are never parsed as approval or workflow
   state.

## Consequences

The UI process has one write path, so the "UI never changes the store"
sentence of the local web UI contract becomes "the UI only appends commands".
Command latency is up to one second plus the work itself. Approving needs a
coordinator-rendered preview first, because only the coordinator talks to the
tracker; the approval then binds the digests of that preview. Automatic page
refresh pauses while a form has focus or unsent input, so typed input is not
lost. The inbox keeps completed commands; like other session rows, they go
away with `factory reset`.

## Considered options

- **A local control socket.** Rejected in ADR 0021: it loses requests on a
  crash and needs the coordinator to be running for reads.
- **Serving the UI from `factory start`.** Rejected: run pages must work while
  the coordinator is stopped.
- **Relaxing the CSP to `form-action 'self'`.** Rejected: a plain form post
  cannot carry the token header or a JSON body, and the policy would allow
  form posts from any page that a browser opens on this origin.
- **Completing commands only after the handler returns.** Rejected: a stop
  between the session write and the completion would apply a non-idempotent
  edit twice; the same-transaction completion removes that window.
