package factory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	// Step names the transition that stopped the pass. It is used in the
	// published waiting reason and defaults to an unexplained stall.
	Step string
}

// progressionState is one consistent read of the run and the invocation it
// currently delegates to.
type progressionState struct {
	// Run is the persisted non-terminal or terminal run, when one exists.
	Run *store.Run
	// Active is the invocation that may still produce a report.
	Active *store.Invocation
	// ActiveInvocations contains every invocation that may still produce a
	// report. Active remains populated with the first item for compatibility
	// with older progression tests and embedders.
	ActiveInvocations []*store.Invocation
	// Latest is the most recent invocation of any status.
	Latest *store.Invocation
	// Supported reports whether the operational store exposes the durable
	// invocation history that exactly-once progression requires.
	Supported bool
	// ReadyInvocationIDs records the report-readiness observation made while
	// reading each active invocation. The planner must not repeat that I/O.
	ReadyInvocationIDs map[string]bool
	// AgentTimeout is the parsed agent deadline from the frozen specification
	// packet. A zero value preserves the no-deadline behavior of legacy packets
	// that predate the required timeout field.
	AgentTimeout time.Duration
	// Now is the coordinator clock observation paired with AgentTimeout.
	Now time.Time
}

// progressionActionKind identifies one coordinator seam that can advance a
// run. The set is intentionally closed so dispatch cannot silently ignore a
// newly invented transition.
type progressionActionKind string

const (
	// progressionActionStartAgent launches the role selected for a run stage.
	progressionActionStartAgent progressionActionKind = "start_agent"
	// progressionActionRunBaseline evaluates the frozen pre-edit gate suite.
	progressionActionRunBaseline progressionActionKind = "run_baseline"
	// progressionActionAcceptAgentReport accepts one invocation's report.
	progressionActionAcceptAgentReport progressionActionKind = "accept_agent_report"
	// progressionActionCreateDraftPullRequest creates the run's draft pull request.
	progressionActionCreateDraftPullRequest progressionActionKind = "create_draft_pull_request"
	// progressionActionStartReviewRound launches the concurrent review roles.
	progressionActionStartReviewRound progressionActionKind = "start_review_round"
	// progressionActionRetryReviewReadiness retries final pull-request readiness.
	progressionActionRetryReviewReadiness progressionActionKind = "retry_review_readiness"
)

// progressionAction is a typed coordinator command selected from durable run
// state. It contains only the operands needed to dispatch an existing seam;
// it never captures a service, adapter, or context.
type progressionAction struct {
	// kind identifies the coordinator seam to dispatch.
	kind progressionActionKind
	// name is the operator-facing transition identity used in status text.
	name string
	// runID identifies the run that owns the transition.
	runID string
	// role optionally selects a specific role for an agent launch.
	role string
	// stage optionally selects a specific invocation stage for an agent launch.
	stage store.Stage
	// invocationID identifies the report to accept.
	invocationID string
}

// progressionActionError reports an invalid or unhandled typed command.
type progressionActionError struct {
	// Action is the command that could not be dispatched.
	Action progressionAction
	// Reason explains the invalid operand or unsupported kind.
	Reason string
}

// Error returns an actionable description of the invalid progression command.
func (e *progressionActionError) Error() string {
	if e == nil {
		return "invalid progression action"
	}
	if e.Action.kind == "" {
		return "invalid progression action: kind is required"
	}
	if e.Reason == "" {
		return fmt.Sprintf("invalid progression action %q", e.Action.kind)
	}
	return fmt.Sprintf("invalid progression action %q: %s", e.Action.kind, e.Reason)
}

// validateNoAgentOperands rejects operands that belong only to agent-launch
// or report-acceptance commands.
func (a progressionAction) validateNoAgentOperands() error {
	if a.role != "" || a.stage != "" || a.invocationID != "" {
		return &progressionActionError{Action: a, Reason: "unexpected role, stage, or invocation operands"}
	}
	return nil
}

