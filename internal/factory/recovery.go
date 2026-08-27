package factory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// RecoveryRequiredCode is the stable code for the pre-reconciliation safety
// boundary. Issue #21 supersedes this refusal with complete reconciliation.
const RecoveryRequiredCode = "recovery-required"

// RecoveryDiscrepancyKind identifies whether a restart disagreement is an
// infrastructure observation or a workflow-owned failure.
type RecoveryDiscrepancyKind string

const (
	// RecoveryDiscrepancyInfrastructure means the coordinator could not prove
	// that a host or external projection matches the durable run identity.
	RecoveryDiscrepancyInfrastructure RecoveryDiscrepancyKind = "infrastructure"
	// RecoveryDiscrepancyWorkflow means the durable workflow itself contains an
	// invalid or failed state and must not be retried as infrastructure.
	RecoveryDiscrepancyWorkflow RecoveryDiscrepancyKind = "workflow"
)

// RecoveryOutcome identifies the result of one reconciliation attempt.
type RecoveryOutcome string

const (
	// RecoveryOutcomeReconciled means the persisted and external projections
	// agree and the run may continue.
	RecoveryOutcomeReconciled RecoveryOutcome = "reconciled"
	// RecoveryOutcomeWaitingForHuman means reconciliation paused the run because
	// a disagreement could not be resolved safely.
	RecoveryOutcomeWaitingForHuman RecoveryOutcome = "waiting_for_human"
)

// RecoveryDiscrepancy describes one observed disagreement between persisted
// run identity and an external projection.
type RecoveryDiscrepancy struct {
	// Kind distinguishes infrastructure uncertainty from workflow failure.
	Kind RecoveryDiscrepancyKind
	// Source identifies the projection that disagrees with persisted state.
	Source string
	// Field identifies the identity field that differs.
	Field string
	// Expected is the persisted or coordinator-derived value.
	Expected string
	// Observed is the value read from the external projection.
	Observed string
}

// RecoveryDiagnosis is the result of a read-only comparison of an interrupted
// run and its persisted and external projections. It contains no claim that
// reconciliation has mutated or repaired any projection.
type RecoveryDiagnosis struct {
	// Code is always RecoveryRequiredCode for a non-terminal run.
	Code string
	// RunID identifies the interrupted run being diagnosed.
	RunID string
	// SourcesAgree reports whether any discovered projection disagreed.
	SourcesAgree bool
	// Discrepancies contains every disagreement found during the read-only check.
	Discrepancies []RecoveryDiscrepancy
	// InvocationExists reports persisted invocation history for the diagnosed run.
	InvocationExists bool
	// SafeActions identifies operator actions that do not claim recovery.
	SafeActions []string
	// PendingEffect is the effect that was present when diagnosis ran, if any.
	PendingEffect *store.PendingEffect
}

// RecoveryResult contains the durable run after an explicit reconciliation
// request and the final read-only diagnosis used to make the decision.
type RecoveryResult struct {
	// Run is the reconciled run projection, when a non-terminal run exists.
	Run *store.Run
	// Diagnosis is the final cross-system comparison.
	Diagnosis RecoveryDiagnosis
	// Outcome identifies whether the run was reconciled or paused.
	Outcome RecoveryOutcome
}

// AbandonPendingEffectRequest identifies one pending effect that a human has
// inspected and decided not to replay. The reason is included in the waiting
// projection so the decision remains visible without storing sensitive detail.
type AbandonPendingEffectRequest struct {
	// RunID optionally constrains the operation to one persisted run.
	RunID string
	// EffectID is the exact durable effect identity shown by factory status.
	EffectID string
	// Reason is the bounded operator explanation for abandoning the effect.
	Reason string
}

// InfrastructureDiscrepancyError reports a restart disagreement that is not a
// deterministic workflow failure. The run has already been paused when the
// coordinator could publish that state safely.
type InfrastructureDiscrepancyError struct {
	// Diagnosis contains every observed discrepancy.
	Diagnosis RecoveryDiagnosis
}

// Error returns the stable infrastructure discrepancy category and details.
func (e *InfrastructureDiscrepancyError) Error() string {
	if e == nil {
		return "recovery infrastructure discrepancy"
	}
	if len(e.Diagnosis.Discrepancies) == 0 {
		return fmt.Sprintf("recovery infrastructure discrepancy: run %q is waiting for human reconciliation", e.Diagnosis.RunID)
	}
	details := make([]string, 0, len(e.Diagnosis.Discrepancies))
	for _, discrepancy := range e.Diagnosis.Discrepancies {
		details = append(details, fmt.Sprintf("%s.%s expected=%q observed=%q", discrepancy.Source, discrepancy.Field, discrepancy.Expected, discrepancy.Observed))
	}
	return fmt.Sprintf("recovery infrastructure discrepancy: run %q is waiting for human reconciliation: %s", e.Diagnosis.RunID, strings.Join(details, ", "))
}

// Unwrap preserves compatibility with callers that used the pre-reconciliation
// RecoveryRequiredError while exposing the stronger infrastructure category to
// new callers.
func (e *InfrastructureDiscrepancyError) Unwrap() error {
	if e == nil {
		return nil
	}
	return &RecoveryRequiredError{Diagnosis: e.Diagnosis}
}

// WorkflowFailureError reports a workflow-owned failure without mislabeling it
// as an unavailable host or external service.
type WorkflowFailureError struct {
	// RunID identifies the affected workflow.
	RunID string
	// Cause is the deterministic workflow failure.
	Cause error
}

// Error returns the workflow failure category and its bounded cause.
func (e *WorkflowFailureError) Error() string {
	if e == nil {
		return "workflow failure during recovery"
	}
	return fmt.Sprintf("workflow failure during recovery for run %q: %v", e.RunID, e.Cause)
}

