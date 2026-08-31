package factory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// ResumeRequest selects the active run whose persisted worker and native
// harness session should be resumed by an operator.
type ResumeRequest struct {
	// RunID optionally selects one opaque factory run identifier.
	RunID string
}

// ResumeResult reports the invocation restored by an explicit resume command.
type ResumeResult struct {
	// Run is the resulting durable run projection.
	Run store.Run
	// Invocation is the resumed or newly launched invocation.
	Invocation store.Invocation
	// WaitingForAttach reports that the native session must be acknowledged by
	// factory attach before another workflow action can proceed.
	WaitingForAttach bool
}

// AttachRequest selects the active run whose visible native session should be
// reattached and released for workflow progression.
type AttachRequest struct {
	// RunID optionally selects one opaque factory run identifier.
	RunID string
}

// AttachResult reports the run and invocation after an attach operation.
type AttachResult struct {
	// Run is the resulting durable run projection.
	Run store.Run
	// Invocation is the attached invocation.
	Invocation store.Invocation
}

// AuthRefreshRequest selects the harness credential source to reseed into the
// factory-managed worker credential store.
type AuthRefreshRequest struct {
	// RunID optionally selects one opaque factory run identifier.
	RunID string
	// Harness selects codex or claude. Empty uses the invocation's harness.
	Harness config.Harness
}

// AuthRefreshResult reports the credential store identity after a refresh.
type AuthRefreshResult struct {
	// Run is unchanged workflow state after credential refresh.
	Run store.Run
	// Invocation is the invocation whose worker credential store was reseeded.
	Invocation store.Invocation
	// Harness identifies the refreshed adapter.
	Harness config.Harness
}

// ManualResumeRequiredError reports that an explicit native resume completed
// but the operator has not yet acknowledged the visible session.
type ManualResumeRequiredError struct {
	// RunID identifies the blocked run.
	RunID string
}

// Error explains the attach gate without including any harness output.
func (e *ManualResumeRequiredError) Error() string {
	if e == nil {
		return "manual harness resume requires attach"
	}
	return fmt.Sprintf("manual harness resume for run %q requires `factory attach` before progression", e.RunID)
}

