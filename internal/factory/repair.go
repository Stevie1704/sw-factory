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
	"github.com/Stevie1704/sw-factory/internal/gate"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/prompt"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

const (
	// checkRepairPacketVersion identifies the JSON shape mounted for one repair.
	checkRepairPacketVersion = 1
	// maxRepairDiagnosticRunes prevents command output from turning a repair
	// packet into an unbounded prompt or artifact.
	maxRepairDiagnosticRunes = 8000
)

// CheckRepairPacket is the complete coordinator-owned context for one
// deterministic checkpoint failure. It is mounted in the next implementation
// invocation and may contain command diagnostics because it is an invocation
// artifact, not part of the content-free evaluation summary.
type CheckRepairPacket struct {
	// Version identifies this packet shape.
	Version int `json:"version"`
	// RunID identifies the factory run that owns the failed checkpoint.
	RunID string `json:"run_id"`
	// CheckpointSHA identifies the exact checkpoint whose gates failed.
	CheckpointSHA string `json:"checkpoint_sha"`
	// Attempt is the one-based repair attempt being launched.
	Attempt int `json:"attempt"`
	// Budget is the frozen maximum number of repair attempts.
	Budget int `json:"budget"`
	// Setup records the shared setup result for this gate suite.
	Setup CheckRepairCommand `json:"setup"`
	// Gates retains every declared gate result, including passed and skipped
	// gates, so one packet represents the complete suite observation.
	Gates []CheckRepairGate `json:"gates"`
}

// CheckRepairCommand is a bounded deterministic command observation included
// in a repair packet.
type CheckRepairCommand struct {
	// ExitCode is the process exit code returned by the worker.
	ExitCode int `json:"exit_code"`
	// Stdout is bounded command output useful for repairing the failure.
	Stdout string `json:"stdout,omitempty"`
	// Stderr is bounded command output useful for repairing the failure.
	Stderr string `json:"stderr,omitempty"`
}

// CheckRepairGate is one content-bearing gate observation in a repair packet.
type CheckRepairGate struct {
	// CheckpointSHA identifies the exact evaluated commit.
	CheckpointSHA string `json:"checkpoint_sha"`
	// GateName identifies the frozen declared gate.
	GateName string `json:"gate_name"`
	// Outcome is the coordinator-owned gate result vocabulary.
	Outcome gate.Outcome `json:"outcome"`
	// Blocking records whether the gate blocks the suite.
	Blocking bool `json:"blocking"`
	// Status is the exact GitHub status state published for the checkpoint.
	Status github.CommitStatusState `json:"status"`
	// Command is the deterministic gate command observation.
	Command CheckRepairCommand `json:"command"`
	// Skipped reports that the command was not executed.
	Skipped bool `json:"skipped"`
	// SkipReason explains a dependency or setup skip.
	SkipReason string `json:"skip_reason,omitempty"`
	// TimedOut distinguishes a typed gate timeout from a runtime failure.
	TimedOut bool `json:"timed_out"`
}

// CheckRepairOutcome identifies the coordinator action taken after a failed
// checkpoint suite.
type CheckRepairOutcome string

const (
	// CheckRepairStarted means a native-resumed implementation session is active.
	CheckRepairStarted CheckRepairOutcome = "started"
	// CheckRepairWaitingForHarness means infrastructure must recover before the
	// same checkpoint evaluation or repair launch can continue.
	CheckRepairWaitingForHarness CheckRepairOutcome = "waiting_for_harness"
	// CheckRepairWaitingForHuman means the coordinator cannot safely continue.
	CheckRepairWaitingForHuman CheckRepairOutcome = "waiting_for_human"
	// CheckRepairExhausted means the configured repair ceiling was reached.
	CheckRepairExhausted CheckRepairOutcome = "exhausted"
)

// CheckRepairResult reports a check-repair decision and the resulting run.
type CheckRepairResult struct {
	// Outcome identifies the bounded workflow decision.
	Outcome CheckRepairOutcome
	// Run is the persisted run after the decision.
	Run store.Run
	// Packet is present when a native-resumed repair was launched.
	Packet CheckRepairPacket
	// Invocation is the new active repair invocation when one was launched.
	Invocation store.Invocation
	// Attempt is the current one-based consumed attempt count.
	Attempt int
	// Budget is the configured repair ceiling.
	Budget int
	// Remaining is the number of attempts still available after the decision.
	Remaining int
}

