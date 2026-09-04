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
	effectkernel "github.com/Stevie1704/sw-factory/internal/effect"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// RecoveryRequiredCode is the stable code for the pre-reconciliation safety
// boundary. Issue #21 supersedes this refusal with complete reconciliation.
const RecoveryRequiredCode = "recovery-required"

// restartReconciliationPausePrefix identifies a pause created by recovery
// itself, which is the only waiting-for-human state eligible for check-stage
// re-entry without a new agent invocation.
const restartReconciliationPausePrefix = "restart reconciliation paused:"

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
	// RecoveryOutcomeWaitingForHarness means the harness reported temporary
	// capacity pressure and the run remains eligible for an automatic retry.
	RecoveryOutcomeWaitingForHarness RecoveryOutcome = "waiting_for_harness"
)

// RecoveryDiscrepancy describes one observed disagreement between persisted
// run identity and an external projection.
type RecoveryDiscrepancy struct {
	// InvocationID identifies the active or historical invocation whose
	// external projection disagrees, when the discrepancy is invocation-scoped.
	InvocationID string
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
	// Recoverable marks an infrastructure loss that the coordinator may repair
	// from the persisted run identity, such as a stopped or missing worker.
	Recoverable bool
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
	// Recoverable reports that all discrepancies are coordinator-repairable.
	// It is informational; reconciliation still records every discrepancy.
	Recoverable bool
	// workflowCause preserves a typed deterministic cause for callers that need
	// to classify a recovery refusal beyond its bounded discrepancy projection.
	workflowCause error
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
	diagnosis.SourcesAgree = recoverySourcesAgree(diagnosis)
	diagnosis.Recoverable = len(diagnosis.Discrepancies) > 0 && diagnosis.SourcesAgree
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

// inspectInvocationProjection verifies every active invocation's durable
// external identity without launching or mutating anything. A latest terminal
// invocation is used only when no active invocation exists.
func (s *Service) inspectInvocationProjection(ctx context.Context, diagnosis *RecoveryDiagnosis, registration config.RepositoryRegistration, runStore OperationalStore, run store.Run) {
	if activeStore, ok := runStore.(ActiveInvocationsStore); ok {
		active, err := activeStore.ActiveInvocations(ctx, run.ID)
		if err != nil {
			addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
				Kind:     RecoveryDiscrepancyInfrastructure,
				Source:   "operational store",
				Field:    "active invocations",
				Expected: "read active invocations",
				Observed: err.Error(),
			})
			return
		}
		if len(active) > 0 {
			for index := range active {
				selected := selectedActiveInvocationStore{OperationalStore: runStore, active: active[index]}
				s.inspectInvocationProjectionSingle(ctx, diagnosis, registration, selected, run)
			}
			return
		}
		inactive := inactiveInvocationStore{OperationalStore: runStore}
		if latestStore, exposesLatest := runStore.(LatestInvocationStore); exposesLatest {
			s.inspectInvocationProjectionSingle(ctx, diagnosis, registration, latestInactiveInvocationStore{
				inactiveInvocationStore: inactive,
				LatestInvocationStore:   latestStore,
			}, run)
			return
		}
		s.inspectInvocationProjectionSingle(ctx, diagnosis, registration, inactive, run)
		return
	}
	s.inspectInvocationProjectionSingle(ctx, diagnosis, registration, runStore, run)
}

// inactiveInvocationStore adapts an empty plural active projection to the
// legacy single-invocation inspection implementation.
type inactiveInvocationStore struct {
	OperationalStore
}

// ActiveInvocation reports that the plural projection contains no invocation.
func (inactiveInvocationStore) ActiveInvocation(context.Context, string) (*store.Invocation, error) {
	return nil, nil
}

// latestInactiveInvocationStore preserves the optional latest-invocation
// projection while adapting an empty plural active projection.
type latestInactiveInvocationStore struct {
	inactiveInvocationStore
	LatestInvocationStore
}

// selectedActiveInvocationStore supplies one member of a plural active
// projection to the legacy single-invocation inspection implementation.
type selectedActiveInvocationStore struct {
	OperationalStore
	active store.Invocation
}

// ActiveInvocation returns the selected active invocation.
func (s selectedActiveInvocationStore) ActiveInvocation(context.Context, string) (*store.Invocation, error) {
	active := s.active
	return &active, nil
}