// Resume explicitly restores one persisted native session, retries a run that
// is waiting for harness capacity, or re-enters a coordinator-owned check
// paused by restart reconciliation. It never consumes the automatic recovery
// ceiling.
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
	if store.IsTerminalStatus(run.Status) {
		return ResumeResult{}, fmt.Errorf("cannot resume terminal run %q with status %q", run.ID, run.Status)
	}
	if pending, journaled := runStore.(PendingEffectStore); journaled {
		effect, pendingErr := pending.PendingEffect(ctx, run.ID)
		if pendingErr != nil {
			return ResumeResult{}, fmt.Errorf("inspect pending effect before resume: %w", pendingErr)
		}
		if effect != nil {
			updated, _, _, reconcileErr := s.reconcileInterruptedRun(ctx, registration, runStore, *run)
			*run = updated
			if reconcileErr != nil {
				return ResumeResult{Run: *run}, reconcileErr
			}
		}
	}

	activeStore, ok := runStore.(ActiveInvocationStore)
	if !ok {
		return ResumeResult{}, errors.New("operational store does not support invocation recovery")
	}
	active, err := activeStore.ActiveInvocation(ctx, run.ID)
	if err != nil {
		return ResumeResult{}, fmt.Errorf("read active invocation for resume: %w", err)
	}
	if active != nil {
		if active.AttachRequired {
			return ResumeResult{Run: *run, Invocation: *active, WaitingForAttach: true}, &ManualResumeRequiredError{RunID: run.ID}
		}
		if strings.TrimSpace(active.NativeSessionID) == "" {
			if err := s.supersedeInvocation(ctx, registration, runStore, *active); err != nil {
				return ResumeResult{}, err
			}
			active = nil
		}
	}
	if active == nil {
		if run.Stage == store.StageCheck && isRestartReconciliationPause(*run) {
			resumed, resumeErr := s.resumeRecoveredCheck(ctx, registration, runStore, *run)
			if resumeErr != nil {
				return ResumeResult{Run: resumed}, resumeErr
			}
			return ResumeResult{Run: resumed}, nil
		}
		requestForRun := normalizeAgentRequest(agentRequestForRun(*run))
		launchRun := *run
		if launchRun.Status != store.StatusActive {
			launchRun.Status = store.StatusActive
		}
		launch, launchErr := s.startAgentWithStore(ctx, registration, runStore, &launchRun, requestForRun)
		if launchErr != nil {
			return ResumeResult{Run: *run, Invocation: launch.Invocation}, launchErr
		}
		return ResumeResult{Run: launchRun, Invocation: launch.Invocation}, nil
	}

	updatedInvocation, resumeErr := s.resumePersistedInvocationManually(ctx, registration, runStore, *run, *active)
	if resumeErr != nil {
		var credentialErr *credentialProjectionError
		if errors.As(resumeErr, &credentialErr) {
			paused, pauseErr := s.pauseForAuthentication(ctx, registration, runStore, *run, active.Harness)
			return ResumeResult{Run: paused, Invocation: updatedInvocation}, errors.Join(credentialErr, pauseErr)
		}
		classified := classifyHarnessRuntimeErrorForInvocation(*active, resumeErr)
		if harness.IsRateLimited(classified) {
			paused, waitErr := s.pauseForHarnessCapacity(ctx, registration, runStore, *run, active.Harness)
			return ResumeResult{Run: paused, Invocation: updatedInvocation}, errors.Join(classified, waitErr)
		}
		if harness.IsAuthenticationExpired(classified) {
			paused, pauseErr := s.pauseForAuthentication(ctx, registration, runStore, *run, active.Harness)
			return ResumeResult{Run: paused, Invocation: updatedInvocation}, errors.Join(classified, pauseErr)
		}
		if harness.IsUnexpectedExit(classified) {
			paused, pauseErr := s.pauseForManualRecovery(ctx, registration, runStore, *run, active.Harness, classified)
			return ResumeResult{Run: paused, Invocation: updatedInvocation}, errors.Join(classified, pauseErr)
		}
		return ResumeResult{Run: *run, Invocation: updatedInvocation}, resumeErr
	}

	if updatedInvocation.AttachRequired {
		next := *run
		next.Status = store.StatusWaitingForHuman
		next.LifecycleReason = "manual native session resumed; attach is required before progression"
		next.UpdatedAt = s.deps.Now().UTC()
		if next.Revision <= run.Revision {
			next.Revision = run.Revision + 1
		}
		persistErr := s.persistAgentRunState(ctx, registration, runStore, *run, next)
		if persistErr != nil {
			return ResumeResult{Run: next, Invocation: updatedInvocation, WaitingForAttach: true}, errors.Join(&ManualResumeRequiredError{RunID: run.ID}, persistErr)
		}
		return ResumeResult{Run: next, Invocation: updatedInvocation, WaitingForAttach: true}, &ManualResumeRequiredError{RunID: run.ID}
	}
	return ResumeResult{Run: *run, Invocation: updatedInvocation}, nil
}