// checkRepairFailureKind is the coordinator's narrow classification seam for
// deciding whether a gate result can safely consume a repair attempt.
type checkRepairFailureKind string

const (
	// checkRepairDeterministicFailure identifies a typed gate observation that
	// is safe to send back to the implementation session.
	checkRepairDeterministicFailure checkRepairFailureKind = "deterministic_failure"
	// checkRepairInfrastructureFailure identifies an execution or transport
	// failure that must wait without consuming a repair attempt.
	checkRepairInfrastructureFailure checkRepairFailureKind = "infrastructure_failure"
)

// checkRepairDecisionKind identifies the state-machine action after a suite.
type checkRepairDecisionKind string

const (
	// checkRepairStartDecision launches the next bounded native repair.
	checkRepairStartDecision checkRepairDecisionKind = "start"
	// checkRepairWaitDecision pauses until the harness can be retried.
	checkRepairWaitDecision checkRepairDecisionKind = "wait"
	// checkRepairExhaustDecision escalates after the frozen repair ceiling.
	checkRepairExhaustDecision checkRepairDecisionKind = "exhaust"
)

// checkRepairDecision contains the validated next state at the workflow seam.
type checkRepairDecision struct {
	kind            checkRepairDecisionKind
	nextStage       store.Stage
	nextStatus      store.Status
	consumesAttempt bool
	attempt         int
	remaining       int
}

// ErrCheckRepairSessionUnavailable identifies a missing native session or
// visible surface that requires human recovery instead of a blind retry.
var ErrCheckRepairSessionUnavailable = errors.New("implementation session is unavailable for native check repair")

// decideCheckRepair is the table-tested workflow decision boundary for the
// check-repair loop. It admits only an active check stage and never consumes a
// budget for infrastructure failures.
func decideCheckRepair(run store.Run, kind checkRepairFailureKind) (checkRepairDecision, error) {
	if run.Stage != store.StageCheck {
		return checkRepairDecision{}, fmt.Errorf("check repair requires check stage, not %q", run.Stage)
	}
	if run.Status != store.StatusActive {
		return checkRepairDecision{}, fmt.Errorf("check repair requires active status, not %q", run.Status)
	}
	if run.CheckRepairPendingAttempt != 0 {
		return checkRepairDecision{}, fmt.Errorf("check repair attempt %d is pending reconciliation", run.CheckRepairPendingAttempt)
	}
	if run.CheckRepairBudget <= 0 {
		return checkRepairDecision{}, errors.New("check repair budget must be positive")
	}
	if run.CheckRepairAttempts < 0 || run.CheckRepairAttempts > run.CheckRepairBudget {
		return checkRepairDecision{}, fmt.Errorf("check repair attempts %d are outside budget %d", run.CheckRepairAttempts, run.CheckRepairBudget)
	}
	switch kind {
	case checkRepairInfrastructureFailure:
		return checkRepairDecision{
			kind:       checkRepairWaitDecision,
			nextStage:  store.StageCheck,
			nextStatus: store.StatusWaitingForHarness,
			attempt:    run.CheckRepairAttempts,
			remaining:  remainingCheckRepairBudget(run.CheckRepairAttempts, run.CheckRepairBudget),
		}, nil
	case checkRepairDeterministicFailure:
		if run.CheckRepairAttempts >= run.CheckRepairBudget {
			return checkRepairDecision{
				kind:       checkRepairExhaustDecision,
				nextStage:  store.StageCheck,
				nextStatus: store.StatusWaitingForHuman,
				attempt:    run.CheckRepairAttempts,
				remaining:  0,
			}, nil
		}
		attempt := run.CheckRepairAttempts + 1
		return checkRepairDecision{
			kind:            checkRepairStartDecision,
			nextStage:       store.StageImplementation,
			nextStatus:      store.StatusActive,
			consumesAttempt: true,
			attempt:         attempt,
			remaining:       remainingCheckRepairBudget(attempt, run.CheckRepairBudget),
		}, nil
	default:
		return checkRepairDecision{}, fmt.Errorf("unsupported check repair failure kind %q", kind)
	}
}

