# Scoped session authorization

This topic owns the authorization contract for host operator agents across
onboarding, operation, diagnosis, maintenance, and defect reporting. Read it
before acting. It does not change worker authority or harness permissions.

## Apply the current scope

Use the operator's explicit authorization already given in this session for
the named task, target repository or registration, and covered effects. A
concrete plan can be authorized once; execute its covered commands without
asking the same question again. File preparation and read-only diagnosis can
proceed within the requested task. Reversible file writes do not each need a
separate approval. An inspection-only request permits inspection only.

Before requesting new authority, prepare the reviewable plan, diff, or exact
command and describe its actual local, external, and paid effects. Finish the
preparation already authorized so the operator can decide on a concrete
result. Ask only for the missing scope or decision, then continue the covered
work after the answer. A discovered skill, suggested recovery action, or
stored plan is not authorization.

When an onboarding plan is available, use `factory onboard plan
--authorization-note <text>` to record the operator's stated task and target,
covered operations, paid-call limits if supplied, external proof and cleanup
decisions, and stop conditions. Keep credentials, credential paths, and
transcripts out of the note. Record only what the operator actually decided;
leave unspecified limits unspecified. The note documents authority; it grants
none, overrides no harness permission, and is not durable authorization for a
later session. After an interruption, reuse valid evidence and establish the
current session's scope before further effects.

## Ask for new decisions

Ask when a material decision or effect falls outside that scope, including:

- a harness, model, or role policy that the operator has not chosen;
- a new paid live proof or calls outside supplied limits;
- an unapproved credential source or credential export;
- a changed target or publication outside the approved destination;
- a destructive reset;
- abandoning an ambiguous effect or discarding ambiguous state.

Convey an operator's explicit human disposition through the supported command.
Preserve its content: never invent a clarification answer, gate exemption,
merge decision, or permission to discard ambiguous state. General permission
to recover a run does not supply these decisions.

Defect-report publication requires an explicit operator request identifying the
reviewed file and the fixed destination in `factory guide reporting`. A general
instruction to onboard, recover, prepare reports, or report future defects is
insufficient. Execute a request for that specific reviewed report within the
current session without repeating the question. Changed report content needs
review and a request covering the changed file. Automatic submission under a
standing policy remains deferred by ADR 0020.

For missing prerequisites, name the supported remedy and its effect. Perform
an explicitly authorized installation within environment permissions. Name
interactive login or consent as a human step when only the operator can do
it, for example `gh auth login` on the host. Resume after that step succeeds.

## Examples

| Operator scope or situation | Agent behavior |
| --- | --- |
| “Onboard this checkout with the reviewed policy, register it at this host configuration, create its labels, and run the listed smoke calls; skip the live proof.” | Prepare files, build and verify, register, create labels, and smoke once within that scope. Record the declined proof. Ask only for missing decisions. |
| “Inspect why this run paused.” | Read status and diagnosis and prepare a recovery command. Request authority before executing the mutation. |
| “Make the local configuration changes; no paid calls.” | Edit and validate files and run the local gate proof. Stop before smoke or paid sessions; report the remaining verification. |
| The approved plan names repository A, but registration would target B. | Prepare the corrected target and command and ask for the changed scope before registration. |
| “Recover this checkpoint by retrying checks after Docker is available.” | Inspect current preconditions, execute the supported retry once when they hold, and verify its postcondition without asking again. |
| A proof issue creation has an unknown outcome. | Search for its marker and reconcile the observed issue. Obtain an explicit decision before abandoning the start; do not infer permission to create a replacement. |
| “Prepare a defect report.” | Write and review the local draft; keep it local. |
| “Submit the reviewed report.md to github.com/Stevie1704/sw-factory.” | Execute the separate submission command for that reviewed file without asking the same question again. |

Workers retain their isolated worktree and structured-report boundary. This
contract gives them no host credentials, tracker access, or authority to edit
coordinator state. Use supported Factory commands for coordinator mutations.