// Unwrap exposes the deterministic workflow cause.
func (e *WorkflowFailureError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// RecoveryInfrastructureError is a compatibility alias for callers that use
// the longer typed-error vocabulary.
type RecoveryInfrastructureError = InfrastructureDiscrepancyError

// RecoveryWorkflowError is a compatibility alias for callers that use the
// recovery-prefixed workflow error vocabulary.
type RecoveryWorkflowError = WorkflowFailureError

// RecoveryRequiredError preserves the compatibility error returned by legacy
// stores that cannot expose a pending-effect journal. Journaled stores return
// the typed reconciliation errors instead.
type RecoveryRequiredError struct {
	// Diagnosis is the complete read-only startup diagnosis.
	Diagnosis RecoveryDiagnosis
}

// Error returns a stable recovery-required message without implying that the
// coordinator repaired, resumed, or otherwise recovered the run.
func (e *RecoveryRequiredError) Error() string {
	if e == nil {
		return RecoveryRequiredCode
	}
	agreement := "sources disagree"
	if e.Diagnosis.SourcesAgree {
		agreement = "sources agree"
	}
	discrepancyCount := len(e.Diagnosis.Discrepancies)
	message := fmt.Sprintf("%s: run %q is interrupted; %s; %d discrepancies; no recovery occurred", RecoveryRequiredCode, e.Diagnosis.RunID, agreement, discrepancyCount)
	if discrepancyCount != 0 {
		details := make([]string, 0, discrepancyCount)
		for _, discrepancy := range e.Diagnosis.Discrepancies {
			details = append(details, fmt.Sprintf("%s.%s expected=%q observed=%q", discrepancy.Source, discrepancy.Field, discrepancy.Expected, discrepancy.Observed))
		}
		message += "; discrepancies: " + strings.Join(details, ", ")
	}
	if e.Diagnosis.InvocationExists {
		message += "; persisted invocation history exists"
	}
	if len(e.Diagnosis.SafeActions) != 0 {
		message += "; safe actions: " + strings.Join(e.Diagnosis.SafeActions, "; ")
	}
	return message
}

// Code returns the machine-readable refusal code.
func (e *RecoveryRequiredError) Code() string { return RecoveryRequiredCode }

// diagnoseInterruptedRun compares all available persisted and external
// identity projections without mutating Git, GitHub, the worker, the terminal,
// the harness, or operational workflow state.
func (s *Service) diagnoseInterruptedRun(ctx context.Context, registration config.RepositoryRegistration, run store.Run) RecoveryDiagnosis {
	return s.diagnoseInterruptedRunWithStore(ctx, registration, nil, run)
}

// diagnoseInterruptedRunWithStore extends the read-only recovery diagnosis
// with persisted invocation, worker, terminal, native-session, and remote
// branch projections when the operational store exposes those identities.
func (s *Service) diagnoseInterruptedRunWithStore(ctx context.Context, registration config.RepositoryRegistration, runStore OperationalStore, run store.Run) RecoveryDiagnosis {
	diagnosis := newRecoveryDiagnosis(run.ID)
	comparePersistedRunIdentity(&diagnosis, registration.Path, run)
	if _, err := decodeSpecificationPacket(run.SpecificationPacket); err != nil {
		addRecoveryDiscrepancy(&diagnosis, RecoveryDiscrepancy{
			Kind:     RecoveryDiscrepancyWorkflow,
			Source:   "run",
			Field:    "specification packet",
			Expected: "decodable specification packet",
			Observed: err.Error(),
		})
	}

	inspector := s.worktreeInspector()
	inspectWorktreeProjection(ctx, &diagnosis, inspector, registration.Path, run)

	repository := github.Repository{Owner: registration.GitHub.Owner, Name: registration.GitHub.Repository}
	inspectGitHubProjection(ctx, &diagnosis, s.deps.GitHub, s.pullRequestClient(), repository, run)
	inspectRemoteBranchProjection(ctx, &diagnosis, s.gitWorkspace(), run)
	s.inspectInvocationProjection(ctx, &diagnosis, registration, runStore, run)
	diagnosis.SourcesAgree = len(diagnosis.Discrepancies) == 0
	return diagnosis
}

// inspectRemoteBranchProjection verifies a pushed run branch when the Git
// adapter offers a read-only remote-head projection. Earlier stages may not
// have pushed a branch yet, so only a tracked pull request requires this check.
func inspectRemoteBranchProjection(ctx context.Context, diagnosis *RecoveryDiagnosis, workspace gitadapter.GitWorkspace, run store.Run) {
	if run.PullRequestNumber <= 0 {
		return
	}
	if workspace == nil {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
			Kind:     RecoveryDiscrepancyInfrastructure,
			Source:   "remote branch",
			Field:    "inspection",
			Expected: "read-only remote branch inspector",
			Observed: "inspector unavailable",
		})
		return
	}
	inspector, ok := workspace.(gitadapter.RemoteBranchInspector)
	if !ok {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
			Kind:     RecoveryDiscrepancyInfrastructure,
			Source:   "remote branch",
			Field:    "inspection",
			Expected: "read-only remote branch inspector",
			Observed: "inspector unavailable",
		})
		return
	}
	head, err := inspector.RemoteBranchHead(ctx, gitadapter.PushRequest{WorktreePath: run.Worktree, Branch: run.Branch})
	if err != nil {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
			Kind:     RecoveryDiscrepancyInfrastructure,
			Source:   "remote branch",
			Field:    "head",
			Expected: run.CheckpointSHA,
			Observed: err.Error(),
		})
		return
	}
	if head == run.CheckpointSHA {
		return
	}
	// A checkpoint is committed and persisted before the gate suite runs, and
	// pushed only afterwards. A remote head that is an ancestor of the local
	// checkpoint is therefore an ordinary unpushed commit, not a disagreement:
	// the branch has not diverged and the pending push effect still owns it.
	if ancestry, ok := workspace.(gitadapter.AncestryInspector); ok && head != "" {
		reachable, ancestryErr := ancestry.IsAncestor(ctx, run.Worktree, head, run.CheckpointSHA)
		if ancestryErr == nil && reachable {
			return
		}
	}
	addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
		Kind:     RecoveryDiscrepancyInfrastructure,
		Source:   "remote branch",
		Field:    "head",
		Expected: run.CheckpointSHA,
		Observed: head,
	})
}

