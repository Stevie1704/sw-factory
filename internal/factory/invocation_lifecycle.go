package factory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/prompt"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// LaunchOutcome identifies the result selected by admission.
type LaunchOutcome string

const (
	// LaunchOutcomeLaunch means admission produced a new invocation launch.
	LaunchOutcomeLaunch LaunchOutcome = "launch"
	// LaunchOutcomeAdopt means admission adopted an invocation recovered by a
	// different coordinator process.
	LaunchOutcomeAdopt LaunchOutcome = "adopt"
)

// ActiveLaunchObservation records an active invocation and the coordinator
// process-local observation made during gather.
type ActiveLaunchObservation struct {
	// Invocation is the persisted active invocation.
	Invocation store.Invocation
	// StartedHere reports whether this coordinator launched the invocation.
	StartedHere bool
}

// LaunchSnapshot contains the read-only observations gathered before admission.
type LaunchSnapshot struct {
	// Run is the persisted run observed by the gather phase.
	Run store.Run
	// Request is the role- and stage-resolved request used by gather.
	Request AgentRequest
	// Packet is the decoded frozen specification packet.
	Packet SpecificationPacket
	// InvocationID is the coordinator-resolved identity for a possible launch.
	InvocationID string
	// ResumeSource is the invocation selected for a test revision or implementation
	// continuation, when one was gathered.
	ResumeSource *store.Invocation
	// LatestInvocation is the newest invocation used by the duplicate-history gate.
	LatestInvocation *store.Invocation
	// ActiveInvocations contains every active invocation observed for the run.
	ActiveInvocations []ActiveLaunchObservation
	// ActiveInvocationsSupported distinguishes an absent compatibility projection
	// from an empty active-invocation result.
	ActiveInvocationsSupported bool
	// ReviewContext is the read-only reviewer context gathered for a review role.
	ReviewContext *prompt.ReviewContext
}

// LaunchPlan is the pure admission decision for one visible invocation.
type LaunchPlan struct {
	// Outcome identifies whether activation launches or adopts an invocation.
	Outcome LaunchOutcome
	// Run is the gathered run projection.
	Run store.Run
	// Request is the role- and stage-resolved request.
	Request AgentRequest
	// Packet is the frozen packet used by materialisation.
	Packet SpecificationPacket
	// RoleDefinition is the factory-owned role declaration.
	RoleDefinition workflow.RoleDefinition
	// ResumeSource is the native session continued by activation, when any.
	ResumeSource *store.Invocation
	// ReviewContext is the gathered reviewer input, when any.
	ReviewContext *prompt.ReviewContext
	// DesignHandoff is the route-specific architecture handoff.
	DesignHandoff *store.RoleHandoff
	// Policy is the resolved harness/model policy.
	Policy AgentPolicy
	// InvocationID is the coordinator-resolved identity for materialisation.
	InvocationID string
	// TestRevision reports whether this is an objection-cycle test revision.
	TestRevision bool
	// ReviewRepair reports whether this is a blocking-review repair launch.
	ReviewRepair bool
	// ImplementationResume reports whether this continues implementation.
	ImplementationResume bool
	// AdoptedInvocation is populated only for the adoption outcome.
	AdoptedInvocation *store.Invocation
}

// InvocationLaunchRequest supplies the coordinator-owned inputs for one
// lifecycle launch. The caller resolves InvocationID before gather.
type InvocationLaunchRequest struct {
	// Registration identifies the repository and visible terminal configuration.
	Registration config.RepositoryRegistration
	// RunStore is the already-open operational store.
	RunStore RunStore
	// Run is the run projection that activation may update after success.
	Run *store.Run
	// Request selects the role and policy overrides.
	Request AgentRequest
	// InvocationID is the coordinator-resolved identity for a new invocation.
	InvocationID string
	// StartedHere observes process-local launch ownership during gather.
	StartedHere func(string) bool
}

// InvocationRecoveryRequest supplies one already-open run to a recovery
// operation delegated by a Service entry point.
type InvocationRecoveryRequest struct {
	// Registration identifies the repository and terminal configuration.
	Registration config.RepositoryRegistration
	// RunStore is the already-open operational store.
	RunStore RunStore
	// Run is the active run projection.
	Run *store.Run
	// InvocationID is resolved by the coordinator for a possible fresh launch.
	InvocationID string
	// NewInvocationID resolves an ID only when this recovery needs a fresh
	// launch; it keeps the generator outside the lifecycle module.
	NewInvocationID func() (string, error)
}

// InvocationStopRequest selects the active workers delegated to a run.
type InvocationStopRequest struct {
	// RunStore provides the active-invocation projection.
	RunStore RunStore
	// Run identifies the run whose workers should stop.
	Run store.Run
}

// InvocationLifecycle is the small seam used by the coordinator for launching,
// recovering, attaching, and stopping visible invocations.
type InvocationLifecycle interface {
	// Launch admits, materialises, and activates one visible invocation.
	Launch(context.Context, InvocationLaunchRequest) (AgentLaunchResult, error)
	// Resume recovers the persisted invocation or launches the next role.
	Resume(context.Context, InvocationRecoveryRequest) (ResumeResult, error)
	// Attach restores the visible session and releases its attach gate.
	Attach(context.Context, InvocationRecoveryRequest) (AttachResult, error)
	// Stop stops every worker currently delegated to a run.
	Stop(context.Context, InvocationStopRequest) error
}

// invocationLifecycleHooks are coordinator-owned effects passed explicitly to
// the module. The module never receives the coordinator's dependency bundle.
type invocationLifecycleHooks struct {
	startWorker              func(context.Context, RunStore, worker.StartRequest) error
	resumeHarness            func(context.Context, RunStore, InvocationStore, string, harness.Runtime, store.Invocation, harness.StartRequest) (store.Invocation, error)
	resumeHarnessManually    func(context.Context, RunStore, InvocationStore, string, harness.Runtime, store.Invocation, harness.StartRequest) (store.Invocation, error)
	persistRun               func(context.Context, config.RepositoryRegistration, RunStore, store.Run, store.Run) error
	notifyWorkspace          func(context.Context, config.RepositoryRegistration, string, string) error
	publishReviewStatus      func(context.Context, config.RepositoryRegistration, RunStore, store.Run, string, github.CommitStatusState, string) error
	refreshReviewPullRequest func(context.Context, config.RepositoryRegistration, RunStore, store.Run) error
	reconcileInterrupted     func(context.Context, config.RepositoryRegistration, RunStore, store.Run, bool) (store.Run, RecoveryDiagnosis, RecoveryOutcome, error)
	resumeRecoveredCheck     func(context.Context, config.RepositoryRegistration, RunStore, store.Run) (store.Run, error)
	resetStartup             func()
	captureReviewDiff        func(context.Context, store.Run, store.Invocation) (reviewDiffCapture, error)
}

// invocationLifecycle owns the worker, harness, terminal, and capability
// adapters used by launch and recovery, plus explicitly supplied coordinator
// effect hooks.
type invocationLifecycle struct {
	worker              worker.WorkerRuntime
	terminal            terminal.TerminalRuntime
	harness             harness.Runtime
	harnessCapabilities harness.CapabilityResolver
	worktree            gitadapter.WorktreeInspector
	clock               Clock
	hooks               invocationLifecycleHooks
	runtimeMu           sync.Mutex
	runtimeSocketPath   string
	runtimePathSet      bool
	harnessRuntimes     map[config.Harness]harness.Runtime
}

var _ InvocationLifecycle = (*invocationLifecycle)(nil)

// newInvocationLifecycle constructs the module from explicit adapters and
// hooks. It intentionally accepts only explicit adapters and effects.
func newInvocationLifecycle(workerRuntime worker.WorkerRuntime, terminalRuntime terminal.TerminalRuntime, harnessRuntime harness.Runtime, capabilities harness.CapabilityResolver, worktree gitadapter.WorktreeInspector, clock Clock, hooks invocationLifecycleHooks) *invocationLifecycle {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &invocationLifecycle{
		worker: workerRuntime, terminal: terminalRuntime, harness: harnessRuntime,
		harnessCapabilities: capabilities, worktree: worktree, clock: clock, hooks: hooks,
		harnessRuntimes: make(map[config.Harness]harness.Runtime, 2),
	}
}

// Resume restores an active native session, retries a harness-capacity wait,
// or launches the next visible role after reconciling a durable interruption.
func (l *invocationLifecycle) Resume(ctx context.Context, request InvocationRecoveryRequest) (ResumeResult, error) {
	if request.Run == nil {
		return ResumeResult{}, errors.New("no persisted run")
	}
	run := *request.Run
	if store.IsTerminalStatus(run.Status) {
		return ResumeResult{}, fmt.Errorf("cannot resume terminal run %q with status %q", run.ID, run.Status)
	}
	if pending, journaled := request.RunStore.(PendingEffectStore); journaled {
		effect, err := pending.PendingEffect(ctx, run.ID)
		if err != nil {
			return ResumeResult{}, fmt.Errorf("inspect pending effect before resume: %w", err)
		}
		if effect != nil {
			if l.hooks.reconcileInterrupted == nil {
				return ResumeResult{}, errors.New("interrupted-run reconciliation hook is required")
			}
			updated, _, _, reconcileErr := l.hooks.reconcileInterrupted(ctx, request.Registration, request.RunStore, run, false)
			run = updated
			if reconcileErr != nil {
				return ResumeResult{Run: run}, reconcileErr
			}
		}
	}
	activeStore, ok := request.RunStore.(ActiveInvocationStore)
	if !ok {
		return ResumeResult{}, errors.New("operational store does not support invocation recovery")
	}
	active, err := activeStore.ActiveInvocation(ctx, run.ID)
	if err != nil {
		return ResumeResult{}, fmt.Errorf("read active invocation for resume: %w", err)
	}
	if active != nil {
		return l.resumeActiveInvocation(ctx, request, run, *active)
	}
	return l.resumeWithoutActiveInvocation(ctx, request, run)
}

// resumeWithoutActiveInvocation either re-enters a coordinator-owned check
// pause or launches the role selected by the persisted run.
func (l *invocationLifecycle) resumeWithoutActiveInvocation(ctx context.Context, request InvocationRecoveryRequest, run store.Run) (ResumeResult, error) {
	if run.Stage == store.StageCheck && isRestartReconciliationPause(run) {
		if l.hooks.resumeRecoveredCheck == nil {
			return ResumeResult{}, errors.New("recovered-check resume hook is required")
		}
		resumed, err := l.hooks.resumeRecoveredCheck(ctx, request.Registration, request.RunStore, run)
		if err != nil {
			return ResumeResult{Run: resumed}, err
		}
		return ResumeResult{Run: resumed}, nil
	}
	invocationID, err := l.recoveryInvocationID(request)
	if err != nil {
		return ResumeResult{Run: run}, err
	}
	launchRun := run
	launchRun.Status = store.StatusActive
	launch, err := l.Launch(ctx, InvocationLaunchRequest{Registration: request.Registration, RunStore: request.RunStore, Run: &launchRun, Request: normalizeAgentRequest(agentRequestForRun(run)), InvocationID: invocationID})
	if err != nil {
		return ResumeResult{Run: run, Invocation: launch.Invocation}, err
	}
	return ResumeResult{Run: launchRun, Invocation: launch.Invocation}, nil
}