// inspectInvocationProjectionSingle verifies one invocation's durable external
// identities without launching or mutating anything.
func (s *Service) inspectInvocationProjectionSingle(ctx context.Context, diagnosis *RecoveryDiagnosis, registration config.RepositoryRegistration, runStore OperationalStore, run store.Run) {
	discrepancyStart := len(diagnosis.Discrepancies)
	invocationID := ""
	defer func() {
		if invocationID == "" {
			return
		}
		for index := discrepancyStart; index < len(diagnosis.Discrepancies); index++ {
			if diagnosis.Discrepancies[index].InvocationID == "" {
				diagnosis.Discrepancies[index].InvocationID = invocationID
			}
		}
	}()
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
		latestStore, exposesLatest := runStore.(LatestInvocationStore)
		if !exposesLatest {
			if _, journaled := runStore.(PendingEffectStore); journaled {
				addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
					Kind:     RecoveryDiscrepancyInfrastructure,
					Source:   "operational store",
					Field:    "latest invocation",
					Expected: "latest invocation lookup",
					Observed: "store does not expose latest invocation projection",
				})
			}
			return
		}
		active, err = latestStore.LatestInvocation(ctx, run.ID)
		if err != nil {
			addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
				Kind:     RecoveryDiscrepancyInfrastructure,
				Source:   "operational store",
				Field:    "latest invocation",
				Expected: "read latest invocation",
				Observed: err.Error(),
			})
			return
		}
		if active == nil {
			return
		}
	}
	invocationID = active.ID
	diagnosis.InvocationExists = true
	// Isolated legacy projections may omit the frozen packet entirely. The full
	// recovery diagnosis reports that absence above; invoke craft validation for
	// every persisted packet or invocation identity so malformed packets fail closed.
	if strings.TrimSpace(run.SpecificationPacket) != "" || active.PromptCraftSourcePath != "" || active.PromptCraftSHA256 != "" {
		if craftErr := validatePersistedRepositoryCraft(run, *active); craftErr != nil {
			addRepositoryCraftRecoveryDiscrepancy(diagnosis, *active, craftErr)
		}
	}
	if invocationProjectionNeverEstablished(*active) {
		// The launch boundary rolled this invocation back before its agent
		// started, so it owns no live worker, terminal, or harness projection.
		// Any workspace or surface it had already reserved was torn down by the
		// same rollback. Its identities are the durable result of that completed
		// rollback rather than restart drift, and comparing them with live
		// projections would block every later start permanently.
		return
	}
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
	if terminalInvocationProjectionExpectedStopped(run, *active) {
		// A completed implementation invocation intentionally leaves its worker
		// and native process stopped while the coordinator evaluates the check
		// stage (and readiness similarly has no live agent). The worker adapter
		// may be unable to inspect a stopped process, so its historical terminal
		// identities are not a live projection here.
		return
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
	expectedStopped := workerProjectionExpectedStopped(run, *active)
	workerProjectionLost := false
	workerInspection, inspectErr := s.deps.Worker.Inspect(ctx, workerIDForInvocation(*active))
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
			workerProjectionLost = hasNativeSession && active.Status == store.InvocationStatusActive
			if !workerProjectionExpectedStopped(run, *active) {
				addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
					Kind:        RecoveryDiscrepancyInfrastructure,
					Source:      "worker",
					Field:       "existence",
					Expected:    "persisted worker",
					Observed:    "missing",
					Recoverable: workerProjectionLost,
				})
			}
		} else if !workerInspection.Running {
			workerProjectionLost = hasNativeSession && active.Status == store.InvocationStatusActive
			if !workerProjectionExpectedStopped(run, *active) {
				addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
					Kind:        RecoveryDiscrepancyInfrastructure,
					Source:      "worker",
					Field:       "lifecycle",
					Expected:    "running",
					Observed:    "stopped",
					Recoverable: workerProjectionLost,
				})
			}
		} else if expectedStopped {
			addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
				Kind:     RecoveryDiscrepancyInfrastructure,
				Source:   "worker",
				Field:    "lifecycle",
				Expected: "stopped",
				Observed: "running",
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
					WorkerID:          workerIDForInvocation(*active),
					WorktreeReadOnly:  roleIsKind(*active, workflow.RoleKindReview),
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

	_, harnessRuntime, runtimeErr := s.lifecycleModule().ensureAgentRuntime(registration.Cmux.SocketPath, config.Harness(active.Harness))
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
	if expectedStopped {
		// A human-waiting boundary intentionally finishes the harness session
		// and stops the worker. The native session cannot be inspected without
		// a running worker, so its absence is the expected projection here
		// rather than a discrepancy that must block the coordinator.
		return
	}
	if inspector, ok := harnessRuntime.(harness.NativeSessionInspector); ok {
		observed, providerErr := inspector.NativeSessionID(ctx, harness.NativeSessionRequest{RunID: run.ID, WorkerID: workerIDForInvocation(*active), Harness: active.Harness})
		if providerErr != nil {
			addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
				Kind:        RecoveryDiscrepancyInfrastructure,
				Source:      "native session",
				Field:       "identity",
				Expected:    active.NativeSessionID,
				Observed:    providerErr.Error(),
				Recoverable: workerProjectionLost,
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
	if active.Status == store.InvocationStatusActive {
		if livenessInspector, ok := harnessRuntime.(harness.NativeSessionLivenessInspector); ok {
			running, livenessErr := livenessInspector.NativeSessionRunning(ctx, harness.NativeSessionRequest{RunID: run.ID, WorkerID: workerIDForInvocation(*active), Harness: active.Harness})
			if livenessErr != nil {
				addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
					Kind:        RecoveryDiscrepancyInfrastructure,
					Source:      "harness",
					Field:       "lifecycle",
					Expected:    "persisted native session process running",
					Observed:    livenessErr.Error(),
					Recoverable: workerProjectionLost,
				})
			} else if !running {
				addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
					Kind:        RecoveryDiscrepancyInfrastructure,
					Source:      "harness",
					Field:       "lifecycle",
					Expected:    "persisted native session process running",
					Observed:    "exited",
					Recoverable: true,
				})
			}
		}
	}
}