// inspectInvocationProjection verifies the active invocation's durable
// external identities without launching or mutating anything. A missing native
// session is left for the progression seam to reject as an incomplete launch;
// treating it as a state mutation here would make concurrent first launches
// indistinguishable from restart recovery.
func (s *Service) inspectInvocationProjection(ctx context.Context, diagnosis *RecoveryDiagnosis, registration config.RepositoryRegistration, runStore OperationalStore, run store.Run) {
	if runStore == nil {
		return
	}
	history, hasHistory := runStore.(InvocationHistoryStore)
	if hasHistory {
		exists, err := history.HasInvocation(ctx, run.ID)
		if err != nil {
			addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
				Kind:     RecoveryDiscrepancyInfrastructure,
				Source:   "operational store",
				Field:    "invocation history",
				Expected: "read invocation history",
				Observed: err.Error(),
			})
			return
		}
		diagnosis.InvocationExists = exists
	}
	activeStore, ok := runStore.(ActiveInvocationStore)
	if !ok {
		if _, journaled := runStore.(PendingEffectStore); journaled {
			addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
				Kind:     RecoveryDiscrepancyInfrastructure,
				Source:   "operational store",
				Field:    "active invocation",
				Expected: "active invocation lookup",
				Observed: "store does not expose active invocation projection",
			})
		}
		return
	}
	active, err := activeStore.ActiveInvocation(ctx, run.ID)
	if err != nil {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
			Kind:     RecoveryDiscrepancyInfrastructure,
			Source:   "operational store",
			Field:    "active invocation",
			Expected: "read active invocation",
			Observed: err.Error(),
		})
		return
	}
	if active == nil {
		return
	}
	diagnosis.InvocationExists = true
	hasNativeSession := strings.TrimSpace(active.NativeSessionID) != ""
	if !hasNativeSession {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
			Kind:     RecoveryDiscrepancyInfrastructure,
			Source:   "native session",
			Field:    "identity",
			Expected: "persisted native session identifier",
			Observed: "empty",
		})
	}
	if s.deps.Worker == nil {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
			Kind:     RecoveryDiscrepancyInfrastructure,
			Source:   "worker",
			Field:    "runtime",
			Expected: "worker runtime for active invocation",
			Observed: "worker runtime unavailable",
		})
		return
	}
	workerInspection, inspectErr := s.deps.Worker.Inspect(ctx, run.ID)
	if inspectErr != nil {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
			Kind:     RecoveryDiscrepancyInfrastructure,
			Source:   "worker",
			Field:    "inspection",
			Expected: "read active worker",
			Observed: inspectErr.Error(),
		})
	} else {
		if !workerInspection.Exists {
			addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
				Kind:     RecoveryDiscrepancyInfrastructure,
				Source:   "worker",
				Field:    "existence",
				Expected: "persisted worker",
				Observed: "missing",
			})
		} else if !workerInspection.Running {
			addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
				Kind:     RecoveryDiscrepancyInfrastructure,
				Source:   "worker",
				Field:    "lifecycle",
				Expected: "running",
				Observed: "stopped",
			})
		} else {
			packet, packetErr := decodeSpecificationPacket(run.SpecificationPacket)
			if packetErr != nil {
				addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
					Kind:     RecoveryDiscrepancyWorkflow,
					Source:   "run",
					Field:    "specification packet",
					Expected: "decodable packet for active worker",
					Observed: packetErr.Error(),
				})
			} else {
				if !workerImageMatches(workerInspection.Image, packet.RepositoryConfig.WorkerBuild.Image, run.ImageDigest) {
					addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
						Kind:     RecoveryDiscrepancyInfrastructure,
						Source:   "worker",
						Field:    "image",
						Expected: packet.RepositoryConfig.WorkerBuild.Image + "@" + run.ImageDigest,
						Observed: workerInspection.Image,
					})
				}
				workerRequest := worker.StartRequest{
					RunID:             run.ID,
					WorktreePath:      run.Worktree,
					GitMetadataPath:   gitMetadataProjectionPath(run.ID, run.Worktree),
					Image:             packet.RepositoryConfig.WorkerBuild.Image,
					ImageDigest:       run.ImageDigest,
					Caches:            workerCaches(packet.RepositoryConfig.Caches),
					InvocationPath:    active.InvocationDirectory,
					ResultPath:        active.ResultDirectory,
					CredentialStoreID: active.CredentialStoreID,
					Role:              active.Role,
				}
				known, matches := workerInspection.MountContractStatus(workerRequest)
				if !known {
					addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
						Kind:     RecoveryDiscrepancyInfrastructure,
						Source:   "worker",
						Field:    "mount contract inspection",
						Expected: "adapter-owned worker mount identities",
						Observed: "unavailable",
					})
				} else if !matches {
					addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
						Kind:     RecoveryDiscrepancyInfrastructure,
						Source:   "worker",
						Field:    "mount contract",
						Expected: "persisted worktree, Git, cache, invocation, result, and role identities",
						Observed: "worker mount identity mismatch",
					})
				}
			}
		}
	}

	terminalRuntime := s.deps.Terminal
	if terminalRuntime == nil {
		terminalRuntime = terminal.NewCmuxRuntime(nil, registration.Cmux.SocketPath)
	}
	terminalInspector, ok := terminalRuntime.(terminal.WorkspaceInspector)
	if !ok {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
			Kind:     RecoveryDiscrepancyInfrastructure,
			Source:   "cmux",
			Field:    "inspection",
			Expected: "read persisted workspace and surface identities",
			Observed: "terminal inspector unavailable",
		})
	} else {
		inspectTerminalProjection(ctx, diagnosis, terminalInspector, *active)
	}

	_, harnessRuntime, runtimeErr := s.ensureAgentRuntime(registration.Cmux.SocketPath, config.Harness(active.Harness))
	if runtimeErr != nil {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
			Kind:     RecoveryDiscrepancyInfrastructure,
			Source:   "harness",
			Field:    "adapter",
			Expected: active.Harness,
			Observed: runtimeErr.Error(),
		})
		return
	}
	if !hasNativeSession {
		return
	}
	if inspector, ok := harnessRuntime.(harness.NativeSessionInspector); ok {
		observed, providerErr := inspector.NativeSessionID(ctx, harness.NativeSessionRequest{RunID: run.ID, Harness: active.Harness})
		if providerErr != nil {
			addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
				Kind:     RecoveryDiscrepancyInfrastructure,
				Source:   "native session",
				Field:    "identity",
				Expected: active.NativeSessionID,
				Observed: providerErr.Error(),
			})
		} else if observed != active.NativeSessionID {
			addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
				Kind:     RecoveryDiscrepancyInfrastructure,
				Source:   "native session",
				Field:    "identity",
				Expected: active.NativeSessionID,
				Observed: observed,
			})
		}
	} else {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
			Kind:     RecoveryDiscrepancyInfrastructure,
			Source:   "native session",
			Field:    "inspection",
			Expected: "adapter-owned native session inspection",
			Observed: "harness adapter does not expose native session inspection",
		})
	}
}

// inspectTerminalProjection compares every persisted non-empty cmux handle
// with the current read-only topology.
func inspectTerminalProjection(ctx context.Context, diagnosis *RecoveryDiagnosis, inspector terminal.WorkspaceInspector, invocation store.Invocation) {
	workspaceID := terminal.WorkspaceID(invocation.WorkspaceID)
	if workspaceID == "" {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Kind: RecoveryDiscrepancyInfrastructure, Source: "cmux", Field: "workspace", Expected: "persisted workspace identity", Observed: "empty"})
		return
	}
	observed, err := inspector.InspectWorkspace(ctx, workspaceID)
	if err != nil {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Kind: RecoveryDiscrepancyInfrastructure, Source: "cmux", Field: "inspection", Expected: string(workspaceID), Observed: err.Error()})
		return
	}
	if !observed.Exists {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Kind: RecoveryDiscrepancyInfrastructure, Source: "cmux", Field: "workspace", Expected: string(workspaceID), Observed: "missing"})
		return
	}
	if observed.WorkspaceID != "" && observed.WorkspaceID != workspaceID {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Kind: RecoveryDiscrepancyInfrastructure, Source: "cmux", Field: "workspace identity", Expected: string(workspaceID), Observed: string(observed.WorkspaceID)})
	}
	expectedSurfaces := []struct {
		name string
		id   string
	}{
		{name: "status", id: invocation.StatusSurfaceID},
		{name: "implementation", id: invocation.ImplementationSurfaceID},
		{name: "checks", id: invocation.ChecksSurfaceID},
	}
	observedIDs := make(map[string]struct{}, len(observed.Surfaces))
	observedSurfaces := make(map[string]terminal.Surface, len(observed.Surfaces))
	for _, surface := range observed.Surfaces {
		observedIDs[string(surface.ID)] = struct{}{}
		observedSurfaces[string(surface.ID)] = surface
	}
	for _, expected := range expectedSurfaces {
		if strings.TrimSpace(expected.id) == "" {
			continue
		}
		if _, exists := observedIDs[expected.id]; !exists {
			addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Kind: RecoveryDiscrepancyInfrastructure, Source: "cmux", Field: expected.name + " surface", Expected: expected.id, Observed: "missing"})
			continue
		}
		if surface := observedSurfaces[expected.id]; surface.WorkspaceID != "" && surface.WorkspaceID != workspaceID {
			addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Kind: RecoveryDiscrepancyInfrastructure, Source: "cmux", Field: expected.name + " workspace", Expected: string(workspaceID), Observed: string(surface.WorkspaceID)})
		}
	}
}