// resumeActiveInvocation handles the manual-resume branch of Resume, including
// its typed waiting-state projections.
func (l *invocationLifecycle) resumeActiveInvocation(ctx context.Context, request InvocationRecoveryRequest, run store.Run, active store.Invocation) (ResumeResult, error) {
	if active.AttachRequired {
		return ResumeResult{Run: run, Invocation: active, WaitingForAttach: true}, &ManualResumeRequiredError{RunID: run.ID}
	}
	if strings.TrimSpace(active.NativeSessionID) == "" {
		if err := l.supersedeInvocation(ctx, request.Registration, request.RunStore, active); err != nil {
			return ResumeResult{}, err
		}
		return l.resumeWithoutActiveInvocation(ctx, request, run)
	}
	updatedInvocation, resumeErr := l.resumePersistedInvocationManually(ctx, request.Registration, request.RunStore, run, active)
	if resumeErr != nil {
		return l.resumeActiveInvocationError(ctx, request, run, active, updatedInvocation, resumeErr)
	}
	if !updatedInvocation.AttachRequired {
		return ResumeResult{Run: run, Invocation: updatedInvocation}, nil
	}
	next := run
	next.Status = store.StatusWaitingForHuman
	next.LifecycleReason = "manual native session resumed; attach is required before progression"
	next.UpdatedAt = l.clock().UTC()
	if next.Revision <= run.Revision {
		next.Revision = run.Revision + 1
	}
	if err := l.persistLifecycleRun(ctx, request.Registration, request.RunStore, run, next); err != nil {
		return ResumeResult{Run: next, Invocation: updatedInvocation, WaitingForAttach: true}, errors.Join(&ManualResumeRequiredError{RunID: run.ID}, err)
	}
	return ResumeResult{Run: next, Invocation: updatedInvocation, WaitingForAttach: true}, &ManualResumeRequiredError{RunID: run.ID}
}

// recoveryInvocationID resolves a fresh identity only on the branch that will
// call Launch, preserving recovery behavior for already persisted sessions.
func (l *invocationLifecycle) recoveryInvocationID(request InvocationRecoveryRequest) (string, error) {
	if strings.TrimSpace(request.InvocationID) != "" {
		return request.InvocationID, nil
	}
	if request.NewInvocationID == nil {
		return "", errors.New("generate invocation identifier: run id generator is required")
	}
	return request.NewInvocationID()
}

// resumeActiveInvocationError applies the recovery category policy for an
// explicit native resume.
func (l *invocationLifecycle) resumeActiveInvocationError(ctx context.Context, request InvocationRecoveryRequest, run store.Run, active, updated store.Invocation, resumeErr error) (ResumeResult, error) {
	var credentialErr *credentialProjectionError
	if errors.As(resumeErr, &credentialErr) {
		paused, pauseErr := l.pauseForAuthentication(ctx, request.Registration, request.RunStore, run, active.Harness)
		return ResumeResult{Run: paused, Invocation: updated}, errors.Join(credentialErr, pauseErr)
	}
	classified := classifyHarnessRuntimeErrorForInvocation(active, resumeErr)
	if harness.IsRateLimited(classified) {
		paused, pauseErr := l.pauseForHarnessCapacity(ctx, request.Registration, request.RunStore, run, active.Harness)
		return ResumeResult{Run: paused, Invocation: updated}, errors.Join(classified, pauseErr)
	}
	if harness.IsAuthenticationExpired(classified) {
		paused, pauseErr := l.pauseForAuthentication(ctx, request.Registration, request.RunStore, run, active.Harness)
		return ResumeResult{Run: paused, Invocation: updated}, errors.Join(classified, pauseErr)
	}
	if harness.IsUnexpectedExit(classified) {
		paused, pauseErr := l.pauseForManualRecovery(ctx, request.Registration, request.RunStore, run, active.Harness, classified)
		return ResumeResult{Run: paused, Invocation: updated}, errors.Join(classified, pauseErr)
	}
	return ResumeResult{Run: run, Invocation: updated}, resumeErr
}

// Attach restores worker and terminal topology, then releases the manual
// native-session gate for workflow progression.
func (l *invocationLifecycle) Attach(ctx context.Context, request InvocationRecoveryRequest) (AttachResult, error) {
	if request.Run == nil {
		return AttachResult{}, errors.New("no persisted run")
	}
	run := *request.Run
	if store.IsTerminalStatus(run.Status) {
		return AttachResult{}, fmt.Errorf("cannot attach terminal run %q with status %q", run.ID, run.Status)
	}
	activeStore, ok := request.RunStore.(ActiveInvocationStore)
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
	terminalRuntime, _, err := l.ensureAgentRuntime(request.Registration.Cmux.SocketPath, config.Harness(active.Harness))
	if err != nil {
		return AttachResult{}, fmt.Errorf("ensure agent runtime for attach: %w", err)
	}
	_, recovered, err := l.ensureWorkerForInvocation(ctx, request.Registration, request.RunStore, run, *active)
	if err != nil {
		return AttachResult{}, err
	}
	active = &recovered
	workspaceID, implementationSurface, statusSurface, checksSurface, restored, err := l.restoreInvocationTerminal(ctx, terminalRuntime, run, *active)
	if err != nil {
		return AttachResult{}, err
	}
	updated := *active
	updated.WorkspaceID = string(workspaceID)
	setInvocationSurface(&updated, implementationSurface)
	updated.StatusSurfaceID = string(statusSurface.ID)
	updated.ChecksSurfaceID = string(checksSurface.ID)
	if restored {
		resumed, resumeErr := l.resumePersistedInvocationManually(ctx, request.Registration, request.RunStore, run, updated)
		if resumeErr != nil {
			return l.attachResumeError(ctx, request, run, active, resumed, resumeErr)
		}
		updated = resumed
	} else if err := l.restoreCredentialProjection(ctx, request.Registration, run, updated); err != nil {
		paused, pauseErr := l.pauseForAuthentication(ctx, request.Registration, request.RunStore, run, active.Harness)
		return AttachResult{Run: paused, Invocation: updated}, errors.Join(err, pauseErr)
	}
	if restored || updated.AttachRequired {
		updated.AttachRequired = false
		updated.UpdatedAt = l.clock().UTC()
		invocationStore, ok := request.RunStore.(InvocationStore)
		if !ok {
			return AttachResult{Invocation: updated}, errors.New("operational store does not support invocation persistence")
		}
		if err := invocationStore.SaveInvocation(ctx, updated); err != nil {
			return AttachResult{Invocation: updated}, fmt.Errorf("persist attached invocation: %w", err)
		}
	}
	next := run
	next.Status = store.StatusActive
	next.LifecycleReason = "manual native session attached"
	next.UpdatedAt = l.clock().UTC()
	if next.Revision <= run.Revision {
		next.Revision = run.Revision + 1
	}
	if err := l.persistLifecycleRun(ctx, request.Registration, request.RunStore, run, next); err != nil {
		return AttachResult{Run: next, Invocation: updated}, err
	}
	l.resetStartupState()
	return AttachResult{Run: next, Invocation: updated}, nil
}

// attachResumeError maps credential failures during a recreated topology to
// the same human-waiting state as the regular attach path.
func (l *invocationLifecycle) attachResumeError(ctx context.Context, request InvocationRecoveryRequest, run store.Run, active *store.Invocation, resumed store.Invocation, resumeErr error) (AttachResult, error) {
	var credentialErr *credentialProjectionError
	if errors.As(resumeErr, &credentialErr) {
		paused, pauseErr := l.pauseForAuthentication(ctx, request.Registration, request.RunStore, run, active.Harness)
		return AttachResult{Run: paused, Invocation: resumed}, errors.Join(credentialErr, pauseErr)
	}
	return AttachResult{Invocation: resumed}, resumeErr
}

// RefreshAuth reseeds the registered harness credential source into the
// invocation's managed worker volume.
func (l *invocationLifecycle) refreshAuth(ctx context.Context, request InvocationRecoveryRequest, requestedHarness config.Harness) (AuthRefreshResult, error) {
	if request.Run == nil {
		return AuthRefreshResult{}, errors.New("no persisted run")
	}
	run := *request.Run
	if store.IsTerminalStatus(run.Status) {
		return AuthRefreshResult{}, fmt.Errorf("cannot refresh auth for terminal run %q", run.ID)
	}
	invocationStore, ok := request.RunStore.(InvocationStore)
	if !ok {
		return AuthRefreshResult{}, errors.New("operational store does not support invocation auth refresh")
	}
	invocation, err := latestInvocationForAuthRefresh(ctx, request.RunStore, run)
	if err != nil {
		return AuthRefreshResult{}, err
	}
	harnessName := requestedHarness
	if harnessName == "" {
		harnessName = config.Harness(invocation.Harness)
	}
	if string(harnessName) != invocation.Harness {
		return AuthRefreshResult{}, fmt.Errorf("auth refresh harness %q does not match invocation harness %q", harnessName, invocation.Harness)
	}
	seed, credentialStoreID, err := l.credentialSeeding(request.Registration, AgentRequest{}, harnessName)
	if err != nil {
		return AuthRefreshResult{}, err
	}
	if seed == nil || strings.TrimSpace(credentialStoreID) == "" {
		return AuthRefreshResult{}, fmt.Errorf("no factory-managed %s credential source is registered", harnessName)
	}
	if invocation.CredentialStoreID == "" {
		invocation.CredentialStoreID = credentialStoreID
		invocation.UpdatedAt = l.clock().UTC()
		if err := invocationStore.SaveInvocation(ctx, *invocation); err != nil {
			return AuthRefreshResult{}, fmt.Errorf("persist credential store identity: %w", err)
		}
	}
	_, recovered, err := l.ensureWorkerForInvocation(ctx, request.Registration, request.RunStore, run, *invocation)
	if err != nil {
		return AuthRefreshResult{}, err
	}
	*invocation = recovered
	if err := seed(ctx, run.ID, workerIDForInvocation(*invocation)); err != nil {
		return AuthRefreshResult{}, newCredentialProjectionError(string(harnessName))
	}
	return AuthRefreshResult{Run: run, Invocation: *invocation, Harness: harnessName}, nil
}

// Stop stops every worker currently delegated to the selected run.
func (l *invocationLifecycle) Stop(ctx context.Context, request InvocationStopRequest) error {
	return l.stopActiveRunWorkers(ctx, request.RunStore, request.Run)
}

// Launch gathers the read-only snapshot, plans admission, materialises the
// packet, and activates one visible invocation in that order.
func (l *invocationLifecycle) Launch(ctx context.Context, request InvocationLaunchRequest) (AgentLaunchResult, error) {
	snapshot, err := l.gatherLaunch(ctx, request)
	if err != nil {
		return AgentLaunchResult{}, err
	}
	plan, err := PlanLaunch(snapshot, snapshot.Request)
	if err != nil {
		return AgentLaunchResult{}, err
	}
	if plan.Outcome == LaunchOutcomeAdopt {
		return adoptedLaunchResult(plan), nil
	}
	materialised, err := l.materialiseLaunch(ctx, request.Registration, plan)
	if err != nil {
		return AgentLaunchResult{}, err
	}
	return l.activateLaunch(ctx, request, plan, materialised)
}