// workerProjectionExpectedStopped reports the workflow states whose terminal
// invocation intentionally leaves the worker stopped, such as readiness or a
// human-waiting review/clarification boundary.
func workerProjectionExpectedStopped(run store.Run, invocation store.Invocation) bool {
	if invocation.Status == store.InvocationStatusActive {
		return false
	}
	return run.Stage == store.StageReady || run.Stage == store.StageCheck || run.Status == store.StatusWaitingForHuman
}

// invocationProjectionNeverEstablished reports that a historical invocation was
// rolled back at the launch boundary before its agent ever started. The native
// session identifier is the anchor of every live projection, so a rolled-back
// invocation without one owns no worker, terminal, or harness state to compare,
// whether or not it had already reserved a workspace or a surface: the rollback
// stops the worker and closes the surface it created. The coordinator must read
// those identities as recorded history rather than as an infrastructure
// discrepancy. A legacy cannot-proceed rollback is accepted for compatibility;
// a superseded rollback is accepted only with the durable LaunchVoided marker
// and a definitively absent regular report.
func invocationProjectionNeverEstablished(invocation store.Invocation) bool {
	if strings.TrimSpace(invocation.NativeSessionID) != "" {
		return false
	}
	presence, err := structuredReportPresenceForInvocation(invocation)
	if err != nil || presence != structuredReportAbsent {
		return false
	}
	if invocation.Status == store.InvocationStatusCannotProceed {
		return true
	}
	return invocation.Status == store.InvocationStatusSuperseded && invocation.LaunchVoided
}

// terminalInvocationProjectionExpectedStopped reports the coordinator-owned
// boundaries where a completed invocation is historical rather than live. A
// draft pull request may retain a gate worker whose contract deliberately
// differs from the earlier implementation invocation.
func terminalInvocationProjectionExpectedStopped(run store.Run, invocation store.Invocation) bool {
	if invocation.Status == store.InvocationStatusActive {
		return false
	}
	if (run.Stage == store.StageCheck || run.Stage == store.StageDraftPR) && invocation.Stage == store.StageImplementation {
		return true
	}
	return run.Stage == store.StageReady
}

// hasRecoverableInvocationLoss reports whether a previously consumed automatic
// resume now has a worker, terminal, or harness projection that can be rebuilt
// from the durable invocation identity, but must still be escalated before
// resuming.
func hasRecoverableInvocationLoss(diagnosis RecoveryDiagnosis) bool {
	for _, discrepancy := range diagnosis.Discrepancies {
		if discrepancy.Recoverable && (discrepancy.Source == "worker" || discrepancy.Source == "cmux" || discrepancy.Source == "harness") {
			return true
		}
	}
	return false
}

// recoveryTargetActiveInvocation selects the active invocation whose own
// projection needs recovery. With multiple reviewers, a discrepancy for one
// worker must never cause the coordinator to resume a healthy other worker.
func recoveryTargetActiveInvocation(diagnosis RecoveryDiagnosis, activeValues []store.Invocation) *store.Invocation {
	if len(activeValues) == 0 {
		return nil
	}
	if len(activeValues) == 1 || len(diagnosis.Discrepancies) == 0 {
		return &activeValues[0]
	}
	for index := range activeValues {
		if hasRecoverableInvocationLossFor(diagnosis, activeValues[index].ID) {
			return &activeValues[index]
		}
	}
	return nil
}

// hasRecoverableInvocationLossFor reports whether one invocation owns a
// recoverable external discrepancy in the read-only recovery diagnosis.
func hasRecoverableInvocationLossFor(diagnosis RecoveryDiagnosis, invocationID string) bool {
	if strings.TrimSpace(invocationID) == "" {
		return false
	}
	for _, discrepancy := range diagnosis.Discrepancies {
		if discrepancy.InvocationID == invocationID && discrepancy.Recoverable {
			return true
		}
	}
	return false
}

// inspectTerminalProjection compares every persisted non-empty cmux handle
// with the current read-only topology.
func inspectTerminalProjection(ctx context.Context, diagnosis *RecoveryDiagnosis, inspector terminal.WorkspaceInspector, invocation store.Invocation) {
	recoverable := invocation.Status == store.InvocationStatusActive && strings.TrimSpace(invocation.NativeSessionID) != ""
	workspaceID := terminal.WorkspaceID(invocation.WorkspaceID)
	if workspaceID == "" {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Kind: RecoveryDiscrepancyInfrastructure, Source: "cmux", Field: "workspace", Expected: "persisted workspace identity", Observed: "empty", Recoverable: recoverable})
		return
	}
	observed, err := inspector.InspectWorkspace(ctx, workspaceID)
	if err != nil {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Kind: RecoveryDiscrepancyInfrastructure, Source: "cmux", Field: "inspection", Expected: string(workspaceID), Observed: err.Error()})
		return
	}
	if !observed.Exists {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Kind: RecoveryDiscrepancyInfrastructure, Source: "cmux", Field: "workspace", Expected: string(workspaceID), Observed: "missing", Recoverable: recoverable})
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
		{name: "role", id: string(invocationSurface(invocation).ID)},
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
			addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Kind: RecoveryDiscrepancyInfrastructure, Source: "cmux", Field: expected.name + " surface", Expected: expected.id, Observed: "missing", Recoverable: recoverable})
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
	abandoner, ok := runStore.(effectkernel.PendingEffectAbandoner)
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
	return s.reconcileInterruptedRunWithMode(ctx, registration, runStore, run, false)
}

