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
	effectkernel "github.com/Stevie1704/sw-factory/internal/effect"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/prompt"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
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
	// ReviewConcurrency is the host-local simultaneous review limit.
	ReviewConcurrency int
	// ReviewAuthorizedUnits is the host-local maximum fan-out a maintainer may
	// authorize for this installation.
	ReviewAuthorizedUnits int
}

// LaunchPlan is the pure admission decision for one harness invocation.
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
	// Registration identifies the repository and host authentication sources.
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
	// EvaluationRecorder records content-free evaluation invocation metadata
	// after the invocation has been persisted.
	EvaluationRecorder launchEvaluationRecorder
}

// InvocationRecoveryRequest supplies one already-open run to a recovery
// operation delegated by a Service entry point.
type InvocationRecoveryRequest struct {
	// Registration identifies the repository and host authentication sources.
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
	// EvaluationRecorder records content-free evaluation invocation metadata
	// for a fresh launch created during recovery.
	EvaluationRecorder launchEvaluationRecorder
}

// launchEvaluationRecorder is the narrow evaluation projection required by
// launch activation. The lifecycle module receives it explicitly instead of
// discovering the coordinator's broader store capability itself.
type launchEvaluationRecorder interface {
	EnsureEvaluationSummary(context.Context, store.Run) error
	RecordEvaluationInvocation(context.Context, string, store.Invocation, string, int) error
}

// InvocationStopRequest selects the active workers delegated to a run.
type InvocationStopRequest struct {
	// RunStore provides the active-invocation projection.
	RunStore RunStore
	// Run identifies the run whose workers should stop.
	Run store.Run
}

// InvocationLifecycle is the small seam used by the coordinator for launching,
// recovering, and stopping headless invocations.
type InvocationLifecycle interface {
	// Launch admits, materialises, and activates one headless invocation.
	Launch(context.Context, InvocationLaunchRequest) (AgentLaunchResult, error)
	// Resume recovers the persisted invocation or launches the next role.
	Resume(context.Context, InvocationRecoveryRequest) (ResumeResult, error)
	// Stop stops every worker currently delegated to a run.
	Stop(context.Context, InvocationStopRequest) error
}

// invocationLifecycleHooks are coordinator-owned effects passed explicitly to
// the module. The module never receives the coordinator's dependency bundle.
type invocationLifecycleHooks struct {
	persistRun               func(context.Context, config.RepositoryRegistration, RunStore, store.Run, store.Run) error
	publishReviewStatus      func(context.Context, config.RepositoryRegistration, RunStore, store.Run, string, github.CommitStatusState, string) error
	refreshReviewPullRequest func(context.Context, config.RepositoryRegistration, RunStore, store.Run) error
	reconcileInterrupted     func(context.Context, config.RepositoryRegistration, RunStore, store.Run, bool) (store.Run, RecoveryDiagnosis, RecoveryOutcome, error)
	resumeRecoveredCheck     func(context.Context, config.RepositoryRegistration, RunStore, store.Run) (store.Run, error)
	resetStartup             func()
	materialiseReviewDiff    func(context.Context, store.Run, store.Invocation) (reviewDiffMetadata, error)
}

// invocationLifecycle owns worker and headless-harness adapters used by launch
// and recovery, plus explicitly supplied coordinator effect hooks.
type invocationLifecycle struct {
	journal             invocationJournal
	worker              worker.WorkerRuntime
	headlessHarnesses   map[config.Harness]harness.HeadlessRuntime
	harnessCapabilities harness.CapabilityResolver
	worktree            gitadapter.WorktreeInspector
	checkpointFiles     gitadapter.CheckpointFileReader
	clock               Clock
	hooks               invocationLifecycleHooks
}

var _ InvocationLifecycle = (*invocationLifecycle)(nil)

// newInvocationLifecycle constructs the module from explicit adapters and
// hooks. It intentionally accepts only explicit adapters and effects.
func newInvocationLifecycle(journal invocationJournal, workerRuntime worker.WorkerRuntime, capabilities harness.CapabilityResolver, worktree gitadapter.WorktreeInspector, checkpointFiles gitadapter.CheckpointFileReader, clock Clock, hooks invocationLifecycleHooks, headless map[config.Harness]harness.HeadlessRuntime) *invocationLifecycle {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &invocationLifecycle{
		journal: journal, worker: workerRuntime,
		headlessHarnesses:   headless,
		harnessCapabilities: capabilities, worktree: worktree, checkpointFiles: checkpointFiles, clock: clock, hooks: hooks,
	}
}