// Attach restores the worker and visible terminal topology, then clears the
// durable manual-resume gate so report acceptance can proceed.
func (s *Service) Attach(ctx context.Context, request AttachRequest) (AttachResult, error) {
	if err := validateOptionalRunID(request.RunID); err != nil {
		return AttachResult{}, err
	}
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	registration, runStore, err := s.openRunStore(ctx)
	if err != nil {
		return AttachResult{}, err
	}
	defer func() { _ = runStore.Close() }()
	run, err := readReconciliationRun(ctx, runStore)
	if err != nil {
		return AttachResult{}, fmt.Errorf("read run for attach: %w", err)
	}
	if run == nil {
		return AttachResult{}, errors.New("no persisted run")
	}
	if request.RunID != "" && request.RunID != run.ID {
		return AttachResult{}, fmt.Errorf("active run is %s, not %s", run.ID, request.RunID)
	}
	if store.IsTerminalStatus(run.Status) {
		return AttachResult{}, fmt.Errorf("cannot attach terminal run %q with status %q", run.ID, run.Status)
	}
	activeStore, ok := runStore.(ActiveInvocationStore)
	if !ok {
		return AttachResult{}, errors.New("operational store does not support invocation recovery")
	}
	active, err := activeStore.ActiveInvocation(ctx, run.ID)
	if err != nil {
		return AttachResult{}, fmt.Errorf("read active invocation for attach: %w", err)
	}
	if active == nil {
		return AttachResult{}, errors.New("run has no active invocation to attach")
	}
	if strings.TrimSpace(active.NativeSessionID) == "" {
		return AttachResult{}, errors.New("active invocation has no persisted native session identifier")
	}
	terminalRuntime, _, err := s.ensureAgentRuntime(registration.Cmux.SocketPath, config.Harness(active.Harness))
	if err != nil {
		return AttachResult{}, fmt.Errorf("ensure agent runtime for attach: %w", err)
	}
	_, recoveredInvocation, err := s.ensureWorkerForInvocation(ctx, registration, runStore, *run, *active)
	if err != nil {
		return AttachResult{}, err
	}
	active = &recoveredInvocation
	workspaceID, implementationSurface, statusSurface, checksSurface, restored, err := s.restoreInvocationTerminal(ctx, terminalRuntime, *run, *active)
	if err != nil {
		return AttachResult{}, err
	}
	updated := *active
	updated.WorkspaceID = string(workspaceID)
	setInvocationSurface(&updated, implementationSurface)
	updated.StatusSurfaceID = string(statusSurface.ID)
	updated.ChecksSurfaceID = string(checksSurface.ID)
	if restored {
		// A recreated cmux topology needs a native resume to place the process
		// into the newly created surface. The nested resume restores credentials
		// itself.
		resumed, resumeErr := s.resumePersistedInvocationManually(ctx, registration, runStore, *run, updated)
		if resumeErr != nil {
			var credentialErr *credentialProjectionError
			if errors.As(resumeErr, &credentialErr) {
				paused, pauseErr := s.pauseForAuthentication(ctx, registration, runStore, *run, active.Harness)
				return AttachResult{Run: paused, Invocation: resumed}, errors.Join(credentialErr, pauseErr)
			}
			return AttachResult{Invocation: resumed}, resumeErr
		}
		updated = resumed
	} else if err := s.restoreCredentialProjection(ctx, registration, *run, updated); err != nil {
		paused, pauseErr := s.pauseForAuthentication(ctx, registration, runStore, *run, active.Harness)
		return AttachResult{Run: paused, Invocation: updated}, errors.Join(err, pauseErr)
	}
	if restored || updated.AttachRequired {
		updated.AttachRequired = false
		updated.UpdatedAt = s.deps.Now().UTC()
		invocationStore, ok := runStore.(InvocationStore)
		if !ok {
			return AttachResult{Invocation: updated}, errors.New("operational store does not support invocation persistence")
		}
		if err := invocationStore.SaveInvocation(ctx, updated); err != nil {
			return AttachResult{Invocation: updated}, fmt.Errorf("persist attached invocation: %w", err)
		}
	}
	next := *run
	next.Status = store.StatusActive
	next.LifecycleReason = "manual native session attached"
	next.UpdatedAt = s.deps.Now().UTC()
	if next.Revision <= run.Revision {
		next.Revision = run.Revision + 1
	}
	err = s.persistAgentRunState(ctx, registration, runStore, *run, next)
	if err != nil {
		return AttachResult{Run: next, Invocation: updated}, err
	}
	s.resetStartupState()
	return AttachResult{Run: next, Invocation: updated}, nil
}

// RefreshAuth reseeds one host credential source into the factory-managed
// worker volume. The registration path is read only; no host credential file
// or source configuration is changed.
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
	if store.IsTerminalStatus(run.Status) {
		return AuthRefreshResult{}, fmt.Errorf("cannot refresh auth for terminal run %q", run.ID)
	}
	invocationStore, ok := runStore.(InvocationStore)
	if !ok {
		return AuthRefreshResult{}, errors.New("operational store does not support invocation auth refresh")
	}
	invocation, err := latestInvocationForAuthRefresh(ctx, runStore, *run)
	if err != nil {
		return AuthRefreshResult{}, err
	}
	harnessName := request.Harness
	if harnessName == "" {
		harnessName = config.Harness(invocation.Harness)
	}
	if string(harnessName) != invocation.Harness {
		return AuthRefreshResult{}, fmt.Errorf("auth refresh harness %q does not match invocation harness %q", harnessName, invocation.Harness)
	}
	seed, credentialStoreID, err := s.credentialSeeding(registration, AgentRequest{}, harnessName)
	if err != nil {
		return AuthRefreshResult{}, err
	}
	if seed == nil || strings.TrimSpace(credentialStoreID) == "" {
		return AuthRefreshResult{}, fmt.Errorf("no factory-managed %s credential source is registered", harnessName)
	}
	if invocation.CredentialStoreID == "" {
		invocation.CredentialStoreID = credentialStoreID
		invocation.UpdatedAt = s.deps.Now().UTC()
		if err := invocationStore.SaveInvocation(ctx, *invocation); err != nil {
			return AuthRefreshResult{}, fmt.Errorf("persist credential store identity: %w", err)
		}
	}
	_, recoveredInvocation, err := s.ensureWorkerForInvocation(ctx, registration, runStore, *run, *invocation)
	if err != nil {
		return AuthRefreshResult{}, err
	}
	*invocation = recoveredInvocation
	if err := seed(ctx, run.ID, workerIDForInvocation(*invocation)); err != nil {
		return AuthRefreshResult{}, newCredentialProjectionError(string(harnessName))
	}
	return AuthRefreshResult{Run: *run, Invocation: *invocation, Harness: harnessName}, nil
}