// gatherLaunch reads every projection needed by launch admission and context
// construction. It does not persist state or create filesystem resources.
func (l *invocationLifecycle) gatherLaunch(ctx context.Context, request InvocationLaunchRequest) (LaunchSnapshot, error) {
	invocationStore, ok := request.RunStore.(InvocationStore)
	if !ok {
		return LaunchSnapshot{}, errors.New("operational store does not support visible invocations")
	}
	if request.Run == nil {
		return LaunchSnapshot{}, errors.New("no active run")
	}
	run := *request.Run
	agentRequest := request.Request
	if agentRequest.RunID != "" && agentRequest.RunID != run.ID {
		return LaunchSnapshot{}, fmt.Errorf("active run is %s, not %s", run.ID, agentRequest.RunID)
	}
	if run.CheckRepairPendingAttempt != 0 {
		return LaunchSnapshot{}, fmt.Errorf("check-repair attempt %d is pending reconciliation", run.CheckRepairPendingAttempt)
	}
	if agentRequest.Harness == "" && run.HarnessOverride != "" {
		agentRequest.Harness = config.Harness(run.HarnessOverride)
	}
	if err := validateAgentRunState(run); err != nil {
		return LaunchSnapshot{}, err
	}
	if !filepath.IsAbs(run.Worktree) {
		return LaunchSnapshot{}, errors.New("active run worktree must be absolute")
	}
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return LaunchSnapshot{}, err
	}
	if !testRolePolicySatisfied(packet) {
		return LaunchSnapshot{}, errors.New("frozen repository packet lacks the mandatory test-stage role policy")
	}
	agentRequest, err = selectAgentRole(run, agentRequest)
	if err != nil {
		return LaunchSnapshot{}, err
	}
	if err := validateAgentRequest(agentRequest); err != nil {
		return LaunchSnapshot{}, err
	}
	roleDefinition, exists := workflow.DefaultRegistry().Role(agentRequest.Role)
	if !exists {
		return LaunchSnapshot{}, fmt.Errorf("agent role %q is not declared by the workflow registry", agentRequest.Role)
	}
	testRevision, reviewRepair, implementationResume, err := launchModes(run, agentRequest, roleDefinition)
	if err != nil {
		return LaunchSnapshot{}, err
	}
	snapshot := LaunchSnapshot{Run: run, Packet: packet, Request: agentRequest, InvocationID: request.InvocationID}
	snapshot.ResumeSource, err = l.gatherResumeSource(ctx, request.RunStore, invocationStore, run, testRevision, reviewRepair, implementationResume)
	if err != nil {
		return LaunchSnapshot{}, err
	}
	if !testRevision && !reviewRepair && !implementationResume {
		if latestStore, ok := request.RunStore.(LatestInvocationStore); ok {
			snapshot.LatestInvocation, err = latestStore.LatestInvocation(ctx, run.ID)
			if err != nil {
				return LaunchSnapshot{}, fmt.Errorf("look up latest invocation before launch: %w", err)
			}
		}
	}
	snapshot.ActiveInvocations, snapshot.ActiveInvocationsSupported, err = l.gatherActiveInvocations(ctx, invocationStore, run.ID, request.StartedHere)
	if err != nil {
		return LaunchSnapshot{}, err
	}
	if roleDefinition.Kind == workflow.RoleKindReview {
		if err := l.gatherReview(ctx, request.RunStore, run, agentRequest.Role, &snapshot); err != nil {
			return LaunchSnapshot{}, err
		}
	}
	if err := ensureBaselineReadyForLaunch(ctx, request.RunStore, run, packet); err != nil {
		return LaunchSnapshot{}, err
	}
	return snapshot, nil
}

// gatherResumeSource reads the one native-session source selected by launch
// mode, leaving source validation to pure admission.
func (l *invocationLifecycle) gatherResumeSource(ctx context.Context, runStore RunStore, invocationStore InvocationStore, run store.Run, testRevision, reviewRepair, implementationResume bool) (*store.Invocation, error) {
	if testRevision {
		id := run.TestInvocationID
		if id == "" && run.TestObjection != nil {
			id = run.TestObjection.InvocationID
		}
		if id == "" {
			return nil, nil
		}
		invocation, err := invocationStore.Invocation(ctx, run.ID, id)
		if err != nil {
			return nil, fmt.Errorf("read original test invocation for objection revision: %w", err)
		}
		return invocation, nil
	}
	if reviewRepair || implementationResume {
		return latestImplementationInvocation(ctx, runStore, run.ID)
	}
	return nil, nil
}

// gatherActiveInvocations reads active invocation rows and records process-local
// ownership as an observation rather than allowing admission to touch the map.
func (l *invocationLifecycle) gatherActiveInvocations(ctx context.Context, invocationStore InvocationStore, runID string, startedHere func(string) bool) ([]ActiveLaunchObservation, bool, error) {
	active, supported, err := activeInvocationsForRun(ctx, invocationStore, runID)
	if err != nil {
		return nil, supported, fmt.Errorf("look up active visible invocations: %w", err)
	}
	observations := make([]ActiveLaunchObservation, 0, len(active))
	for _, invocation := range active {
		observation := ActiveLaunchObservation{Invocation: invocation}
		if startedHere != nil {
			observation.StartedHere = startedHere(invocation.ID)
		}
		observations = append(observations, observation)
	}
	return observations, supported, nil
}

// gatherReview reads the immutable review preconditions and exact review
// context, including the bounded diff, before activation can publish status.
func (l *invocationLifecycle) gatherReview(ctx context.Context, runStore RunStore, run store.Run, role string, snapshot *LaunchSnapshot) error {
	if err := ensureReviewStartForRoleWithInspector(ctx, run, role, l.worktree); err != nil {
		return err
	}
	reviewContext, err := reviewContextForRole(ctx, run, runStore, role)
	if err != nil {
		return err
	}
	invocation := store.Invocation{ID: snapshot.InvocationID, RunID: run.ID, Role: role, Stage: snapshot.Request.Stage}
	if l.hooks.captureReviewDiff != nil {
		capture, captureErr := l.hooks.captureReviewDiff(ctx, run, invocation)
		if captureErr != nil {
			return captureErr
		}
		reviewContext.CurrentDiff = capture.currentDiff
		reviewContext.OmittedDiffBytes = capture.omittedDiffBytes
		reviewContext.ChangedPathsCommand = capture.changedPathsCommand
		reviewContext.DiffPathCommand = capture.diffPathCommand
	}
	snapshot.ReviewContext = reviewContext
	return nil
}

// adoptedLaunchResult converts the pure adoption plan into the public result;
// the coordinator sequencer marks the invocation after this function returns.
func adoptedLaunchResult(plan LaunchPlan) AgentLaunchResult {
	return AgentLaunchResult{Invocation: *plan.AdoptedInvocation, TestPolicyMode: testPolicyModeForRun(plan.Run), Route: plan.Packet.Route}
}

// PlanLaunch applies all launch policy to a gathered snapshot without a
// context, adapter, or write. Its error is the rejection outcome; successful
// plans are either a fresh launch or an adoption.
func PlanLaunch(snapshot LaunchSnapshot, request AgentRequest) (LaunchPlan, error) {
	if err := validateLaunchRun(snapshot, request); err != nil {
		return LaunchPlan{}, err
	}
	request, err := selectAgentRole(snapshot.Run, request)
	if err != nil {
		return LaunchPlan{}, err
	}
	if err := validateAgentRequest(request); err != nil {
		return LaunchPlan{}, err
	}
	roleDefinition, exists := workflow.DefaultRegistry().Role(request.Role)
	if !exists {
		return LaunchPlan{}, fmt.Errorf("agent role %q is not declared by the workflow registry", request.Role)
	}
	testRevision, reviewRepair, implementationResume, err := launchModes(snapshot.Run, request, roleDefinition)
	if err != nil {
		return LaunchPlan{}, err
	}
	if roleDefinition.Kind == workflow.RoleKindTest && snapshot.Run.TestRevisionBudget == 0 {
		snapshot.Run.TestRevisionBudget = snapshot.Packet.RepositoryConfig.RetryLimits.TestRevision
	}
	resumeSource := snapshot.ResumeSource
	if (reviewRepair || implementationResume) && resumeSource != nil && strings.TrimSpace(resumeSource.NativeSessionID) == "" {
		resumeSource = nil
		snapshot.ResumeSource = nil
	}
	if err := validateLaunchSources(snapshot, testRevision, reviewRepair, implementationResume); err != nil {
		return LaunchPlan{}, err
	}
	if len(request.PermittedPaths) == 0 {
		request.PermittedPaths = append([]string(nil), roleDefinition.DefaultPermittedPaths...)
	}
	if err := report.ValidatePermittedPaths(request.PermittedPaths); err != nil {
		return LaunchPlan{}, err
	}
	adopted, err := validateLaunchHistory(snapshot, request, testRevision, reviewRepair, implementationResume, roleDefinition)
	if err != nil {
		return LaunchPlan{}, err
	}
	if adopted != nil {
		return LaunchPlan{Outcome: LaunchOutcomeAdopt, Run: snapshot.Run, Request: request, Packet: snapshot.Packet, RoleDefinition: roleDefinition, InvocationID: snapshot.InvocationID, TestRevision: testRevision, ReviewRepair: reviewRepair, ImplementationResume: implementationResume, AdoptedInvocation: adopted}, nil
	}
	if err := validateLaunchStagePolicy(snapshot.Run, snapshot.Packet, request, roleDefinition); err != nil {
		return LaunchPlan{}, err
	}
	policy, err := resolveAgentPolicy(snapshot.Packet.RepositoryConfig, request)
	if err != nil {
		return LaunchPlan{}, err
	}
	if resumeSource != nil {
		if resumeSource.Harness != string(policy.Harness) {
			return LaunchPlan{}, fmt.Errorf("test objection must resume the original %s harness, not %s", resumeSource.Harness, policy.Harness)
		}
		if resumeSource.Model != "" {
			policy.Model = resumeSource.Model
		}
		if resumeSource.ReasoningEffort != "" {
			policy.ReasoningEffort = resumeSource.ReasoningEffort
		}
	}
	return LaunchPlan{Outcome: LaunchOutcomeLaunch, Run: snapshot.Run, Request: request, Packet: snapshot.Packet, RoleDefinition: roleDefinition, ResumeSource: resumeSource, ReviewContext: snapshot.ReviewContext, DesignHandoff: designHandoffForInvocation(snapshot.Run, snapshot.Packet, roleDefinition), Policy: policy, InvocationID: snapshot.InvocationID, TestRevision: testRevision, ReviewRepair: reviewRepair, ImplementationResume: implementationResume}, nil
}