// reconcileInterruptedRunWithMode drains a pending effect and reconciles every
// projection. Live liveness monitoring may opt into same-process recovery
// because it has directly observed the native process exit; ordinary startup
// reconciliation keeps the same-process duplicate-launch guard.
func (s *Service) reconcileInterruptedRunWithMode(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, allowSameProcessRecovery bool) (store.Run, RecoveryDiagnosis, RecoveryOutcome, error) {
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
				if pending.Kind == store.PendingEffectKindHarnessResume {
					classified := harness.ClassifyError(replayErr, pendingHarnessName(*pending))
					if harness.IsRateLimited(classified) {
						diagnosis.PendingEffect = nil
						paused, waitErr := s.lifecycleModule().pauseForHarnessCapacity(ctx, registration, runStore, run, pendingHarnessName(*pending))
						if waitErr != nil {
							return paused, diagnosis, RecoveryOutcomeWaitingForHarness, errors.Join(classified, waitErr)
						}
						return paused, diagnosis, RecoveryOutcomeWaitingForHarness, nil
					}
					if harness.IsAuthenticationExpired(classified) {
						diagnosis.PendingEffect = nil
						paused, pauseErr := s.lifecycleModule().pauseForAuthentication(ctx, registration, runStore, run, pendingHarnessName(*pending))
						if pauseErr != nil {
							return paused, diagnosis, RecoveryOutcomeWaitingForHuman, errors.Join(classified, pauseErr)
						}
						return paused, diagnosis, RecoveryOutcomeWaitingForHuman, classified
					}
				}
				paused, pauseErr := s.pauseRunLocally(ctx, runStore, run, diagnosis)
				if pauseErr != nil {
					return paused, diagnosis, RecoveryOutcomeWaitingForHuman, errors.Join(recoveryErrorWithCause(diagnosis, replayErr), pauseErr)
				}
				return paused, diagnosis, RecoveryOutcomeWaitingForHuman, recoveryErrorWithCause(diagnosis, replayErr)
			}
			return run, diagnosis, RecoveryOutcomeWaitingForHuman, recoveryErrorWithCause(diagnosis, replayErr)
		}
		run = updated
		if pending.Kind == store.PendingEffectKindResultAcceptance {
			acceptedInvocation, acceptedReport, isReview, reviewErr := readAcceptedReviewReportFromEffect(*pending)
			if reviewErr != nil {
				return s.pauseAfterReplayedReviewProjectionError(ctx, registration, runStore, run, reviewErr)
			}
			if isReview {
				if acceptedInvocation.RunID != run.ID {
					return s.pauseAfterReplayedReviewProjectionError(ctx, registration, runStore, run, fmt.Errorf("accepted review invocation belongs to run %q, current run is %q", acceptedInvocation.RunID, run.ID))
				}
				if _, reviewErr := s.acceptReviewReport(ctx, registration, runStore, &run, &acceptedInvocation, acceptedReport); reviewErr != nil {
					return s.pauseAfterReplayedReviewProjectionError(ctx, registration, runStore, run, reviewErr)
				}
			}
		}
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
	if run.Status == store.StatusWaitingForHarness {
		// Capacity waiting is an intentional coordinator state. Its active
		// invocation may have no native identity yet, so ordinary interrupted
		// projection checks would incorrectly turn a retryable wait into a human
		// discrepancy.
		return run, waitingForHarnessDiagnosis(run.ID), RecoveryOutcomeWaitingForHarness, nil
	}
	if store.IsTerminalStatus(run.Status) {
		return run, reconciledRecoveryDiagnosis(run.ID), RecoveryOutcomeReconciled, nil
	}
	diagnosis := s.diagnoseInterruptedRunWithStore(ctx, registration, runStore, run)
	if pending, err := journal.PendingEffect(ctx, run.ID); err == nil {
		diagnosis.PendingEffect = pending
	}
	if diagnosis.SourcesAgree && run.Status == store.StatusActive {
		activeValues, activeSupported, activeErr := activeInvocationsForRun(ctx, runStore, run.ID)
		if activeSupported {
			if activeErr != nil {
				addRecoveryDiscrepancy(&diagnosis, RecoveryDiscrepancy{
					Kind:     RecoveryDiscrepancyInfrastructure,
					Source:   "operational store",
					Field:    "active invocation",
					Expected: "read active invocation before automatic recovery",
					Observed: activeErr.Error(),
				})
				diagnosis.SourcesAgree = false
			} else if active := recoveryTargetActiveInvocation(diagnosis, activeValues); active != nil && (allowSameProcessRecovery || !s.invocationStartedHere(active.ID)) && active.Status == store.InvocationStatusActive && !active.AttachRequired && strings.TrimSpace(active.NativeSessionID) != "" {
				if diagnosis.SourcesAgree && active.RecoveryResumeCount > 0 && !hasRecoverableInvocationLoss(diagnosis) {
					if credentialErr := s.lifecycleModule().restoreCredentialProjection(ctx, registration, run, *active); credentialErr != nil {
						return s.pauseForCredentialProjection(ctx, registration, runStore, run, &diagnosis, active.Harness, credentialErr)
					}
				}
				if active.RecoveryResumeCount > 0 && hasRecoverableInvocationLoss(diagnosis) {
					_, repairedInvocation, repairErr := s.lifecycleModule().ensureWorkerForInvocation(ctx, registration, runStore, run, *active)
					if repairErr != nil {
						if paused, pausedDiagnosis, outcome, credentialPauseErr, handled := s.pauseForCredentialProjectionError(ctx, registration, runStore, run, &diagnosis, active.Harness, repairErr); handled {
							return paused, pausedDiagnosis, outcome, credentialPauseErr
						}
						addRecoveryDiscrepancy(&diagnosis, RecoveryDiscrepancy{
							Kind:     RecoveryDiscrepancyInfrastructure,
							Source:   "worker",
							Field:    "recreation after automatic recovery",
							Expected: "worker recreated from the frozen invocation request",
							Observed: repairErr.Error(),
						})
						diagnosis.SourcesAgree = false
					} else {
						if credentialErr := s.lifecycleModule().restoreCredentialProjection(ctx, registration, run, repairedInvocation); credentialErr != nil {
							return s.pauseForCredentialProjection(ctx, registration, runStore, run, &diagnosis, active.Harness, credentialErr)
						}
						paused, pauseErr := s.lifecycleModule().pauseForManualRecovery(ctx, registration, runStore, run, active.Harness, harness.NewUnexpectedExitError(active.Harness))
						if pauseErr != nil {
							return paused, diagnosis, RecoveryOutcomeWaitingForHuman, pauseErr
						}
						return paused, diagnosis, RecoveryOutcomeWaitingForHuman, harness.NewUnexpectedExitError(active.Harness)
					}
				}
				if diagnosis.SourcesAgree && active.RecoveryResumeCount == 0 {
					_, resumeErr := s.lifecycleModule().resumePersistedInvocation(ctx, registration, runStore, run, *active)
					if resumeErr != nil {
						if paused, pausedDiagnosis, outcome, credentialPauseErr, handled := s.pauseForCredentialProjectionError(ctx, registration, runStore, run, &diagnosis, active.Harness, resumeErr); handled {
							return paused, pausedDiagnosis, outcome, credentialPauseErr
						}
						classified := harness.ClassifyError(resumeErr, active.Harness)
						if harness.IsRateLimited(classified) {
							paused, waitErr := s.lifecycleModule().pauseForHarnessCapacity(ctx, registration, runStore, run, active.Harness)
							if waitErr != nil {
								return paused, diagnosis, RecoveryOutcomeWaitingForHarness, errors.Join(classified, waitErr)
							}
							return paused, diagnosis, RecoveryOutcomeWaitingForHarness, nil
						}
						if harness.IsAuthenticationExpired(classified) {
							paused, pauseErr := s.lifecycleModule().pauseForAuthentication(ctx, registration, runStore, run, active.Harness)
							if pauseErr != nil {
								return paused, diagnosis, RecoveryOutcomeWaitingForHuman, errors.Join(classified, pauseErr)
							}
							return paused, diagnosis, RecoveryOutcomeWaitingForHuman, classified
						}
						if harness.IsUnexpectedExit(classified) {
							paused, pauseErr := s.lifecycleModule().pauseForManualRecovery(ctx, registration, runStore, run, active.Harness, classified)
							if pauseErr != nil {
								return paused, diagnosis, RecoveryOutcomeWaitingForHuman, errors.Join(classified, pauseErr)
							}
							return paused, diagnosis, RecoveryOutcomeWaitingForHuman, classified
						}
						observed := resumeErr.Error()
						addRecoveryDiscrepancy(&diagnosis, RecoveryDiscrepancy{
							Kind:     RecoveryDiscrepancyInfrastructure,
							Source:   "harness",
							Field:    "native session resume",
							Expected: "resume persisted session exactly once",
							Observed: observed,
						})
						diagnosis.SourcesAgree = false
						paused, pauseErr := s.pauseForRecovery(ctx, registration, runStore, run, diagnosis)
						if pauseErr != nil {
							return paused, diagnosis, RecoveryOutcomeWaitingForHuman, errors.Join(classified, pauseErr)
						}
						return paused, diagnosis, RecoveryOutcomeWaitingForHuman, classified
					}
				}
			}
		}
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
				return paused, diagnosis, RecoveryOutcomeWaitingForHuman, errors.Join(&WorkflowFailureError{RunID: run.ID, Cause: recoveryWorkflowCause(diagnosis)}, pauseErr)
			}
			return paused, diagnosis, RecoveryOutcomeWaitingForHuman, errors.Join(&InfrastructureDiscrepancyError{Diagnosis: diagnosis}, pauseErr)
		}
		if recoveryProjectionOnly(diagnosis) {
			finalDiagnosis := s.diagnoseInterruptedRunWithStore(ctx, registration, runStore, paused)
			if pending, pendingErr := journal.PendingEffect(ctx, paused.ID); pendingErr != nil {
				addRecoveryDiscrepancy(&finalDiagnosis, RecoveryDiscrepancy{
					Kind:     RecoveryDiscrepancyInfrastructure,
					Source:   "operational store",
					Field:    "pending effect",
					Expected: "no pending effect after recovery pause",
					Observed: pendingErr.Error(),
				})
				finalDiagnosis.SourcesAgree = false
			} else if pending != nil {
				finalDiagnosis.PendingEffect = pending
				addRecoveryDiscrepancy(&finalDiagnosis, RecoveryDiscrepancy{
					Kind:     RecoveryDiscrepancyInfrastructure,
					Source:   "pending effect",
					Field:    string(pending.Kind),
					Expected: "no pending effect after recovery pause",
					Observed: "still pending",
				})
				finalDiagnosis.SourcesAgree = false
			}
			if finalDiagnosis.SourcesAgree {
				return paused, finalDiagnosis, RecoveryOutcomeReconciled, nil
			}
			diagnosis = finalDiagnosis
		}
		if hasWorkflowDiscrepancy(diagnosis) {
			return paused, diagnosis, RecoveryOutcomeWaitingForHuman, &WorkflowFailureError{RunID: run.ID, Cause: recoveryWorkflowCause(diagnosis)}
		}
		return paused, diagnosis, RecoveryOutcomeWaitingForHuman, &InfrastructureDiscrepancyError{Diagnosis: diagnosis}
	}
	return run, diagnosis, RecoveryOutcomeReconciled, nil
}