// Reconcile performs one explicit restart reconciliation for the registered
// repository. Start and every progression entry point invoke the same private
// pass automatically; this method exposes the separate read-only diagnosis and
// the reconciliation outcome to operators and embedders.
func (s *Service) Reconcile(ctx context.Context) (RecoveryResult, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	registration, runStore, err := s.openRunStore(ctx)
	if err != nil {
		return RecoveryResult{}, err
	}
	defer func() { _ = runStore.Close() }()
	run, err := readReconciliationRun(ctx, runStore)
	if err != nil {
		return RecoveryResult{}, fmt.Errorf("read active run for reconciliation: %w", err)
	}
	if run == nil {
		return RecoveryResult{Outcome: RecoveryOutcomeReconciled}, nil
	}
	updated, diagnosis, outcome, reconcileErr := s.reconcileInterruptedRun(ctx, registration, runStore, *run)
	appendPendingEffectAction(&diagnosis)
	result := RecoveryResult{Run: &updated, Diagnosis: diagnosis, Outcome: outcome}
	return result, reconcileErr
}

// AbandonPendingEffect clears one durable effect only after an explicit human
// decision, then pauses the run so a later progression cannot guess whether a
// partially applied external mutation should be replayed.
func (s *Service) AbandonPendingEffect(ctx context.Context, request AbandonPendingEffectRequest) (RecoveryResult, error) {
	if strings.TrimSpace(request.EffectID) == "" || strings.ContainsAny(request.EffectID, "\x00\r\n") {
		return RecoveryResult{}, errors.New("pending effect id is required")
	}
	if strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 512 || strings.ContainsAny(request.Reason, "\x00\r\n") {
		return RecoveryResult{}, errors.New("pending effect abandonment reason is required")
	}
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	registration, runStore, err := s.openRunStore(ctx)
	if err != nil {
		return RecoveryResult{}, err
	}
	defer func() { _ = runStore.Close() }()
	run, err := readReconciliationRun(ctx, runStore)
	if err != nil {
		return RecoveryResult{}, fmt.Errorf("read run for pending effect abandonment: %w", err)
	}
	if run == nil {
		return RecoveryResult{Outcome: RecoveryOutcomeReconciled}, nil
	}
	if request.RunID != "" && request.RunID != run.ID {
		return RecoveryResult{}, fmt.Errorf("active run is %s, not %s", run.ID, request.RunID)
	}
	abandoner, ok := runStore.(PendingEffectAbandoner)
	if !ok {
		return RecoveryResult{}, errors.New("operational store does not support pending effect abandonment")
	}
	journal, ok := runStore.(PendingEffectStore)
	if !ok {
		return RecoveryResult{}, errors.New("operational store does not support pending effect inspection")
	}
	pending, err := journal.PendingEffect(ctx, run.ID)
	if err != nil {
		return RecoveryResult{}, fmt.Errorf("read pending effect for abandonment: %w", err)
	}
	if pending == nil {
		return RecoveryResult{Run: run, Diagnosis: reconciledRecoveryDiagnosis(run.ID), Outcome: RecoveryOutcomeReconciled}, nil
	}
	if pending.ID != request.EffectID {
		return RecoveryResult{}, fmt.Errorf("pending effect for run %q is %q, not %q", run.ID, pending.ID, request.EffectID)
	}
	abandoned := *pending
	if err := abandoner.AbandonPendingEffect(ctx, run.ID, request.EffectID, request.Reason); err != nil {
		return RecoveryResult{}, fmt.Errorf("abandon pending effect %q: %w", request.EffectID, err)
	}
	diagnosis := newRecoveryDiagnosis(run.ID)
	diagnosis.PendingEffect = &abandoned
	diagnosis.SourcesAgree = false
	addRecoveryDiscrepancy(&diagnosis, RecoveryDiscrepancy{
		Kind:     RecoveryDiscrepancyInfrastructure,
		Source:   "pending effect",
		Field:    string(pending.Kind),
		Expected: "human-reviewed effect outcome",
		Observed: "abandoned: " + safeStatusCommentValue(request.Reason),
	})
	if store.IsTerminalStatus(run.Status) {
		return RecoveryResult{Run: run, Diagnosis: diagnosis, Outcome: RecoveryOutcomeReconciled}, nil
	}
	paused, pauseErr := s.pauseForRecovery(ctx, registration, runStore, *run, diagnosis)
	if pauseErr != nil {
		return RecoveryResult{Run: &paused, Diagnosis: diagnosis, Outcome: RecoveryOutcomeWaitingForHuman}, fmt.Errorf("pause run after abandoning pending effect: %w", pauseErr)
	}
	return RecoveryResult{Run: &paused, Diagnosis: diagnosis, Outcome: RecoveryOutcomeWaitingForHuman}, nil
}

// readReconciliationRun returns the active run, or the latest terminal run
// when no active run exists. The latter matters because result acceptance can
// persist its terminal projection immediately before the journal clear; a
// restart must still drain that one pending effect.
func readReconciliationRun(ctx context.Context, runStore OperationalStore) (*store.Run, error) {
	run, err := runStore.CurrentRun(ctx)
	if err != nil || run != nil {
		return run, err
	}
	latestStore, ok := runStore.(LatestRunStore)
	if !ok {
		return nil, nil
	}
	return latestStore.LatestRun(ctx)
}