// validateLaunchRun preserves run identity, repair, status, stage, worktree,
// and frozen test-policy guards at the pure admission seam.
func validateLaunchRun(snapshot LaunchSnapshot, request AgentRequest) error {
	if snapshot.Run.ID == "" {
		return errors.New("no active run")
	}
	if request.RunID != "" && request.RunID != snapshot.Run.ID {
		return fmt.Errorf("active run is %s, not %s", snapshot.Run.ID, request.RunID)
	}
	if snapshot.Run.CheckRepairPendingAttempt != 0 {
		return fmt.Errorf("check-repair attempt %d is pending reconciliation", snapshot.Run.CheckRepairPendingAttempt)
	}
	if snapshot.Run.Status != store.StatusActive {
		return fmt.Errorf("cannot start visible agent from run status %q", snapshot.Run.Status)
	}
	registry := workflow.DefaultRegistry()
	if _, exists := registry.RoleForRunStage(snapshot.Run.Stage); !exists {
		if _, invocationStage := registry.RoleForInvocationStage(snapshot.Run.Stage); !invocationStage {
			return fmt.Errorf("cannot start visible agent from run stage %q", snapshot.Run.Stage)
		}
	}
	if !filepath.IsAbs(snapshot.Run.Worktree) {
		return errors.New("active run worktree must be absolute")
	}
	if !testRolePolicySatisfied(snapshot.Packet) {
		return errors.New("frozen repository packet lacks the mandatory test-stage role policy")
	}
	return nil
}

// launchModes validates and classifies the coordinator-internal launch modes.
func launchModes(run store.Run, request AgentRequest, role workflow.RoleDefinition) (bool, bool, bool, error) {
	testRevision := role.Kind == workflow.RoleKindTest && run.Stage == store.StageTest && run.TestObjection != nil
	reviewRepair := request.reviewRepair && request.Role == workflow.RoleImplementation && request.Stage == store.StageImplementation && run.Stage == store.StageImplementation
	implementationResume := request.resumeImplementation && request.Role == workflow.RoleImplementation && request.Stage == store.StageImplementation && run.Stage == store.StageImplementation
	switch {
	case request.reviewRepair && !reviewRepair:
		return false, false, false, errors.New("review repair requires the implementation role at the implementation stage")
	case request.resumeImplementation && !implementationResume:
		return false, false, false, errors.New("implementation resume requires the implementation role at the implementation stage")
	case reviewRepair && implementationResume:
		return false, false, false, errors.New("review repair and implementation resume are mutually exclusive")
	case reviewRepair && run.ReviewRepairPacket == nil:
		return false, false, false, errors.New("review repair requires a persisted review-repair packet")
	case request.testRevision && !testRevision:
		return false, false, false, errors.New("test revision requires an active test objection")
	}
	return testRevision, reviewRepair, implementationResume, nil
}

// validateLaunchSources verifies native-session sources gathered before
// admission and preserves the old fresh-session fallback for harnesses without
// native resume.
func validateLaunchSources(snapshot LaunchSnapshot, testRevision, reviewRepair, implementationResume bool) error {
	source := snapshot.ResumeSource
	if testRevision {
		if source == nil {
			invocationID := snapshot.Run.TestInvocationID
			if invocationID == "" && snapshot.Run.TestObjection != nil {
				invocationID = snapshot.Run.TestObjection.InvocationID
			}
			if invocationID != "" {
				return fmt.Errorf("original test invocation %q does not belong to run %q", invocationID, snapshot.Run.ID)
			}
			return errors.New("test objection has no original test invocation")
		}
		if source.Role != workflow.RoleTest || source.Stage != store.StageTest {
			return errors.New("test objection original invocation is not a test-stage invocation")
		}
		if source.Status == store.InvocationStatusActive {
			return fmt.Errorf("original test invocation %q is still active", source.ID)
		}
		if strings.TrimSpace(source.NativeSessionID) == "" {
			return errors.New("original test invocation has no native session identifier")
		}
	}
	if reviewRepair || implementationResume {
		if source != nil && source.Status == store.InvocationStatusActive {
			return fmt.Errorf("latest implementation invocation %q is still active", source.ID)
		}
	}
	return nil
}

// validateLaunchHistory preserves duplicate-history and active-invocation
// guards while making adoption a pure third admission outcome.
func validateLaunchHistory(snapshot LaunchSnapshot, request AgentRequest, testRevision, reviewRepair, implementationResume bool, role workflow.RoleDefinition) (*store.Invocation, error) {
	if snapshot.LatestInvocation != nil && !testRevision && !reviewRepair && !implementationResume && snapshot.LatestInvocation.Status != store.InvocationStatusSuperseded && snapshot.LatestInvocation.Role == request.Role && snapshot.LatestInvocation.Stage == request.Stage {
		return nil, fmt.Errorf("run %q already has invocation history for %s/%s", snapshot.Run.ID, request.Role, request.Stage)
	}
	if !snapshot.ActiveInvocationsSupported {
		return nil, nil
	}
	isReview := role.Kind == workflow.RoleKindReview
	for _, active := range snapshot.ActiveInvocations {
		if active.Invocation.AttachRequired {
			return nil, fmt.Errorf("run %q has a manually resumed invocation that requires `factory attach`", snapshot.Run.ID)
		}
		if isReview && roleIsKind(active.Invocation, workflow.RoleKindReview) && active.Invocation.Role != request.Role {
			continue
		}
		if !active.StartedHere && active.Invocation.RecoveryResumeCount > 0 && active.Invocation.Role == request.Role {
			adopted := active.Invocation
			return &adopted, nil
		}
		return nil, fmt.Errorf("run %q already has active invocation %q", snapshot.Run.ID, active.Invocation.ID)
	}
	return nil, nil
}

// validateLaunchStagePolicy preserves the packet-only stage policy guards that
// remain pure after gather and adoption.
func validateLaunchStagePolicy(run store.Run, packet SpecificationPacket, request AgentRequest, role workflow.RoleDefinition) error {
	if role.Kind == workflow.RoleKindTest && !independentTestStageDeclared(packet) {
		return errors.New("test role is unavailable in advisory mode without a selected route; implementation owns TDD")
	}
	if role.RequiresTestHandoff && independentTestStageDeclared(packet) && run.Stage == store.StageClaim && !run.TestStageSkipped {
		return errors.New("implementation agent cannot bypass the configured test stage")
	}
	return nil
}

// launchMaterialisation contains the packet, prompt, invocation identity, and
// worker request prepared before any activation effect crosses an adapter.
type launchMaterialisation struct {
	root             string
	packetDirectory  string
	resultDirectory  string
	workerID         string
	invocation       store.Invocation
	invocationPacket InvocationPacket
	promptText       string
	workerRequest    worker.StartRequest
}

// launchWorkspace groups the control and run terminal handles used by
// activation without widening the terminal adapter interface.
type launchWorkspace struct {
	control terminal.Workspace
	run     terminal.RunWorkspace
}

// launchContextValues selects the run handoffs that belong in a new packet.
type launchContextValues struct {
	testHandoff         *store.TestHandoff
	testObjection       *store.TestObjection
	testRevisionAttempt int
	testRevisionBudget  int
	reviewRepair        *store.ReviewRepairPacket
	protectedTestPaths  []store.ProtectedTestPath
	testExemption       *store.TestExemption
}

// materialiseLaunch creates the packet and result directories, writes the
// immutable invocation packet, and prepares Git metadata for activation.
func (l *invocationLifecycle) materialiseLaunch(ctx context.Context, registration config.RepositoryRegistration, plan LaunchPlan) (launchMaterialisation, error) {
	root := invocationRoot(plan.Run, plan.InvocationID)
	packetDirectory := filepath.Join(root, "packet")
	resultDirectory := filepath.Join(root, "results")
	if err := os.MkdirAll(packetDirectory, 0o700); err != nil {
		return launchMaterialisation{}, fmt.Errorf("create invocation packet directory: %w", err)
	}
	if err := os.MkdirAll(resultDirectory, 0o700); err != nil {
		return launchMaterialisation{}, fmt.Errorf("create invocation result directory: %w", err)
	}
	if err := validateRepositoryCraftPacket(plan.Packet, plan.InvocationID); err != nil {
		return launchMaterialisation{}, err
	}
	craft, err := frozenRepositoryCraftForRole(plan.Packet, plan.Request.Role, plan.InvocationID)
	if err != nil {
		return launchMaterialisation{}, err
	}
	contextValues := launchContextForPlan(plan)
	invocation := newLaunchInvocation(plan, packetDirectory, resultDirectory, craft, l.clock().UTC())
	invocationPacket, promptText, err := buildLaunchPacket(plan, invocation, craft, contextValues)
	if err != nil {
		return launchMaterialisation{}, err
	}
	if err := writeInvocationPacket(packetDirectory, invocationPacket); err != nil {
		return launchMaterialisation{}, err
	}
	gitMetadataPath, err := prepareGitMetadataProjection(plan.Run.ID, registration.Path, plan.Run.Worktree)
	if err != nil {
		return launchMaterialisation{}, fmt.Errorf("prepare worker Git metadata: %w", err)
	}
	workerID := workerIDForInvocation(invocation)
	workerRequest := worker.StartRequest{RunID: plan.Run.ID, WorkerID: workerID, WorktreeReadOnly: plan.RoleDefinition.Kind == workflow.RoleKindReview, WorktreePath: plan.Run.Worktree, GitMetadataPath: gitMetadataPath, Image: plan.Packet.RepositoryConfig.WorkerBuild.Image, ImageDigest: plan.Run.ImageDigest, Caches: workerCaches(plan.Packet.RepositoryConfig.Caches), InvocationPath: packetDirectory, ResultPath: resultDirectory, Role: plan.Request.Role}
	return launchMaterialisation{root: root, packetDirectory: packetDirectory, resultDirectory: resultDirectory, workerID: workerID, invocation: invocation, invocationPacket: invocationPacket, promptText: promptText, workerRequest: workerRequest}, nil
}

// launchContextForPlan copies the run context needed by a role and clears
// upstream test state for isolated review invocations.
func launchContextForPlan(plan LaunchPlan) launchContextValues {
	values := launchContextValues{testHandoff: plan.Run.TestHandoff, testObjection: plan.Run.TestObjection, testRevisionAttempt: plan.Run.TestRevisionAttempts, testRevisionBudget: plan.Run.TestRevisionBudget, reviewRepair: plan.Run.ReviewRepairPacket, protectedTestPaths: append([]store.ProtectedTestPath(nil), plan.Run.ProtectedTestPaths...), testExemption: plan.Run.TestExemption}
	if plan.RoleDefinition.Kind == workflow.RoleKindReview {
		return launchContextValues{}
	}
	return values
}

// newLaunchInvocation creates the durable identity before activation persists
// it, keeping directory and prompt metadata tied to one immutable ID.
func newLaunchInvocation(plan LaunchPlan, packetDirectory, resultDirectory string, craft *RepositoryCraftDocument, createdAt time.Time) store.Invocation {
	sourcePath, sourceSHA := craftMetadataForInvocation(craft)
	invocation := store.Invocation{ID: plan.InvocationID, RunID: plan.Run.ID, Harness: string(plan.Policy.Harness), Role: plan.Request.Role, Stage: plan.Request.Stage, Model: plan.Policy.Model, ReasoningEffort: plan.Policy.ReasoningEffort, InvocationDirectory: packetDirectory, ResultDirectory: resultDirectory, PermittedPaths: append([]string(nil), plan.Request.PermittedPaths...), PromptVersion: plan.RoleDefinition.PromptVersion, PromptCraftSourcePath: sourcePath, PromptCraftSHA256: sourceSHA, Status: store.InvocationStatusActive, CreatedAt: createdAt, UpdatedAt: createdAt}
	if plan.ResumeSource != nil {
		invocation.NativeSessionID = plan.ResumeSource.NativeSessionID
	}
	return invocation
}