// remainingCheckRepairBudget computes a nonnegative operator-visible budget.
func remainingCheckRepairBudget(attempts, budget int) int {
	remaining := budget - attempts
	if remaining < 0 {
		return 0
	}
	return remaining
}

// isRepairableCheckFailure accepts only typed deterministic gate failures. A
// setup failure, runtime error, dependency-only failure, or status-publication
// error must wait without consuming the repair budget.
func isRepairableCheckFailure(err error) bool {
	suiteFailure, ok := oneError(err, func(candidate error) (*gate.SuiteFailure, bool) {
		failure, ok := candidate.(*gate.SuiteFailure)
		return failure, ok && failure != nil
	})
	if !ok {
		return false
	}
	hasGateFailure := false
	for _, failure := range suiteFailure.Failures {
		var gateFailure *gate.GateFailure
		if errors.As(failure, &gateFailure) {
			if gateFailure.Cause != nil && !gateFailure.TimedOut {
				return false
			}
			hasGateFailure = true
			continue
		}
		var dependencyFailure *gate.DependencyFailure
		if errors.As(failure, &dependencyFailure) {
			continue
		}
		return false
	}
	return hasGateFailure
}

// oneError unwraps a single causal chain and rejects joined errors with more
// than one top-level cause, so a persistence or transport error cannot be
// mistaken for a deterministic gate failure.
func oneError[T any](err error, match func(error) (T, bool)) (T, bool) {
	var zero T
	for err != nil {
		if value, ok := match(err); ok {
			return value, true
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			causes := joined.Unwrap()
			if len(causes) != 1 {
				return zero, false
			}
			err = causes[0]
			continue
		}
		unwrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return zero, false
		}
		err = unwrapped.Unwrap()
	}
	return zero, false
}

// buildCheckRepairPacket assembles every result from one exact checkpoint into
// a single repair packet, preserving independent failures and skipped reasons.
func buildCheckRepairPacket(run store.Run, results []gate.Result, suiteErr error, attempt, budget int) CheckRepairPacket {
	timedOut := make(map[string]bool)
	var suiteFailure *gate.SuiteFailure
	if errors.As(suiteErr, &suiteFailure) {
		for _, failure := range suiteFailure.Failures {
			var gateFailure *gate.GateFailure
			if errors.As(failure, &gateFailure) && gateFailure.TimedOut {
				timedOut[gateFailure.Name] = true
			}
		}
	}
	packet := CheckRepairPacket{
		Version:       checkRepairPacketVersion,
		RunID:         run.ID,
		CheckpointSHA: run.CheckpointSHA,
		Attempt:       attempt,
		Budget:        budget,
		Gates:         make([]CheckRepairGate, 0, len(results)),
	}
	if len(results) > 0 {
		packet.Setup = CheckRepairCommand{
			ExitCode: results[0].Setup.ExitCode,
			Stdout:   boundedRepairDiagnostic(results[0].Setup.Stdout),
			Stderr:   boundedRepairDiagnostic(results[0].Setup.Stderr),
		}
	}
	for _, result := range results {
		packet.Gates = append(packet.Gates, CheckRepairGate{
			CheckpointSHA: result.CheckpointSHA,
			GateName:      result.GateName,
			Outcome:       result.Outcome,
			Blocking:      result.Blocking,
			Status:        result.Status.State,
			Command: CheckRepairCommand{
				ExitCode: result.Gate.ExitCode,
				Stdout:   boundedRepairDiagnostic(result.Gate.Stdout),
				Stderr:   boundedRepairDiagnostic(result.Gate.Stderr),
			},
			Skipped:    result.Skipped,
			SkipReason: result.SkipReason,
			TimedOut:   timedOut[result.GateName],
		})
	}
	return packet
}

// boundedRepairDiagnostic keeps packet diagnostics useful without retaining
// unbounded command output in a worker artifact.
func boundedRepairDiagnostic(value string) string {
	runes := []rune(value)
	if len(runes) <= maxRepairDiagnosticRunes {
		return value
	}
	return string(runes[:maxRepairDiagnosticRunes]) + "\n[truncated]"
}