// Resume restores an active native session, retries a harness-capacity wait,
// or launches the next role after reconciling a durable interruption.
func (l *invocationLifecycle) Resume(ctx context.Context, request InvocationRecoveryRequest) (ResumeResult, error) {
	if request.Run == nil {
		return ResumeResult{}, errors.New("no persisted run")
	}
	run := *request.Run
	if store.IsTerminalStatus(run.Status) {
		return ResumeResult{}, fmt.Errorf("cannot resume terminal run %q with status %q", run.ID, run.Status)
	}
	reconciled := false
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
			reconciled = true
		}
	}
	if run.Stage == store.StageReview && !reconciled && l.hooks.reconcileInterrupted != nil {
		updated, _, _, reconcileErr := l.hooks.reconcileInterrupted(ctx, request.Registration, request.RunStore, run, false)
		run = updated
		if reconcileErr != nil {
			return ResumeResult{Run: run}, reconcileErr
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
	launchRequest, requestErr := reviewResumeAgentRequest(ctx, request.RunStore, run)
	if requestErr != nil {
		return ResumeResult{Run: run}, requestErr
	}
	launch, err := l.Launch(ctx, InvocationLaunchRequest{Registration: request.Registration, RunStore: request.RunStore, Run: &launchRun, Request: normalizeAgentRequest(launchRequest), InvocationID: invocationID, EvaluationRecorder: request.EvaluationRecorder})
	if err != nil {
		return ResumeResult{Run: run, Invocation: launch.Invocation}, err
	}
	return ResumeResult{Run: launchRun, Invocation: launch.Invocation}, nil
}

// resumeActiveInvocation handles the manual-resume branch of Resume.
func (l *invocationLifecycle) resumeActiveInvocation(ctx context.Context, request InvocationRecoveryRequest, run store.Run, active store.Invocation) (ResumeResult, error) {
	if reviewRoleInvocation(active) {
		if active.ReviewRoundID != "" {
			partitioned, ok := request.RunStore.(store.ReviewRoundStore)
			if !ok {
				return ResumeResult{Run: run, Invocation: active}, errors.New("operational store does not support partitioned review recovery")
			}
			round, roundErr := partitioned.ReviewRound(ctx, run.ID, run.CheckpointSHA)
			if roundErr != nil {
				return ResumeResult{Run: run, Invocation: active}, fmt.Errorf("read persisted review round for resume: %w", roundErr)
			}
			if round == nil || round.ID != active.ReviewRoundID {
				return ResumeResult{Run: run, Invocation: active}, errors.New("active review invocation does not match the persisted review round")
			}
			if _, manifestErr := validatePersistedReviewManifest(ctx, partitioned, run, *round); manifestErr != nil {
				return ResumeResult{Run: run, Invocation: active}, fmt.Errorf("validate persisted review manifest: %w", manifestErr)
			}
		}
		if err := validatePersistedReviewDiff(active); err != nil {
			return ResumeResult{Run: run, Invocation: active}, fmt.Errorf("validate persisted review diff: %w", err)
		}
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
	if run.Status == store.StatusActive {
		return ResumeResult{Run: run, Invocation: updatedInvocation}, nil
	}
	next := run
	next.Status = store.StatusActive
	next.LifecycleReason = "manual native session resumed"
	next.UpdatedAt = l.clock().UTC()
	if next.Revision <= run.Revision {
		next.Revision = run.Revision + 1
	}
	if err := l.persistLifecycleRun(ctx, request.Registration, request.RunStore, run, next); err != nil {
		return ResumeResult{Run: next, Invocation: updatedInvocation}, err
	}
	return ResumeResult{Run: next, Invocation: updatedInvocation}, nil
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
		if credentialErr.Cause != nil {
			paused, pauseErr := l.pauseForCaptureLimit(ctx, request.Registration, request.RunStore, run, active.Harness)
			return ResumeResult{Run: paused, Invocation: updated}, errors.Join(credentialErr, pauseErr)
		}
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
		paused, pauseErr := l.pauseForManualRecovery(ctx, request.Registration, request.RunStore, run, active.Harness)
		return ResumeResult{Run: paused, Invocation: updated}, errors.Join(classified, pauseErr)
	}
	return ResumeResult{Run: run, Invocation: updated}, resumeErr
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
		return AuthRefreshResult{}, fmt.Errorf("no factory-managed %s credential source is registered; configure it with `factory register --update --%s-auth <path>` and retry `factory auth refresh`", harnessName, harnessName)
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
		projectionErr := newCredentialProjectionError(string(harnessName), err)
		if credentialProjectionCaptureLimit(projectionErr) {
			paused, pauseErr := l.pauseForCaptureLimit(ctx, request.Registration, request.RunStore, run, credentialProjectionHarness(projectionErr))
			return AuthRefreshResult{Run: paused, Invocation: *invocation, Harness: harnessName}, errors.Join(projectionErr, pauseErr)
		}
		return AuthRefreshResult{}, projectionErr
	}
	return AuthRefreshResult{Run: run, Invocation: *invocation, Harness: harnessName}, nil
}

// Stop stops every worker currently delegated to the selected run.
func (l *invocationLifecycle) Stop(ctx context.Context, request InvocationStopRequest) error {
	return l.stopActiveRunWorkers(ctx, request.RunStore, request.Run)
}

// Launch gathers the read-only snapshot, plans admission, materialises the
// packet, and activates one harness invocation in that order.
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
	materialised, err := l.materialiseLaunch(ctx, request.Registration, request.RunStore, plan)
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
		return LaunchSnapshot{}, errors.New("operational store does not support harness invocations")
	}
	if request.Run == nil {
		return LaunchSnapshot{}, errors.New("no active run")
	}
	run := *request.Run
	agentRequest := request.Request
	if agentRequest.Harness == "" && run.HarnessOverride != "" {
		agentRequest.Harness = config.Harness(run.HarnessOverride)
	}
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return LaunchSnapshot{}, err
	}
	agentRequest, roleDefinition := resolveLaunchRoleForGather(run, agentRequest)
	testRevision, reviewRepair, implementationResume := launchModeHints(run, agentRequest, roleDefinition)
	snapshot := LaunchSnapshot{Run: run, Packet: packet, Request: agentRequest, InvocationID: request.InvocationID}
	// Source reads are selected only by immutable mode hints. Their ownership
	// and validity remain admission decisions in PlanLaunch.
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
		hostReview := config.EffectiveReviewHostConfig(request.Registration.Review)
		snapshot.ReviewConcurrency = hostReview.Concurrency
		snapshot.ReviewAuthorizedUnits = hostReview.AuthorizedUnits
		if partitioned, ok := request.RunStore.(store.ReviewRoundStore); ok {
			if round, roundErr := partitioned.ReviewRound(ctx, run.ID, run.CheckpointSHA); roundErr != nil {
				return LaunchSnapshot{}, fmt.Errorf("read persisted review concurrency: %w", roundErr)
			} else if round != nil && round.Concurrency > 0 && snapshot.ReviewConcurrency > round.Concurrency {
				snapshot.ReviewConcurrency = round.Concurrency
			}
		}
	}
	if err := ensureBaselineReadyForLaunch(ctx, l.checkpointFiles, request.RunStore, run, packet); err != nil {
		return LaunchSnapshot{}, err
	}
	return snapshot, nil
}