// buildLaunchPacket builds and writes no external state; the caller owns the
// final packet write so materialisation has one visible filesystem boundary.
func buildLaunchPacket(plan LaunchPlan, invocation store.Invocation, craft *RepositoryCraftDocument, values launchContextValues) (InvocationPacket, string, error) {
	specification, err := promptSpecification(plan.Packet)
	if err != nil {
		return InvocationPacket{}, "", err
	}
	packet := InvocationPacket{SchemaVersion: invocationPacketVersion, InvocationID: invocation.ID, RunID: plan.Run.ID, Role: invocation.Role, Stage: invocation.Stage, SpecificationPacket: plan.Run.SpecificationPacket, PromptVersion: invocation.PromptVersion, PromptCraftSourcePath: invocation.PromptCraftSourcePath, PromptCraftSHA256: invocation.PromptCraftSHA256, TestPolicyMode: plan.Packet.RepositoryConfig.TestPolicy.Mode, Route: plan.Packet.Route, DesignHandoff: plan.DesignHandoff, PermittedPaths: append([]string(nil), invocation.PermittedPaths...), TestHandoff: values.testHandoff, TestObjection: values.testObjection, TestRevisionAttempt: values.testRevisionAttempt, TestRevisionBudget: values.testRevisionBudget, ProtectedTestPaths: values.protectedTestPaths, TestExemption: values.testExemption, ReviewRepair: values.reviewRepair, ReviewContext: plan.ReviewContext, Continuation: plan.ResumeSource != nil}
	promptText, err := prompt.Build(prompt.Request{InvocationID: invocation.ID, RunID: plan.Run.ID, Role: invocation.Role, Stage: string(invocation.Stage), SpecificationPacket: specification, Continuation: packet.Continuation, RepositoryGuidance: plan.Packet.RepositoryGuidance, RepositoryCraft: repositoryCraftContent(craft), PromptVersion: invocation.PromptVersion, TestPolicyMode: string(plan.Packet.RepositoryConfig.TestPolicy.Mode), Route: plan.Packet.Route, DesignHandoff: plan.DesignHandoff, TestHandoff: values.testHandoff, TestObjection: values.testObjection, TestRevisionAttempt: values.testRevisionAttempt, TestRevisionBudget: values.testRevisionBudget, ProtectedTestPaths: values.protectedTestPaths, TestExemption: values.testExemption, ReviewRepair: values.reviewRepair, TestPaths: plan.Packet.RepositoryConfig.TestPolicy.TestPaths, TestInfrastructurePaths: plan.Packet.RepositoryConfig.TestPolicy.InfrastructurePaths, ReviewContext: plan.ReviewContext})
	return packet, promptText, err
}

// activateLaunch performs all durable and external launch effects after
// materialisation has succeeded. Review status publication deliberately begins
// here, after the packet and result directories exist.
func (l *invocationLifecycle) activateLaunch(ctx context.Context, request InvocationLaunchRequest, plan LaunchPlan, materialised launchMaterialisation) (result AgentLaunchResult, returnErr error) {
	invocationStore, ok := request.RunStore.(InvocationStore)
	if !ok {
		return AgentLaunchResult{}, errors.New("operational store does not support visible invocations")
	}
	if plan.RoleDefinition.Kind == workflow.RoleKindReview {
		if l.hooks.publishReviewStatus == nil {
			return AgentLaunchResult{}, fmt.Errorf("commit-status publisher is required for %s", plan.Request.Role)
		}
		if err := l.hooks.publishReviewStatus(ctx, request.Registration, request.RunStore, plan.Run, plan.Request.Role, github.CommitStatusPending, fmt.Sprintf("%s in progress", plan.Request.Role)); err != nil {
			return AgentLaunchResult{}, fmt.Errorf("publish pending %s status: %w", plan.Request.Role, err)
		}
	}
	seedCredentials, credentialStoreID, err := l.credentialSeeding(request.Registration, plan.Request, plan.Policy.Harness)
	if err != nil {
		return AgentLaunchResult{}, err
	}
	terminalRuntime, harnessRuntime, err := l.ensureAgentRuntime(request.Registration.Cmux.SocketPath, plan.Policy.Harness)
	if err != nil {
		return AgentLaunchResult{}, fmt.Errorf("ensure agent runtime: %w", err)
	}
	invocation := materialised.invocation
	invocation.CredentialStoreID = credentialStoreID
	materialised.workerRequest.CredentialStoreID = credentialStoreID
	invocationPersisted, workerStarted, preserveInvocation := false, false, false
	cleanupSurface := terminal.Surface{}
	defer func() {
		if returnErr != nil && invocationPersisted && !preserveInvocation {
			returnErr = l.rollbackLaunch(ctx, request.RunStore, invocation, materialised.workerID, workerStarted, cleanupSurface, terminalRuntime, returnErr)
		}
	}()
	if err := invocationStore.SaveInvocation(ctx, invocation); err != nil {
		return AgentLaunchResult{}, fmt.Errorf("persist visible invocation: %w", err)
	}
	invocationPersisted = true
	if err := l.recordLaunchEvaluation(ctx, request.RunStore, *request.Run, invocation); err != nil {
		return AgentLaunchResult{}, err
	}
	if l.hooks.startWorker == nil {
		return AgentLaunchResult{}, errors.New("worker runtime is required")
	}
	if err := l.hooks.startWorker(ctx, request.RunStore, materialised.workerRequest); err != nil {
		return AgentLaunchResult{}, fmt.Errorf("start worker for visible agent: %w", err)
	}
	workerStarted = true
	if seedCredentials != nil {
		if err := seedCredentials(ctx, request.Run.ID, materialised.workerID); err != nil {
			return AgentLaunchResult{}, newCredentialProjectionError(string(plan.Policy.Harness))
		}
	}
	workspace, agentSurface, err := l.ensureLaunchTerminal(ctx, request.Registration, *request.Run, plan.RoleDefinition)
	if err != nil {
		return AgentLaunchResult{}, err
	}
	cleanupSurface = agentSurface
	applyLaunchSurfaces(&invocation, workspace, agentSurface)
	if err := invocationStore.SaveInvocation(ctx, invocation); err != nil {
		return AgentLaunchResult{}, fmt.Errorf("persist visible terminal handles: %w", err)
	}
	if err := terminalRuntime.Notify(ctx, terminal.Notification{WorkspaceID: workspace.control.ID, Title: "factory run started", Body: fmt.Sprintf("%s %s agent active", plan.Run.ID, plan.Request.Role)}); err != nil {
		return AgentLaunchResult{}, err
	}
	session, updatedInvocation, preserved, err := l.startLaunchHarness(ctx, request, plan, materialised, terminalRuntime, harnessRuntime, invocation, workspace.run.ID, agentSurface)
	if updatedInvocation.ID != "" {
		invocation = updatedInvocation
	}
	if preserved {
		preserveInvocation = true
	}
	if err != nil {
		return AgentLaunchResult{Invocation: invocation}, err
	}
	if session.NativeSessionID != "" {
		invocation.NativeSessionID = session.NativeSessionID
	}
	if session.Surface.ID != "" {
		agentSurface = session.Surface
		cleanupSurface = agentSurface
		setInvocationSurface(&invocation, agentSurface)
	}
	if err := invocationStore.SaveInvocation(ctx, invocation); err != nil {
		return AgentLaunchResult{}, fmt.Errorf("persist harness session identity: %w", err)
	}
	if err := l.commitLaunchRun(ctx, request, plan, invocation); err != nil {
		return AgentLaunchResult{}, err
	}
	return AgentLaunchResult{Invocation: invocation, Prompt: materialised.promptText, TestPolicyMode: plan.Packet.RepositoryConfig.TestPolicy.Mode, Route: plan.Packet.Route}, nil
}

// recordLaunchEvaluation records only the content-free invocation metadata when
// the open store exposes the optional evaluation recorder seam.
func (l *invocationLifecycle) recordLaunchEvaluation(ctx context.Context, runStore RunStore, run store.Run, invocation store.Invocation) error {
	recorder, ok := runStore.(evaluationRecorder)
	if !ok {
		return nil
	}
	if err := recorder.EnsureEvaluationSummary(ctx, run); err != nil {
		return fmt.Errorf("ensure local evaluation summary: %w", err)
	}
	if err := recorder.RecordEvaluationInvocation(ctx, run.ID, invocation, run.ImageDigest, report.SchemaVersion); err != nil {
		return fmt.Errorf("record local evaluation invocation: %w", err)
	}
	return nil
}

// ensureLaunchTerminal restores the control and run workspaces and selects the
// role-owned surface strategy for a fresh invocation.
func (l *invocationLifecycle) ensureLaunchTerminal(ctx context.Context, registration config.RepositoryRegistration, run store.Run, role workflow.RoleDefinition) (launchWorkspace, terminal.Surface, error) {
	if l.terminal == nil {
		return launchWorkspace{}, terminal.Surface{}, errors.New("terminal runtime is required")
	}
	control, err := l.terminal.EnsureControlWorkspace(ctx, terminal.WorkspaceRequest{Name: defaultString(registration.Cmux.ControlWorkspace, "factory-control"), Description: "software factory coordinator", WorkingDirectory: registration.Path})
	if err != nil {
		return launchWorkspace{}, terminal.Surface{}, err
	}
	runWorkspace, err := l.terminal.EnsureRunWorkspace(ctx, terminal.RunWorkspaceRequest{RunID: run.ID, Name: "factory-" + run.ID, Description: fmt.Sprintf("factory run for issue #%d", run.IssueNumber), WorkingDirectory: run.Worktree})
	if err != nil {
		return launchWorkspace{}, terminal.Surface{}, err
	}
	agentSurface := runWorkspace.Implementation
	switch role.Surface {
	case workflow.SurfaceChecks:
		agentSurface = runWorkspace.Checks
	case workflow.SurfaceRole:
		agentSurface = terminal.Surface{WorkspaceID: runWorkspace.ID, Name: role.Name}
	}
	return launchWorkspace{control: control, run: runWorkspace}, agentSurface, nil
}

// applyLaunchSurfaces projects terminal handles onto the persisted invocation.
func applyLaunchSurfaces(invocation *store.Invocation, workspace launchWorkspace, agentSurface terminal.Surface) {
	invocation.WorkspaceID = string(workspace.run.ID)
	invocation.StatusSurfaceID = string(workspace.run.Status.ID)
	setInvocationSurface(invocation, agentSurface)
	invocation.ChecksSurfaceID = string(workspace.run.Checks.ID)
}

