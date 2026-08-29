package factory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// maxProgressionSteps bounds one unattended progression pass. A healthy run
// needs a handful of transitions between two harness invocations, so a pass
// that keeps changing state beyond this ceiling is a coordinator defect rather
// than ordinary work.
const maxProgressionSteps = 32

// progressionOutcome identifies why one unattended progression pass stopped.
type progressionOutcome string

const (
	// progressionIdle means no run was available to drive.
	progressionIdle progressionOutcome = "idle"
	// progressionInvocationActive means a harness invocation is executing and
	// the coordinator is waiting for its structured result.
	progressionInvocationActive progressionOutcome = "invocation_active"
	// progressionWaiting means the run reached a human or infrastructure
	// waiting state that unattended progression must not cross.
	progressionWaiting progressionOutcome = "waiting"
	// progressionDraftPullRequest means the run reached its draft pull
	// request, which is the end of unattended claim-to-draft-PR progression.
	progressionDraftPullRequest progressionOutcome = "draft_pull_request"
	// progressionTerminal means the run has no further progression.
	progressionTerminal progressionOutcome = "terminal"
	// progressionUnsupported means the operational store cannot expose the
	// durable invocation history that exactly-once progression requires.
	progressionUnsupported progressionOutcome = "unsupported"
)

// progressionResult reports one unattended progression pass.
type progressionResult struct {
	// Outcome identifies why the pass stopped.
	Outcome progressionOutcome
	// Run is the run projection observed when the pass stopped.
	Run store.Run
	// Steps counts the coordinator transitions performed in this pass.
	Steps int
	// Reason is the operator-facing explanation of Outcome.
	Reason string
}

// progressionState is one consistent read of the run and the invocation it
// currently delegates to.
type progressionState struct {
	// Run is the persisted non-terminal or terminal run, when one exists.
	Run *store.Run
	// Active is the invocation that may still produce a report.
	Active *store.Invocation
	// Latest is the most recent invocation of any status.
	Latest *store.Invocation
	// Supported reports whether the operational store exposes the durable
	// invocation history that exactly-once progression requires.
	Supported bool
}

// driveRun performs one bounded unattended progression pass for the registered
// repository. It drives the current run as far as the frozen route, the
// repository policy, and the durable evidence allow, and stops at the first
// state a person or infrastructure owns. Every transition runs through the
// same coordinator seam the matching one-shot command uses, so a repeated pass
// or a coordinator restart reuses their exactly-once effect journal instead of
// creating a second invocation, checkpoint, push, comment, or pull request.
func (s *Service) driveRun(ctx context.Context, registration config.RepositoryRegistration) (progressionResult, error) {
	result := progressionResult{Outcome: progressionIdle}
	for result.Steps < maxProgressionSteps {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		state, err := s.readProgressionState(ctx, registration)
		if err != nil {
			return result, err
		}
		if !state.Supported {
			return progressionResult{Outcome: progressionUnsupported, Steps: result.Steps, Reason: "operational store has no durable invocation history"}, nil
		}
		if state.Run == nil {
			return progressionResult{Outcome: progressionIdle, Steps: result.Steps, Reason: "no run to drive"}, nil
		}
		result.Run = *state.Run
		step, stop := s.progressionStep(state)
		if stop != nil {
			stop.Steps = result.Steps
			return s.publishProgressionStop(ctx, registration, *state.Run, *stop)
		}
		if err := step.action(ctx); err != nil {
			return s.stopProgressionAfterFailure(ctx, registration, *state.Run, step.name, err, result.Steps)
		}
		result.Steps++
		advanced, err := s.readProgressionState(ctx, registration)
		if err != nil {
			return result, err
		}
		if advanced.Run == nil {
			return progressionResult{Outcome: progressionIdle, Steps: result.Steps, Reason: "run disappeared after " + step.name}, nil
		}
		result.Run = *advanced.Run
		if !progressionAdvanced(*state.Run, state.Active, *advanced.Run, advanced.Active) {
			return s.publishProgressionStop(ctx, registration, *advanced.Run, progressionResult{
				Outcome: progressionWaiting,
				Steps:   result.Steps,
				Reason:  fmt.Sprintf("%s did not advance the run", step.name),
			})
		}
	}
	return s.publishProgressionStop(ctx, registration, result.Run, progressionResult{
		Outcome: progressionWaiting,
		Steps:   result.Steps,
		Reason:  fmt.Sprintf("progression exceeded %d coordinator transitions", maxProgressionSteps),
	})
}

// progressionAction is one coordinator-owned transition selected from durable
// run state. The coordinator, never an agent, chooses it.
type progressionAction struct {
	// name is the operator-facing transition identity used in status text.
	name string
	// action performs the transition through an existing coordinator seam.
	action func(context.Context) error
}