// pauseAfterReplayedReviewProjectionError records a bounded workflow discrepancy
// when the journaled harness boundary completed but the review-specific
// coordinator projection could not be resumed. The accepted invocation and
// its durable result remain available for human inspection; no second report
// effect is attempted from this failure path.
func (s *Service) pauseAfterReplayedReviewProjectionError(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, cause error) (store.Run, RecoveryDiagnosis, RecoveryOutcome, error) {
	diagnosis := s.diagnoseInterruptedRunWithStore(ctx, registration, runStore, run)
	addRecoveryDiscrepancy(&diagnosis, RecoveryDiscrepancy{
		Kind:     RecoveryDiscrepancyWorkflow,
		Source:   "pending effect",
		Field:    "review result projection",
		Expected: "replayed review result routed through the coordinator",
		Observed: cause.Error(),
	})
	diagnosis.SourcesAgree = false
	paused, pauseErr := s.pauseRunLocally(ctx, runStore, run, diagnosis)
	if pauseErr != nil {
		return paused, diagnosis, RecoveryOutcomeWaitingForHuman, errors.Join(recoveryErrorWithCause(diagnosis, cause), pauseErr)
	}
	return paused, diagnosis, RecoveryOutcomeWaitingForHuman, recoveryErrorWithCause(diagnosis, cause)
}