// LatestInvocationStore is the optional store projection needed to recover
// the last implementation session for a native check repair.
type LatestInvocationStore interface {
	LatestInvocation(context.Context, string) (*store.Invocation, error)
}

// routeCheckRepair applies one gate-suite decision and, when legal, launches a
// new invocation that resumes the last implementation session natively.
func (s *Service) routeCheckRepair(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, packet SpecificationPacket, results []gate.Result, suiteErr error) (CheckRepairResult, error) {
	if run.CheckRepairBudget <= 0 {
		run.CheckRepairBudget = packet.RepositoryConfig.RetryLimits.CheckRepair
	}
	decisionKind := checkRepairInfrastructureFailure
	if isRepairableCheckFailure(suiteErr) {
		decisionKind = checkRepairDeterministicFailure
	}
	decision, err := decideCheckRepair(run, decisionKind)
	if err != nil {
		return CheckRepairResult{Run: run, Attempt: run.CheckRepairAttempts, Budget: run.CheckRepairBudget, Remaining: remainingCheckRepairBudget(run.CheckRepairAttempts, run.CheckRepairBudget)}, err
	}
	baseResult := CheckRepairResult{
		Run:       run,
		Attempt:   decision.attempt,
		Budget:    run.CheckRepairBudget,
		Remaining: decision.remaining,
	}
	switch decision.kind {
	case checkRepairWaitDecision:
		next := run
		next.CheckRepairBudget = run.CheckRepairBudget
		next.Stage = decision.nextStage
		next.Status = decision.nextStatus
		next.LifecycleReason = "check repair waiting for infrastructure"
		next.UpdatedAt = s.deps.Now().UTC()
		stopErr := s.stopCheckWorker(ctx, run.ID)
		transitionErr := s.persistAgentRunState(ctx, registration, runStore, run, next)
		updated := next
		baseResult.Outcome = CheckRepairWaitingForHarness
		baseResult.Run = updated
		baseResult.Attempt = updated.CheckRepairAttempts
		baseResult.Remaining = remainingCheckRepairBudget(updated.CheckRepairAttempts, updated.CheckRepairBudget)
		return baseResult, errors.Join(stopErr, transitionErr)
	case checkRepairExhaustDecision:
		next := run
		next.CheckRepairBudget = run.CheckRepairBudget
		next.Stage = decision.nextStage
		next.Status = decision.nextStatus
		next.LifecycleReason = "check-repair budget exhausted"
		next.UpdatedAt = s.deps.Now().UTC()
		stopErr := s.stopCheckWorker(ctx, run.ID)
		transitionErr := s.persistAgentRunState(ctx, registration, runStore, run, next)
		updated := next
		if recorder, ok := runStore.(evaluationRecorder); ok && transitionErr == nil {
			transitionErr = errors.Join(transitionErr, recorder.RecordEvaluationBudgetExhaustion(ctx, run.ID))
			transitionErr = errors.Join(transitionErr, recorder.RecordEvaluationEscalation(ctx, run.ID, store.EvaluationEscalationBudget))
		}
		baseResult.Outcome = CheckRepairExhausted
		baseResult.Run = updated
		baseResult.Attempt = updated.CheckRepairAttempts
		baseResult.Remaining = 0
		return baseResult, errors.Join(stopErr, transitionErr)
	case checkRepairStartDecision:
		repairPacket := buildCheckRepairPacket(run, results, suiteErr, decision.attempt, run.CheckRepairBudget)
		invocation, updated, launchErr := s.startCheckRepair(ctx, registration, runStore, run, packet, repairPacket)
		if launchErr == nil {
			baseResult.Outcome = CheckRepairStarted
			baseResult.Run = updated
			baseResult.Packet = repairPacket
			baseResult.Invocation = invocation
			baseResult.Attempt = updated.CheckRepairAttempts
			baseResult.Remaining = remainingCheckRepairBudget(updated.CheckRepairAttempts, updated.CheckRepairBudget)
			return baseResult, nil
		}
		if journal, journaled := runStore.(PendingEffectStore); journaled {
			pending, pendingErr := journal.PendingEffect(ctx, run.ID)
			if pendingErr != nil {
				return baseResult, errors.Join(launchErr, fmt.Errorf("inspect pending check-repair effect: %w", pendingErr))
			}
			if pending != nil {
				// The failed launch has a durable external intent. Do not issue a
				// competing stop or state transition; startup reconciliation owns
				// the retry and the one-effect invariant.
				baseResult.Run = updated
				baseResult.Attempt = updated.CheckRepairAttempts
				baseResult.Remaining = remainingCheckRepairBudget(updated.CheckRepairAttempts, updated.CheckRepairBudget)
				return baseResult, launchErr
			}
		}
		next := updated
		next.CheckRepairBudget = run.CheckRepairBudget
		if errors.Is(launchErr, ErrCheckRepairSessionUnavailable) {
			next.Status = store.StatusWaitingForHuman
			next.LifecycleReason = "implementation session unavailable for check repair"
			baseResult.Outcome = CheckRepairWaitingForHuman
		} else {
			next.Status = store.StatusWaitingForHarness
			next.LifecycleReason = "check repair harness unavailable"
			baseResult.Outcome = CheckRepairWaitingForHarness
		}
		next.Stage = store.StageCheck
		next.UpdatedAt = s.deps.Now().UTC()
		stopErr := s.stopCheckWorker(ctx, run.ID)
		transitionErr := s.persistAgentRunState(ctx, registration, runStore, updated, next)
		updated = next
		baseResult.Run = updated
		baseResult.Attempt = updated.CheckRepairAttempts
		baseResult.Remaining = remainingCheckRepairBudget(updated.CheckRepairAttempts, updated.CheckRepairBudget)
		return baseResult, errors.Join(launchErr, stopErr, transitionErr)
	default:
		return baseResult, fmt.Errorf("unsupported check repair decision %q", decision.kind)
	}
}