// retryWaitingForHarness performs one automatic retry for a run paused only
// by temporary harness capacity. It is called by the supervised polling loop.
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
	if run.Status != store.StatusWaitingForHarness {
		return s.reconcileActiveHarnessLiveness(ctx, registration, runStore, *run)
	}
	activeStore, ok := runStore.(ActiveInvocationStore)
	if !ok {
		return errors.New("operational store does not support active invocation lookup")
	}
	active, err := activeStore.ActiveInvocation(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("read active invocation for harness retry: %w", err)
	}
	if active != nil && active.AttachRequired {
		return nil
	}
	if active != nil && strings.TrimSpace(active.NativeSessionID) != "" {
		if active.RecoveryResumeCount > 0 {
			// A consumed automatic slot is a hard boundary. A capacity wait must
			// never turn a later polling tick into a second native resume.
			_, pauseErr := s.pauseForManualRecovery(ctx, registration, runStore, *run, active.Harness, harness.NewUnexpectedExitError(active.Harness))
			if pauseErr != nil {
				return pauseErr
			}
			return nil
		}
		updated, resumeErr := s.resumePersistedInvocation(ctx, registration, runStore, *run, *active)
		if resumeErr != nil {
			classified := classifyHarnessRuntimeErrorForInvocation(*active, resumeErr)
			if harness.IsRateLimited(classified) {
				_, waitErr := s.pauseForHarnessCapacity(ctx, registration, runStore, *run, active.Harness)
				if waitErr != nil {
					return waitErr
				}
				return nil
			}
			if harness.IsAuthenticationExpired(classified) {
				_, pauseErr := s.pauseForAuthentication(ctx, registration, runStore, *run, active.Harness)
				if pauseErr != nil {
					return pauseErr
				}
				return nil
			}
			if harness.IsUnexpectedExit(classified) {
				_, pauseErr := s.pauseForManualRecovery(ctx, registration, runStore, *run, active.Harness, classified)
				if pauseErr != nil {
					return pauseErr
				}
				return nil
			}
			return resumeErr
		}
		next := *run
		next.Status = store.StatusActive
		next.LifecycleReason = "harness capacity returned; native session resumed"
		next.UpdatedAt = s.deps.Now().UTC()
		next.Revision = run.Revision + 1
		stateErr := s.persistAgentRunState(ctx, registration, runStore, *run, next)
		_ = updated
		return stateErr
	}
	if active != nil {
		if err := s.supersedeInvocation(ctx, registration, runStore, *active); err != nil {
			return err
		}
	}
	launchRun := *run
	launchRun.Status = store.StatusActive
	launch, launchErr := s.startAgentWithStore(ctx, registration, runStore, &launchRun, normalizeAgentRequest(agentRequestForRun(*run)))
	if launchErr != nil {
		classified := classifyHarnessRuntimeErrorForInvocation(launch.Invocation, launchErr)
		if harness.IsRateLimited(classified) {
			return nil
		}
		if harness.IsAuthenticationExpired(classified) || harness.IsUnexpectedExit(classified) {
			return nil
		}
		return launchErr
	}
	return nil
}