// progressionStep selects the single legal next transition for a run, or the
// waiting state that stops unattended progression. Exactly one of the two
// return values is set.
func (s *Service) progressionStep(state progressionState) (progressionAction, *progressionResult) {
	run := *state.Run
	switch RunActivityFor(run) {
	case ActivityTerminal:
		return progressionAction{}, &progressionResult{Outcome: progressionTerminal, Reason: "run is " + string(run.Status)}
	case ActivityWaitingForHuman:
		return progressionAction{}, &progressionResult{Outcome: progressionWaiting, Reason: waitingReason(run, "waiting for human disposition")}
	case ActivityWaitingForHarness:
		return progressionAction{}, &progressionResult{Outcome: progressionWaiting, Reason: waitingReason(run, "waiting for harness capacity")}
	}
	if run.CheckRepairPendingAttempt != 0 {
		return progressionAction{}, &progressionResult{Outcome: progressionWaiting, Reason: fmt.Sprintf("check-repair attempt %d is pending reconciliation", run.CheckRepairPendingAttempt)}
	}
	if state.Active != nil {
		if state.Active.AttachRequired {
			return progressionAction{}, &progressionResult{Outcome: progressionWaiting, Reason: fmt.Sprintf("invocation %q requires `factory attach`", state.Active.ID)}
		}
		if !agentResultReady(*state.Active) {
			return progressionAction{}, &progressionResult{Outcome: progressionInvocationActive, Reason: fmt.Sprintf("invocation %q is executing", state.Active.ID)}
		}
		invocationID := state.Active.ID
		return progressionAction{name: "accept agent report", action: func(ctx context.Context) error {
			_, err := s.AcceptAgentReport(ctx, AgentReportRequest{RunID: run.ID, InvocationID: invocationID})
			return err
		}}, nil
	}
	return s.progressionStageStep(state)
}

// progressionStageStep selects the transition the workflow registry declares
// for the run stage once no invocation is executing. The registry stays the
// stage authority: a stage that declares a coordinator event is driven by the
// draft-pull-request seam that owns those events, and a stage that declares a
// role is driven by launching that role.
func (s *Service) progressionStageStep(state progressionState) (progressionAction, *progressionResult) {
	run := *state.Run
	registry := workflow.DefaultRegistry()
	switch run.Stage {
	case store.StageClaim:
		// Leaving claim is the frozen baseline, whose healthy result selects
		// the entry stage from the route and the repository test policy.
		return progressionAction{name: "run baseline", action: func(ctx context.Context) error {
			_, err := s.RunBaseline(ctx, BaselineRequest{RunID: run.ID})
			return err
		}}, nil
	case store.StageDraftPR:
		// The independent specification review is a deliberate step, so
		// unattended progression ends at the draft pull request.
		return progressionAction{}, &progressionResult{Outcome: progressionDraftPullRequest, Reason: fmt.Sprintf("draft pull request #%d is ready for review", run.PullRequestNumber)}
	}
	definition, declared := registry.Stage(run.Stage)
	if !declared {
		return progressionAction{}, &progressionResult{Outcome: progressionWaiting, Reason: fmt.Sprintf("run stage %q is not declared by the workflow registry", run.Stage)}
	}
	_, roleDeclared := registry.RoleForRunStage(run.Stage)
	stageComplete := state.Latest != nil && state.Latest.Stage == run.Stage && state.Latest.Status == store.InvocationStatusCompleted
	if len(definition.Events) > 0 && (!roleDeclared || stageComplete) {
		return progressionAction{name: "create draft pull request", action: func(ctx context.Context) error {
			_, err := s.CreateDraftPullRequest(ctx, DraftPullRequestRequest{RunID: run.ID})
			return err
		}}, nil
	}
	if !roleDeclared {
		return progressionAction{}, &progressionResult{Outcome: progressionWaiting, Reason: fmt.Sprintf("no coordinator transition is declared for run stage %q", run.Stage)}
	}
	return progressionAction{name: "start agent", action: func(ctx context.Context) error {
		_, err := s.StartAgent(ctx, AgentRequest{RunID: run.ID})
		return err
	}}, nil
}

// readProgressionState reads the run and its invocation projection once. A
// store without durable invocation history cannot support exactly-once
// unattended progression, so it reports the run as undrivable rather than
// guessing whether a harness is executing.
func (s *Service) readProgressionState(ctx context.Context, registration config.RepositoryRegistration) (progressionState, error) {
	opened, err := s.deps.OpenStore(ctx, registration.OperationalDataPath)
	if err != nil {
		return progressionState{}, fmt.Errorf("open operational store for progression: %w", err)
	}
	defer func() { _ = opened.Close() }()
	run, err := opened.CurrentRun(ctx)
	if err != nil {
		return progressionState{}, fmt.Errorf("read run for progression: %w", err)
	}
	activeStore, hasActive := opened.(ActiveInvocationStore)
	latestStore, hasLatest := opened.(LatestInvocationStore)
	if !hasActive || !hasLatest {
		return progressionState{}, nil
	}
	state := progressionState{Run: run, Supported: true}
	if run == nil {
		return state, nil
	}
	if state.Active, err = activeStore.ActiveInvocation(ctx, run.ID); err != nil {
		return progressionState{}, fmt.Errorf("read active invocation for progression: %w", err)
	}
	if state.Latest, err = latestStore.LatestInvocation(ctx, run.ID); err != nil {
		return progressionState{}, fmt.Errorf("read latest invocation for progression: %w", err)
	}
	return state, nil
}