// startLaunchHarness starts or resumes the native session after its worker,
// credential projection, and terminal handles are durable. Typed waiting
// outcomes retain the invocation for the recovery path; other failures return
// no public launch result so rollback can close the partial attempt.
func (l *invocationLifecycle) startLaunchHarness(ctx context.Context, request InvocationLaunchRequest, plan LaunchPlan, materialised launchMaterialisation, terminalRuntime terminal.TerminalRuntime, harnessRuntime harness.Runtime, invocation store.Invocation, workspaceID terminal.WorkspaceID, surface terminal.Surface) (harness.Session, store.Invocation, bool, error) {
	invocationStore, ok := request.RunStore.(InvocationStore)
	if !ok {
		return harness.Session{}, store.Invocation{}, false, errors.New("operational store does not support visible invocations")
	}
	if harnessRuntime == nil {
		return harness.Session{}, store.Invocation{}, false, errors.New("harness runtime is required")
	}
	startRequest := harness.StartRequest{
		InvocationID: invocation.ID, RunID: plan.Run.ID, WorkerID: materialised.workerID,
		Role: invocation.Role, Stage: string(invocation.Stage),
		CheckpointSHA: reviewCheckpointSHA(plan.RoleDefinition.Kind == workflow.RoleKindReview, plan.Run.CheckpointSHA),
		WorkspaceID:   workspaceID, Surface: surface, Prompt: materialised.promptText,
		Model: invocation.Model, ReasoningEffort: invocation.ReasoningEffort,
	}
	var session harness.Session
	var err error
	if plan.ResumeSource != nil {
		startRequest.ResumeSessionID = plan.ResumeSource.NativeSessionID
		session, err = harnessRuntime.Resume(ctx, startRequest)
	} else {
		session, err = harnessRuntime.Start(ctx, startRequest)
	}
	if err == nil {
		return session, invocation, false, nil
	}
	if strings.TrimSpace(session.NativeSessionID) != "" {
		invocation.NativeSessionID = session.NativeSessionID
	}
	classified := harness.ClassifyError(err, string(plan.Policy.Harness))
	if diagnostic := writeHarnessFailureDiagnostic(materialised.root, "launch", err, harness.LaunchTranscript(err), l.clock().UTC()); diagnostic != "" {
		classified = fmt.Errorf("%w (launch diagnostic: %s)", classified, diagnostic)
	}
	if harness.IsRateLimited(classified) {
		return l.retainLaunchAfterHarnessFailure(ctx, request, plan, invocationStore, invocation, classified, func() (store.Run, error) {
			return l.pauseForHarnessCapacity(ctx, request.Registration, request.RunStore, plan.Run, string(plan.Policy.Harness))
		})
	}
	if harness.IsAuthenticationExpired(classified) {
		return l.retainLaunchAfterHarnessFailure(ctx, request, plan, invocationStore, invocation, classified, func() (store.Run, error) {
			return l.pauseForAuthentication(ctx, request.Registration, request.RunStore, plan.Run, string(plan.Policy.Harness))
		})
	}
	if harness.IsUnexpectedExit(classified) {
		return l.retainLaunchAfterHarnessFailure(ctx, request, plan, invocationStore, invocation, classified, func() (store.Run, error) {
			return l.pauseForManualRecovery(ctx, request.Registration, request.RunStore, plan.Run, string(plan.Policy.Harness), classified)
		})
	}
	return harness.Session{}, store.Invocation{}, false, fmt.Errorf("launch %s %s agent: %w", plan.Policy.Harness, invocation.Role, classified)
}

// retainLaunchAfterHarnessFailure persists the invocation boundary before
// moving the run into one of the typed waiting states.
func (l *invocationLifecycle) retainLaunchAfterHarnessFailure(ctx context.Context, request InvocationLaunchRequest, plan LaunchPlan, invocationStore InvocationStore, invocation store.Invocation, cause error, pause func() (store.Run, error)) (harness.Session, store.Invocation, bool, error) {
	invocation.Status = store.InvocationStatusSuperseded
	invocation.UpdatedAt = l.clock().UTC()
	if err := invocationStore.SaveInvocation(ctx, invocation); err != nil {
		return harness.Session{}, store.Invocation{}, false, fmt.Errorf("persist harness failure state: %w", err)
	}
	if _, err := pause(); err != nil {
		return harness.Session{}, invocation, true, errors.Join(cause, err)
	}
	return harness.Session{}, invocation, true, cause
}

// commitLaunchRun applies the run projection only after the harness has
// returned a durable session identity.
func (l *invocationLifecycle) commitLaunchRun(ctx context.Context, request InvocationLaunchRequest, plan LaunchPlan, invocation store.Invocation) error {
	if request.Run == nil {
		return errors.New("no active run")
	}
	previous := *request.Run
	next := previous
	if plan.RoleDefinition.Kind == workflow.RoleKindReview {
		next.Stage = store.StageReview
	} else {
		next.Stage = plan.Request.Stage
	}
	next.Status = store.StatusActive
	if consumesBoundedRepairRound(plan.ReviewRepair, next.ReviewRepairPacket) {
		next.ReviewRepairAttempts = next.ReviewRepairPacket.Attempt
		next.ReviewRepairPendingAttempt = 0
		next.ReviewRepairHistory = reviewRepairHistoryWithOutcome(next.ReviewRepairHistory, next.ReviewRepairPacket.Attempt, store.ReviewRepairStarted, next.ReviewRepairPacket)
	}
	addActiveInvocation(&next, invocation.ID)
	if plan.RoleDefinition.Kind == workflow.RoleKindTest {
		next.TestInvocationID = invocation.ID
		if next.TestRevisionBudget == 0 {
			next.TestRevisionBudget = plan.Packet.RepositoryConfig.RetryLimits.TestRevision
		}
	}
	next.UpdatedAt = l.clock().UTC()
	if l.hooks.persistRun == nil {
		return errors.New("run persistence hook is required")
	}
	if err := l.hooks.persistRun(ctx, request.Registration, request.RunStore, previous, next); err != nil {
		return fmt.Errorf("persist %s stage: %w", plan.Request.Role, err)
	}
	*request.Run = next
	if plan.RoleDefinition.Kind == workflow.RoleKindReview && l.hooks.refreshReviewPullRequest != nil {
		if err := l.hooks.refreshReviewPullRequest(ctx, request.Registration, request.RunStore, next); err != nil {
			return err
		}
	}
	return nil
}

// rollbackLaunch closes a launch that failed before a native session or
// structured report made it recoverable, while preserving any indeterminate
// journal boundary for reconciliation.
func (l *invocationLifecycle) rollbackLaunch(ctx context.Context, runStore RunStore, invocation store.Invocation, workerID string, workerStarted bool, cleanupSurface terminal.Surface, terminalRuntime terminal.TerminalRuntime, original error) error {
	invocationStore, ok := runStore.(InvocationStore)
	if !ok {
		return original
	}
	pendingIndeterminate := false
	if journal, journaled := runStore.(PendingEffectStore); journaled {
		pending, err := journal.PendingEffect(context.WithoutCancel(ctx), invocation.RunID)
		if err != nil {
			pendingIndeterminate = true
			original = fmt.Errorf("%w; durable pending-effect lookup was indeterminate; invocation kept protected: %v", original, err)
		}
		if pending != nil {
			return original
		}
	}
	voided := false
	if !pendingIndeterminate {
		var err error
		voided, err = failedLaunchHasNoEvidence(invocation)
		if err != nil {
			original = fmt.Errorf("%w; failed launch report presence was indeterminate; invocation kept protected: %v", original, err)
			voided = false
		}
		if voided && !strings.Contains(original.Error(), failedLaunchVoidMarker) {
			original = fmt.Errorf("%w; %s", original, failedLaunchRetryGuidance(invocation))
		}
	}
	rollbackContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if voided {
		invocation.LaunchVoided = true
		invocation.Status = store.InvocationStatusSuperseded
	} else {
		invocation.Status = store.InvocationStatusCannotProceed
	}
	invocation.UpdatedAt = l.clock().UTC()
	_ = invocationStore.SaveInvocation(rollbackContext, invocation)
	if workerStarted && l.worker != nil {
		_ = l.worker.Stop(rollbackContext, workerID)
	}
	if cleanupSurface.ID != "" && terminalRuntime != nil {
		_ = terminalRuntime.CloseSurface(rollbackContext, cleanupSurface.ID)
	}
	return original
}

// ensureTerminalRuntime binds the lifecycle module to one registered cmux
// endpoint, constructing the default adapter only when no runtime was injected.
func (l *invocationLifecycle) ensureTerminalRuntime(socketPath string) (terminal.TerminalRuntime, error) {
	l.runtimeMu.Lock()
	defer l.runtimeMu.Unlock()
	return l.terminalRuntimeLocked(socketPath)
}

// terminalRuntimeLocked resolves the cached terminal adapter while the module
// runtime mutex is held.
func (l *invocationLifecycle) terminalRuntimeLocked(socketPath string) (terminal.TerminalRuntime, error) {
	if l.runtimePathSet && l.runtimeSocketPath != socketPath {
		return nil, fmt.Errorf("cmux socket path %q conflicts with cached path %q", socketPath, l.runtimeSocketPath)
	}
	if !l.runtimePathSet {
		l.runtimeSocketPath = socketPath
		l.runtimePathSet = true
	}
	if l.terminal == nil {
		l.terminal = terminal.NewCmuxRuntime(nil, socketPath)
	}
	return l.terminal, nil
}

// ensureAgentRuntime resolves the terminal and selected harness adapters from
// the explicit lifecycle adapter set.
func (l *invocationLifecycle) ensureAgentRuntime(socketPath string, selected config.Harness) (terminal.TerminalRuntime, harness.Runtime, error) {
	l.runtimeMu.Lock()
	defer l.runtimeMu.Unlock()
	terminalRuntime, err := l.terminalRuntimeLocked(socketPath)
	if err != nil {
		return nil, nil, err
	}
	if l.harness != nil {
		return terminalRuntime, l.harness, nil
	}
	if cached, exists := l.harnessRuntimes[selected]; exists {
		return terminalRuntime, cached, nil
	}
	harnessRuntime, err := harness.New(string(selected), l.worker, terminalRuntime)
	if err != nil {
		return nil, nil, err
	}
	if l.harnessRuntimes == nil {
		l.harnessRuntimes = make(map[config.Harness]harness.Runtime, 2)
	}
	l.harnessRuntimes[selected] = harnessRuntime
	return terminalRuntime, harnessRuntime, nil
}

