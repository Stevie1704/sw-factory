package factory

import (
	"context"
	"errors"
	"fmt"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// lifecycleModule returns the coordinator's lazily initialized invocation
// lifecycle module and its explicit adapter and effect seams.
func (s *Service) lifecycleModule() *invocationLifecycle {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.lifecycle != nil {
		return s.lifecycle
	}
	s.lifecycle = newInvocationLifecycle(
		serviceJournal{service: s},
		s.deps.Worker,
		s.deps.HarnessCapabilities,
		s.worktreeInspector(),
		s.checkpointFileReader(),
		s.deps.Now,
		invocationLifecycleHooks{
			persistRun:               s.persistAgentRunState,
			publishReviewStatus:      s.publishReviewStatus,
			refreshReviewPullRequest: s.refreshSpecificationReviewPullRequest,
			reconcileInterrupted:     s.reconcileInterruptedRunWithMode,
			resumeRecoveredCheck:     s.resumeRecoveredCheck,
			resetStartup:             s.resetStartupStateProjection,
			materialiseReviewDiff:    s.materialiseReviewDiff,
		},
		s.deps.HeadlessHarnesses,
	)
	return s.lifecycle
}

// startAgentWithStore sequences the lifecycle module and keeps the process-
// local adoption marker on the coordinator boundary.
func (s *Service) startAgentWithStore(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run, request AgentRequest) (AgentLaunchResult, error) {
	invocationID, err := s.newInvocationID()
	if err != nil {
		return AgentLaunchResult{}, err
	}
	result, err := s.lifecycleModule().Launch(ctx, InvocationLaunchRequest{Registration: registration, RunStore: runStore, Run: run, Request: request, InvocationID: invocationID, StartedHere: s.invocationStartedHere, EvaluationRecorder: launchEvaluationRecorderForRunStore(runStore)})
	if err == nil {
		s.markInvocationStarted(result.Invocation.ID)
	}
	return result, err
}

// newInvocationID resolves the coordinator-owned identity before a lifecycle
// operation can gather a launch snapshot.
func (s *Service) newInvocationID() (string, error) {
	if s.deps.NewRunID == nil {
		return "", errors.New("generate invocation identifier: run id generator is required")
	}
	runID, err := s.deps.NewRunID()
	if err != nil {
		return "", fmt.Errorf("generate invocation identifier: %w", err)
	}
	return "inv-" + runID, nil
}

// Resume is the coordinator entry point for the lifecycle module's recovery
// operation. Store opening and command locking remain coordinator concerns.
func (s *Service) Resume(ctx context.Context, request ResumeRequest) (ResumeResult, error) {
	if err := validateOptionalRunID(request.RunID); err != nil {
		return ResumeResult{}, err
	}
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	registration, runStore, err := s.openRunStore(ctx)
	if err != nil {
		return ResumeResult{}, err
	}
	defer func() { _ = runStore.Close() }()
	run, err := readReconciliationRun(ctx, runStore)
	if err != nil {
		return ResumeResult{}, fmt.Errorf("read run for resume: %w", err)
	}
	if run == nil {
		return ResumeResult{}, errors.New("no persisted run")
	}
	if request.RunID != "" && request.RunID != run.ID {
		return ResumeResult{}, fmt.Errorf("active run is %s, not %s", run.ID, request.RunID)
	}
	return s.resumeWithStore(ctx, registration, runStore, run)
}

// RefreshAuth is the coordinator entry point for managed credential
// projection and delegates the invocation-specific work to the module.
func (s *Service) RefreshAuth(ctx context.Context, request AuthRefreshRequest) (AuthRefreshResult, error) {
	if err := validateOptionalRunID(request.RunID); err != nil {
		return AuthRefreshResult{}, err
	}
	if request.Harness != "" && request.Harness != config.HarnessCodex && request.Harness != config.HarnessClaude {
		return AuthRefreshResult{}, fmt.Errorf("unsupported harness %q", request.Harness)
	}
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	registration, runStore, err := s.openRunStore(ctx)
	if err != nil {
		return AuthRefreshResult{}, err
	}
	defer func() { _ = runStore.Close() }()
	run, err := readReconciliationRun(ctx, runStore)
	if err != nil {
		return AuthRefreshResult{}, fmt.Errorf("read run for auth refresh: %w", err)
	}
	if run == nil {
		return AuthRefreshResult{}, errors.New("no persisted run")
	}
	if request.RunID != "" && request.RunID != run.ID {
		return AuthRefreshResult{}, fmt.Errorf("active run is %s, not %s", run.ID, request.RunID)
	}
	if request.Resume {
		if err := s.recoverHeadlessNativeSessionIdentities(ctx, registration, runStore, *run); err != nil {
			return AuthRefreshResult{Run: *run}, fmt.Errorf("recover headless native session identity before auth refresh resume: %w", err)
		}
	}
	result, refreshErr := s.lifecycleModule().refreshAuth(ctx, InvocationRecoveryRequest{Registration: registration, RunStore: runStore, Run: run}, request.Harness, request.Resume)
	if request.Resume && result.Resumed && result.Invocation.ID != "" {
		s.markInvocationStarted(result.Invocation.ID)
	}
	return result, refreshErr
}

// resumeWithStore applies the explicit resume lifecycle to an already-open
// operational store. Command handling uses this seam while holding the
// coordinator lock, avoiding a second lock acquisition during polling.
func (s *Service) resumeWithStore(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run) (ResumeResult, error) {
	if run == nil {
		return ResumeResult{}, errors.New("no persisted run")
	}
	if err := s.recoverHeadlessNativeSessionIdentities(ctx, registration, runStore, *run); err != nil {
		return ResumeResult{Run: *run}, fmt.Errorf("recover headless native session identity before resume: %w", err)
	}
	result, err := s.lifecycleModule().Resume(ctx, InvocationRecoveryRequest{Registration: registration, RunStore: runStore, Run: run, NewInvocationID: s.newInvocationID, EvaluationRecorder: launchEvaluationRecorderForRunStore(runStore)})
	if err == nil && result.Invocation.ID != "" {
		s.markInvocationStarted(result.Invocation.ID)
	}
	return result, err
}

// retryWaitingForHarness opens the operational store as a coordinator entry
// point, then delegates the recovery decision and effects to the module.
func (s *Service) retryWaitingForHarness(ctx context.Context, registration config.RepositoryRegistration) error {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	opened, err := s.deps.OpenStore(ctx, registration.OperationalDataPath)
	if err != nil {
		return fmt.Errorf("open operational store for harness retry: %w", err)
	}
	runStore, ok := opened.(RunStore)
	if !ok {
		_ = opened.Close()
		return errors.New("operational store does not support harness retry")
	}
	defer func() { _ = runStore.Close() }()
	run, err := runStore.CurrentRun(ctx)
	if err != nil {
		return fmt.Errorf("read run for harness retry: %w", err)
	}
	if run == nil {
		return nil
	}
	launch := func(launchRun store.Run) (AgentLaunchResult, error) {
		invocationID, err := s.newInvocationID()
		if err != nil {
			return AgentLaunchResult{}, err
		}
		result, err := s.lifecycleModule().Launch(ctx, InvocationLaunchRequest{Registration: registration, RunStore: runStore, Run: &launchRun, Request: normalizeAgentRequest(agentRequestForRun(launchRun)), InvocationID: invocationID, StartedHere: s.invocationStartedHere, EvaluationRecorder: launchEvaluationRecorderForRunStore(runStore)})
		if err == nil {
			s.markInvocationStarted(result.Invocation.ID)
		}
		return result, err
	}
	return s.lifecycleModule().retryWaitingForHarness(ctx, registration, runStore, *run, launch)
}

// launchEvaluationRecorderForRunStore narrows a coordinator-owned store to
// the content-free evaluation projection needed by lifecycle activation.
func launchEvaluationRecorderForRunStore(runStore RunStore) launchEvaluationRecorder {
	recorder, _ := runStore.(launchEvaluationRecorder)
	return recorder
}
