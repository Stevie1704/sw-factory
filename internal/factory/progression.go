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

// ProgressionOutcome identifies why one unattended progression pass stopped.
type ProgressionOutcome string

const (
	// ProgressionIdle means no run was available to drive.
	ProgressionIdle ProgressionOutcome = "idle"
	// ProgressionInvocationActive means a harness invocation is executing and
	// the coordinator is waiting for its structured result.
	ProgressionInvocationActive ProgressionOutcome = "invocation_active"
	// ProgressionWaiting means the run reached a human or infrastructure
	// waiting state that automatic progression must not cross.
	ProgressionWaiting ProgressionOutcome = "waiting"
	// ProgressionDraftPullRequest means the run reached its draft pull
	// request, which is the end of unattended claim-to-draft-PR progression.
	ProgressionDraftPullRequest ProgressionOutcome = "draft_pull_request"
	// ProgressionTerminal means the run has no further progression.
	ProgressionTerminal ProgressionOutcome = "terminal"
	// ProgressionUnsupported means the operational store cannot expose the
	// durable invocation history that exactly-once progression requires.
	ProgressionUnsupported ProgressionOutcome = "unsupported"
)

// ProgressionResult reports one unattended progression pass.
type ProgressionResult struct {
	// Outcome identifies why the pass stopped.
	Outcome ProgressionOutcome
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

// AdvanceRun drives the current run as far as the frozen route, repository
// policy, and durable evidence allow, without an operator running a
// stage-driving command. Every transition is performed through the same
// coordinator seams the one-shot commands use, so a repeated pass or a
// coordinator restart reuses their exactly-once effect journal instead of
// creating a second invocation, checkpoint, push, comment, or pull request.
func (s *Service) AdvanceRun(ctx context.Context) (ProgressionResult, error) {
	registration, err := s.registration()
	if err != nil {
		return ProgressionResult{}, err
	}
	return s.driveRun(ctx, registration)
}

// driveRun performs bounded unattended progression for one registered
// repository. It stops at the first state a person or infrastructure owns.
func (s *Service) driveRun(ctx context.Context, registration config.RepositoryRegistration) (ProgressionResult, error) {
	result := ProgressionResult{Outcome: ProgressionIdle}
	for result.Steps < maxProgressionSteps {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		state, err := s.readProgressionState(ctx, registration)
		if err != nil {
			return result, err
		}
		if !state.Supported {
			return ProgressionResult{Outcome: ProgressionUnsupported, Steps: result.Steps, Reason: "operational store has no durable invocation history"}, nil
		}
		if state.Run == nil {
			return ProgressionResult{Outcome: ProgressionIdle, Steps: result.Steps, Reason: "no run to drive"}, nil
		}
		result.Run = *state.Run
		step, stop := s.progressionStep(state)
		if stop != nil {
			stop.Steps = result.Steps
			stop.Run = *state.Run
			return *stop, nil
		}
		if err := step.action(ctx); err != nil {
			paused, pauseErr := s.pauseProgression(ctx, registration, *state.Run, step.name, err)
			if pauseErr != nil {
				return result, errors.Join(err, pauseErr)
			}
			outcome := ProgressionWaiting
			if store.IsTerminalStatus(paused.Status) {
				outcome = ProgressionTerminal
			}
			return ProgressionResult{
				Outcome: outcome,
				Run:     paused,
				Steps:   result.Steps,
				Reason:  waitingReason(paused, err.Error()),
			}, nil
		}
		result.Steps++
		advanced, err := s.readProgressionState(ctx, registration)
		if err != nil {
			return result, err
		}
		if advanced.Run == nil {
			return ProgressionResult{Outcome: ProgressionIdle, Steps: result.Steps, Reason: "run disappeared after " + step.name}, nil
		}
		result.Run = *advanced.Run
		if !progressionAdvanced(*state.Run, state.Active, *advanced.Run, advanced.Active) {
			return ProgressionResult{
				Outcome: ProgressionWaiting,
				Run:     *advanced.Run,
				Steps:   result.Steps,
				Reason:  fmt.Sprintf("%s did not advance the run", step.name),
			}, nil
		}
	}
	result.Outcome = ProgressionWaiting
	result.Reason = fmt.Sprintf("progression exceeded %d coordinator transitions", maxProgressionSteps)
	return result, nil
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
// waiting state that stops automatic progression. Exactly one of the two
// return values is set.
func (s *Service) progressionStep(state progressionState) (progressionAction, *ProgressionResult) {
	run := *state.Run
	if store.IsTerminalStatus(run.Status) {
		return progressionAction{}, &ProgressionResult{Outcome: ProgressionTerminal, Reason: "run is " + string(run.Status)}
	}
	switch run.Status {
	case store.StatusWaitingForHuman:
		return progressionAction{}, &ProgressionResult{Outcome: ProgressionWaiting, Reason: waitingReason(run, "waiting for human disposition")}
	case store.StatusWaitingForHarness:
		return progressionAction{}, &ProgressionResult{Outcome: ProgressionWaiting, Reason: waitingReason(run, "waiting for harness capacity")}
	}
	if run.CheckRepairPendingAttempt != 0 {
		return progressionAction{}, &ProgressionResult{Outcome: ProgressionWaiting, Reason: fmt.Sprintf("check-repair attempt %d is pending reconciliation", run.CheckRepairPendingAttempt)}
	}
	if state.Active != nil {
		if state.Active.AttachRequired {
			return progressionAction{}, &ProgressionResult{Outcome: ProgressionWaiting, Reason: fmt.Sprintf("invocation %q requires `factory attach`", state.Active.ID)}
		}
		if !agentResultReady(*state.Active) {
			return progressionAction{}, &ProgressionResult{Outcome: ProgressionInvocationActive, Reason: fmt.Sprintf("invocation %q is executing", state.Active.ID)}
		}
		invocationID := state.Active.ID
		return progressionAction{name: "accept agent report", action: func(ctx context.Context) error {
			_, err := s.AcceptAgentReport(ctx, AgentReportRequest{RunID: run.ID, InvocationID: invocationID})
			return err
		}}, nil
	}
	return s.progressionStageStep(state)
}

// progressionStageStep selects the transition owned by the run stage once no
// invocation is executing.
func (s *Service) progressionStageStep(state progressionState) (progressionAction, *ProgressionResult) {
	run := *state.Run
	switch run.Stage {
	case store.StageClaim:
		return progressionAction{name: "run baseline", action: func(ctx context.Context) error {
			_, err := s.RunBaseline(ctx, BaselineRequest{RunID: run.ID})
			return err
		}}, nil
	case store.StagePreflight:
		return progressionAction{}, &ProgressionResult{Outcome: ProgressionWaiting, Reason: waitingReason(run, "baseline preflight needs human attention")}
	case store.StageCheck:
		return progressionAction{name: "create draft pull request", action: func(ctx context.Context) error {
			_, err := s.CreateDraftPullRequest(ctx, DraftPullRequestRequest{RunID: run.ID})
			return err
		}}, nil
	case store.StageDraftPR:
		return progressionAction{}, &ProgressionResult{Outcome: ProgressionDraftPullRequest, Reason: fmt.Sprintf("draft pull request #%d is ready for review", run.PullRequestNumber)}
	case store.StageReady:
		return progressionAction{}, &ProgressionResult{Outcome: ProgressionWaiting, Reason: waitingReason(run, "run is ready for human review")}
	}
	if _, declared := workflow.DefaultRegistry().RoleForRunStage(run.Stage); !declared {
		return progressionAction{}, &ProgressionResult{Outcome: ProgressionWaiting, Reason: fmt.Sprintf("no factory role owns run stage %q", run.Stage)}
	}
	if state.Latest != nil && state.Latest.Stage == run.Stage && state.Latest.Status == store.InvocationStatusCompleted {
		if run.Stage != store.StageImplementation {
			return progressionAction{}, &ProgressionResult{Outcome: ProgressionWaiting, Reason: fmt.Sprintf("stage %q has a completed invocation but no declared coordinator transition", run.Stage)}
		}
		return progressionAction{name: "create draft pull request", action: func(ctx context.Context) error {
			_, err := s.CreateDraftPullRequest(ctx, DraftPullRequestRequest{RunID: run.ID})
			return err
		}}, nil
	}
	return progressionAction{name: "start agent", action: func(ctx context.Context) error {
		_, err := s.StartAgent(ctx, AgentRequest{RunID: run.ID})
		return err
	}}, nil
}

// readProgressionState reads the run and its invocation projection once. A
// store without durable invocation history cannot support exactly-once
// unattended progression, so it reports no run instead of guessing.
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
	info, err := os.Stat(filepath.Join(invocation.ResultDirectory, report.ReportFileName))
	return err == nil && info.Mode().IsRegular()
}

// waitingReason prefers the coordinator's recorded lifecycle reason so the
// published status keeps the exact cause of the pause.
func waitingReason(run store.Run, fallback string) string {
	if run.LifecycleReason != "" {
		return run.LifecycleReason
	}
	return fallback
}

// pauseProgression stops automatic progression in a visible human waiting
// state and publishes an actionable status. A run that a seam already moved
// out of active is left untouched, because that seam owns the more specific
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
	next.LifecycleReason = fmt.Sprintf("automatic progression stopped at %s: %s", step, cause)
	next.UpdatedAt = s.deps.Now().UTC()
	if err := s.persistAgentRunState(ctx, registration, runStore, previous, next); err != nil {
		return previous, errors.Join(cause, fmt.Errorf("publish progression waiting state: %w", err))
	}
	return next, nil
}