// stopCheckWorker releases the gate worker before a run waits for harness or
// human action, while keeping the role volumes available for later resume.
func (s *Service) stopCheckWorker(ctx context.Context, runID string) error {
	if s.deps.Worker == nil {
		return nil
	}
	if err := s.deps.Worker.Stop(ctx, runID); err != nil {
		return fmt.Errorf("stop check worker: %w", err)
	}
	return nil
}

// startCheckRepair persists a fresh invocation packet, refreshes the pinned
// worker mounts, and resumes the previous implementation's native session.
// The run counter is durably reserved immediately before native resume and is
// rolled back when resume has not started, so failed infrastructure launches
// do not consume budget while a successful launch cannot be undercounted. The
// active invocation is durable before any effect and the startup guard fails
// closed on interruption, preventing a second launch before reconciliation.
func (s *Service) startCheckRepair(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, packet SpecificationPacket, repairPacket CheckRepairPacket) (invocation store.Invocation, updated store.Run, returnErr error) {
	invocationStore, ok := runStore.(InvocationStore)
	if !ok {
		return store.Invocation{}, run, errors.New("operational store does not support visible invocations")
	}
	historyStore, ok := runStore.(LatestInvocationStore)
	if !ok {
		return store.Invocation{}, run, errors.New("operational store does not support latest invocation lookup")
	}
	previous, err := historyStore.LatestInvocation(ctx, run.ID)
	if err != nil {
		return store.Invocation{}, run, fmt.Errorf("read implementation session for check repair: %w", err)
	}
	if previous == nil || previous.Stage != store.StageImplementation || strings.TrimSpace(previous.NativeSessionID) == "" {
		return store.Invocation{}, run, fmt.Errorf("%w: latest implementation invocation has no resumable native session", ErrCheckRepairSessionUnavailable)
	}
	roleDefinition, roleErr := roleDefinitionForInvocation(*previous)
	if roleErr != nil || roleDefinition.Name != workflow.RoleImplementation {
		return store.Invocation{}, run, fmt.Errorf("%w: latest implementation invocation has no resumable role identity", ErrCheckRepairSessionUnavailable)
	}
	if previous.WorkspaceID == "" || invocationSurface(*previous).ID == "" {
		return store.Invocation{}, run, fmt.Errorf("%w: latest implementation invocation has no recoverable surface", ErrCheckRepairSessionUnavailable)
	}
	terminalRuntime, harnessRuntime, err := s.ensureAgentRuntime(registration.Cmux.SocketPath, config.Harness(previous.Harness))
	if err != nil {
		return store.Invocation{}, run, fmt.Errorf("%w: %v", ErrCheckRepairSessionUnavailable, err)
	}
	// A native session belongs to the harness that created it, so a repair
	// continues in that harness or not at all. This is what makes mid-session
	// migration between Codex and Claude impossible.
	capabilities := harnessRuntime.Capabilities()
	if capabilities.Name != previous.Harness {
		return store.Invocation{}, run, fmt.Errorf("%w: harness %q cannot resume a %q session", ErrCheckRepairSessionUnavailable, capabilities.Name, previous.Harness)
	}
	seedCredentials, configuredCredentialStoreID, credentialErr := s.credentialSeeding(registration, AgentRequest{}, config.Harness(previous.Harness))
	if credentialErr != nil || (strings.TrimSpace(previous.CredentialStoreID) != "" && seedCredentials == nil) {
		return store.Invocation{}, run, newCredentialProjectionError(previous.Harness)
	}
	identifier, err := s.deps.NewRunID()
	if err != nil {
		return store.Invocation{}, run, fmt.Errorf("generate check-repair invocation identifier: %w", err)
	}
	invocationID := "inv-" + identifier
	createdAt := s.deps.Now().UTC()
	root := invocationRoot(run, invocationID)
	packetDirectory := filepath.Join(root, "packet")
	resultDirectory := filepath.Join(root, "results")
	if err := os.MkdirAll(packetDirectory, 0o700); err != nil {
		return store.Invocation{}, run, fmt.Errorf("create check-repair packet directory: %w", err)
	}
	if err := os.MkdirAll(resultDirectory, 0o700); err != nil {
		return store.Invocation{}, run, fmt.Errorf("create check-repair result directory: %w", err)
	}
	promptVersion := previous.PromptVersion
	if strings.TrimSpace(promptVersion) == "" {
		promptVersion = prompt.Version
	}
	if err := validateRepositoryCraftPacket(packet, invocationID); err != nil {
		return store.Invocation{}, run, err
	}
	repositoryCraft, err := frozenRepositoryCraftForRole(packet, previous.Role, invocationID)
	if err != nil {
		return store.Invocation{}, run, err
	}
	craftSourcePath, craftSHA256 := craftMetadataForInvocation(repositoryCraft)
	repairSpecification, err := promptSpecification(packet)
	if err != nil {
		return store.Invocation{}, run, err
	}
	promptText, err := prompt.Build(prompt.Request{
		InvocationID:        invocationID,
		RunID:               run.ID,
		Role:                previous.Role,
		Stage:               string(store.StageImplementation),
		SpecificationPacket: repairSpecification,
		// A check repair only exists as a resumed session, refused above when
		// the previous invocation has no native session to continue.
		Continuation:       true,
		RepositoryGuidance: packet.RepositoryGuidance,
		RepositoryCraft:    repositoryCraftContent(repositoryCraft),
		PromptVersion:      promptVersion,
		TestPolicyMode:     string(packet.RepositoryConfig.TestPolicy.Mode),
		Route:              packet.Route,
		CheckRepairAttempt: repairPacket.Attempt,
		CheckRepairBudget:  repairPacket.Budget,
	})
	if err != nil {
		return store.Invocation{}, run, err
	}
	if err := writeInvocationPacket(packetDirectory, InvocationPacket{
		SchemaVersion:         invocationPacketVersion,
		InvocationID:          invocationID,
		RunID:                 run.ID,
		Role:                  previous.Role,
		Stage:                 store.StageImplementation,
		SpecificationPacket:   run.SpecificationPacket,
		PromptVersion:         promptVersion,
		PromptCraftSourcePath: craftSourcePath,
		PromptCraftSHA256:     craftSHA256,
		TestPolicyMode:        packet.RepositoryConfig.TestPolicy.Mode,
		Route:                 packet.Route,
		PermittedPaths:        append([]string(nil), previous.PermittedPaths...),
		CheckRepair:           &repairPacket,
		Continuation:          true,
	}); err != nil {
		return store.Invocation{}, run, err
	}
	credentialStoreID := previous.CredentialStoreID
	if credentialStoreID == "" {
		credentialStoreID = configuredCredentialStoreID
	}
	invocation = store.Invocation{
		ID:                      invocationID,
		RunID:                   run.ID,
		Harness:                 previous.Harness,
		Role:                    previous.Role,
		Stage:                   store.StageImplementation,
		Model:                   previous.Model,
		ReasoningEffort:         previous.ReasoningEffort,
		CredentialStoreID:       credentialStoreID,
		NativeSessionID:         previous.NativeSessionID,
		WorkspaceID:             previous.WorkspaceID,
		StatusSurfaceID:         previous.StatusSurfaceID,
		RoleSurfaceID:           string(invocationSurface(*previous).ID),
		ImplementationSurfaceID: previous.ImplementationSurfaceID,
		ChecksSurfaceID:         previous.ChecksSurfaceID,
		InvocationDirectory:     packetDirectory,
		ResultDirectory:         resultDirectory,
		PermittedPaths:          append([]string(nil), previous.PermittedPaths...),
		PromptVersion:           promptVersion,
		PromptCraftSourcePath:   craftSourcePath,
		PromptCraftSHA256:       craftSHA256,
		Status:                  store.InvocationStatusActive,
		CreatedAt:               createdAt,
		UpdatedAt:               createdAt,
	}
	persisted := false
	workerStarted := false
	attemptReserved := false
	resumeStarted := false
	_, journaled := runStore.(PendingEffectStore)
	reservedRun := run
	persistedInvocation := invocation
	defer func() {
		if returnErr == nil || !persisted {
			return
		}
		rollbackContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if workerStarted && !(journaled && resumeStarted) {
			_ = s.deps.Worker.Stop(rollbackContext, run.ID)
		}
		if journaled && resumeStarted {
			// A journaled resume crossed an external boundary. Leave its active
			// worker and invocation available for the pending state/resume
			// effect to reconcile instead of declaring a successful session
			// unusable during error cleanup.
			updated = reservedRun
			return
		}
		if attemptReserved && !resumeStarted {
			rollbackErr := s.persistAgentRunState(rollbackContext, registration, runStore, reservedRun, run)
			if rollbackErr != nil {
				// A failed rollback is fail-closed: retain the reserved count in
				// the returned state so a later retry cannot exceed the ceiling.
				updated = reservedRun
				returnErr = errors.Join(returnErr, fmt.Errorf("rollback reserved check-repair attempt: %w", rollbackErr))
			} else {
				updated = run
			}
		} else if attemptReserved {
			// Native resume succeeded, but the consumed counter remains pending
			// until the final run-state write succeeds. Reconciliation can use
			// this marker after a process interruption.
			updated = reservedRun
		}
		cleanupInvocation := invocation
		if cleanupInvocation.ID == "" {
			cleanupInvocation = persistedInvocation
		}
		cleanupInvocation.Status = store.InvocationStatusCannotProceed
		cleanupInvocation.UpdatedAt = s.deps.Now().UTC()
		_ = invocationStore.SaveInvocation(rollbackContext, cleanupInvocation)
	}()
	if err := invocationStore.SaveInvocation(ctx, invocation); err != nil {
		return store.Invocation{}, run, fmt.Errorf("persist check-repair invocation: %w", err)
	}
	persisted = true
	persistedInvocation = invocation
	if recorder, ok := runStore.(evaluationRecorder); ok {
		if err := recorder.EnsureEvaluationSummary(ctx, run); err != nil {
			return store.Invocation{}, run, fmt.Errorf("ensure local evaluation summary for check repair: %w", err)
		}
		if err := recorder.RecordEvaluationInvocation(ctx, run.ID, invocation, run.ImageDigest, report.SchemaVersion); err != nil {
			return store.Invocation{}, run, fmt.Errorf("record local evaluation invocation for check repair: %w", err)
		}
	}
	control, err := terminalRuntime.EnsureControlWorkspace(ctx, terminal.WorkspaceRequest{
		Name:             defaultString(registration.Cmux.ControlWorkspace, "factory-control"),
		Description:      "software factory coordinator",
		WorkingDirectory: registration.Path,
	})
	if err != nil {
		return store.Invocation{}, run, fmt.Errorf("ensure check-repair control workspace: %w", err)
	}
	if err := terminalRuntime.Notify(ctx, terminal.Notification{
		WorkspaceID: control.ID,
		Title:       "factory check repair started",
		Body:        fmt.Sprintf("%s implementation repair attempt %d/%d resumed", run.ID, repairPacket.Attempt, repairPacket.Budget),
	}); err != nil {
		return store.Invocation{}, run, fmt.Errorf("notify check-repair start: %w", err)
	}
	gitMetadataPath, err := prepareGitMetadataProjection(run.ID, registration.Path, run.Worktree)
	if err != nil {
		return store.Invocation{}, run, fmt.Errorf("prepare check-repair Git metadata: %w", err)
	}
	workerRequest := worker.StartRequest{
		RunID:             run.ID,
		WorktreePath:      run.Worktree,
		GitMetadataPath:   gitMetadataPath,
		Image:             packet.RepositoryConfig.WorkerBuild.Image,
		ImageDigest:       run.ImageDigest,
		Caches:            workerCaches(packet.RepositoryConfig.Caches),
		InvocationPath:    packetDirectory,
		ResultPath:        resultDirectory,
		CredentialStoreID: credentialStoreID,
		Role:              previous.Role,
	}
	if err := s.startWorkerWithEffect(ctx, runStore, workerRequest); err != nil {
		return store.Invocation{}, run, fmt.Errorf("start worker for check repair: %w", err)
	}
	workerStarted = true
	if seedCredentials != nil {
		if err := seedCredentials(ctx, run.ID, workerIDForInvocation(invocation)); err != nil {
			return store.Invocation{}, run, newCredentialProjectionError(previous.Harness)
		}
	}
	next := run
	next.Stage = store.StageImplementation
	next.Status = store.StatusActive
	addActiveInvocation(&next, invocation.ID)
	next.CheckRepairBudget = repairPacket.Budget
	next.CheckRepairPendingAttempt = repairPacket.Attempt
	next.LifecycleReason = fmt.Sprintf("check-repair attempt %d active", repairPacket.Attempt)
	next.UpdatedAt = s.deps.Now().UTC()
	if err := s.persistAgentRunState(ctx, registration, runStore, run, next); err != nil {
		return store.Invocation{}, run, fmt.Errorf("persist check-repair attempt reservation: %w", err)
	}
	reservedRun = next
	attemptReserved = true
	updated = reservedRun
	resumeRequest := harness.StartRequest{
		InvocationID:    invocation.ID,
		RunID:           run.ID,
		Role:            previous.Role,
		Stage:           string(store.StageImplementation),
		WorkspaceID:     terminal.WorkspaceID(previous.WorkspaceID),
		Surface:         invocationSurface(*previous),
		Prompt:          promptText,
		Model:           previous.Model,
		ReasoningEffort: previous.ReasoningEffort,
		ResumeSessionID: previous.NativeSessionID,
	}
	resumedInvocation, resumeErr := s.resumeHarnessWithEffect(ctx, runStore, invocationStore, registration.Cmux.SocketPath, harnessRuntime, invocation, resumeRequest)
	if resumedInvocation.RecoveryResumeCount > invocation.RecoveryResumeCount {
		resumeStarted = true
	}
	if resumeErr != nil {
		return store.Invocation{}, run, fmt.Errorf("resume %s implementation agent for check repair: %w", previous.Harness, resumeErr)
	}
	invocation = resumedInvocation
	persistedInvocation = invocation
	if recorder, ok := runStore.(evaluationRecorder); ok {
		if err := recorder.RecordEvaluationAttempt(ctx, run.ID, store.EvaluationAttemptCheckRepair); err != nil {
			return store.Invocation{}, run, fmt.Errorf("record check-repair attempt: %w", err)
		}
	}
	completedRun := reservedRun
	completedRun.CheckRepairAttempts = repairPacket.Attempt
	completedRun.CheckRepairPendingAttempt = 0
	completedRun.LifecycleReason = fmt.Sprintf("check-repair attempt %d active", repairPacket.Attempt)
	completedRun.UpdatedAt = s.deps.Now().UTC()
	if err := s.persistAgentRunState(ctx, registration, runStore, reservedRun, completedRun); err != nil {
		return store.Invocation{}, reservedRun, fmt.Errorf("persist completed check-repair attempt: %w", err)
	}
	updated = completedRun
	return invocation, updated, nil
}