// reconcileActiveHarnessLiveness observes an active native session on every
// supervised polling pass. A process exit is handed to the same reconciliation
// state machine used at startup, with same-process recovery enabled because
// this observer has directly established the interruption boundary.
func (s *Service) reconcileActiveHarnessLiveness(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run) error {
	if run.Status != store.StatusActive {
		return nil
	}
	activeValues, supported, err := activeInvocationsForRun(ctx, runStore, run.ID)
	if !supported {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read active invocation for liveness monitoring: %w", err)
	}
	for index := range activeValues {
		active := activeValues[index]
		if active.AttachRequired || strings.TrimSpace(active.NativeSessionID) == "" {
			continue
		}
		_, harnessRuntime, runtimeErr := s.ensureAgentRuntime(registration.Cmux.SocketPath, config.Harness(active.Harness))
		if runtimeErr != nil {
			return fmt.Errorf("ensure harness for liveness monitoring: %w", runtimeErr)
		}
		livenessInspector, ok := harnessRuntime.(harness.NativeSessionLivenessInspector)
		if !ok {
			continue
		}
		running, livenessErr := livenessInspector.NativeSessionRunning(ctx, harness.NativeSessionRequest{
			RunID: run.ID, WorkerID: workerIDForInvocation(active), Harness: active.Harness,
		})
		if livenessErr != nil {
			return fmt.Errorf("check native session liveness: %w", livenessErr)
		}
		if running {
			continue
		}
		// The coordinator, not the adapter, is the first to see that a session
		// is gone. Claude Code assigns its own session identifier, so a Claude
		// launch that dies immediately is only ever observed here. Capture the
		// surface before reconciliation stops the worker behind it.
		s.recordSessionExitDiagnostic(ctx, registration, run, active)
		_, _, _, reconcileErr := s.reconcileInterruptedRunWithMode(ctx, registration, runStore, run, true)
		if reconcileErr != nil {
			return reconcileErr
		}
		return nil
	}
	return nil
}

// pauseForHarnessCapacity changes an active run to the non-budgeted harness
// waiting state, stops its worker, and sends one operator notification for the
// transition.
func (s *Service) pauseForHarnessCapacity(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, harnessName string) (store.Run, error) {
	if strings.TrimSpace(harnessName) == "" {
		harnessName = "harness"
	}
	changed := run.Status != store.StatusWaitingForHarness || !strings.HasPrefix(run.LifecycleReason, "harness capacity unavailable")
	if err := s.stopActiveRunWorkers(ctx, runStore, run); err != nil {
		return run, err
	}
	if !changed {
		return run, nil
	}
	next := run
	next.Status = store.StatusWaitingForHarness
	next.LifecycleReason = fmt.Sprintf("harness capacity unavailable (%s); waiting for capacity", harnessName)
	next.Revision = run.Revision + 1
	next.UpdatedAt = s.deps.Now().UTC()
	updated := next
	err := s.persistAgentRunState(ctx, registration, runStore, run, next)
	if err != nil {
		return updated, err
	}
	if notifyErr := s.notifyWorkspace(ctx, registration, "factory harness capacity", fmt.Sprintf("%s is waiting for %s harness capacity", run.ID, harnessName)); notifyErr != nil {
		return updated, notifyErr
	}
	return updated, nil
}

// pauseForAuthentication records a redacted human-waiting state after the
// harness rejects its credential. The source credential remains untouched.
func (s *Service) pauseForAuthentication(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, harnessName string) (store.Run, error) {
	if err := s.stopActiveRunWorkers(ctx, runStore, run); err != nil {
		return run, err
	}
	changed := run.Status != store.StatusWaitingForHuman || !strings.HasPrefix(run.LifecycleReason, "harness authentication expired")
	if !changed {
		return run, nil
	}
	next := run
	next.Status = store.StatusWaitingForHuman
	next.LifecycleReason = fmt.Sprintf("harness authentication expired (%s); run is waiting for `factory auth refresh`", harnessName)
	next.Revision = run.Revision + 1
	next.UpdatedAt = s.deps.Now().UTC()
	updated := next
	err := s.persistAgentRunState(ctx, registration, runStore, run, next)
	if err != nil {
		return updated, err
	}
	if notifyErr := s.notifyWorkspace(ctx, registration, "factory authentication expired", fmt.Sprintf("%s requires `factory auth refresh` for %s", run.ID, harnessName)); notifyErr != nil {
		return updated, notifyErr
	}
	return updated, nil
}