// resolveLaunchRoleForGather supplies only the role information needed to
// choose read projections. It deliberately does not validate the request or
// whether the role may start from the run; PlanLaunch owns those decisions.
func resolveLaunchRoleForGather(run store.Run, request AgentRequest) (AgentRequest, workflow.RoleDefinition) {
	registry := workflow.DefaultRegistry()
	roleWasSpecified := request.Role != ""
	if request.Role == "" && request.Stage == "" {
		if definition, exists := registry.RoleForRunStage(run.Stage); exists {
			request.Role = definition.Name
			request.Stage = definition.Stage
		}
	}
	if request.Role == "" && request.Stage != "" {
		if definition, exists := registry.RoleForInvocationStage(request.Stage); exists {
			request.Role = definition.Name
		}
	}
	if request.Stage == "" && request.Role != "" {
		if definition, exists := registry.Role(request.Role); exists {
			request.Stage = definition.Stage
		}
	}
	if !roleWasSpecified && request.Role == "" {
		return request, workflow.RoleDefinition{}
	}
	definition, _ := registry.Role(request.Role)
	return request, definition
}

// launchModeHints selects only the projections gather must read before pure
// admission. Invalid mode combinations are rejected later by launchModes.
func launchModeHints(run store.Run, request AgentRequest, role workflow.RoleDefinition) (bool, bool, bool) {
	testRevision := role.Kind == workflow.RoleKindTest && run.Stage == store.StageTest && run.TestObjection != nil
	reviewRepair := request.reviewRepair && request.Role == workflow.RoleImplementation && request.Stage == store.StageImplementation && run.Stage == store.StageImplementation
	implementationResume := request.resumeImplementation && request.Role == workflow.RoleImplementation && request.Stage == store.StageImplementation && run.Stage == store.StageImplementation
	return testRevision, reviewRepair, implementationResume
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
		return nil, supported, fmt.Errorf("look up active harness invocations: %w", err)
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

// gatherReview reads the immutable review preconditions and bounded review
// context before activation can publish status. Artifact materialisation stays
// in materialiseLaunch so gather remains read-only.
func (l *invocationLifecycle) gatherReview(ctx context.Context, runStore RunStore, run store.Run, role string, snapshot *LaunchSnapshot) error {
	if err := ensureReviewStartForRoleWithInspector(ctx, run, role, l.worktree); err != nil {
		return err
	}
	reviewContext, err := reviewContextForRole(ctx, run, runStore, role)
	if err != nil {
		return err
	}
	snapshot.ReviewContext = reviewContext
	if partitioned, ok := runStore.(store.ReviewRoundStore); ok {
		round, roundErr := partitioned.ReviewRound(ctx, run.ID, run.CheckpointSHA)
		if roundErr != nil {
			return fmt.Errorf("read persisted review round: %w", roundErr)
		}
		if round != nil {
			unit, unitErr := nextReviewUnit(ctx, partitioned, run, *round, role, snapshot.Request.ReviewUnitID, snapshot.ActiveInvocations)
			if unitErr != nil {
				return unitErr
			}
			if unit != nil {
				units, unitsErr := partitioned.ReviewUnits(ctx, round.ID)
				if unitsErr != nil {
					return fmt.Errorf("read persisted review unit count: %w", unitsErr)
				}
				snapshot.Request.ReviewUnitID = unit.UnitID
				snapshot.ReviewContext = reviewContextWithUnit(*reviewContext, *round, *unit, len(units))
			}
		}
	}
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
	if roleDefinition.Kind == workflow.RoleKindReview {
		if request.ReviewUnitID != "" && !safeLaunchIdentifier(request.ReviewUnitID) {
			return LaunchPlan{}, errors.New("review unit id is unsafe")
		}
		concurrency := snapshot.ReviewConcurrency
		if concurrency <= 0 {
			concurrency = config.EffectiveReviewHostConfig(config.ReviewHostConfig{}).Concurrency
		}
		activeReviews := 0
		for _, active := range snapshot.ActiveInvocations {
			if roleIsKind(active.Invocation, workflow.RoleKindReview) {
				activeReviews++
			}
		}
		if activeReviews >= concurrency {
			return LaunchPlan{}, fmt.Errorf("review concurrency limit %d is already in use", concurrency)
		}
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
		partitionedReview := false
		if request.ReviewUnitID != "" {
			if definition, ok := workflow.DefaultRegistry().Role(request.Role); ok {
				partitionedReview = definition.Kind == workflow.RoleKindReview && (snapshot.Run.Stage == store.StageDraftPR || snapshot.Run.Stage == store.StageReview) && (request.Stage == store.StageReview || request.Stage == workflow.StageStandardsReview)
			}
		}
		if snapshot.Run.Status != store.StatusWaitingForHuman || !partitionedReview {
			return fmt.Errorf("cannot start harness invocation from run status %q", snapshot.Run.Status)
		}
	}
	registry := workflow.DefaultRegistry()
	if _, exists := registry.RoleForRunStage(snapshot.Run.Stage); !exists {
		if _, invocationStage := registry.RoleForInvocationStage(snapshot.Run.Stage); !invocationStage {
			return fmt.Errorf("cannot start harness invocation from run stage %q", snapshot.Run.Stage)
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
	partitionedReview := role.Kind == workflow.RoleKindReview && request.ReviewUnitID != ""
	latestSameReviewUnit := partitionedReview && snapshot.LatestInvocation != nil && snapshot.LatestInvocation.ReviewUnitID == request.ReviewUnitID
	if snapshot.LatestInvocation != nil && !testRevision && !reviewRepair && !implementationResume && !partitionedReview && snapshot.LatestInvocation.Status != store.InvocationStatusSuperseded && snapshot.LatestInvocation.Role == request.Role && snapshot.LatestInvocation.Stage == request.Stage {
		return nil, fmt.Errorf("run %q already has invocation history for %s/%s", snapshot.Run.ID, request.Role, request.Stage)
	}
	if latestSameReviewUnit && snapshot.LatestInvocation.Status != store.InvocationStatusSuperseded && snapshot.LatestInvocation.Role == request.Role && snapshot.LatestInvocation.Stage == request.Stage {
		return nil, fmt.Errorf("run %q already has invocation history for %s/%s unit %s", snapshot.Run.ID, request.Role, request.Stage, request.ReviewUnitID)
	}
	if !snapshot.ActiveInvocationsSupported {
		return nil, nil
	}
	isReview := role.Kind == workflow.RoleKindReview
	for _, active := range snapshot.ActiveInvocations {
		if isReview && roleIsKind(active.Invocation, workflow.RoleKindReview) && active.Invocation.Role != request.Role {
			continue
		}
		if partitionedReview && active.Invocation.Role == request.Role && active.Invocation.ReviewUnitID != request.ReviewUnitID {
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
func (l *invocationLifecycle) materialiseLaunch(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, plan LaunchPlan) (launchMaterialisation, error) {
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
	if plan.RoleDefinition.Kind == workflow.RoleKindReview {
		if plan.ReviewContext == nil {
			return launchMaterialisation{}, fmt.Errorf("review context is required for %s", plan.Request.Role)
		}
		if l.hooks.materialiseReviewDiff == nil {
			return launchMaterialisation{}, fmt.Errorf("review diff materialisation is required for %s", plan.Request.Role)
		}
		metadata, err := l.hooks.materialiseReviewDiff(ctx, plan.Run, invocation)
		if err != nil {
			return launchMaterialisation{}, err
		}
		reviewContext := *plan.ReviewContext
		reviewContext.DiffPath = metadata.path
		reviewContext.DiffBytes = metadata.bytes
		reviewContext.DiffSHA256 = metadata.sha256
		// These fields remain decodable for historical packets, but a newly
		// written packet has one file artifact and never carries old delivery
		// branches.
		reviewContext.CurrentDiff = ""
		reviewContext.OmittedDiffBytes = 0
		reviewContext.ChangedPathsCommand = ""
		reviewContext.DiffPathCommand = ""
		plan.ReviewContext = &reviewContext
		if partitioned, ok := runStore.(store.ReviewRoundStore); ok {
			prepared, err := l.preparePartitionedReview(ctx, partitioned, plan.Run, invocation, *plan.ReviewContext, plan.Request.ReviewUnitID, registration)
			if err != nil {
				return launchMaterialisation{}, err
			}
			plan.ReviewContext = prepared
			invocation.ReviewRoundID = prepared.ReviewRoundID
			invocation.ReviewUnitID = prepared.ReviewUnitID
		}
	}
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
	caches, err := resolveWorkerCaches(plan.Packet.RepositoryConfig.Caches, registration)
	if err != nil {
		return launchMaterialisation{}, fmt.Errorf("resolve launch worker caches: %w", err)
	}
	workerRequest := worker.StartRequest{RunID: plan.Run.ID, WorkerID: workerID, WorktreeReadOnly: plan.RoleDefinition.Kind == workflow.RoleKindReview, WorktreePath: plan.Run.Worktree, GitMetadataPath: gitMetadataPath, Image: plan.Packet.RepositoryConfig.WorkerBuild.Image, ImageDigest: plan.Run.ImageDigest, Caches: caches, InvocationPath: packetDirectory, ResultPath: resultDirectory, Role: plan.Request.Role}
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
	packet := InvocationPacket{SchemaVersion: invocationPacketVersion, InvocationID: invocation.ID, RunID: plan.Run.ID, Role: invocation.Role, Stage: invocation.Stage, SpecificationPacket: plan.Run.SpecificationPacket, PromptVersion: invocation.PromptVersion, PromptCraftSourcePath: invocation.PromptCraftSourcePath, PromptCraftSHA256: invocation.PromptCraftSHA256, TestPolicyMode: plan.Packet.RepositoryConfig.TestPolicy.Mode, Route: plan.Packet.Route, DesignHandoff: plan.DesignHandoff, PermittedPaths: append([]string(nil), invocation.PermittedPaths...), TestHandoff: values.testHandoff, TestObjection: values.testObjection, TestRevisionAttempt: values.testRevisionAttempt, TestRevisionBudget: values.testRevisionBudget, ProtectedTestPaths: values.protectedTestPaths, TestExemption: values.testExemption, ReviewRepair: values.reviewRepair, ReviewContext: plan.ReviewContext, ReviewRoundID: invocation.ReviewRoundID, ReviewUnitID: invocation.ReviewUnitID, Continuation: plan.ResumeSource != nil}
	promptText, err := prompt.Build(prompt.Request{InvocationID: invocation.ID, RunID: plan.Run.ID, Role: invocation.Role, Stage: string(invocation.Stage), SpecificationPacket: specification, Continuation: packet.Continuation, RepositoryGuidance: plan.Packet.RepositoryGuidance, RepositoryCraft: repositoryCraftContent(craft), PromptVersion: invocation.PromptVersion, TestPolicyMode: string(plan.Packet.RepositoryConfig.TestPolicy.Mode), Route: plan.Packet.Route, DesignHandoff: plan.DesignHandoff, TestHandoff: values.testHandoff, TestObjection: values.testObjection, TestRevisionAttempt: values.testRevisionAttempt, TestRevisionBudget: values.testRevisionBudget, ProtectedTestPaths: values.protectedTestPaths, TestExemption: values.testExemption, ReviewRepair: values.reviewRepair, TestPaths: plan.Packet.RepositoryConfig.TestPolicy.TestPaths, TestInfrastructurePaths: plan.Packet.RepositoryConfig.TestPolicy.InfrastructurePaths, ReviewContext: plan.ReviewContext})
	return packet, promptText, err
}

// activateLaunch performs all durable and external launch effects after
// materialisation has succeeded. Review status publication deliberately begins
// here, after the packet and result directories exist.
func (l *invocationLifecycle) activateLaunch(ctx context.Context, request InvocationLaunchRequest, plan LaunchPlan, materialised launchMaterialisation) (result AgentLaunchResult, returnErr error) {
	invocationStore, ok := request.RunStore.(InvocationStore)
	if !ok {
		return AgentLaunchResult{}, errors.New("operational store does not support harness invocations")
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
	harnessRuntime, err := l.ensureCoordinatorHarnessRuntime(plan.Policy.Harness)
	if err != nil {
		return AgentLaunchResult{}, fmt.Errorf("ensure agent runtime: %w", err)
	}
	invocation := materialised.invocation
	invocation.CredentialStoreID = credentialStoreID
	materialised.workerRequest.CredentialStoreID = credentialStoreID
	invocationPersisted, workerStarted, preserveInvocation := false, false, false
	defer func() {
		if returnErr != nil && invocationPersisted && !preserveInvocation {
			returnErr = l.rollbackLaunch(ctx, request.RunStore, invocation, materialised.workerID, workerStarted, returnErr)
		}
	}()
	if err := invocationStore.SaveInvocation(ctx, invocation); err != nil {
		return AgentLaunchResult{}, fmt.Errorf("persist harness invocation: %w", err)
	}
	invocationPersisted = true
	if err := l.recordLaunchEvaluation(ctx, request.EvaluationRecorder, *request.Run, invocation); err != nil {
		return AgentLaunchResult{}, err
	}
	if l.journal == nil {
		return AgentLaunchResult{}, errors.New("worker runtime is required")
	}
	if err := l.journal.StartWorker(ctx, request.RunStore, materialised.workerRequest); err != nil {
		return AgentLaunchResult{}, fmt.Errorf("start worker for harness invocation: %w", err)
	}
	workerStarted = true
	if seedCredentials != nil {
		if err := seedCredentials(ctx, request.Run.ID, materialised.workerID); err != nil {
			return AgentLaunchResult{}, newCredentialProjectionError(string(plan.Policy.Harness), err)
		}
	}
	session, updatedInvocation, preserved, err := l.startLaunchHarness(ctx, request, plan, materialised, harnessRuntime, invocation)
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
func (l *invocationLifecycle) recordLaunchEvaluation(ctx context.Context, recorder launchEvaluationRecorder, run store.Run, invocation store.Invocation) error {
	if recorder == nil {
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

// startLaunchHarness starts or resumes the native session after its worker and
// credential projection are durable. Typed waiting
// outcomes retain the invocation for the recovery path; other failures return
// no public launch result so rollback can close the partial attempt.
func (l *invocationLifecycle) startLaunchHarness(ctx context.Context, request InvocationLaunchRequest, plan LaunchPlan, materialised launchMaterialisation, harnessRuntime harness.Runtime, invocation store.Invocation) (harness.Session, store.Invocation, bool, error) {
	invocationStore, ok := request.RunStore.(InvocationStore)
	if !ok {
		return harness.Session{}, store.Invocation{}, false, errors.New("operational store does not support harness invocations")
	}
	if harnessRuntime == nil {
		return harness.Session{}, store.Invocation{}, false, errors.New("harness runtime is required")
	}
	startRequest := harness.StartRequest{
		InvocationID: invocation.ID, RunID: plan.Run.ID, WorkerID: materialised.workerID,
		Role: invocation.Role, Stage: string(invocation.Stage),
		CheckpointSHA: reviewCheckpointSHA(plan.RoleDefinition.Kind == workflow.RoleKindReview, plan.Run.CheckpointSHA),
		ReviewRoundID: invocation.ReviewRoundID, ReviewUnitID: invocation.ReviewUnitID,
		Prompt: materialised.promptText,
		Model:  invocation.Model, ReasoningEffort: invocation.ReasoningEffort,
	}
	var session harness.Session
	var err error
	if plan.ResumeSource != nil {
		startRequest.ResumeSessionID = plan.ResumeSource.NativeSessionID
		session, err = harnessRuntime.ResumeHeadless(ctx, startRequest)
	} else {
		session, err = harnessRuntime.StartHeadless(ctx, startRequest)
	}
	if err == nil {
		return session, invocation, false, nil
	}
	if strings.TrimSpace(session.NativeSessionID) != "" {
		invocation.NativeSessionID = session.NativeSessionID
	}
	classified := harness.ClassifyError(err, string(plan.Policy.Harness))
	transcript := harness.HeadlessDiagnostics(err)
	if diagnostic := writeHarnessFailureDiagnostic(materialised.root, "launch", err, transcript, l.clock().UTC()); diagnostic != "" {
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
			return l.pauseForManualRecovery(ctx, request.Registration, request.RunStore, plan.Run, string(plan.Policy.Harness))
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
func (l *invocationLifecycle) rollbackLaunch(ctx context.Context, runStore RunStore, invocation store.Invocation, workerID string, workerStarted bool, original error) error {
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
			if l.headlessAdapter(config.Harness(invocation.Harness)) != nil {
				cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				_ = l.cancelHeadlessInvocation(cleanupContext, invocation)
				cancel()
			}
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
	return original
}

// ensureCoordinatorHarnessRuntime resolves the selected detached adapter used
// by launch, recovery, and journal replay.
func (l *invocationLifecycle) ensureCoordinatorHarnessRuntime(selected config.Harness) (harness.Runtime, error) {
	if l.harnessCapabilities != nil {
		capabilities, err := l.harnessCapabilities(string(selected))
		if err != nil {
			return nil, err
		}
		if capabilities.Name != string(selected) || !capabilities.Headless {
			return nil, fmt.Errorf("harness %q does not provide the required headless adapter", selected)
		}
	}
	adapter := l.headlessAdapter(selected)
	if adapter == nil {
		return nil, fmt.Errorf("harness %q does not provide the required headless adapter", selected)
	}
	return adapter, nil
}

// headlessAdapter returns the detached adapter for a selected harness.
func (l *invocationLifecycle) headlessAdapter(selected config.Harness) harness.HeadlessRuntime {
	return headlessAdapterFor(l.headlessHarnesses, selected)
}

// headlessAdapterFor returns the adapter that may run one selected harness
// as a detached process. An adapter qualifies only when it reports the identity it
// is keyed by and reports headless support, so diagnosis and launch cannot
// disagree about the same role.
func headlessAdapterFor(adapters map[config.Harness]harness.HeadlessRuntime, selected config.Harness) harness.HeadlessRuntime {
	adapter, exists := adapters[selected]
	if !exists || adapter == nil {
		return nil
	}
	capabilities := adapter.Capabilities()
	if !capabilities.Headless || capabilities.Name != string(selected) {
		return nil
	}
	return adapter
}

// classifyHeadlessExit asks the headless adapter to interpret one detached
// process projection after liveness has gone false. The returned diagnostics
// remain local-only; the returned error is already reduced to the factory's
// typed harness vocabulary.
func classifyHeadlessExit(runtime harness.Runtime, ctx context.Context, request harness.HeadlessInspectionRequest, harnessName string) (error, string, bool) {
	inspector, ok := runtime.(harness.HeadlessFailureInspector)
	if !ok {
		return nil, "", false
	}
	failure := inspector.HeadlessFailureFor(ctx, request)
	if failure == nil {
		return nil, "", false
	}
	return harness.ClassifyError(failure, harnessName), harness.HeadlessDiagnostics(failure), true
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
		return invocation, newCredentialProjectionError(invocation.Harness, err)
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
		return newCredentialProjectionError(invocation.Harness, err)
	}
	if err := seed(ctx, run.ID, workerIDForInvocation(invocation)); err != nil {
		return newCredentialProjectionError(invocation.Harness, err)
	}
	return nil
}

// ensureWorkerForInvocation validates or recreates the pinned worker and
// returns the exact request used by the durable worker-launch effect.
func (l *invocationLifecycle) ensureWorkerForInvocation(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, invocation store.Invocation) (worker.StartRequest, store.Invocation, error) {
	if reviewRoleInvocation(invocation) {
		if err := validatePersistedReviewDiff(invocation); err != nil {
			return worker.StartRequest{}, invocation, fmt.Errorf("validate persisted review diff: %w", err)
		}
	}
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
	caches, err := resolveWorkerCaches(packet.RepositoryConfig.Caches, registration)
	if err != nil {
		return worker.StartRequest{}, invocation, fmt.Errorf("resolve recovery worker caches: %w", err)
	}
	request := worker.StartRequest{RunID: run.ID, WorkerID: workerIDForInvocation(invocation), WorktreeReadOnly: roleIsKind(invocation, workflow.RoleKindReview), WorktreePath: run.Worktree, GitMetadataPath: gitMetadataPath, Image: packet.RepositoryConfig.WorkerBuild.Image, ImageDigest: run.ImageDigest, Caches: caches, InvocationPath: invocation.InvocationDirectory, ResultPath: invocation.ResultDirectory, CredentialStoreID: invocation.CredentialStoreID, Role: invocation.Role}
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
	if l.journal == nil {
		return worker.StartRequest{}, invocation, errors.New("worker launch hook is required for invocation recovery")
	}
	if err := l.journal.StartWorker(ctx, runStore, request); err != nil {
		return worker.StartRequest{}, invocation, fmt.Errorf("start or recreate worker during invocation recovery: %w", err)
	}
	return request, invocation, nil
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
	harnessRuntime, err := l.ensureCoordinatorHarnessRuntime(config.Harness(invocation.Harness))
	if err != nil {
		return invocation, err
	}
	capabilities := harnessRuntime.Capabilities()
	if capabilities.Name != invocation.Harness {
		return invocation, fmt.Errorf("harness %q cannot resume persisted %q session", capabilities.Name, invocation.Harness)
	}
	if !capabilities.NativeResume {
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
	resumeRequest := harness.StartRequest{InvocationID: invocation.ID, RunID: run.ID, WorkerID: workerIDForInvocation(invocation), Role: invocation.Role, Stage: string(invocation.Stage), CheckpointSHA: reviewCheckpointSHA(roleDefinition.Kind == workflow.RoleKindReview, run.CheckpointSHA), ReviewRoundID: invocation.ReviewRoundID, ReviewUnitID: invocation.ReviewUnitID, Prompt: promptText, Model: invocation.Model, ReasoningEffort: invocation.ReasoningEffort, ResumeSessionID: invocation.NativeSessionID}
	if automatic {
		if l.journal == nil {
			return invocation, errors.New("harness resume hook is required")
		}
		return l.journal.ResumeHarness(ctx, runStore, invocationStore, effectkernel.HarnessResume{Runtime: harnessRuntime, Invocation: invocation, Request: resumeRequest})
	}
	if l.journal == nil {
		return invocation, errors.New("manual harness resume hook is required")
	}
	return l.journal.ResumeHarnessManually(ctx, runStore, invocationStore, effectkernel.HarnessResume{Runtime: harnessRuntime, Invocation: invocation, Request: resumeRequest})
}

// resumePersistedInvocation performs the bounded automatic native resume.
func (l *invocationLifecycle) resumePersistedInvocation(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, invocation store.Invocation) (store.Invocation, error) {
	return l.resumePersistedInvocationWithMode(ctx, registration, runStore, run, invocation, true)
}

// resumePersistedInvocationManually performs an explicit native resume.
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

// pauseRunWithReason stops delegated workers and persists one idempotent run
// waiting transition for the supplied status and bounded reason.
func (l *invocationLifecycle) pauseRunWithReason(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, status store.Status, reasonPrefix, reason string) (store.Run, error) {
	next, changed, err := l.prepareRunPause(ctx, runStore, run, status, reasonPrefix, reason)
	if err != nil {
		return run, err
	}
	if !changed {
		return run, nil
	}
	if err := l.persistLifecycleRun(ctx, registration, runStore, run, next); err != nil {
		return next, err
	}
	return next, nil
}

// prepareRunPause stops delegated workers and builds one idempotent waiting
// transition; callers choose how its durable transition is persisted.
func (l *invocationLifecycle) prepareRunPause(ctx context.Context, runStore RunStore, run store.Run, status store.Status, reasonPrefix, reason string) (store.Run, bool, error) {
	if err := l.stopActiveRunWorkers(ctx, runStore, run); err != nil {
		return run, false, err
	}
	changed := run.Status != status || !strings.HasPrefix(run.LifecycleReason, reasonPrefix)
	if !changed {
		return run, false, nil
	}
	next := run
	next.Status = status
	next.LifecycleReason = reason
	next.Revision = run.Revision + 1
	next.UpdatedAt = l.clock().UTC()
	return next, true, nil
}

// pauseForHarnessCapacity stops delegated workers and records a non-budgeted
// capacity wait that polling may retry.
func (l *invocationLifecycle) pauseForHarnessCapacity(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, harnessName string) (store.Run, error) {
	if strings.TrimSpace(harnessName) == "" {
		harnessName = "harness"
	}
	return l.pauseRunWithReason(ctx, registration, runStore, run, store.StatusWaitingForHarness, "harness capacity unavailable", fmt.Sprintf("harness capacity unavailable (%s); waiting for capacity", harnessName))
}

// pauseForAuthentication stops delegated workers and records a redacted
// human-waiting credential state.
func (l *invocationLifecycle) pauseForAuthentication(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, harnessName string) (store.Run, error) {
	return l.pauseRunWithReason(ctx, registration, runStore, run, store.StatusWaitingForHuman, "harness authentication expired", fmt.Sprintf("harness authentication expired (%s); run is waiting for `factory auth refresh`", harnessName))
}

// captureLimitRecoveryPrefix identifies the durable reason for the dedicated
// capture-limit recovery state.
const captureLimitRecoveryPrefix = "worker capture limit exceeded"

// captureLimitRecoveryReason records the human-owned recovery decision for a
// deterministic worker capture-limit failure without suggesting auth refresh.
func captureLimitRecoveryReason(harnessName string) string {
	return fmt.Sprintf("%s (%s); manual recovery required", captureLimitRecoveryPrefix, credentialHarnessLabel(harnessName))
}

// pauseForCaptureLimit stops delegated workers and records a deterministic
// capture-limit failure as a human-waiting state. Retrying the same credential
// projection cannot make the fixed per-stream capture limit sufficient.
func (l *invocationLifecycle) pauseForCaptureLimit(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, harnessName string) (store.Run, error) {
	return l.pauseRunWithReason(ctx, registration, runStore, run, store.StatusWaitingForHuman, captureLimitRecoveryPrefix, captureLimitRecoveryReason(harnessName))
}

// pauseForManualRecovery records the bounded automatic-recovery boundary and
// leaves the native session for an explicit operator-requested resume.
func (l *invocationLifecycle) pauseForManualRecovery(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, harnessName string) (store.Run, error) {
	next, changed, err := l.prepareRunPause(ctx, runStore, run, store.StatusWaitingForHuman, "automatic harness recovery exhausted", fmt.Sprintf("automatic harness recovery exhausted (%s); manual native resume required", harnessName))
	if err != nil {
		return run, err
	}
	if !changed {
		return run, nil
	}
	var persistErr error
	if journal, ok := runStore.(PendingEffectStore); ok {
		pending, pendingErr := journal.PendingEffect(ctx, run.ID)
		if pendingErr != nil {
			return run, fmt.Errorf("inspect pending effect before manual recovery pause: %w", pendingErr)
		}
		if pending != nil {
			persistErr = saveRunWithRetry(ctx, runStore, next)
		} else {
			persistErr = l.persistLifecycleRun(ctx, registration, runStore, run, next)
		}
	} else {
		persistErr = l.persistLifecycleRun(ctx, registration, runStore, run, next)
	}
	if persistErr != nil {
		return next, persistErr
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

// stopActiveRunWorkers stops every currently delegated worker for a run and
// retains the run's credential volume and invocation artifacts.
func (l *invocationLifecycle) stopActiveRunWorkers(ctx context.Context, runStore effectkernel.RunStore, run store.Run) error {
	if l.worker == nil {
		return nil
	}
	activeValues, supported, err := activeInvocationsForRun(ctx, runStore, run.ID)
	if err != nil {
		return fmt.Errorf("read active invocations before stopping workers: %w", err)
	}
	workerIDs := make([]string, 0, len(activeValues))
	seen := make(map[string]struct{}, len(activeValues))
	var stopErr error
	if supported {
		for _, invocation := range activeValues {
			if err := l.cancelHeadlessInvocation(ctx, invocation); err != nil {
				stopErr = errors.Join(stopErr, err)
			}
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
	for _, workerID := range workerIDs {
		stopErr = errors.Join(stopErr, l.stopRunWorker(ctx, workerID))
	}
	return stopErr
}

// cancelHeadlessInvocation requests process cancellation before its worker is
// stopped. The worker stop remains the outer idempotent cleanup boundary, but
// this explicit step lets a detached harness helper persist its cancelled state
// for recovery and prevents a later restart from mistaking it for a lost run.
func (l *invocationLifecycle) cancelHeadlessInvocation(ctx context.Context, invocation store.Invocation) error {
	adapter := l.headlessAdapter(config.Harness(invocation.Harness))
	if adapter == nil {
		return nil
	}
	if err := adapter.CancelHeadless(ctx, harness.HeadlessSession{
		InvocationID:    invocation.ID,
		RunID:           invocation.RunID,
		WorkerID:        workerIDForInvocation(invocation),
		Role:            invocation.Role,
		NativeSessionID: invocation.NativeSessionID,
	}); err != nil {
		return fmt.Errorf("cancel headless %s invocation %q: %w", invocation.Harness, invocation.ID, err)
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

// resetStartupState clears the coordinator's cached startup diagnosis.
func (l *invocationLifecycle) resetStartupState() {
	if l.hooks.resetStartup != nil {
		l.hooks.resetStartup()
	}
}

// recordSessionExitDiagnostic captures adapter-bounded process output before
// recovery stops the worker and returns only the local diagnostic path.
func (l *invocationLifecycle) recordSessionExitDiagnostic(ctx context.Context, _ config.RepositoryRegistration, run store.Run, invocation store.Invocation) string {
	if adapter := l.headlessAdapter(config.Harness(invocation.Harness)); adapter != nil {
		_, transcript, classified := classifyHeadlessExit(adapter, ctx, harness.HeadlessInspectionRequest{
			InvocationID: invocation.ID, RunID: run.ID, WorkerID: workerIDForInvocation(invocation), Role: invocation.Role,
		}, invocation.Harness)
		if classified {
			cause := fmt.Errorf("%s native session %q exited before reporting", invocation.Harness, invocation.NativeSessionID)
			return writeHarnessFailureDiagnostic(invocationRoot(run, invocation.ID), "headless session exit", cause, transcript, l.clock().UTC())
		}
		return ""
	}
	return ""
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
	if active != nil && strings.TrimSpace(active.NativeSessionID) != "" {
		if active.RecoveryResumeCount > 0 {
			_, pauseErr := l.pauseForManualRecovery(ctx, registration, runStore, run, active.Harness)
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
		if credentialProjectionCaptureLimit(launchErr) {
			_, pauseErr := l.pauseForCaptureLimit(ctx, registration, runStore, run, credentialProjectionHarness(launchErr))
			return pauseErr
		}
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
	if credentialProjectionCaptureLimit(resumeErr) {
		_, err := l.pauseForCaptureLimit(ctx, registration, runStore, run, credentialProjectionHarness(resumeErr))
		return err
	}
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
		_, err := l.pauseForManualRecovery(ctx, registration, runStore, run, active.Harness)
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
		if strings.TrimSpace(active.NativeSessionID) == "" {
			continue
		}
		harnessRuntime, err := l.ensureCoordinatorHarnessRuntime(config.Harness(active.Harness))
		if err != nil {
			return fmt.Errorf("ensure harness for liveness monitoring: %w", err)
		}
		inspector, ok := harnessRuntime.(harness.NativeSessionLivenessInspector)
		if !ok {
			continue
		}
		running, err := inspector.NativeSessionRunning(ctx, harness.NativeSessionRequest{RunID: run.ID, InvocationID: active.ID, WorkerID: workerIDForInvocation(active), Harness: active.Harness})
		if err != nil {
			return fmt.Errorf("check native session liveness: %w", err)
		}
		if running {
			continue
		}
		presence, presenceErr := structuredReportPresenceForInvocation(active)
		if presenceErr == nil && presence == structuredReportPresent {
			// The detached process may have exited after publishing the
			// authoritative report. Progression owns validation and
			// acceptance, so recovery must leave this report observable.
			continue
		}
		failure, diagnostics, classified := classifyHeadlessExit(harnessRuntime, ctx, harness.HeadlessInspectionRequest{
			InvocationID: active.ID, RunID: run.ID, WorkerID: workerIDForInvocation(active), Role: active.Role,
		}, active.Harness)
		if classified {
			if diagnostics != "" {
				_ = writeHarnessFailureDiagnostic(invocationRoot(run, active.ID), "headless session exit", failure, diagnostics, l.clock().UTC())
			}
			if harness.IsRateLimited(failure) {
				_, pauseErr := l.pauseForHarnessCapacity(ctx, registration, runStore, run, active.Harness)
				return pauseErr
			}
			if harness.IsAuthenticationExpired(failure) {
				_, pauseErr := l.pauseForAuthentication(ctx, registration, runStore, run, active.Harness)
				return pauseErr
			}
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
// projection without exposing captured process output.
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