// validate checks that a typed command has exactly the operands its dispatch
// arm accepts. A malformed command therefore cannot reach an adapter seam.
func (a progressionAction) validate() error {
	if strings.TrimSpace(a.name) == "" {
		return &progressionActionError{Action: a, Reason: "operator-facing name is required"}
	}
	if strings.TrimSpace(a.runID) == "" {
		return &progressionActionError{Action: a, Reason: "run id is required"}
	}

	switch a.kind {
	case progressionActionStartAgent:
		hasRole := strings.TrimSpace(a.role) != ""
		hasStage := strings.TrimSpace(string(a.stage)) != ""
		if (a.role != "" && !hasRole) || (a.stage != "" && !hasStage) {
			return &progressionActionError{Action: a, Reason: "agent role and stage cannot be blank"}
		}
		if hasRole != hasStage {
			return &progressionActionError{Action: a, Reason: "agent role and stage must be provided together"}
		}
		if a.invocationID != "" {
			return &progressionActionError{Action: a, Reason: "agent launch cannot include an invocation id"}
		}
	case progressionActionAcceptAgentReport:
		if strings.TrimSpace(a.invocationID) == "" {
			return &progressionActionError{Action: a, Reason: "invocation id is required"}
		}
		if a.role != "" || a.stage != "" {
			return &progressionActionError{Action: a, Reason: "report acceptance cannot include role or stage operands"}
		}
	case progressionActionRunBaseline, progressionActionCreateDraftPullRequest,
		progressionActionStartReviewRound, progressionActionRetryReviewReadiness:
		if err := a.validateNoAgentOperands(); err != nil {
			return err
		}
	default:
		return &progressionActionError{Action: a, Reason: "kind is not declared"}
	}
	return nil
}