// pauseForManualRecovery escalates a failed explicit resume without changing
// any workflow retry budget.
func (s *Service) pauseForManualRecovery(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, harnessName string, cause error) (store.Run, error) {
	if err := s.stopActiveRunWorkers(ctx, runStore, run); err != nil {
		return run, err
	}
	changed := run.Status != store.StatusWaitingForHuman || !strings.HasPrefix(run.LifecycleReason, "automatic harness recovery exhausted")
	if !changed {
		return run, nil
	}
	next := run
	next.Status = store.StatusWaitingForHuman
	next.LifecycleReason = fmt.Sprintf("automatic harness recovery exhausted (%s); manual resume and attach required", harnessName)
	next.Revision = run.Revision + 1
	next.UpdatedAt = s.deps.Now().UTC()
	updated := next
	var err error
	if journal, ok := runStore.(PendingEffectStore); ok {
		pending, pendingErr := journal.PendingEffect(ctx, run.ID)
		if pendingErr != nil {
			return run, fmt.Errorf("inspect pending effect before manual recovery pause: %w", pendingErr)
		}
		if pending != nil {
			err = saveRunWithRetry(ctx, runStore, next)
		} else {
			err = s.persistAgentRunState(ctx, registration, runStore, run, next)
		}
	} else {
		err = s.persistAgentRunState(ctx, registration, runStore, run, next)
	}
	if err != nil {
		return updated, err
	}
	if notifyErr := s.notifyWorkspace(ctx, registration, "factory harness recovery exhausted", fmt.Sprintf("%s requires manual harness recovery", run.ID)); notifyErr != nil {
		return updated, errors.Join(cause, notifyErr)
	}
	return updated, nil
}

// ensureWorkerForInvocation validates or recreates the pinned worker for an
// invocation and returns the exact start request plus any persisted credential
// store identity added from the current registered source.
func (s *Service) ensureWorkerForInvocation(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, invocation store.Invocation) (worker.StartRequest, store.Invocation, error) {
	invocation, err := s.ensureCredentialStoreIdentity(ctx, registration, runStore, invocation)
	if err != nil {
		return worker.StartRequest{}, invocation, err
	}
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return worker.StartRequest{}, invocation, fmt.Errorf("decode specification packet for worker recovery: %w", err)
	}
	gitMetadataPath, err := prepareGitMetadataProjection(run.ID, registration.Path, run.Worktree)
	if err != nil {
		return worker.StartRequest{}, invocation, fmt.Errorf("prepare recovery Git metadata: %w", err)
	}
	request := worker.StartRequest{
		RunID:             run.ID,
		WorkerID:          workerIDForInvocation(invocation),
		WorktreeReadOnly:  roleIsKind(invocation, workflow.RoleKindReview),
		WorktreePath:      run.Worktree,
		GitMetadataPath:   gitMetadataPath,
		Image:             packet.RepositoryConfig.WorkerBuild.Image,
		ImageDigest:       run.ImageDigest,
		Caches:            workerCaches(packet.RepositoryConfig.Caches),
		InvocationPath:    invocation.InvocationDirectory,
		ResultPath:        invocation.ResultDirectory,
		CredentialStoreID: invocation.CredentialStoreID,
		Role:              invocation.Role,
	}
	if s.deps.Worker == nil {
		return worker.StartRequest{}, invocation, errors.New("worker runtime is required for invocation recovery")
	}
	inspection, err := s.deps.Worker.Inspect(ctx, workerIDForInvocation(invocation))
	if err != nil {
		return worker.StartRequest{}, invocation, fmt.Errorf("inspect worker for invocation recovery: %w", err)
	}
	if inspection.Exists {
		if !workerImageMatches(inspection.Image, request.Image, request.ImageDigest) {
			return worker.StartRequest{}, invocation, fmt.Errorf("persisted worker image %q does not match frozen image %q@%s", inspection.Image, request.Image, request.ImageDigest)
		}
		if inspection.Running {
			known, matches := inspection.MountContractStatus(request)
			if known && matches {
				return request, invocation, nil
			}
		}
	}
	if err := s.startWorkerWithEffect(ctx, runStore, request); err != nil {
		return worker.StartRequest{}, invocation, fmt.Errorf("start or recreate worker during invocation recovery: %w", err)
	}
	return request, invocation, nil
}