// reconcileInterruptedRun consumes a durable pending effect, then compares all
// available projections. Only a known in-flight effect or the explicitly
// recoverable loss of a worker may be continued automatically; every other
// disagreement pauses the run for human reconciliation.
func (s *Service) reconcileInterruptedRun(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run) (store.Run, RecoveryDiagnosis, RecoveryOutcome, error) {
	journal, ok := runStore.(PendingEffectStore)
	if !ok {
		if store.IsTerminalStatus(run.Status) {
			return run, reconciledRecoveryDiagnosis(run.ID), RecoveryOutcomeReconciled, nil
		}
		diagnosis := s.diagnoseInterruptedRun(ctx, registration, run)
		return run, diagnosis, RecoveryOutcomeWaitingForHuman, recoveryDiscrepancyError(diagnosis)
	}
	if pending, err := journal.PendingEffect(ctx, run.ID); err != nil {
		diagnosis := s.diagnoseInterruptedRunWithStore(ctx, registration, runStore, run)
		addRecoveryDiscrepancy(&diagnosis, RecoveryDiscrepancy{
			Kind:     RecoveryDiscrepancyInfrastructure,
			Source:   "operational store",
			Field:    "pending effect",
			Expected: "read pending effect journal",
			Observed: err.Error(),
		})
		diagnosis.SourcesAgree = false
		paused := run
		if !store.IsTerminalStatus(run.Status) {
			var pauseErr error
			paused, pauseErr = s.pauseRunLocally(ctx, runStore, run, diagnosis)
			if pauseErr != nil {
				return paused, diagnosis, RecoveryOutcomeWaitingForHuman, errors.Join(&InfrastructureDiscrepancyError{Diagnosis: diagnosis}, pauseErr)
			}
		}
		return paused, diagnosis, RecoveryOutcomeWaitingForHuman, &InfrastructureDiscrepancyError{Diagnosis: diagnosis}
	} else if pending != nil {
		resumeWasAlreadyReserved := harnessResumeWasAlreadyReserved(ctx, runStore, *pending)
		updated, replayErr := s.replayPendingEffect(ctx, runStore, *pending)
		if replayErr != nil {
			diagnosis := s.diagnoseInterruptedRunWithStore(ctx, registration, runStore, run)
			diagnosis.PendingEffect = pending
			replayKind := RecoveryDiscrepancyInfrastructure
			var workflowErr *workflowProjectionError
			if errors.As(replayErr, &workflowErr) {
				replayKind = RecoveryDiscrepancyWorkflow
			}
			addRecoveryDiscrepancy(&diagnosis, RecoveryDiscrepancy{
				Kind:     replayKind,
				Source:   "pending effect",
				Field:    string(pending.Kind),
				Expected: "complete or recognize the recorded effect",
				Observed: replayErr.Error(),
			})
			diagnosis.SourcesAgree = false
			if !store.IsTerminalStatus(run.Status) {
				paused, pauseErr := s.pauseRunLocally(ctx, runStore, run, diagnosis)
				if pauseErr != nil {
					return paused, diagnosis, RecoveryOutcomeWaitingForHuman, errors.Join(recoveryErrorWithCause(diagnosis, replayErr), pauseErr)
				}
				return paused, diagnosis, RecoveryOutcomeWaitingForHuman, recoveryErrorWithCause(diagnosis, replayErr)
			}
			return run, diagnosis, RecoveryOutcomeWaitingForHuman, recoveryErrorWithCause(diagnosis, replayErr)
		}
		run = updated
		if resumeWasAlreadyReserved {
			// The reservation was persisted before the native command, so a
			// pending row with the reservation already consumed is ambiguous:
			// the command may have succeeded just before the prior process died.
			// Never issue a second native resume; force human verification.
			resumeDiagnosis := s.diagnoseInterruptedRunWithStore(ctx, registration, runStore, run)
			addRecoveryDiscrepancy(&resumeDiagnosis, RecoveryDiscrepancy{
				Kind:     RecoveryDiscrepancyInfrastructure,
				Source:   "harness",
				Field:    "native session resume",
				Expected: "one unambiguous persisted resume result",
				Observed: "resume reservation crossed the persistence boundary before the journal was cleared",
			})
			resumeDiagnosis.SourcesAgree = false
			paused, pauseErr := s.pauseForRecovery(ctx, registration, runStore, run, resumeDiagnosis)
			if pauseErr != nil {
				return paused, resumeDiagnosis, RecoveryOutcomeWaitingForHuman, errors.Join(&InfrastructureDiscrepancyError{Diagnosis: resumeDiagnosis}, pauseErr)
			}
			return paused, resumeDiagnosis, RecoveryOutcomeWaitingForHuman, &InfrastructureDiscrepancyError{Diagnosis: resumeDiagnosis}
		}
	}
	if store.IsTerminalStatus(run.Status) {
		return run, reconciledRecoveryDiagnosis(run.ID), RecoveryOutcomeReconciled, nil
	}
	diagnosis := s.diagnoseInterruptedRunWithStore(ctx, registration, runStore, run)
	if pending, err := journal.PendingEffect(ctx, run.ID); err == nil {
		diagnosis.PendingEffect = pending
	}
	if diagnosis.SourcesAgree {
		updated, repairErr := s.completePendingCheckRepair(ctx, registration, runStore, run)
		if repairErr != nil {
			addRecoveryDiscrepancy(&diagnosis, RecoveryDiscrepancy{
				Kind:     RecoveryDiscrepancyInfrastructure,
				Source:   "check-repair",
				Field:    "attempt reservation",
				Expected: "active resumed invocation or no pending attempt",
				Observed: repairErr.Error(),
			})
			diagnosis.SourcesAgree = false
		} else {
			run = updated
		}
	}
	if !diagnosis.SourcesAgree {
		paused, pauseErr := s.pauseForRecovery(ctx, registration, runStore, run, diagnosis)
		if pauseErr != nil {
			if hasWorkflowDiscrepancy(diagnosis) {
				return paused, diagnosis, RecoveryOutcomeWaitingForHuman, errors.Join(&WorkflowFailureError{RunID: run.ID, Cause: errors.New(recoveryDiscrepancyReason(diagnosis))}, pauseErr)
			}
			return paused, diagnosis, RecoveryOutcomeWaitingForHuman, errors.Join(&InfrastructureDiscrepancyError{Diagnosis: diagnosis}, pauseErr)
		}
		if hasWorkflowDiscrepancy(diagnosis) {
			return paused, diagnosis, RecoveryOutcomeWaitingForHuman, &WorkflowFailureError{RunID: run.ID, Cause: errors.New(recoveryDiscrepancyReason(diagnosis))}
		}
		return paused, diagnosis, RecoveryOutcomeWaitingForHuman, &InfrastructureDiscrepancyError{Diagnosis: diagnosis}
	}
	return run, diagnosis, RecoveryOutcomeReconciled, nil
}