// pauseForCredentialProjection records a bounded credential discrepancy and
// transitions the run through the same stopped-worker authentication boundary
// used for an expired harness credential.
func (s *Service) pauseForCredentialProjection(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, diagnosis *RecoveryDiagnosis, harnessName string, cause error) (store.Run, RecoveryDiagnosis, RecoveryOutcome, error) {
	recordCredentialProjectionDiscrepancy(diagnosis)
	paused, pauseErr := s.lifecycleModule().pauseForAuthentication(ctx, registration, runStore, run, harnessName)
	if diagnosis == nil {
		return paused, RecoveryDiagnosis{}, RecoveryOutcomeWaitingForHuman, errors.Join(cause, pauseErr)
	}
	return paused, *diagnosis, RecoveryOutcomeWaitingForHuman, errors.Join(cause, pauseErr)
}

// pauseForCredentialProjectionError handles a credential failure from either
// worker preparation or native resume and reports whether it recognized one.
func (s *Service) pauseForCredentialProjectionError(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, diagnosis *RecoveryDiagnosis, harnessName string, cause error) (store.Run, RecoveryDiagnosis, RecoveryOutcome, error, bool) {
	var credentialErr *credentialProjectionError
	if !errors.As(cause, &credentialErr) {
		if diagnosis == nil {
			return run, RecoveryDiagnosis{}, "", nil, false
		}
		return run, *diagnosis, "", nil, false
	}
	paused, pausedDiagnosis, outcome, pauseErr := s.pauseForCredentialProjection(ctx, registration, runStore, run, diagnosis, harnessName, credentialErr)
	return paused, pausedDiagnosis, outcome, pauseErr, true
}