// progressionAdvanced reports whether a transition changed durable run state.
// Without this check a seam that returns success without advancing the run
// would keep the coordinator repeating the same external operation.
func progressionAdvanced(previous store.Run, previousInvocation *store.Invocation, next store.Run, nextInvocation *store.Invocation) bool {
	if previous.Stage != next.Stage || previous.Status != next.Status || previous.Revision != next.Revision {
		return true
	}
	if previous.CheckpointSHA != next.CheckpointSHA || previous.PullRequestNumber != next.PullRequestNumber {
		return true
	}
	return invocationIdentity(previousInvocation) != invocationIdentity(nextInvocation)
}

// invocationIdentity renders the comparable identity of an optional
// invocation for progress detection.
func invocationIdentity(invocation *store.Invocation) string {
	if invocation == nil {
		return ""
	}
	return invocation.ID + "/" + string(invocation.Status)
}

// agentResultReady reports whether the invocation has written its structured
// report. `factory-report` renames the file into place, so its presence is the
// durable signal that the coordinator may validate a completed result.
func agentResultReady(invocation store.Invocation) bool {
	if invocation.ResultDirectory == "" {
		return false
	}
	info, err := os.Stat(reportPath(invocation))
	return err == nil && info.Mode().IsRegular()
}

// reportPath returns the only accepted structured-report location inside one
// invocation result directory.
func reportPath(invocation store.Invocation) string {
	return filepath.Join(invocation.ResultDirectory, report.ReportFileName)
}

// waitingReason prefers the coordinator's recorded lifecycle reason so the
// published status keeps the exact cause of the pause.
func waitingReason(run store.Run, fallback string) string {
	if run.LifecycleReason != "" {
		return run.LifecycleReason
	}
	return fallback
}

// publishProgressionStop makes a stall visible. A run that unattended
// progression refuses to move must never stay active and silent, because the
// `agent-running` label would then imply work nobody is doing. Waiting states
// a seam already published, and the legitimate active outcomes, are returned
// unchanged.
func (s *Service) publishProgressionStop(ctx context.Context, registration config.RepositoryRegistration, run store.Run, stop progressionResult) (progressionResult, error) {
	stop.Run = run
	if stop.Outcome != progressionWaiting || run.Status != store.StatusActive {
		return stop, nil
	}
	paused, err := s.pauseProgression(ctx, registration, run, "stall", errors.New(stop.Reason))
	if err != nil {
		return stop, err
	}
	stop.Run = paused
	stop.Reason = waitingReason(paused, stop.Reason)
	return stop, nil
}

// stopProgressionAfterFailure parks the run in a visible waiting state,
// publishes an actionable status, and reports the pass that stopped. A
// cancelled context is an operator `factory stop`, which must leave the run
// exactly as it was.
func (s *Service) stopProgressionAfterFailure(ctx context.Context, registration config.RepositoryRegistration, observed store.Run, step string, cause error, steps int) (progressionResult, error) {
	if pollingContextDone(cause) || ctx.Err() != nil {
		return progressionResult{Outcome: progressionWaiting, Run: observed, Steps: steps, Reason: "coordinator stopped"}, nil
	}
	paused, err := s.pauseProgression(ctx, registration, observed, step, cause)
	if err != nil {
		return progressionResult{Run: observed, Steps: steps}, errors.Join(cause, err)
	}
	outcome := progressionWaiting
	if store.IsTerminalStatus(paused.Status) {
		outcome = progressionTerminal
	}
	return progressionResult{Outcome: outcome, Run: paused, Steps: steps, Reason: waitingReason(paused, cause.Error())}, nil
}

// pauseProgression stops unattended progression in a visible human waiting
// state. The coordinator fails closed here: it cannot tell a transient
// adapter failure from one that already changed external state, so it asks a
// person rather than retrying an effect. A run that a seam already moved out
// of active is left untouched, because that seam owns the more specific
// waiting state it published.
func (s *Service) pauseProgression(ctx context.Context, registration config.RepositoryRegistration, observed store.Run, step string, cause error) (store.Run, error) {
	opened, err := s.deps.OpenStore(ctx, registration.OperationalDataPath)
	if err != nil {
		return observed, fmt.Errorf("open operational store to pause progression: %w", err)
	}
	defer func() { _ = opened.Close() }()
	runStore, ok := opened.(RunStore)
	if !ok {
		return observed, errors.New("operational store does not support pausing progression")
	}
	current, err := readReconciliationRun(ctx, runStore)
	if err != nil {
		return observed, fmt.Errorf("read run to pause progression: %w", err)
	}
	if current == nil {
		return observed, nil
	}
	if current.Status != store.StatusActive {
		return *current, nil
	}
	previous := *current
	next := previous
	next.Status = store.StatusWaitingForHuman
	next.LifecycleReason = fmt.Sprintf("unattended progression stopped at %s: %s", step, cause)
	next.UpdatedAt = s.deps.Now().UTC()
	if err := s.persistAgentRunState(ctx, registration, runStore, previous, next); err != nil {
		return previous, fmt.Errorf("publish progression waiting state: %w", err)
	}
	return next, nil
}