// harnessResumeWasAlreadyReserved reports whether a pending native-resume
// effect had already persisted its one-resume ceiling before this replay.
// Such an effect is ambiguous and must not issue another native command.
func harnessResumeWasAlreadyReserved(ctx context.Context, runStore RunStore, effect store.PendingEffect) bool {
	if effect.Kind != store.PendingEffectKindHarnessResume {
		return false
	}
	var payload harnessResumeEffectPayload
	if err := decodePendingEffect(effect, &payload); err != nil {
		return false
	}
	invocationStore, ok := runStore.(InvocationStore)
	if !ok {
		return false
	}
	invocation, err := invocationStore.Invocation(ctx, effect.RunID, payload.Invocation.ID)
	return err == nil && invocation != nil && invocation.RecoveryResumeCount >= payload.TargetResumeCount
}

// completePendingCheckRepair closes the durable check-repair reservation once
// its replacement invocation has crossed the native resume boundary. A
// restart after that boundary must not consume the same retry budget twice or
// leave the run permanently blocked at implementation.
func (s *Service) completePendingCheckRepair(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run) (store.Run, error) {
	if run.CheckRepairPendingAttempt == 0 {
		return run, nil
	}
	activeStore, ok := runStore.(ActiveInvocationStore)
	if !ok {
		return run, errors.New("operational store does not support active check-repair lookup")
	}
	active, err := activeStore.ActiveInvocation(ctx, run.ID)
	if err != nil {
		return run, fmt.Errorf("read active check-repair invocation: %w", err)
	}
	if active == nil {
		return run, errors.New("pending check-repair attempt has no active invocation")
	}
	if active.RecoveryResumeCount == 0 {
		return run, nil
	}
	if s.deps.GitHub == nil {
		return run, errors.New("GitHub client is required to complete check-repair reservation")
	}
	issue, err := s.deps.GitHub.Issue(ctx, commandRepository(registration), run.IssueNumber)
	if err != nil {
		return run, fmt.Errorf("read issue while completing check-repair reservation: %w", err)
	}
	next := run
	next.CheckRepairAttempts = run.CheckRepairPendingAttempt
	next.CheckRepairPendingAttempt = 0
	next.LifecycleReason = fmt.Sprintf("check-repair attempt %d active", next.CheckRepairAttempts)
	next.UpdatedAt = s.deps.Now().UTC()
	updated, err := s.applyStateTransition(ctx, runStore, stateTransition{
		Repository: commandRepository(registration),
		Issue:      issue,
		Previous:   run,
		Next:       next,
	})
	if err != nil {
		return run, fmt.Errorf("persist completed check-repair reservation: %w", err)
	}
	return updated, nil
}

// reconciledRecoveryDiagnosis returns the compact result for a terminal run
// whose pending journal, if any, has been consumed successfully.
func reconciledRecoveryDiagnosis(runID string) RecoveryDiagnosis {
	diagnosis := newRecoveryDiagnosis(runID)
	diagnosis.SourcesAgree = true
	return diagnosis
}

// pauseRunLocally records a human-visible waiting state without attempting a
// second external effect. It is used when the existing pending effect or its
// journal cannot be replayed safely, so that the one-effect invariant remains
// intact for an operator to resolve.
func (s *Service) pauseRunLocally(ctx context.Context, runStore RunStore, run store.Run, diagnosis RecoveryDiagnosis) (store.Run, error) {
	if run.Status == store.StatusWaitingForHuman && strings.HasPrefix(run.LifecycleReason, "restart reconciliation paused:") {
		return run, nil
	}
	next := run
	next.Status = store.StatusWaitingForHuman
	next.LifecycleReason = recoveryDiscrepancyReason(diagnosis)
	next.Revision = run.Revision + 1
	next.UpdatedAt = s.deps.Now().UTC()
	if err := saveRunWithRetry(ctx, runStore, next); err != nil {
		return next, fmt.Errorf("persist local restart reconciliation pause: %w", err)
	}
	return next, nil
}

// pauseForRecovery records a discrepancy in the durable workflow projection
// and uses the ordinary state-transition effect journal to publish the waiting
// label and status comment when those projections are available.
func (s *Service) pauseForRecovery(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, diagnosis RecoveryDiagnosis) (store.Run, error) {
	reason := recoveryDiscrepancyReason(diagnosis)
	if run.Status == store.StatusWaitingForHuman && strings.HasPrefix(run.LifecycleReason, "restart reconciliation paused:") {
		return run, nil
	}
	next := run
	next.Status = store.StatusWaitingForHuman
	next.LifecycleReason = reason
	next.UpdatedAt = s.deps.Now().UTC()
	if journal, ok := runStore.(PendingEffectStore); ok {
		pending, err := journal.PendingEffect(ctx, run.ID)
		if err != nil {
			paused, pauseErr := s.pauseRunLocally(ctx, runStore, run, diagnosis)
			if pauseErr != nil {
				return paused, errors.Join(fmt.Errorf("inspect pending effect before pausing for reconciliation: %w", err), pauseErr)
			}
			return paused, fmt.Errorf("inspect pending effect before pausing for reconciliation: %w", err)
		}
		if pending != nil {
			return s.pauseRunLocally(ctx, runStore, run, diagnosis)
		}
	}
	if s.deps.GitHub == nil || strings.TrimSpace(next.StatusCommentID) == "" {
		if err := saveRunWithRetry(ctx, runStore, next); err != nil {
			return next, fmt.Errorf("persist restart reconciliation pause: %w", err)
		}
		return next, nil
	}
	issue, err := s.deps.GitHub.Issue(ctx, commandRepository(registration), next.IssueNumber)
	if err != nil {
		if saveErr := saveRunWithRetry(ctx, runStore, next); saveErr != nil {
			return next, errors.Join(fmt.Errorf("read issue while pausing for reconciliation: %w", err), saveErr)
		}
		return next, fmt.Errorf("read issue while pausing for reconciliation: %w", err)
	}
	updated, err := s.applyStateTransition(ctx, runStore, stateTransition{
		Repository: commandRepository(registration),
		Issue:      issue,
		Previous:   run,
		Next:       next,
	})
	if err != nil {
		return updated, fmt.Errorf("publish restart reconciliation pause: %w", err)
	}
	return updated, nil
}

// recoveryDiscrepancyReason renders a bounded single-line workflow reason so
// the human can identify exactly which projection prevented continuation.
func recoveryDiscrepancyReason(diagnosis RecoveryDiagnosis) string {
	parts := make([]string, 0, len(diagnosis.Discrepancies))
	for _, discrepancy := range diagnosis.Discrepancies {
		parts = append(parts, fmt.Sprintf("%s.%s expected=%q observed=%q", discrepancy.Source, discrepancy.Field, safeStatusCommentValue(discrepancy.Expected), safeStatusCommentValue(discrepancy.Observed)))
	}
	if len(parts) == 0 {
		return "restart reconciliation paused: unresolved discrepancy"
	}
	return "restart reconciliation paused: " + strings.Join(parts, "; ")
}