// recordCredentialProjectionDiscrepancy adds only a bounded credential-store
// observation to recovery diagnosis; source paths and adapter output stay out
// of persisted or operator-visible recovery metadata.
func recordCredentialProjectionDiscrepancy(diagnosis *RecoveryDiagnosis) {
	if diagnosis == nil {
		return
	}
	addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
		Kind:        RecoveryDiscrepancyInfrastructure,
		Source:      "credential store",
		Field:       "projection",
		Expected:    "configured credentials projected into the factory-managed store",
		Observed:    "credential projection unavailable",
		Recoverable: false,
	})
	diagnosis.SourcesAgree = false
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
	if err != nil || invocation == nil {
		return false
	}
	if payload.Manual {
		return invocation.AttachRequired
	}
	return invocation.RecoveryResumeCount >= payload.TargetResumeCount
}

// pendingHarnessName extracts the non-secret adapter identity from a pending
// harness-resume payload for a bounded waiting-state message.
func pendingHarnessName(effect store.PendingEffect) string {
	var payload harnessResumeEffectPayload
	if err := decodePendingEffect(effect, &payload); err != nil || strings.TrimSpace(payload.Invocation.Harness) == "" {
		return "harness"
	}
	return payload.Invocation.Harness
}

// waitingForHarnessDiagnosis describes an intentional capacity wait without
// manufacturing an infrastructure discrepancy from an absent native session.
func waitingForHarnessDiagnosis(runID string) RecoveryDiagnosis {
	diagnosis := newRecoveryDiagnosis(runID)
	diagnosis.SourcesAgree = true
	return diagnosis
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
	if err := s.lifecycleModule().stopActiveRunWorkers(ctx, runStore, run); err != nil {
		return run, err
	}
	reason := recoveryDiscrepancyReason(diagnosis)
	if isRestartReconciliationPause(run) {
		if run.LifecycleReason == reason {
			return run, nil
		}
		// Repeated reconciliation must refresh a stale local diagnosis without
		// advancing the pending effect's revision or crossing an external
		// boundary a second time.
		next := run
		next.LifecycleReason = reason
		next.UpdatedAt = s.deps.Now().UTC()
		if err := saveRunWithRetry(ctx, runStore, next); err != nil {
			return next, fmt.Errorf("refresh local restart reconciliation pause: %w", err)
		}
		return next, nil
	}
	next := run
	next.Status = store.StatusWaitingForHuman
	next.LifecycleReason = reason
	next.Revision = run.Revision + 1
	next.UpdatedAt = s.deps.Now().UTC()
	if err := saveRunWithRetry(ctx, runStore, next); err != nil {
		return next, fmt.Errorf("persist local restart reconciliation pause: %w", err)
	}
	return next, nil
}

