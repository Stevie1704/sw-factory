---
status: accepted
---

# Automatic recovery of typed lifecycle pauses

A credential expiry pauses a run in `waiting_for_human` with a typed lifecycle reason that names its own remedy. Its recovery diagnosis agrees with all sources, and every discrepancy only restates that the paused worker and its harness process stopped. The operator still had to adjudicate a transition that the state machine had already decided (issue #195).

The polling supervisor now recovers such a pause without a human command, but only when a conjunction holds that is cheap to evaluate and easy to audit:

- the lifecycle reason is on an explicit allow-list (today: `harness authentication expired`), and the run has no open clarification question;
- the recovery sources agree;
- every discrepancy belongs to the process-exit family: a recoverable `worker.existence`, `worker.lifecycle`, `harness.lifecycle`, or `native session.identity`, and any classified headless failure is the same credential expiry;
- no pending effect is journaled;
- the blocker has cleared outside the coordinator: the registered host credential source was modified after the pause.

Before the attempt crosses the native boundary, the coordinator records `automatic recovery attempted after host credential change` in the pause reason and moves the run's update time to the observed credential change or later. One credential change therefore admits at most one attempt, also after a crash or with a clock that is behind the file. The attempt uses the `factory auth refresh --resume` path: it projects the host credential and resumes the exact recorded native session. It does not change the workflow retry budget, as a manual `factory resume` does not. On success, the published lifecycle reason starts with `automatic recovery:` and names the reason that admitted it. On failure, the run stays paused, and the manual commands stay available.

Anything outside the conjunction keeps the human gate unchanged.

## Considered options

| Option | Result |
| --- | --- |
| Resume on host credential change | Accepted. The trigger is deterministic and costs one file stat per poll tick. A pause whose update time moves for another reason after the credential change is not recovered automatically; the manual commands still apply. |
| Plain `factory auth refresh` also resumes | Rejected. It reverses the opt-in default of #194, and polling still could not act alone. |
| Retry the resume on every poll tick | Rejected. Each tick would start a worker and resume the session against a credential that is still expired. |
| An LLM repair agent clears the pause | Rejected. The information is already typed, and an agent that optimizes for unblocked runs erodes the `sources=agree` / `disagree` distinction where a wrong decision costs most. Agent reasoning stays at the work layer (`/factory repair`, `check_repair`, `review_repair`). |
| Persist a credential fingerprint at pause time | Rejected for now. It needs a store schema migration, and the modification time is sufficient for the one allow-listed reason. |