// credentialSeeding selects the registered, harness-specific source and
// returns a post-worker projection step plus its factory-managed identity.
func (l *invocationLifecycle) credentialSeeding(registration config.RepositoryRegistration, request AgentRequest, harnessName config.Harness) (func(context.Context, string, string) error, string, error) {
	var registeredAuthPath, overrideAuthPath string
	var seed func(context.Context, worker.CredentialSeedRequest) error
	switch harnessName {
	case config.HarnessCodex:
		registeredAuthPath = registration.Authentication.CodexAuthPath
		overrideAuthPath = request.CodexAuthPath
		if seeder, ok := l.worker.(worker.CredentialSeeder); ok {
			seed = seeder.SeedCodexCredentials
		}
	case config.HarnessClaude:
		registeredAuthPath = registration.Authentication.ClaudeAuthPath
		overrideAuthPath = request.ClaudeAuthPath
		if seeder, ok := l.worker.(worker.ClaudeCredentialSeeder); ok {
			seed = seeder.SeedClaudeCredentials
		}
	}
	if overrideAuthPath != "" && (registeredAuthPath == "" || filepath.Clean(overrideAuthPath) != filepath.Clean(registeredAuthPath)) {
		return nil, "", fmt.Errorf("%s auth source override is not restart-safe; configure the source on the registered repository", harnessName)
	}
	authPath := defaultString(overrideAuthPath, registeredAuthPath)
	if authPath == "" {
		return nil, "", nil
	}
	if seed == nil {
		return nil, "", fmt.Errorf("worker runtime does not support %s credential seeding", harnessName)
	}
	return func(ctx context.Context, runID, workerID string) error {
		return seed(ctx, worker.CredentialSeedRequest{RunID: runID, WorkerID: workerID, AuthPath: authPath})
	}, registration.Path, nil
}

// ensureCredentialStoreIdentity restores the persisted identity for an older
// invocation before its worker is recreated.
func (l *invocationLifecycle) ensureCredentialStoreIdentity(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, invocation store.Invocation) (store.Invocation, error) {
	_, credentialStoreID, err := l.credentialSeeding(registration, AgentRequest{}, config.Harness(invocation.Harness))
	if err != nil {
		return invocation, newCredentialProjectionError(invocation.Harness)
	}
	if strings.TrimSpace(credentialStoreID) == "" || strings.TrimSpace(invocation.CredentialStoreID) != "" {
		return invocation, nil
	}
	invocationStore, ok := runStore.(InvocationStore)
	if !ok {
		return invocation, errors.New("operational store does not support credential store persistence")
	}
	invocation.CredentialStoreID = credentialStoreID
	invocation.UpdatedAt = l.clock().UTC()
	if err := invocationStore.SaveInvocation(ctx, invocation); err != nil {
		return invocation, fmt.Errorf("persist credential store identity during recovery: %w", err)
	}
	return invocation, nil
}

// restoreCredentialProjection reseeds the configured host source into the
// already-mounted factory-managed worker volume.
func (l *invocationLifecycle) restoreCredentialProjection(ctx context.Context, registration config.RepositoryRegistration, run store.Run, invocation store.Invocation) error {
	seed, _, err := l.credentialSeeding(registration, AgentRequest{}, config.Harness(invocation.Harness))
	if err != nil || seed == nil {
		if err == nil && strings.TrimSpace(invocation.CredentialStoreID) == "" {
			return nil
		}
		return newCredentialProjectionError(invocation.Harness)
	}
	if err := seed(ctx, run.ID, workerIDForInvocation(invocation)); err != nil {
		return newCredentialProjectionError(invocation.Harness)
	}
	return nil
}

// ensureWorkerForInvocation validates or recreates the pinned worker and
// returns the exact request used by the durable worker-launch effect.
func (l *invocationLifecycle) ensureWorkerForInvocation(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, invocation store.Invocation) (worker.StartRequest, store.Invocation, error) {
	invocation, err := l.ensureCredentialStoreIdentity(ctx, registration, runStore, invocation)
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
	request := worker.StartRequest{RunID: run.ID, WorkerID: workerIDForInvocation(invocation), WorktreeReadOnly: roleIsKind(invocation, workflow.RoleKindReview), WorktreePath: run.Worktree, GitMetadataPath: gitMetadataPath, Image: packet.RepositoryConfig.WorkerBuild.Image, ImageDigest: run.ImageDigest, Caches: workerCaches(packet.RepositoryConfig.Caches), InvocationPath: invocation.InvocationDirectory, ResultPath: invocation.ResultDirectory, CredentialStoreID: invocation.CredentialStoreID, Role: invocation.Role}
	if l.worker == nil {
		return worker.StartRequest{}, invocation, errors.New("worker runtime is required for invocation recovery")
	}
	inspection, err := l.worker.Inspect(ctx, request.WorkerID)
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
	if l.hooks.startWorker == nil {
		return worker.StartRequest{}, invocation, errors.New("worker launch hook is required for invocation recovery")
	}
	if err := l.hooks.startWorker(ctx, runStore, request); err != nil {
		return worker.StartRequest{}, invocation, fmt.Errorf("start or recreate worker during invocation recovery: %w", err)
	}
	return request, invocation, nil
}

// restoreInvocationTerminal returns persisted handles when they still exist
// and recreates a lost run workspace or role surface when necessary.
func (l *invocationLifecycle) restoreInvocationTerminal(ctx context.Context, terminalRuntime terminal.TerminalRuntime, run store.Run, invocation store.Invocation) (terminal.WorkspaceID, terminal.Surface, terminal.Surface, terminal.Surface, bool, error) {
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
	runWorkspace, err := terminalRuntime.EnsureRunWorkspace(ctx, terminal.RunWorkspaceRequest{RunID: run.ID, Name: "factory-" + run.ID, Description: fmt.Sprintf("factory run for issue #%d", run.IssueNumber), WorkingDirectory: run.Worktree})
	if err != nil {
		return workspaceID, roleSurface, status, checks, false, fmt.Errorf("restore run terminal workspace: %w", err)
	}
	workspaceID, status, checks = runWorkspace.ID, runWorkspace.Status, runWorkspace.Checks
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

// resumePersistedInvocationWithMode restores one active invocation through the
// automatic recovery boundary or the explicit operator resume boundary.
func (l *invocationLifecycle) resumePersistedInvocationWithMode(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, invocation store.Invocation, automatic bool) (store.Invocation, error) {
	if automatic && invocation.RecoveryResumeCount > 0 {
		return invocation, nil
	}
	invocationStore, ok := runStore.(InvocationStore)
	if !ok {
		return invocation, errors.New("operational store does not support invocation recovery")
	}
	if strings.TrimSpace(invocation.NativeSessionID) == "" {
		return invocation, errors.New("active invocation has no persisted native session identifier")
	}
	roleDefinition, err := roleDefinitionForInvocation(invocation)
	if err != nil {
		return invocation, err
	}
	if err := l.stopRunWorker(ctx, workerIDForInvocation(invocation)); err != nil {
		return invocation, err
	}
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return invocation, fmt.Errorf("decode specification packet for invocation recovery: %w", err)
	}
	terminalRuntime, harnessRuntime, err := l.ensureAgentRuntime(registration.Cmux.SocketPath, config.Harness(invocation.Harness))
	if err != nil {
		return invocation, err
	}
	capabilities := harnessRuntime.Capabilities()
	if capabilities.Name != invocation.Harness {
		return invocation, fmt.Errorf("harness %q cannot resume persisted %q session", capabilities.Name, invocation.Harness)
	}
	if !capabilities.InteractiveResume {
		return invocation, fmt.Errorf("harness %q does not support native session resume", invocation.Harness)
	}
	promptText, err := promptForPersistedInvocation(run, invocation, packet)
	if err != nil {
		return invocation, err
	}
	_, invocation, err = l.ensureWorkerForInvocation(ctx, registration, runStore, run, invocation)
	if err != nil {
		return invocation, err
	}
	if err := l.restoreCredentialProjection(ctx, registration, run, invocation); err != nil {
		return invocation, err
	}
	workspaceID, roleSurface, statusSurface, checksSurface, restored, err := l.restoreInvocationTerminal(ctx, terminalRuntime, run, invocation)
	if err != nil {
		return invocation, err
	}
	if restored {
		invocation.WorkspaceID = string(workspaceID)
		invocation.StatusSurfaceID = string(statusSurface.ID)
		setInvocationSurface(&invocation, roleSurface)
		invocation.ChecksSurfaceID = string(checksSurface.ID)
		if err := invocationStore.SaveInvocation(ctx, invocation); err != nil {
			return invocation, fmt.Errorf("persist restored terminal handles: %w", err)
		}
	}
	resumeRequest := harness.StartRequest{InvocationID: invocation.ID, RunID: run.ID, WorkerID: workerIDForInvocation(invocation), Role: invocation.Role, Stage: string(invocation.Stage), CheckpointSHA: reviewCheckpointSHA(roleDefinition.Kind == workflow.RoleKindReview, run.CheckpointSHA), WorkspaceID: workspaceID, Surface: roleSurface, Prompt: promptText, Model: invocation.Model, ReasoningEffort: invocation.ReasoningEffort, ResumeSessionID: invocation.NativeSessionID}
	if automatic {
		if l.hooks.resumeHarness == nil {
			return invocation, errors.New("harness resume hook is required")
		}
		return l.hooks.resumeHarness(ctx, runStore, invocationStore, registration.Cmux.SocketPath, harnessRuntime, invocation, resumeRequest)
	}
	if l.hooks.resumeHarnessManually == nil {
		return invocation, errors.New("manual harness resume hook is required")
	}
	return l.hooks.resumeHarnessManually(ctx, runStore, invocationStore, registration.Cmux.SocketPath, harnessRuntime, invocation, resumeRequest)
}

// resumePersistedInvocation performs the bounded automatic native resume.
func (l *invocationLifecycle) resumePersistedInvocation(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, invocation store.Invocation) (store.Invocation, error) {
	return l.resumePersistedInvocationWithMode(ctx, registration, runStore, run, invocation, true)
}

// resumePersistedInvocationManually performs an explicit native resume and
// leaves the attach gate for the operator.
func (l *invocationLifecycle) resumePersistedInvocationManually(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, invocation store.Invocation) (store.Invocation, error) {
	return l.resumePersistedInvocationWithMode(ctx, registration, runStore, run, invocation, false)
}

// stopRunWorker keeps worker shutdown idempotent at the lifecycle seam.
func (l *invocationLifecycle) stopRunWorker(ctx context.Context, workerID string) error {
	if l.worker == nil {
		return nil
	}
	if err := l.worker.Stop(ctx, workerID); err != nil {
		return fmt.Errorf("stop worker for run %q: %w", workerID, err)
	}
	return nil
}

// pauseForHarnessCapacity stops delegated workers and records a non-budgeted
// capacity wait that polling may retry.
func (l *invocationLifecycle) pauseForHarnessCapacity(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, harnessName string) (store.Run, error) {
	if strings.TrimSpace(harnessName) == "" {
		harnessName = "harness"
	}
	changed := run.Status != store.StatusWaitingForHarness || !strings.HasPrefix(run.LifecycleReason, "harness capacity unavailable")
	if err := l.stopActiveRunWorkers(ctx, runStore, run); err != nil {
		return run, err
	}
	if !changed {
		return run, nil
	}
	next := run
	next.Status = store.StatusWaitingForHarness
	next.LifecycleReason = fmt.Sprintf("harness capacity unavailable (%s); waiting for capacity", harnessName)
	next.Revision = run.Revision + 1
	next.UpdatedAt = l.clock().UTC()
	if err := l.persistLifecycleRun(ctx, registration, runStore, run, next); err != nil {
		return next, err
	}
	if err := l.notifyLifecycle(ctx, registration, "factory harness capacity", fmt.Sprintf("%s is waiting for %s harness capacity", run.ID, harnessName)); err != nil {
		return next, err
	}
	return next, nil
}