// pauseForRecovery records a discrepancy in the durable workflow projection
// and uses the ordinary state-transition effect journal to publish or repair
// the waiting label and status comment when those projections are available.
func (s *Service) pauseForRecovery(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, diagnosis RecoveryDiagnosis) (store.Run, error) {
	reason := recoveryDiscrepancyReason(diagnosis)
	if isRestartReconciliationPause(run) && !hasGitHubStateProjectionDiscrepancy(diagnosis) {
		if err := s.lifecycleModule().stopActiveRunWorkers(ctx, runStore, run); err != nil {
			return run, err
		}
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
	if err := s.lifecycleModule().stopActiveRunWorkers(ctx, runStore, run); err != nil {
		return run, err
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

// isRestartReconciliationPause reports whether recovery, rather than a human
// command or harness policy, created the current waiting state.
func isRestartReconciliationPause(run store.Run) bool {
	return run.Status == store.StatusWaitingForHuman && strings.HasPrefix(run.LifecycleReason, restartReconciliationPausePrefix)
}

// hasGitHubStateProjectionDiscrepancy identifies label or status-comment drift
// that the ordinary state-transition effect can repair idempotently.
func hasGitHubStateProjectionDiscrepancy(diagnosis RecoveryDiagnosis) bool {
	for _, discrepancy := range diagnosis.Discrepancies {
		if discrepancy.Source != "github" {
			continue
		}
		switch discrepancy.Field {
		case "state label", "status comment", "status comment marker":
			return true
		}
	}
	return false
}

// recoveryProjectionOnly reports the bounded stale-projection case that can
// be rechecked after pausing. Other discrepancies remain human-owned even if
// the label or status comment was also stale.
func recoveryProjectionOnly(diagnosis RecoveryDiagnosis) bool {
	if len(diagnosis.Discrepancies) == 0 {
		return false
	}
	for _, discrepancy := range diagnosis.Discrepancies {
		if discrepancy.Source != "github" {
			return false
		}
		switch discrepancy.Field {
		case "state label", "status comment", "status comment marker":
		default:
			return false
		}
	}
	return true
}

// resumeRecoveredCheck re-enters a coordinator-owned check after recovery has
// repaired its projections. It never starts an implementation agent and keeps
// ordinary human, clarification, and check-repair pauses fail-closed.
func (s *Service) resumeRecoveredCheck(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run) (store.Run, error) {
	if !isRestartReconciliationPause(run) || run.Stage != store.StageCheck {
		return run, nil
	}
	if _, journaled := runStore.(PendingEffectStore); journaled {
		updated, _, _, reconcileErr := s.reconcileInterruptedRun(ctx, registration, runStore, run)
		if reconcileErr != nil {
			return updated, reconcileErr
		}
		run = updated
	}
	if len(run.PendingQuestions) != 0 {
		return run, errors.New("recovery-paused check has pending clarification questions")
	}
	if run.CheckRepairPendingAttempt != 0 {
		return run, fmt.Errorf("check-repair attempt %d is pending reconciliation", run.CheckRepairPendingAttempt)
	}
	next := run
	next.Status = store.StatusActive
	next.LifecycleReason = "resuming check evaluation after restart reconciliation"
	next.Revision = run.Revision + 1
	next.UpdatedAt = s.deps.Now().UTC()
	if err := s.persistAgentRunState(ctx, registration, runStore, run, next); err != nil {
		return run, fmt.Errorf("resume check evaluation after restart reconciliation: %w", err)
	}
	s.lifecycleModule().resetStartupState()
	return next, nil
}

// recoveryDiscrepancyReason renders a bounded single-line workflow reason so
// the human can identify exactly which projection prevented continuation. A
// deterministic workflow refusal takes precedence over infrastructure
// observations, because those observations may be consequences of the same
// failed transition rather than its cause.
func recoveryDiscrepancyReason(diagnosis RecoveryDiagnosis) string {
	discrepancies := diagnosis.Discrepancies
	if hasWorkflowDiscrepancy(diagnosis) {
		discrepancies = make([]RecoveryDiscrepancy, 0, len(diagnosis.Discrepancies))
		for _, discrepancy := range diagnosis.Discrepancies {
			if discrepancy.Kind == RecoveryDiscrepancyWorkflow {
				discrepancies = append(discrepancies, discrepancy)
			}
		}
	}
	parts := make([]string, 0, len(discrepancies))
	for _, discrepancy := range discrepancies {
		parts = append(parts, fmt.Sprintf("%s.%s expected=%q observed=%q", discrepancy.Source, discrepancy.Field, recoveryReasonValue(discrepancy.Expected), recoveryReasonValue(discrepancy.Observed)))
	}
	if len(parts) == 0 {
		return "restart reconciliation paused: unresolved discrepancy"
	}
	return "restart reconciliation paused: " + strings.Join(parts, "; ")
}

// recoveryReasonValue keeps a prior reconciliation pause from becoming part
// of the next pause's nested lifecycle reason while retaining the observation
// that it was a previous coordinator diagnosis.
func recoveryReasonValue(value string) string {
	value = safeStatusCommentValue(value)
	return strings.ReplaceAll(value, restartReconciliationPausePrefix, "prior reconciliation pause:")
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

// addRepositoryCraftRecoveryDiscrepancy records a typed craft identity failure
// as a workflow discrepancy without retaining any repository craft content.
func addRepositoryCraftRecoveryDiscrepancy(diagnosis *RecoveryDiagnosis, invocation store.Invocation, cause error) {
	if diagnosis == nil || cause == nil {
		return
	}
	diagnosis.workflowCause = cause
	var digestMismatch *CraftDigestMismatchError
	if errors.As(cause, &digestMismatch) {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
			InvocationID: invocation.ID,
			Kind:         RecoveryDiscrepancyWorkflow,
			Source:       "invocation packet",
			Field:        "prompt craft sha256",
			Expected:     digestMismatch.Expected,
			Observed:     digestMismatch.Observed,
		})
		return
	}
	var sourceMismatch *CraftSourcePathMismatchError
	if errors.As(cause, &sourceMismatch) {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
			InvocationID: invocation.ID,
			Kind:         RecoveryDiscrepancyWorkflow,
			Source:       "invocation packet",
			Field:        "prompt craft source path",
			Expected:     sourceMismatch.Expected,
			Observed:     sourceMismatch.Observed,
		})
		return
	}
	addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{
		InvocationID: invocation.ID,
		Kind:         RecoveryDiscrepancyWorkflow,
		Source:       "invocation packet",
		Field:        "prompt craft identity",
		Expected:     "verified packet identity",
		Observed:     cause.Error(),
	})
}

// recoverySourcesAgree reports whether every observed discrepancy is within
// the coordinator's deterministic repair boundary. Recoverable losses remain
// visible in the diagnosis but do not force an operator pause.
func recoverySourcesAgree(diagnosis RecoveryDiagnosis) bool {
	for _, discrepancy := range diagnosis.Discrepancies {
		if !discrepancy.Recoverable {
			return false
		}
	}
	return true
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
			cause = recoveryWorkflowCause(diagnosis)
		}
		return &WorkflowFailureError{RunID: diagnosis.RunID, Cause: errors.Join(cause, recoveryRequiredError(diagnosis))}
	}
	infrastructure := &InfrastructureDiscrepancyError{Diagnosis: diagnosis}
	if cause != nil && (harness.IsRateLimited(cause) || harness.IsAuthenticationExpired(cause) || harness.IsUnexpectedExit(cause)) {
		return errors.Join(cause, infrastructure)
	}
	return infrastructure
}

// recoveryWorkflowCause returns the most specific deterministic cause recorded
// during diagnosis, falling back to the bounded discrepancy summary.
func recoveryWorkflowCause(diagnosis RecoveryDiagnosis) error {
	if diagnosis.workflowCause != nil {
		return diagnosis.workflowCause
	}
	return errors.New(recoveryDiscrepancyReason(diagnosis))
}