// newRecoveryDiagnosis creates the read-only comparison envelope and operator
// actions that explain how a human can resolve a disagreement.
func newRecoveryDiagnosis(runID string) RecoveryDiagnosis {
	return RecoveryDiagnosis{
		Code:  RecoveryRequiredCode,
		RunID: runID,
		SafeActions: []string{
			"inspect the diagnosis with `factory status`",
			"verify the repository, worktree, branch, checkpoint, issue label, status comment, and pull request",
			"resolve the recorded discrepancy before retrying the run",
		},
	}
}

// appendPendingEffectAction makes the explicit abandonment path discoverable
// in status output without implying that an ambiguous mutation is safe to
// discard automatically.
func appendPendingEffectAction(diagnosis *RecoveryDiagnosis) {
	if diagnosis == nil || diagnosis.PendingEffect == nil {
		return
	}
	diagnosis.SafeActions = append(diagnosis.SafeActions, fmt.Sprintf("after human review, run `factory reconcile --abandon-effect %s --reason <reason>`", diagnosis.PendingEffect.ID))
}

// comparePersistedRunIdentity validates the values that can be checked before
// consulting external projections.
func comparePersistedRunIdentity(diagnosis *RecoveryDiagnosis, repositoryPath string, run store.Run) {
	if strings.TrimSpace(run.ID) == "" {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Kind: RecoveryDiscrepancyWorkflow, Source: "run", Field: "identifier", Expected: "non-empty run identifier", Observed: "empty"})
	}
	if strings.TrimSpace(run.RepositoryPath) == "" {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Kind: RecoveryDiscrepancyWorkflow, Source: "run", Field: "repository", Expected: repositoryPath, Observed: "empty"})
	} else if strings.TrimSpace(repositoryPath) == "" || resolvePath(run.RepositoryPath) != resolvePath(repositoryPath) {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Kind: RecoveryDiscrepancyWorkflow, Source: "run", Field: "repository", Expected: repositoryPath, Observed: run.RepositoryPath})
	}
	if strings.TrimSpace(run.Worktree) == "" {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Kind: RecoveryDiscrepancyWorkflow, Source: "run", Field: "worktree", Expected: "non-empty worktree path", Observed: "empty"})
	} else if !filepath.IsAbs(run.Worktree) {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Kind: RecoveryDiscrepancyWorkflow, Source: "run", Field: "worktree", Expected: "absolute worktree path", Observed: run.Worktree})
	}
	if strings.TrimSpace(run.Branch) == "" {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Kind: RecoveryDiscrepancyWorkflow, Source: "run", Field: "branch", Expected: "non-empty branch", Observed: "empty"})
	} else if strings.TrimSpace(run.ID) != "" {
		expectedBranch := "factory/" + run.ID
		if run.Branch != expectedBranch {
			addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Kind: RecoveryDiscrepancyWorkflow, Source: "run", Field: "branch identity", Expected: expectedBranch, Observed: run.Branch})
		}
	}
	if strings.TrimSpace(run.CheckpointSHA) == "" {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Kind: RecoveryDiscrepancyWorkflow, Source: "run", Field: "checkpoint", Expected: "non-empty checkpoint SHA", Observed: "empty"})
	}
	if run.IssueNumber <= 0 {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Kind: RecoveryDiscrepancyWorkflow, Source: "run", Field: "issue", Expected: "positive issue number", Observed: fmt.Sprintf("%d", run.IssueNumber)})
	}
	if run.PullRequestNumber < 0 {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Kind: RecoveryDiscrepancyWorkflow, Source: "run", Field: "pull request", Expected: "zero or positive pull-request number", Observed: fmt.Sprintf("%d", run.PullRequestNumber)})
	}
	if run.PullRequestNumber == 0 && strings.TrimSpace(run.PullRequestURL) != "" {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Kind: RecoveryDiscrepancyWorkflow, Source: "run", Field: "pull request", Expected: "number for persisted pull-request URL", Observed: run.PullRequestURL})
	}
}

// inspectWorktreeProjection compares the persisted checkout identity with a
// read-only Git inspection and records observation failures as discrepancies.
func inspectWorktreeProjection(ctx context.Context, diagnosis *RecoveryDiagnosis, inspector gitadapter.WorktreeInspector, repositoryPath string, run store.Run) {
	if strings.TrimSpace(run.Worktree) == "" || !filepath.IsAbs(run.Worktree) {
		return
	}
	info, err := os.Stat(run.Worktree)
	if err != nil {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "worktree", Field: "path", Expected: "existing directory", Observed: err.Error()})
		return
	}
	if !info.IsDir() {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "worktree", Field: "path", Expected: "directory", Observed: "not a directory"})
		return
	}
	if inspector == nil {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "worktree", Field: "inspection", Expected: "read-only Git inspection", Observed: "inspector unavailable"})
		return
	}
	state, err := inspector.Inspect(ctx, run.Worktree)
	if err != nil {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "worktree", Field: "inspection", Expected: "read-only Git inspection", Observed: err.Error()})
		return
	}
	if strings.TrimSpace(state.RepositoryPath) == "" {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "worktree", Field: "repository", Expected: repositoryPath, Observed: "empty"})
	} else if resolvePath(state.RepositoryPath) != resolvePath(repositoryPath) {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "worktree", Field: "repository", Expected: repositoryPath, Observed: state.RepositoryPath})
	}
	if state.Branch != run.Branch {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "worktree", Field: "branch", Expected: run.Branch, Observed: state.Branch})
	}
	if state.HeadSHA != run.CheckpointSHA {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "worktree", Field: "checkpoint", Expected: run.CheckpointSHA, Observed: state.HeadSHA})
	}
}

