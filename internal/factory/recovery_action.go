package factory

import (
	"context"
	"strconv"

	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// ActionID is the stable identity of one recommended action. It does not
// change when the wording of the action summary changes.
type ActionID string

const (
	// ActionInspectStatus re-reads the read-only status diagnosis.
	ActionInspectStatus ActionID = "status.inspect"
	// ActionInspectDoctor re-runs the read-only startup diagnosis.
	ActionInspectDoctor ActionID = "doctor.inspect"
	// ActionResolveDiscrepancy is a human decision about a recorded
	// discrepancy that no command can resolve on its own.
	ActionResolveDiscrepancy ActionID = "recovery.resolve_discrepancy"
	// ActionRetryChecks re-evaluates the gates of a paused check at the same
	// checkpoint through `factory resume`.
	ActionRetryChecks ActionID = "run.retry_checks"
	// ActionAbandonEffect discards an ambiguous pending effect after a human
	// has inspected its external outcome.
	ActionAbandonEffect ActionID = "pending_effect.abandon"
)

// ActionKind separates an executable action from an inspection and from a
// decision that needs human input.
type ActionKind string

const (
	// ActionKindExecutable is a complete command that changes state.
	ActionKindExecutable ActionKind = "executable"
	// ActionKindInspection is a complete command that only reads state.
	ActionKindInspection ActionKind = "inspection"
	// ActionKindHumanDecision needs a human judgement or human input. Its
	// command is never complete without that input.
	ActionKindHumanDecision ActionKind = "human_decision"
)

// ActionAuthorization is the authority an action needs. A recommendation is
// never itself an authorization.
type ActionAuthorization string

const (
	// AuthorizationNone means the action reads state only.
	AuthorizationNone ActionAuthorization = "none"
	// AuthorizationOperator means the operator must approve the action.
	AuthorizationOperator ActionAuthorization = "operator"
	// AuthorizationHumanReview means a human must inspect evidence and
	// decide; an agent must not supply the decision.
	AuthorizationHumanReview ActionAuthorization = "human_review"
)

// ActionTarget identifies the exact state an action applies to. Zero fields
// do not constrain the target.
type ActionTarget struct {
	// RunID identifies the run.
	RunID string
	// Revision is the run revision the recommendation was made against.
	Revision int64
	// CheckpointSHA is the checkpoint the action applies to.
	CheckpointSHA string
	// InvocationID identifies the invocation the action applies to.
	InvocationID string
	// EffectID identifies the pending effect the action applies to.
	EffectID string
}

// ActionCondition is one structured precondition or postcondition.
type ActionCondition struct {
	// Code is the stable condition identity.
	Code string
	// Description is the human wording of the condition.
	Description string
}

// ActionArgument is one flag of an action command.
type ActionArgument struct {
	// Flag is the flag name without dashes.
	Flag string
	// Value is the fixed value; it is empty when HumanInput is true.
	Value string
	// HumanInput means a human must supply the value.
	HumanInput bool
}

// RecommendedAction is one typed next step of a diagnosis. Inspection never
// runs it; the coordinator rechecks its target and preconditions when the
// command runs.
type RecommendedAction struct {
	// ID is the stable action identity.
	ID ActionID
	// Kind separates executable, inspection, and human-decision actions.
	Kind ActionKind
	// Summary is the human wording of the action.
	Summary string
	// Command is the base argument vector, for example factory resume.
	Command []string
	// Arguments are the typed flags of the command.
	Arguments []ActionArgument
	// Target is the state the action applies to.
	Target ActionTarget
	// Preconditions must be true before the action runs.
	Preconditions []ActionCondition
	// Effect classifies what the action changes.
	Effect doctor.Effect
	// Authorization is the authority the action needs.
	Authorization ActionAuthorization
	// Postcondition is the expected state after a completed action.
	Postcondition ActionCondition
	// Verify is the inspection command that confirms the postcondition.
	Verify []string
}

// Argv returns the complete argument vector. It reports false when a human
// must supply an argument, so a caller cannot execute the action blindly.
func (a RecommendedAction) Argv() ([]string, bool) {
	if len(a.Command) == 0 {
		return nil, false
	}
	argv := append([]string{}, a.Command...)
	for _, argument := range a.Arguments {
		if argument.HumanInput {
			return nil, false
		}
		argv = append(argv, "--"+argument.Flag, argument.Value)
	}
	return argv, true
}

// statusVerification returns the inspection that confirms a recovery
// postcondition. Each call returns a new slice, so no action shares it.
func statusVerification() []string { return []string{"factory", "status", "--json"} }

// baselineRecoveryActions are the actions of every recovery diagnosis. They
// read state or need a human decision; none of them claims recovery.
func baselineRecoveryActions(runID string) []RecommendedAction {
	target := ActionTarget{RunID: runID}
	return []RecommendedAction{
		{
			ID: ActionInspectStatus, Kind: ActionKindInspection, Summary: "inspect the diagnosis with `factory status`",
			Command: statusVerification(), Target: target, Effect: doctor.EffectInspection, Authorization: AuthorizationNone,
		},
		{
			ID: ActionInspectDoctor, Kind: ActionKindInspection, Summary: "run the startup diagnosis with `factory doctor`",
			Command: []string{"factory", "doctor", "--json"}, Target: target, Effect: doctor.EffectInspection, Authorization: AuthorizationNone,
		},
		{
			ID: ActionResolveDiscrepancy, Kind: ActionKindHumanDecision, Summary: "verify the repository, worktree, branch, checkpoint, issue label, status comment, and pull request, then resolve the recorded discrepancy before retrying the run",
			Target: target, Effect: doctor.EffectExternalMutation, Authorization: AuthorizationHumanReview,
			Verify: statusVerification(),
		},
	}
}

// retryChecksAction is the guarded continuation of a check infrastructure
// pause. Its revision and checkpoint let resume refuse a stale recommendation.
func retryChecksAction(run store.Run) RecommendedAction {
	revision := strconv.FormatInt(run.Revision, 10)
	return RecommendedAction{
		ID: ActionRetryChecks, Kind: ActionKindExecutable, Summary: checkContinuationAction,
		Command: []string{"factory", "resume"},
		Arguments: []ActionArgument{
			{Flag: "run-id", Value: run.ID},
			{Flag: "expect-revision", Value: revision},
			{Flag: "expect-checkpoint", Value: run.CheckpointSHA},
		},
		Target: ActionTarget{RunID: run.ID, Revision: run.Revision, CheckpointSHA: run.CheckpointSHA},
		Preconditions: []ActionCondition{
			{Code: "check_infrastructure.available", Description: "the worker runtime that runs the gates is available; `factory doctor` reports no blocking check"},
			{Code: "run.revision_unchanged", Description: "the run still has revision " + revision},
		},
		Effect:        doctor.EffectExternalMutation,
		Authorization: AuthorizationOperator,
		Postcondition: ActionCondition{Code: "run.check_active", Description: "the run is active in the check stage at checkpoint " + run.CheckpointSHA},
		Verify:        statusVerification(),
	}
}

// abandonEffectAction is the human-reviewed abandonment of one pending
// effect. Its reason is human input, so it is never complete on its own.
func abandonEffectAction(runID string, effect store.PendingEffect) RecommendedAction {
	return RecommendedAction{
		ID: ActionAbandonEffect, Kind: ActionKindHumanDecision,
		Summary: "after human review, run `factory reconcile --abandon-effect " + effect.ID + " --reason <reason>`",
		Command: []string{"factory", "reconcile"},
		Arguments: []ActionArgument{
			{Flag: "run-id", Value: runID},
			{Flag: "abandon-effect", Value: effect.ID},
			{Flag: "reason", HumanInput: true},
		},
		Target: ActionTarget{RunID: runID, EffectID: effect.ID},
		Preconditions: []ActionCondition{
			{Code: "pending_effect.reviewed", Description: "a human inspected the external outcome of effect " + effect.ID + " (" + string(effect.Kind) + ")"},
		},
		Effect:        doctor.EffectLocalMutation,
		Authorization: AuthorizationHumanReview,
		Postcondition: ActionCondition{Code: "run.waiting_for_human", Description: "the effect is discarded and the run stays paused"},
		Verify:        statusVerification(),
	}
}

// RefusalCode is the stable identity of a refused operation.
type RefusalCode string

const (
	// RefusalNoRun means no persisted run exists.
	RefusalNoRun RefusalCode = "run.none"
	// RefusalRunMismatch means the selected run is not the active run.
	RefusalRunMismatch RefusalCode = "run.mismatch"
	// RefusalRunTerminal means the run is terminal and cannot continue.
	RefusalRunTerminal RefusalCode = "run.terminal"
	// RefusalRevisionChanged means the run changed after the recommendation.
	RefusalRevisionChanged RefusalCode = "run.revision_changed"
	// RefusalCheckpointChanged means the run checkpoint changed after the
	// recommendation.
	RefusalCheckpointChanged RefusalCode = "run.checkpoint_changed"
	// RefusalEffectChanged means the pending effect is not the named effect.
	RefusalEffectChanged RefusalCode = "pending_effect.changed"
	// RefusalInvocationChanged means the run no longer has the named
	// invocation as its recovery target.
	RefusalInvocationChanged RefusalCode = "invocation.changed"
	// RefusalCheckInfrastructureUnavailable means the worker runtime that runs
	// the gates is still unavailable, so a check retry cannot succeed.
	RefusalCheckInfrastructureUnavailable RefusalCode = "check_infrastructure.unavailable"
)

// OperationRefusedError reports an operation refused before it changed any
// state, because its target or a precondition was not true.
type OperationRefusedError struct {
	// Code is the stable refusal identity.
	Code RefusalCode
	// RunID identifies the run the refusal observed, when one exists.
	RunID string
	// Message is the bounded human explanation.
	Message string
}

// Error returns the human explanation of the refusal.
func (e *OperationRefusedError) Error() string { return e.Message }

// errNoRun is the refusal of a run-scoped operation without a persisted run.
var errNoRun = &OperationRefusedError{Code: RefusalNoRun, Message: "no persisted run"}

// errNoActiveRun is the refusal of a progression without an active run.
var errNoActiveRun = &OperationRefusedError{Code: RefusalNoRun, Message: "no active run"}

// checkRunSelection refuses an operation that selected another run than the
// persisted one. An empty selection accepts the persisted run.
func checkRunSelection(selected string, run store.Run) error {
	if selected != "" && selected != run.ID {
		return runMismatch("active", run.ID, selected)
	}
	return nil
}

// runMismatch is the refusal of a selected run that is not the label run,
// for example the active or the latest run.
func runMismatch(label, runID, selected string) error {
	return &OperationRefusedError{Code: RefusalRunMismatch, RunID: runID, Message: label + " run is " + runID + ", not " + selected}
}

// checkInvocationSelection refuses a recovery that names another invocation
// than the one the run would recover. An empty selection accepts it.
func checkInvocationSelection(selected string, run store.Run, invocation store.Invocation) error {
	if selected != "" && selected != invocation.ID {
		return &OperationRefusedError{Code: RefusalInvocationChanged, RunID: run.ID, Message: "run " + run.ID + " recovers invocation " + invocation.ID + ", not " + selected}
	}
	return nil
}

// dockerProbe is the worker runtime probe that the doctor also runs.
type dockerProbe interface {
	CheckDocker(context.Context) error
}

// checkInfrastructureAvailable rechecks the precondition of a check retry.
// A runtime without the probe gives no evidence, so it is refused too.
func (s *Service) checkInfrastructureAvailable(ctx context.Context, run store.Run) error {
	probe, ok := s.deps.Worker.(dockerProbe)
	if ok && probe.CheckDocker(ctx) == nil {
		return nil
	}
	return &OperationRefusedError{Code: RefusalCheckInfrastructureUnavailable, RunID: run.ID, Message: "the worker runtime that runs the gates is not available; repair it until `factory doctor` reports no blocking check, then retry"}
}

// checkResumeTarget refuses a resume whose target is not the persisted run.
func checkResumeTarget(request ResumeRequest, run store.Run) error {
	if err := checkRunSelection(request.RunID, run); err != nil {
		return err
	}
	if store.IsTerminalStatus(run.Status) {
		return &OperationRefusedError{Code: RefusalRunTerminal, RunID: run.ID, Message: "cannot resume terminal run " + strconv.Quote(run.ID) + " with status " + strconv.Quote(string(run.Status))}
	}
	if request.ExpectedRevision != 0 && request.ExpectedRevision != run.Revision {
		return &OperationRefusedError{Code: RefusalRevisionChanged, RunID: run.ID, Message: "run " + run.ID + " has revision " + strconv.FormatInt(run.Revision, 10) + ", not " + strconv.FormatInt(request.ExpectedRevision, 10)}
	}
	if request.ExpectedCheckpointSHA != "" && request.ExpectedCheckpointSHA != run.CheckpointSHA {
		return &OperationRefusedError{Code: RefusalCheckpointChanged, RunID: run.ID, Message: "run " + run.ID + " has checkpoint " + run.CheckpointSHA + ", not " + request.ExpectedCheckpointSHA}
	}
	return nil
}