// restoreInvocationTerminal returns persisted handles when they still exist
// and recreates the run workspace when cmux lost the workspace or a role
// surface. It never reads terminal screen contents.
func (s *Service) restoreInvocationTerminal(ctx context.Context, terminalRuntime terminal.TerminalRuntime, run store.Run, invocation store.Invocation) (terminal.WorkspaceID, terminal.Surface, terminal.Surface, terminal.Surface, bool, error) {
	roleDefinition, err := roleDefinitionForInvocation(invocation)
	if err != nil {
		return "", terminal.Surface{}, terminal.Surface{}, terminal.Surface{}, false, err
	}
	workspaceID := terminal.WorkspaceID(invocation.WorkspaceID)
	roleSurface := invocationSurface(invocation)
	roleSurface.WorkspaceID = workspaceID
	status := terminal.Surface{ID: terminal.SurfaceID(invocation.StatusSurfaceID), WorkspaceID: workspaceID, Name: "status"}
	checks := terminal.Surface{ID: terminal.SurfaceID(invocation.ChecksSurfaceID), WorkspaceID: workspaceID, Name: "checks"}
	restore := workspaceID == "" || roleSurface.ID == ""
	if inspector, ok := terminalRuntime.(terminal.WorkspaceInspector); ok && !restore {
		observed, err := inspector.InspectWorkspace(ctx, workspaceID)
		if err != nil {
			return workspaceID, roleSurface, status, checks, false, fmt.Errorf("inspect terminal workspace for recovery: %w", err)
		}
		if !observed.Exists {
			restore = true
		} else {
			ids := make(map[terminal.SurfaceID]struct{}, len(observed.Surfaces))
			for _, surface := range observed.Surfaces {
				ids[surface.ID] = struct{}{}
			}
			for _, surface := range []terminal.Surface{roleSurface, status, checks} {
				if surface.ID != "" {
					if _, exists := ids[surface.ID]; !exists {
						restore = true
					}
				}
			}
		}
	}
	if !restore {
		return workspaceID, roleSurface, status, checks, false, nil
	}
	runWorkspace, err := terminalRuntime.EnsureRunWorkspace(ctx, terminal.RunWorkspaceRequest{
		RunID:            run.ID,
		Name:             "factory-" + run.ID,
		Description:      fmt.Sprintf("factory run for issue #%d", run.IssueNumber),
		WorkingDirectory: run.Worktree,
	})
	if err != nil {
		return workspaceID, roleSurface, status, checks, false, fmt.Errorf("restore run terminal workspace: %w", err)
	}
	workspaceID = runWorkspace.ID
	status = runWorkspace.Status
	checks = runWorkspace.Checks
	switch roleDefinition.Surface {
	case workflow.SurfaceChecks:
		roleSurface = runWorkspace.Checks
	case workflow.SurfaceRole:
		roleSurface = terminal.Surface{WorkspaceID: runWorkspace.ID, Name: invocation.Role}
	default:
		roleSurface = runWorkspace.Implementation
	}
	return workspaceID, roleSurface, status, checks, true, nil
}

// latestInvocationForAuthRefresh selects the active invocation or the latest
// superseded attempt that still carries the worker identity needed to reseed.
func latestInvocationForAuthRefresh(ctx context.Context, runStore RunStore, run store.Run) (*store.Invocation, error) {
	activeValues, supported, err := activeInvocationsForRun(ctx, runStore, run.ID)
	if err != nil {
		return nil, fmt.Errorf("read active invocation for auth refresh: %w", err)
	}
	if supported && len(activeValues) > 0 {
		active := activeValues[len(activeValues)-1]
		return &active, nil
	}
	latestStore, ok := runStore.(LatestInvocationStore)
	if !ok {
		return nil, errors.New("operational store does not support latest invocation lookup")
	}
	latest, err := latestStore.LatestInvocation(ctx, run.ID)
	if err != nil {
		return nil, fmt.Errorf("read latest invocation for auth refresh: %w", err)
	}
	if latest == nil {
		return nil, errors.New("run has no invocation to refresh")
	}
	return latest, nil
}

// supersedeInvocation closes an incomplete launch before a fresh capacity or
// authentication retry creates the next immutable invocation identity. It also
// releases the run's delegation marker, so a published activity can never name
// an invocation that will never produce a result.
func (s *Service) supersedeInvocation(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, invocation store.Invocation) error {
	invocationStore, ok := runStore.(InvocationStore)
	if !ok {
		return errors.New("operational store does not support invocation recovery")
	}
	invocation.Status = store.InvocationStatusSuperseded
	invocation.UpdatedAt = s.deps.Now().UTC()
	if err := invocationStore.SaveInvocation(ctx, invocation); err != nil {
		return fmt.Errorf("supersede interrupted invocation: %w", err)
	}
	current, err := runStore.CurrentRun(ctx)
	if err != nil {
		return fmt.Errorf("read run to release superseded invocation: %w", err)
	}
	if current == nil || (current.ActiveInvocationID != invocation.ID && !containsString(current.ActiveInvocationIDs, invocation.ID)) {
		return nil
	}
	previous := *current
	next := previous
	releaseActiveInvocation(&next, invocation.ID)
	next.UpdatedAt = s.deps.Now().UTC()
	if err := s.persistAgentRunState(ctx, registration, runStore, previous, next); err != nil {
		return fmt.Errorf("release superseded invocation delegation: %w", err)
	}
	return nil
}