// inspectGitHubProjection compares the issue, state label, status comment, and
// optional pull request through read-only GitHub adapter methods.
func inspectGitHubProjection(ctx context.Context, diagnosis *RecoveryDiagnosis, client github.Client, pullRequests github.PullRequestClient, repository github.Repository, run store.Run) {
	if client == nil {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "client", Expected: "read-only GitHub client", Observed: "client unavailable"})
		return
	}
	issue, err := client.Issue(ctx, repository, run.IssueNumber)
	if err != nil {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "issue", Expected: fmt.Sprintf("issue #%d", run.IssueNumber), Observed: err.Error()})
	} else {
		if issue.Number != run.IssueNumber {
			addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "issue", Expected: fmt.Sprintf("#%d", run.IssueNumber), Observed: fmt.Sprintf("#%d", issue.Number)})
		}
		if issue.IsPullRequest {
			addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "issue", Expected: "issue, not pull request", Observed: "pull request"})
		}
		expectedLabel := factoryLabelForStatus(run.Status)
		actualLabels := factoryStateLabels(issue.Labels)
		if len(actualLabels) != 1 || actualLabels[0] != expectedLabel {
			addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "state label", Expected: expectedLabel, Observed: strings.Join(actualLabels, ", ")})
		}
	}

	marker := statusCommentMarker(run.ID)
	comment, commentErr := client.FindStatusComment(ctx, repository, run.IssueNumber, marker)
	if commentErr != nil {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "status comment", Expected: run.StatusCommentID, Observed: commentErr.Error()})
	} else if strings.TrimSpace(comment.ID) == "" {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "status comment", Expected: run.StatusCommentID, Observed: "missing"})
	} else if !strings.Contains(comment.Body, marker) {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "status comment marker", Expected: marker, Observed: "marker missing"})
	} else if comment.Body != statusCommentBody(run) {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "status comment", Expected: statusCommentBody(run), Observed: comment.Body})
	} else if strings.TrimSpace(run.StatusCommentID) == "" {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "status comment", Expected: "persisted comment " + comment.ID, Observed: "persisted identity missing"})
	} else if comment.ID != run.StatusCommentID {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "status comment", Expected: run.StatusCommentID, Observed: comment.ID})
	}

	inspectPullRequestProjection(ctx, diagnosis, pullRequests, repository, run)
}

// inspectPullRequestProjection checks an existing or unexpectedly discovered
// pull request without creating, updating, or otherwise mutating it.
func inspectPullRequestProjection(ctx context.Context, diagnosis *RecoveryDiagnosis, pullRequests github.PullRequestClient, repository github.Repository, run store.Run) {
	hasPersistedIdentity := run.PullRequestNumber > 0 || strings.TrimSpace(run.PullRequestURL) != ""
	if pullRequests == nil {
		if hasPersistedIdentity {
			addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "pull request", Expected: "read-only pull-request client", Observed: "client unavailable"})
		}
		return
	}
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "run", Field: "specification packet", Expected: "decodable packet for pull-request target", Observed: err.Error()})
		return
	}
	baseBranch := packet.RepositoryConfig.TargetBranch
	if strings.TrimSpace(baseBranch) == "" {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "run", Field: "target branch", Expected: "non-empty target branch", Observed: "empty"})
		return
	}
	pullRequest, err := pullRequests.FindPullRequest(ctx, repository, run.Branch, baseBranch)
	if err != nil {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "pull request", Expected: pullRequestIdentity(run), Observed: err.Error()})
		return
	}
	if pullRequest.Number == 0 {
		if hasPersistedIdentity {
			addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "pull request", Expected: pullRequestIdentity(run), Observed: "missing"})
		}
		return
	}
	if !hasPersistedIdentity {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "pull request", Expected: "no persisted pull-request identity", Observed: fmt.Sprintf("#%d", pullRequest.Number)})
		return
	}
	if pullRequest.Number != run.PullRequestNumber {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "pull request number", Expected: fmt.Sprintf("#%d", run.PullRequestNumber), Observed: fmt.Sprintf("#%d", pullRequest.Number)})
	}
	if strings.TrimSpace(run.PullRequestURL) != "" && pullRequest.URL != run.PullRequestURL {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "pull request URL", Expected: run.PullRequestURL, Observed: pullRequest.URL})
	}
	if pullRequest.HeadBranch != "" && pullRequest.HeadBranch != run.Branch {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "pull request head", Expected: run.Branch, Observed: pullRequest.HeadBranch})
	}
	if pullRequest.BaseBranch != "" && pullRequest.BaseBranch != baseBranch {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "pull request base", Expected: baseBranch, Observed: pullRequest.BaseBranch})
	}
}

// pullRequestIdentity formats the persisted PR identity for diagnostics.
func pullRequestIdentity(run store.Run) string {
	if run.PullRequestNumber > 0 && strings.TrimSpace(run.PullRequestURL) != "" {
		return fmt.Sprintf("#%d %s", run.PullRequestNumber, run.PullRequestURL)
	}
	if run.PullRequestNumber > 0 {
		return fmt.Sprintf("#%d", run.PullRequestNumber)
	}
	return run.PullRequestURL
}

// factoryStateLabels extracts and sorts only labels owned by the factory so a
// stale or duplicated lifecycle label is visible in a deterministic message.
func factoryStateLabels(labels []string) []string {
	result := make([]string, 0, len(labels))
	for _, label := range labels {
		if hasLabel(github.FactoryStateLabels, label) {
			result = append(result, label)
		}
	}
	sort.Strings(result)
	return result
}

// addRecoveryDiscrepancy appends one non-empty discrepancy to a diagnosis.
func addRecoveryDiscrepancy(diagnosis *RecoveryDiagnosis, discrepancy RecoveryDiscrepancy) {
	if diagnosis == nil {
		return
	}
	if strings.TrimSpace(discrepancy.Source) == "" {
		discrepancy.Source = "unknown"
	}
	if discrepancy.Kind == "" {
		// Persisted run invariants are deterministic workflow failures. Every
		// other untyped observation is uncertainty in an external projection and
		// therefore infrastructure-owned.
		if discrepancy.Source == "run" {
			discrepancy.Kind = RecoveryDiscrepancyWorkflow
		} else {
			discrepancy.Kind = RecoveryDiscrepancyInfrastructure
		}
	}
	diagnosis.Discrepancies = append(diagnosis.Discrepancies, discrepancy)
}

// hasWorkflowDiscrepancy reports whether reconciliation found deterministic
// workflow data that cannot be repaired by retrying an external adapter.
func hasWorkflowDiscrepancy(diagnosis RecoveryDiagnosis) bool {
	for _, discrepancy := range diagnosis.Discrepancies {
		if discrepancy.Kind == RecoveryDiscrepancyWorkflow {
			return true
		}
	}
	return false
}

// recoveryRequiredError wraps a diagnosis in the typed progression refusal.
func recoveryRequiredError(diagnosis RecoveryDiagnosis) error {
	return &RecoveryRequiredError{Diagnosis: diagnosis}
}

// recoveryDiscrepancyError classifies a legacy-store startup refusal while
// retaining RecoveryRequiredError through the typed error's unwrap chain.
func recoveryDiscrepancyError(diagnosis RecoveryDiagnosis) error {
	return recoveryErrorWithCause(diagnosis, nil)
}

// recoveryErrorWithCause returns the typed category for a diagnosis and keeps
// the compatibility recovery error discoverable through the unwrap chain.
func recoveryErrorWithCause(diagnosis RecoveryDiagnosis, cause error) error {
	if hasWorkflowDiscrepancy(diagnosis) {
		if cause == nil {
			cause = errors.New(recoveryDiscrepancyReason(diagnosis))
		}
		return &WorkflowFailureError{RunID: diagnosis.RunID, Cause: errors.Join(cause, recoveryRequiredError(diagnosis))}
	}
	return &InfrastructureDiscrepancyError{Diagnosis: diagnosis}
}