// pauseForAuthentication stops delegated workers and records a redacted
// human-waiting credential state.
func (l *invocationLifecycle) pauseForAuthentication(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, harnessName string) (store.Run, error) {
	if err := l.stopActiveRunWorkers(ctx, runStore, run); err != nil {
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
	next.UpdatedAt = l.clock().UTC()
	if err := l.persistLifecycleRun(ctx, registration, runStore, run, next); err != nil {
		return next, err
	}
	if err := l.notifyLifecycle(ctx, registration, "factory authentication expired", fmt.Sprintf("%s requires `factory auth refresh` for %s", run.ID, harnessName)); err != nil {
		return next, err
	}
	return next, nil
}

// pauseForManualRecovery records the bounded automatic-recovery boundary and
// leaves the native session for an operator to resume and attach.
func (l *invocationLifecycle) pauseForManualRecovery(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, harnessName string, cause error) (store.Run, error) {
	if err := l.stopActiveRunWorkers(ctx, runStore, run); err != nil {
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
	next.UpdatedAt = l.clock().UTC()
	var err error
	if journal, ok := runStore.(PendingEffectStore); ok {
		pending, pendingErr := journal.PendingEffect(ctx, run.ID)
		if pendingErr != nil {
			return run, fmt.Errorf("inspect pending effect before manual recovery pause: %w", pendingErr)
		}
		if pending != nil {
			err = saveRunWithRetry(ctx, runStore, next)
		} else {
			err = l.persistLifecycleRun(ctx, registration, runStore, run, next)
		}
	} else {
		err = l.persistLifecycleRun(ctx, registration, runStore, run, next)
	}
	if err != nil {
		return next, err
	}
	if notifyErr := l.notifyLifecycle(ctx, registration, "factory harness recovery exhausted", fmt.Sprintf("%s requires manual harness recovery", run.ID)); notifyErr != nil {
		return next, errors.Join(cause, notifyErr)
	}
	return next, nil
}

// persistLifecycleRun keeps all run projection mutation behind the explicit
// coordinator hook supplied when the module is constructed.
func (l *invocationLifecycle) persistLifecycleRun(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, previous, next store.Run) error {
	if l.hooks.persistRun == nil {
		return errors.New("run persistence hook is required")
	}
	return l.hooks.persistRun(ctx, registration, runStore, previous, next)
}

// notifyLifecycle sends a bounded operator notification through the explicit
// coordinator notification hook.
func (l *invocationLifecycle) notifyLifecycle(ctx context.Context, registration config.RepositoryRegistration, title, body string) error {
	if l.hooks.notifyWorkspace == nil {
		return nil
	}
	return l.hooks.notifyWorkspace(ctx, registration, title, body)
}

// stopActiveRunWorkers stops every currently delegated worker for a run and
// retains the run's credential volume and invocation artifacts.
func (l *invocationLifecycle) stopActiveRunWorkers(ctx context.Context, runStore RunStore, run store.Run) error {
	if l.worker == nil {
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
	} else if len(run.ActiveInvocationIDs) != 0 {
		workerIDs = append(workerIDs, run.ID)
	}
	if len(workerIDs) == 0 {
		workerIDs = append(workerIDs, run.ID)
	}
	var stopErr error
	for _, workerID := range workerIDs {
		stopErr = errors.Join(stopErr, l.stopRunWorker(ctx, workerID))
	}
	return stopErr
}

// ensureInvocationAttached enforces the durable manual-resume gate before
// workflow code can consume an invocation result.
func (l *invocationLifecycle) ensureInvocationAttached(ctx context.Context, runStore RunStore, run store.Run) error {
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

// supersedeInvocation closes an incomplete invocation and releases its run
// delegation marker before a fresh retry creates the next identity.
func (l *invocationLifecycle) supersedeInvocation(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, invocation store.Invocation) error {
	invocationStore, ok := runStore.(InvocationStore)
	if !ok {
		return errors.New("operational store does not support invocation recovery")
	}
	invocation.Status = store.InvocationStatusSuperseded
	invocation.UpdatedAt = l.clock().UTC()
	if err := invocationStore.SaveInvocation(ctx, invocation); err != nil {
		return fmt.Errorf("supersede interrupted invocation: %w", err)
	}
	current, err := runStore.CurrentRun(ctx)
	if err != nil {
		return fmt.Errorf("read run to release superseded invocation: %w", err)
	}
	if current == nil || !containsString(current.ActiveInvocationIDs, invocation.ID) {
		return nil
	}
	previous := *current
	next := previous
	releaseActiveInvocation(&next, invocation.ID)
	next.UpdatedAt = l.clock().UTC()
	if err := l.persistLifecycleRun(ctx, registration, runStore, previous, next); err != nil {
		return fmt.Errorf("release superseded invocation delegation: %w", err)
	}
	return nil
}

// resetStartupState clears the coordinator's cached startup diagnosis after a
// successful explicit attach.
func (l *invocationLifecycle) resetStartupState() {
	if l.hooks.resetStartup != nil {
		l.hooks.resetStartup()
	}
}

// recordSessionExitDiagnostic captures bounded terminal output before recovery
// stops the worker and returns only the local diagnostic path.
func (l *invocationLifecycle) recordSessionExitDiagnostic(ctx context.Context, registration config.RepositoryRegistration, run store.Run, invocation store.Invocation) string {
	terminalRuntime := l.terminal
	if terminalRuntime == nil {
		terminalRuntime, _ = l.ensureTerminalRuntime(registration.Cmux.SocketPath)
	}
	transcript := captureSurfaceTranscript(ctx, terminalRuntime, invocationSurface(invocation).ID)
	cause := fmt.Errorf("%s native session %q exited before reporting", invocation.Harness, invocation.NativeSessionID)
	return writeHarnessFailureDiagnostic(invocationRoot(run, invocation.ID), "session exit", cause, transcript, l.clock().UTC())
}

// retryWaitingForHarness performs one polling retry for a temporary capacity
// wait. The coordinator supplies the launch callback so ID generation remains
// outside this module.
func (l *invocationLifecycle) retryWaitingForHarness(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, launch func(store.Run) (AgentLaunchResult, error)) error {
	if run.Status != store.StatusWaitingForHarness {
		return l.reconcileActiveHarnessLiveness(ctx, registration, runStore, run)
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
			_, pauseErr := l.pauseForManualRecovery(ctx, registration, runStore, run, active.Harness, harness.NewUnexpectedExitError(active.Harness))
			return pauseErr
		}
		updated, resumeErr := l.resumePersistedInvocation(ctx, registration, runStore, run, *active)
		if resumeErr != nil {
			return l.handleRetryResumeError(ctx, registration, runStore, run, *active, resumeErr)
		}
		next := run
		next.Status = store.StatusActive
		next.LifecycleReason = "harness capacity returned; native session resumed"
		next.UpdatedAt = l.clock().UTC()
		next.Revision = run.Revision + 1
		_ = updated
		return l.persistLifecycleRun(ctx, registration, runStore, run, next)
	}
	if active != nil {
		if err := l.supersedeInvocation(ctx, registration, runStore, *active); err != nil {
			return err
		}
	}
	if launch == nil {
		return errors.New("launch callback is required for harness retry")
	}
	launchRun := run
	launchRun.Status = store.StatusActive
	launchResult, launchErr := launch(launchRun)
	if launchErr != nil {
		classified := classifyHarnessRuntimeErrorForInvocation(launchResult.Invocation, launchErr)
		if harness.IsRateLimited(classified) || harness.IsAuthenticationExpired(classified) || harness.IsUnexpectedExit(classified) {
			return nil
		}
		return launchErr
	}
	return nil
}

// handleRetryResumeError maps failed automatic native resume to its bounded
// capacity, authentication, or manual-recovery projection.
func (l *invocationLifecycle) handleRetryResumeError(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, active store.Invocation, resumeErr error) error {
	classified := classifyHarnessRuntimeErrorForInvocation(active, resumeErr)
	if harness.IsRateLimited(classified) {
		_, err := l.pauseForHarnessCapacity(ctx, registration, runStore, run, active.Harness)
		return err
	}
	if harness.IsAuthenticationExpired(classified) {
		_, err := l.pauseForAuthentication(ctx, registration, runStore, run, active.Harness)
		return err
	}
	if harness.IsUnexpectedExit(classified) {
		_, err := l.pauseForManualRecovery(ctx, registration, runStore, run, active.Harness, classified)
		return err
	}
	return resumeErr
}

// reconcileActiveHarnessLiveness observes active native sessions and delegates
// an observed exit to the restart reconciliation hook.
func (l *invocationLifecycle) reconcileActiveHarnessLiveness(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run) error {
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
	for _, active := range activeValues {
		if active.AttachRequired || strings.TrimSpace(active.NativeSessionID) == "" {
			continue
		}
		_, harnessRuntime, err := l.ensureAgentRuntime(registration.Cmux.SocketPath, config.Harness(active.Harness))
		if err != nil {
			return fmt.Errorf("ensure harness for liveness monitoring: %w", err)
		}
		inspector, ok := harnessRuntime.(harness.NativeSessionLivenessInspector)
		if !ok {
			continue
		}
		running, err := inspector.NativeSessionRunning(ctx, harness.NativeSessionRequest{RunID: run.ID, WorkerID: workerIDForInvocation(active), Harness: active.Harness})
		if err != nil {
			return fmt.Errorf("check native session liveness: %w", err)
		}
		if running {
			continue
		}
		diagnostic := l.recordSessionExitDiagnostic(ctx, registration, run, active)
		if l.hooks.reconcileInterrupted == nil {
			return errors.New("interrupted-run reconciliation hook is required")
		}
		recovered, _, _, reconcileErr := l.hooks.reconcileInterrupted(ctx, registration, runStore, run, true)
		if reconcileErr != nil {
			if diagnostic != "" {
				return fmt.Errorf("%w (session-exit diagnostic: %s)", reconcileErr, diagnostic)
			}
			return reconcileErr
		}
		return l.persistSessionExitReason(ctx, registration, runStore, recovered, diagnostic)
	}
	return nil
}

// persistSessionExitReason adds the diagnostic path to the recovered run
// projection without exposing captured terminal output.
func (l *invocationLifecycle) persistSessionExitReason(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, recovered store.Run, diagnostic string) error {
	if diagnostic == "" {
		return nil
	}
	reason := fmt.Sprintf("session-exit diagnostic: %s", diagnostic)
	next := recovered
	if strings.TrimSpace(next.LifecycleReason) == "" {
		next.LifecycleReason = reason
	} else if !strings.Contains(next.LifecycleReason, reason) {
		next.LifecycleReason += "; " + reason
	}
	if next.LifecycleReason == recovered.LifecycleReason {
		return nil
	}
	next.Revision = recovered.Revision + 1
	next.UpdatedAt = l.clock().UTC()
	if err := l.persistLifecycleRun(ctx, registration, runStore, recovered, next); err != nil {
		return fmt.Errorf("persist session-exit diagnostic projection %q: %w", diagnostic, err)
	}
	return nil
}