// classifyHarnessRuntimeErrorForInvocation preserves typed harness failures
// while avoiding any attempt to expose adapter output for known categories.
func classifyHarnessRuntimeErrorForInvocation(invocation store.Invocation, err error) error {
	return harness.ClassifyError(err, invocation.Harness)
}

// validateOptionalRunID rejects command-line control characters before they
// reach a store lookup or an external adapter.
func validateOptionalRunID(runID string) error {
	if strings.ContainsAny(runID, "\x00\r\n") {
		return errors.New("run id contains control characters")
	}
	return nil
}

// ensureInvocationAttached rejects workflow progression while a manual native
// resume remains unacknowledged by the operator.
func (s *Service) ensureInvocationAttached(ctx context.Context, runStore RunStore, run store.Run) error {
	activeValues, supported, err := activeInvocationsForRun(ctx, runStore, run.ID)
	if !supported {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read active invocation attach gate: %w", err)
	}
	for _, active := range activeValues {
		if active.AttachRequired {
			return &ManualResumeRequiredError{RunID: run.ID}
		}
	}
	return nil
}

// stopActiveRunWorkers stops every currently delegated worker, preserving the
// shared credential volume and all durable invocation artifacts. Review roles
// own separate worker identities, so a run-level pause must stop each one.
func (s *Service) stopActiveRunWorkers(ctx context.Context, runStore RunStore, run store.Run) error {
	if s.deps.Worker == nil {
		return nil
	}
	activeValues, supported, err := activeInvocationsForRun(ctx, runStore, run.ID)
	if err != nil {
		return fmt.Errorf("read active invocations before stopping workers: %w", err)
	}
	workerIDs := make([]string, 0, len(activeValues))
	seen := make(map[string]struct{}, len(activeValues))
	if supported {
		for _, invocation := range activeValues {
			workerID := workerIDForInvocation(invocation)
			if _, exists := seen[workerID]; exists {
				continue
			}
			seen[workerID] = struct{}{}
			workerIDs = append(workerIDs, workerID)
		}
	} else if run.ActiveInvocationID != "" || len(run.ActiveInvocationIDs) != 0 {
		workerIDs = append(workerIDs, run.ID)
	}
	if len(workerIDs) == 0 {
		// Preserve the historical idempotent stop for a run whose invocation
		// history is empty or already terminal. The run-scoped worker is absent
		// for isolated review workers, so this is harmless in that case.
		workerIDs = append(workerIDs, run.ID)
	}
	var stopErr error
	for _, workerID := range workerIDs {
		stopErr = errors.Join(stopErr, s.stopRunWorker(ctx, workerID))
	}
	return stopErr
}

// resetStartupState lets a successful explicit attach reopen progression seams
// in a long-lived embedded service that previously cached the attach gate.
func (s *Service) resetStartupState() {
	s.startupMu.Lock()
	defer s.startupMu.Unlock()
	s.startupChecked = false
	s.startupErr = nil
}

// recordSessionExitDiagnostic captures what a harness printed before its
// session ended and writes it beside the invocation. Without it an exited
// session reports only that it exited, which is the same dead end a failed
// launch used to reach.
func (s *Service) recordSessionExitDiagnostic(ctx context.Context, registration config.RepositoryRegistration, run store.Run, invocation store.Invocation) {
	terminalRuntime := s.deps.Terminal
	if terminalRuntime == nil {
		terminalRuntime = terminal.NewCmuxRuntime(nil, registration.Cmux.SocketPath)
	}
	transcript := captureSurfaceTranscript(ctx, terminalRuntime, invocationSurface(invocation).ID)
	cause := fmt.Errorf("%s native session %q exited before reporting", invocation.Harness, invocation.NativeSessionID)
	writeHarnessFailureDiagnostic(invocationRoot(run, invocation.ID), "session exit", cause, transcript, s.deps.Now().UTC())
}