// driveRun performs one bounded unattended progression pass for the registered
// repository. It drives the current run as far as the frozen route, the
// repository policy, and the durable evidence allow, and stops at the first
// state a person or infrastructure owns. Every transition runs through the
// same coordinator seam the matching one-shot command uses, so a repeated pass
// or a coordinator restart reuses their exactly-once effect journal instead of
// creating a second invocation, checkpoint, push, comment, or pull request.
func (s *Service) driveRun(ctx context.Context, registration config.RepositoryRegistration, eventSinks ...EventSink) (progressionResult, error) {
	events := coordinatorEventSink(eventSinks)
	result := progressionResult{Outcome: progressionIdle}
	lifecycle, err := s.observePullRequestEvents(ctx, registration)
	s.observeCoordinatorStageFromStore(ctx, registration, events)
	if err != nil {
		if pollingContextDone(err) || ctx.Err() != nil {
			return result, err
		}
		state, readErr := s.readProgressionState(ctx, registration)
		if readErr != nil || state.Run == nil {
			return result, errors.Join(err, readErr)
		}
		return s.stopProgressionAfterFailure(ctx, registration, *state.Run, "observe pull-request events", err, result.Steps)
	}
	if lifecycle.Outcome == LifecycleCompleted || lifecycle.Outcome == LifecycleCancelled {
		return progressionResult{Outcome: progressionTerminal, Run: lifecycle.Run, Reason: lifecycle.Reason}, nil
	}
	registry := workflow.DefaultRegistry()
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
		step, stop := progressionStep(state, registry)
		if stop != nil {
			stop.Steps = result.Steps
			return s.publishProgressionStop(ctx, registration, *state.Run, *stop)
		}
		if err := step.validate(); err != nil {
			return result, err
		}
		s.emitCoordinatorEvent(events, CoordinatorEvent{
			Kind:  EventProgressionStep,
			RunID: step.runID,
			Step:  step.name,
		})
		var stepErr error
		switch step.kind {
		case progressionActionStartAgent:
			_, stepErr = s.StartAgent(ctx, AgentRequest{RunID: step.runID, Role: step.role, Stage: step.stage})
		case progressionActionRunBaseline:
			_, stepErr = s.RunBaseline(ctx, BaselineRequest{RunID: step.runID})
		case progressionActionAcceptAgentReport:
			_, stepErr = s.AcceptAgentReport(ctx, AgentReportRequest{RunID: step.runID, InvocationID: step.invocationID})
		case progressionActionCreateDraftPullRequest:
			_, stepErr = s.CreateDraftPullRequest(ctx, DraftPullRequestRequest{RunID: step.runID})
		case progressionActionStartReviewRound:
			stepErr = s.startReviewRound(ctx, step.runID)
		case progressionActionRetryReviewReadiness:
			stepErr = s.retryReviewReadiness(ctx, step.runID)
		default:
			stepErr = &progressionActionError{Action: step, Reason: "kind is not declared for dispatch"}
		}
		if stepErr != nil {
			if after, readErr := s.readProgressionState(ctx, registration); readErr == nil && after.Run != nil {
				s.emitStageTransition(events, state.Run.ID, state.Run.Stage, after.Run.Stage)
			}
			return s.stopProgressionAfterFailure(ctx, registration, *state.Run, step.name, stepErr, result.Steps)
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
		s.emitStageTransition(events, state.Run.ID, state.Run.Stage, advanced.Run.Stage)
		if !progressionAdvancedMany(*state.Run, state.ActiveInvocations, *advanced.Run, advanced.ActiveInvocations) {
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

// observePullRequestEvents makes one bounded GitHub observation for a run that
// already has a tracked pull request, before the pass applies any coordinator
// transition. Pull-request lifecycle is observed first, because a merged or
// closed pull request ends the run and must not be followed by repair,
// readiness, or infrastructure work. A pending durable effect defers both
// observations to reconciliation, which owns the unresolved mutation.
func (s *Service) observePullRequestEvents(ctx context.Context, registration config.RepositoryRegistration) (LifecycleResult, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	opened, err := s.deps.OpenStore(ctx, registration.OperationalDataPath)
	if err != nil {
		return LifecycleResult{}, fmt.Errorf("open operational store to observe pull-request events: %w", err)
	}
	defer func() { _ = opened.Close() }()
	runStore, ok := opened.(RunStore)
	if !ok {
		return LifecycleResult{Outcome: LifecycleUnchanged}, nil
	}
	run, err := runStore.CurrentRun(ctx)
	if err != nil {
		return LifecycleResult{}, fmt.Errorf("read run to observe pull-request events: %w", err)
	}
	if run == nil || run.PullRequestNumber == 0 {
		return LifecycleResult{Outcome: LifecycleUnchanged}, nil
	}
	if journal, journaled := runStore.(PendingEffectStore); journaled {
		pending, pendingErr := journal.PendingEffect(ctx, run.ID)
		if pendingErr != nil {
			return LifecycleResult{}, fmt.Errorf("read pending effect before pull-request observation: %w", pendingErr)
		}
		if pending != nil {
			return LifecycleResult{Outcome: LifecycleUnchanged}, nil
		}
	}
	lifecycle, err := s.observeLifecycle(ctx, registration, runStore, run)
	if err != nil {
		return lifecycle, err
	}
	if lifecycle.Outcome != LifecycleUnchanged || store.IsTerminalStatus(lifecycle.Run.Status) {
		return lifecycle, nil
	}
	return lifecycle, s.consumeHumanReview(ctx, registration, runStore, lifecycle.Run)
}

// startReviewRound launches the specification and standards reviewers for the
// same frozen checkpoint. Launching is coordinator-serialized, while the two
// resulting harness sessions remain live concurrently in their own workers,
// homes, and result directories.
func (s *Service) startReviewRound(ctx context.Context, runID string) error {
	if strings.TrimSpace(runID) == "" {
		return errors.New("review round run id is required")
	}
	if _, err := s.StartAgent(ctx, AgentRequest{
		RunID: runID,
		Role:  workflow.RoleSpecificationReview,
		Stage: store.StageReview,
	}); err != nil {
		return fmt.Errorf("start specification reviewer: %w", err)
	}
	if _, err := s.StartAgent(ctx, AgentRequest{
		RunID: runID,
		Role:  workflow.RoleStandardsReview,
		Stage: workflow.StageStandardsReview,
	}); err != nil {
		return fmt.Errorf("start standards reviewer: %w", err)
	}
	return nil
}

// progressionStep selects the single legal next transition for a run, or the
// waiting state that stops unattended progression. Exactly one of the two
// return values is set. All observations needed by the decision are supplied
// in state and registry.
func progressionStep(state progressionState, registry workflow.Registry) (progressionAction, *progressionResult) {
	run := *state.Run
	if role, ok := missingConcurrentReviewRole(state, registry); ok {
		definition, declared := registry.Role(role)
		if !declared {
			return progressionAction{}, &progressionResult{
				Outcome: progressionWaiting,
				Reason:  fmt.Sprintf("workflow role %q is not declared by the workflow registry", role),
			}
		}
		return progressionAction{
			kind:  progressionActionStartAgent,
			name:  "start missing " + role + " review",
			runID: run.ID,
			role:  definition.Name,
			stage: definition.Stage,
		}, nil
	}
	if reviewReadinessSettled(run) {
		return progressionAction{}, &progressionResult{
			Outcome: progressionWaiting,
			Step:    "pull-request readiness",
			Reason:  fmt.Sprintf("pull request #%d is ready and waiting for human disposition", run.PullRequestNumber),
		}
	}
	if reviewReadinessEligible(run) {
		return progressionAction{
			kind:  progressionActionRetryReviewReadiness,
			name:  "finalize pull-request readiness",
			runID: run.ID,
		}, nil
	}
	switch RunActivityFor(run) {
	case ActivityTerminal:
		return progressionAction{}, &progressionResult{Outcome: progressionTerminal, Reason: "run is " + string(run.Status)}
	case ActivityWaitingForHuman:
		if !hasActiveReviewInvocations(state, registry) {
			return progressionAction{}, &progressionResult{Outcome: progressionWaiting, Reason: waitingReason(run, "waiting for human disposition")}
		}
	case ActivityWaitingForHarness:
		return progressionAction{}, &progressionResult{Outcome: progressionWaiting, Reason: waitingReason(run, "waiting for harness capacity")}
	}
	if run.CheckRepairPendingAttempt != 0 {
		return progressionAction{}, &progressionResult{Outcome: progressionWaiting, Reason: fmt.Sprintf("check-repair attempt %d is pending reconciliation", run.CheckRepairPendingAttempt)}
	}
	activeInvocations := state.ActiveInvocations
	if len(activeInvocations) == 0 && state.Active != nil {
		activeInvocations = []*store.Invocation{state.Active}
	}
	if len(activeInvocations) > 0 {
		for _, active := range activeInvocations {
			if active.AttachRequired {
				return progressionAction{}, &progressionResult{Outcome: progressionWaiting, Reason: fmt.Sprintf("invocation %q requires `factory attach`", active.ID)}
			}
		}
		for _, active := range activeInvocations {
			if !state.ReadyInvocationIDs[active.ID] {
				continue
			}
			return progressionAction{
				kind:         progressionActionAcceptAgentReport,
				name:         "accept agent report",
				runID:        run.ID,
				invocationID: active.ID,
			}, nil
		}
		for _, active := range activeInvocations {
			if elapsed, expired := agentInvocationExpired(*active, state.Now, state.AgentTimeout); expired {
				return progressionAction{}, &progressionResult{
					Outcome: progressionWaiting,
					Step:    "agent deadline",
					Reason:  fmt.Sprintf("invocation %q exceeded agent timeout of %s (elapsed %s)", active.ID, state.AgentTimeout, elapsed),
				}
			}
		}
		ids := make([]string, 0, len(activeInvocations))
		for _, active := range activeInvocations {
			ids = append(ids, active.ID)
		}
		return progressionAction{}, &progressionResult{Outcome: progressionInvocationActive, Reason: fmt.Sprintf("invocations %s are executing", strings.Join(ids, ", "))}
	}
	return progressionStageStep(state, registry)
}

// missingConcurrentReviewRole returns the one reviewer that can be resumed
// after a partial concurrent-review launch. It waits for any active reviewer
// and requires one exact-checkpoint result so a human clarification is never
// mistaken for a failed launch.
func missingConcurrentReviewRole(state progressionState, registry workflow.Registry) (string, bool) {
	if state.Run == nil || state.Run.Stage != store.StageReview || !concurrentReviewsConfigured(*state.Run) || hasActiveReviewInvocations(state, registry) {
		return "", false
	}
	run := *state.Run
	roles := []string{workflow.RoleSpecificationReview, workflow.RoleStandardsReview}
	hasCurrentResult := false
	missingRole := ""
	for _, role := range roles {
		result := reviewResultForRole(run, role)
		if result != nil && result.CheckpointSHA == run.CheckpointSHA {
			hasCurrentResult = true
			continue
		}
		if missingRole == "" {
			missingRole = role
		}
	}
	if hasCurrentResult && missingRole != "" {
		return missingRole, true
	}
	return "", false
}

// hasActiveReviewInvocations reports whether an otherwise human-waiting review
// round still has an independent result that can be serialized and applied.
func hasActiveReviewInvocations(state progressionState, registry workflow.Registry) bool {
	active := state.ActiveInvocations
	if len(active) == 0 && state.Active != nil {
		active = []*store.Invocation{state.Active}
	}
	for _, invocation := range active {
		if invocation == nil {
			continue
		}
		definition, declared := registry.Role(invocation.Role)
		if declared && definition.Stage == invocation.Stage && definition.Kind == workflow.RoleKindReview {
			return true
		}
	}
	return false
}

// progressionStageStep selects the transition the workflow registry declares
// for the run stage once no invocation is executing. The registry stays the
// stage authority: a stage that declares a coordinator event is driven by the
// draft-pull-request seam that owns those events, and a stage that declares a
// role is driven by launching that role.
func progressionStageStep(state progressionState, registry workflow.Registry) (progressionAction, *progressionResult) {
	run := *state.Run
	switch run.Stage {
	case store.StageClaim:
		// Leaving claim is the frozen baseline, whose healthy result selects
		// the entry stage from the route and the repository test policy.
		return progressionAction{
			kind:  progressionActionRunBaseline,
			name:  "run baseline",
			runID: run.ID,
		}, nil
	case store.StageDraftPR:
		if concurrentReviewsConfigured(run) {
			return progressionAction{
				kind:  progressionActionStartReviewRound,
				name:  "start concurrent reviews",
				runID: run.ID,
			}, nil
		}
		// Older packets without the standards role retain the historical
		// draft-pull-request stop until they are explicitly reviewed.
		return progressionAction{}, &progressionResult{Outcome: progressionDraftPullRequest, Reason: fmt.Sprintf("draft pull request #%d is ready for review", run.PullRequestNumber)}
	case store.StageReview:
		return progressionAction{}, &progressionResult{Outcome: progressionWaiting, Reason: "review round has no active invocation"}
	}
	definition, declared := registry.Stage(run.Stage)
	if !declared {
		return progressionAction{}, &progressionResult{Outcome: progressionWaiting, Reason: fmt.Sprintf("run stage %q is not declared by the workflow registry", run.Stage)}
	}
	_, roleDeclared := registry.RoleForRunStage(run.Stage)
	stageComplete := state.Latest != nil && state.Latest.Stage == run.Stage && state.Latest.Status == store.InvocationStatusCompleted
	if len(definition.Events) > 0 && (!roleDeclared || stageComplete) {
		return progressionAction{
			kind:  progressionActionCreateDraftPullRequest,
			name:  "create draft pull request",
			runID: run.ID,
		}, nil
	}
	if !roleDeclared {
		return progressionAction{}, &progressionResult{Outcome: progressionWaiting, Reason: fmt.Sprintf("no coordinator transition is declared for run stage %q", run.Stage)}
	}
	return progressionAction{
		kind:  progressionActionStartAgent,
		name:  "start agent",
		runID: run.ID,
	}, nil
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
	_, hasAllActive := opened.(ActiveInvocationsStore)
	latestStore, hasLatest := opened.(LatestInvocationStore)
	if (!hasActive && !hasAllActive) || !hasLatest {
		return progressionState{}, nil
	}
	state := progressionState{Run: run, Supported: true}
	if run == nil {
		return state, nil
	}
	if allActiveStore, ok := opened.(ActiveInvocationsStore); ok {
		active, activeErr := allActiveStore.ActiveInvocations(ctx, run.ID)
		if activeErr != nil {
			return progressionState{}, fmt.Errorf("read active invocations for progression: %w", activeErr)
		}
		state.ActiveInvocations = make([]*store.Invocation, 0, len(active))
		for index := range active {
			invocation := active[index]
			state.ActiveInvocations = append(state.ActiveInvocations, &invocation)
		}
		if len(state.ActiveInvocations) > 0 {
			state.Active = state.ActiveInvocations[0]
		}
	} else if state.Active, err = activeStore.ActiveInvocation(ctx, run.ID); err != nil {
		return progressionState{}, fmt.Errorf("read active invocation for progression: %w", err)
	} else if state.Active != nil {
		state.ActiveInvocations = []*store.Invocation{state.Active}
	}
	state.ReadyInvocationIDs = make(map[string]bool, len(state.ActiveInvocations))
	if len(state.ActiveInvocations) > 0 {
		state.AgentTimeout, err = agentTimeoutForProgression(*run)
		if err != nil {
			return progressionState{}, err
		}
		if state.AgentTimeout > 0 {
			state.Now = s.deps.Now().UTC()
		}
	}
	for _, invocation := range state.ActiveInvocations {
		if invocation == nil {
			continue
		}
		state.ReadyInvocationIDs[invocation.ID] = agentResultReady(*invocation)
	}
	if state.Latest, err = latestStore.LatestInvocation(ctx, run.ID); err != nil {
		return progressionState{}, fmt.Errorf("read latest invocation for progression: %w", err)
	}
	return state, nil
}

// agentTimeoutForProgression reads the agent deadline from the frozen
// specification packet. A missing value is retained as an unconfigured
// deadline for packets created before the timeout was consumed by progression.
func agentTimeoutForProgression(run store.Run) (time.Duration, error) {
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return 0, fmt.Errorf("decode specification packet for agent deadline: %w", err)
	}
	serialized := strings.TrimSpace(packet.RepositoryConfig.Timeouts.Agent)
	if serialized == "" {
		return 0, nil
	}
	timeout, err := time.ParseDuration(serialized)
	if err != nil || timeout <= 0 {
		if err == nil {
			err = errors.New("duration must be positive")
		}
		return 0, fmt.Errorf("parse frozen agent timeout %q: %w", serialized, err)
	}
	return timeout, nil
}

// agentInvocationExpired reports whether an active invocation has reached its
// frozen agent deadline and returns the observed age for operator-facing
// evidence. Missing timestamps, clocks, or deadlines cannot prove expiry.
func agentInvocationExpired(invocation store.Invocation, now time.Time, timeout time.Duration) (time.Duration, bool) {
	if timeout <= 0 || invocation.CreatedAt.IsZero() || now.IsZero() {
		return 0, false
	}
	elapsed := now.Sub(invocation.CreatedAt)
	if elapsed < timeout {
		return elapsed, false
	}
	return elapsed, true
}

// progressionAdvanced reports whether a transition changed durable run state.
// Without this check a seam that returns success without advancing the run
// would keep the coordinator repeating the same external operation.
func progressionAdvanced(previous store.Run, previousInvocation *store.Invocation, next store.Run, nextInvocation *store.Invocation) bool {
	return progressionAdvancedMany(previous, optionalInvocationList(previousInvocation), next, optionalInvocationList(nextInvocation))
}

// progressionAdvancedMany reports whether a transition changed durable run or
// any member of the concurrent invocation projection.
func progressionAdvancedMany(previous store.Run, previousInvocations []*store.Invocation, next store.Run, nextInvocations []*store.Invocation) bool {
	if previous.Stage != next.Stage || previous.Status != next.Status || previous.Revision != next.Revision {
		return true
	}
	if previous.CheckpointSHA != next.CheckpointSHA || previous.PullRequestNumber != next.PullRequestNumber {
		return true
	}
	if previous.ReadyNotificationSent != next.ReadyNotificationSent {
		return true
	}
	if len(previousInvocations) != len(nextInvocations) {
		return true
	}
	for index := range previousInvocations {
		if invocationIdentity(previousInvocations[index]) != invocationIdentity(nextInvocations[index]) {
			return true
		}
	}
	return false
}

// optionalInvocationList converts the compatibility single-invocation view
// used by older callers into the plural progression representation.
func optionalInvocationList(invocation *store.Invocation) []*store.Invocation {
	if invocation == nil {
		return nil
	}
	return []*store.Invocation{invocation}
}

// invocationIdentity renders the comparable identity of an optional
// invocation for progress detection.
func invocationIdentity(invocation *store.Invocation) string {
	if invocation == nil {
		return ""
	}
	return invocation.ID + "/" + string(invocation.Status)
}

// structuredReportPresence identifies whether report.json is absent, present,
// or impossible to inspect. A stat error other than a definitive not-found
// result must not be treated as proof that an invocation has no report.
type structuredReportPresence uint8

const (
	// structuredReportAbsent means the accepted report path definitively holds
	// no regular file.
	structuredReportAbsent structuredReportPresence = iota
	// structuredReportPresent means a regular report file exists at the only
	// accepted path.
	structuredReportPresent
	// structuredReportIndeterminate means the filesystem could not answer, so
	// the observation proves nothing about the report.
	structuredReportIndeterminate
)

// structuredReportPresenceForInvocation inspects the only accepted report
// path without interpreting an unreadable parent or filesystem as absence.
func structuredReportPresenceForInvocation(invocation store.Invocation) (structuredReportPresence, error) {
	if invocation.ResultDirectory == "" {
		return structuredReportAbsent, nil
	}
	info, err := os.Stat(reportPath(invocation))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return structuredReportAbsent, nil
		}
		return structuredReportIndeterminate, err
	}
	if !info.Mode().IsRegular() {
		return structuredReportAbsent, nil
	}
	return structuredReportPresent, nil
}

// agentResultReady reports whether the invocation has written its structured
// report. `factory-report` renames the file into place, so its presence is the
// durable signal that the coordinator may validate a completed result. An
// indeterminate filesystem observation remains not-ready for progression.
func agentResultReady(invocation store.Invocation) bool {
	presence, err := structuredReportPresenceForInvocation(invocation)
	return err == nil && presence == structuredReportPresent
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
	step := stop.Step
	if step == "" {
		step = "stall"
	}
	paused, err := s.pauseProgression(ctx, registration, run, step, errors.New(stop.Reason))
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
