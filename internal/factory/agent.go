package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/prompt"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// InvocationStore is the operational-store seam required by visible agent
// launch and structured report acceptance.
type InvocationStore interface {
	RunStore
	SaveInvocation(context.Context, store.Invocation) error
	Invocation(context.Context, string, string) (*store.Invocation, error)
}

// ActiveInvocationStore is the optional restart-safe lookup used to prevent
// duplicate visible sessions for one active run.
type ActiveInvocationStore interface {
	ActiveInvocation(context.Context, string) (*store.Invocation, error)
}

// ActiveInvocationsStore is the optional restart-safe projection used by the
// concurrent review round. Implementations return every active invocation,
// while ActiveInvocationStore remains the compatibility fallback.
type ActiveInvocationsStore interface {
	ActiveInvocations(context.Context, string) ([]store.Invocation, error)
}

// activeInvocationsForRun loads all active invocations without requiring older
// embedding stores to implement the concurrent projection.
func activeInvocationsForRun(ctx context.Context, value interface{}, runID string) ([]store.Invocation, bool, error) {
	if activeStore, ok := value.(ActiveInvocationsStore); ok {
		active, err := activeStore.ActiveInvocations(ctx, runID)
		return active, true, err
	}
	if activeStore, ok := value.(ActiveInvocationStore); ok {
		active, err := activeStore.ActiveInvocation(ctx, runID)
		if err != nil || active == nil {
			return nil, true, err
		}
		return []store.Invocation{*active}, true, nil
	}
	return nil, false, nil
}

// InvocationHistoryStore is the operational-store seam used to distinguish a
// completed claim from a run that has ever attempted a visible invocation.
type InvocationHistoryStore interface {
	HasInvocation(context.Context, string) (bool, error)
}

// AgentRequest selects one coordinator-owned visible role invocation.
type AgentRequest struct {
	// RunID selects the active factory run. Empty selects the only active run.
	RunID string
	// Role is the workflow role. Empty selects the role for the active stage.
	Role string
	// Stage is the workflow stage. Empty selects the active run stage.
	Stage store.Stage
	// Harness is an optional issue-level selection constrained by repository policy.
	Harness config.Harness
	// Model is an optional policy-validated model override.
	Model string
	// ReasoningEffort is an optional policy-validated setting.
	ReasoningEffort string
	// CodexAuthPath selects the registered narrow Codex auth source when set;
	// a distinct one-off source is refused because it cannot be restored after
	// a coordinator restart without persisting the host path.
	CodexAuthPath string
	// ClaudeAuthPath selects the registered narrow Claude auth source when set;
	// a distinct one-off source is refused for the same restart-safety reason.
	ClaudeAuthPath string
	// PermittedPaths constrains production paths in the accepted handoff.
	PermittedPaths []string
	// testRevision requests a native resume of the original test session for
	// an automated objection cycle. It is coordinator-internal and never
	// exposed as a user-selectable authority.
	testRevision bool
	// reviewRepair requests a new implementation invocation carrying the
	// coordinator-owned blocking-review packet. It is coordinator-internal and
	// never exposed as a user-selectable authority.
	reviewRepair bool
	// resumeImplementation requests a fresh implementation invocation that
	// reuses the latest implementation session when the harness supports native
	// resume. It is coordinator-internal and never user-selectable.
	resumeImplementation bool
}

// AgentLaunchResult reports the persisted invocation and prompt after launch.
type AgentLaunchResult struct {
	// Invocation is the recoverable operational identity.
	Invocation store.Invocation
	// Prompt is returned for operator diagnostics and is not persisted as a transcript.
	Prompt string
	// TestPolicyMode identifies whether this invocation belongs to an
	// implementation-owned or independently staged TDD workflow.
	TestPolicyMode config.TestMode
	// Route identifies the frozen contract-first workflow route of the run.
	Route workflow.Route
}

// AgentReportRequest selects a previously launched invocation for acceptance.
type AgentReportRequest struct {
	// RunID selects the active run. Empty selects the only active run.
	RunID string
	// InvocationID selects the immutable invocation report directory.
	InvocationID string
	// PermittedPaths repeats the coordinator's path policy for this acceptance.
	PermittedPaths []string
}

// AgentResult contains the accepted invocation and its validated report.
type AgentResult struct {
	// Invocation is the updated recoverable invocation state.
	Invocation store.Invocation
	// Report is the structured proposal accepted by the coordinator.
	Report report.Report
}

// ReviewCheckpointMismatchError reports a review result that does not identify
// the immutable checkpoint assigned to its invocation. It is deliberately
// typed so callers cannot mistake a stale review for a successful round result.
type ReviewCheckpointMismatchError struct {
	// Role identifies the reviewer that produced the mismatched result.
	Role string
	// InvocationID identifies the isolated invocation that produced the result.
	InvocationID string
	// Expected is the run's immutable checkpoint.
	Expected string
	// Observed is the checkpoint named by the review result.
	Observed string
}

// Error returns a bounded checkpoint mismatch description without transcript
// or repository content.
func (e *ReviewCheckpointMismatchError) Error() string {
	if e == nil {
		return "review checkpoint mismatch"
	}
	return fmt.Sprintf("%s invocation %q reviewed checkpoint %q, want %q", e.Role, e.InvocationID, e.Observed, e.Expected)
}

// InvocationPacket is the read-only file mounted into the worker for one
// invocation. It includes no GitHub credentials or host authentication data.
type InvocationPacket struct {
	// SchemaVersion identifies this packet shape.
	SchemaVersion int `json:"schema_version"`
	// InvocationID binds the packet to one invocation.
	InvocationID string `json:"invocation_id"`
	// RunID binds the packet to one run.
	RunID string `json:"run_id"`
	// Role identifies the owning role.
	Role string `json:"role"`
	// Stage identifies the owning stage.
	Stage store.Stage `json:"stage"`
	// SpecificationPacket is the frozen claim packet.
	SpecificationPacket string `json:"specification_packet"`
	// PromptVersion identifies the versioned core prompt.
	PromptVersion string `json:"prompt_version"`
	// PromptCraftSourcePath identifies the frozen repository craft source used by
	// this invocation, when the repository selected one.
	PromptCraftSourcePath string `json:"prompt_craft_source_path,omitempty"`
	// PromptCraftSHA256 identifies the exact frozen repository craft bytes used by
	// this invocation, when the repository selected one.
	PromptCraftSHA256 string `json:"prompt_craft_sha256,omitempty"`
	// TestPolicyMode identifies the frozen TDD ownership mode for this invocation.
	TestPolicyMode config.TestMode `json:"test_policy_mode"`
	// Route identifies the frozen contract-first workflow route of the run.
	Route workflow.Route `json:"route,omitempty"`
	// DesignHandoff carries the accepted architecture design to the test role
	// on the design-acceptance route.
	DesignHandoff *store.RoleHandoff `json:"design_handoff,omitempty"`
	// PermittedPaths lists repository-relative prefixes the coordinator will
	// accept in the completed handoff.
	PermittedPaths []string `json:"permitted_paths"`
	// TestHandoff carries the accepted test-stage evidence to implementation.
	TestHandoff *store.TestHandoff `json:"test_handoff,omitempty"`
	// TestObjection carries the current implementation dispute to a resumed
	// test role.
	TestObjection *store.TestObjection `json:"test_objection,omitempty"`
	// TestRevisionAttempt identifies the active objection cycle.
	TestRevisionAttempt int `json:"test_revision_attempt,omitempty"`
	// TestRevisionBudget is the frozen objection-cycle budget.
	TestRevisionBudget int `json:"test_revision_budget,omitempty"`
	// ProtectedTestPaths records test files implementation must not edit.
	ProtectedTestPaths []store.ProtectedTestPath `json:"protected_test_paths,omitempty"`
	// TestExemption carries a provisional technical exemption to the reviewer.
	TestExemption *store.TestExemption `json:"test_exemption,omitempty"`
	// CheckRepair contains the complete failed-check context when this
	// invocation is a native-resumed repair.
	CheckRepair *CheckRepairPacket `json:"check_repair,omitempty"`
	// ReviewRepair contains the complete blocking-review context when this
	// invocation repairs a reviewed checkpoint.
	ReviewRepair *store.ReviewRepairPacket `json:"review_repair,omitempty"`
	// ReviewContext contains the exact checkpoint and bounded review inputs for
	// an isolated review invocation.
	ReviewContext *prompt.ReviewContext `json:"review_context,omitempty"`
	// Continuation records that this invocation continued a harness session that
	// already held the role's first prompt, so a rebuilt prompt keeps carrying
	// only what changed.
	Continuation bool `json:"continuation,omitempty"`
}

const (
	// invocationPacketMinimumSupportedVersion identifies the oldest append-only
	// packet shape retained for restart recovery.
	invocationPacketMinimumSupportedVersion = 1
	// invocationPacketVersion identifies the read-only invocation packet shape.
	// Version ten records whether the prompt continued an existing session.
	invocationPacketVersion = 10
	// invocationPacketFileName is the stable worker-visible packet filename.
	invocationPacketFileName = "specification.json"
)

// StartAgent prepares the frozen invocation packet, starts the pinned worker,
// creates the visible run surfaces, and launches the selected factory role
// for testing, implementation, architecture, or immutable review work.
func (s *Service) StartAgent(ctx context.Context, request AgentRequest) (result AgentLaunchResult, returnErr error) {
	request = normalizeAgentRequest(request)
	if err := validateAgentRequest(request); err != nil {
		return AgentLaunchResult{}, err
	}
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	if err := report.ValidatePermittedPaths(request.PermittedPaths); err != nil {
		return AgentLaunchResult{}, err
	}
	registration, runStore, run, err := s.openAgentStartRunStore(ctx, request)
	if err != nil {
		return AgentLaunchResult{}, err
	}
	defer func() { _ = runStore.Close() }()
	return s.startAgentWithStore(ctx, registration, runStore, run, request)
}

// AcceptAgentReport reads only the invocation report file, validates its
// identity and observed worktree state, and then lets the coordinator decide
// the resulting workflow status.
func (s *Service) AcceptAgentReport(ctx context.Context, request AgentReportRequest) (AgentResult, error) {
	if strings.TrimSpace(request.InvocationID) == "" {
		return AgentResult{}, errors.New("invocation id is required")
	}
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	registration, runStore, run, err := s.openReportRunStore(ctx)
	if err != nil {
		return AgentResult{}, err
	}
	defer func() { _ = runStore.Close() }()
	invocationStore, ok := runStore.(InvocationStore)
	if !ok {
		return AgentResult{}, errors.New("operational store does not support visible invocations")
	}
	if run == nil {
		return AgentResult{}, errors.New("no active run")
	}
	if request.RunID != "" && request.RunID != run.ID {
		return AgentResult{}, fmt.Errorf("active run is %s, not %s", run.ID, request.RunID)
	}
	if run.CheckRepairPendingAttempt != 0 {
		return AgentResult{}, fmt.Errorf("check-repair attempt %d is pending reconciliation", run.CheckRepairPendingAttempt)
	}
	invocation, err := invocationStore.Invocation(ctx, run.ID, request.InvocationID)
	if err != nil {
		return AgentResult{}, err
	}
	if invocation == nil {
		return AgentResult{}, fmt.Errorf("invocation %q does not belong to run %q", request.InvocationID, run.ID)
	}
	roleDefinition, roleDeclared := workflow.DefaultRegistry().Role(invocation.Role)
	if !roleDeclared || roleDefinition.Stage != invocation.Stage {
		return AgentResult{}, fmt.Errorf("invocation role %q does not own stage %q", invocation.Role, invocation.Stage)
	}
	isTestInvocation := roleDefinition.Kind == workflow.RoleKindTest
	isReviewInvocation := roleDefinition.Kind == workflow.RoleKindReview
	if invocation.AttachRequired {
		return AgentResult{}, fmt.Errorf("invocation %q requires `factory attach` before report acceptance", invocation.ID)
	}
	if invocation.Status != store.InvocationStatusActive {
		if journal, journaled := runStore.(PendingEffectStore); journaled {
			pending, pendingErr := journal.PendingEffect(ctx, run.ID)
			if pendingErr != nil {
				return AgentResult{}, fmt.Errorf("read pending effect for repeated report acceptance: %w", pendingErr)
			}
			// Capture the accepted report from the pending effect before replay clears it
			var acceptedReport report.Report
			var haveAcceptedReport bool
			if pending != nil && pending.Kind == store.PendingEffectKindResultAcceptance {
				if extractedReport, extractErr := readAcceptedReportFromEffect(*pending); extractErr == nil {
					acceptedReport = extractedReport
					haveAcceptedReport = true
				}
			}
			if pending != nil {
				updatedRun, replayErr := s.replayPendingEffect(ctx, runStore, *pending)
				if replayErr != nil {
					return AgentResult{}, fmt.Errorf("replay pending effect before repeated report acceptance: %w", replayErr)
				}
				*run = updatedRun
			}
			refreshed, refreshErr := invocationStore.Invocation(ctx, run.ID, request.InvocationID)
			if refreshErr != nil {
				return AgentResult{}, fmt.Errorf("refresh repeated report invocation: %w", refreshErr)
			}
			if refreshed != nil {
				invocation = refreshed
			}
			// Use the accepted report from the pending effect if available, otherwise
			// fall back to reading from the filesystem for backward compatibility
			value := acceptedReport
			var readErr error
			if !haveAcceptedReport {
				value, readErr = readAcceptedAgentReport(*invocation)
			}
			if haveAcceptedReport || readErr == nil {
				resumed, projected, projectionErr := s.resumeAcceptedStageProjection(ctx, registration, runStore, run, invocation, value)
				if projected {
					return resumed, projectionErr
				}
				return AgentResult{Invocation: *invocation, Report: value}, nil
			}
		}
		return AgentResult{}, fmt.Errorf("invocation %q is already %s", invocation.ID, invocation.Status)
	}
	if err := validateAgentRunState(*run); err != nil && !reviewCanBeAcceptedWhileWaiting(*run, *invocation) {
		return AgentResult{}, err
	}
	if run.Stage != invocation.Stage && !(isReviewInvocation && run.Stage == store.StageReview) {
		return AgentResult{}, fmt.Errorf("invocation stage %q does not match active run stage %q", invocation.Stage, run.Stage)
	}
	if len(request.PermittedPaths) > 0 {
		if err := report.ValidatePermittedPaths(request.PermittedPaths); err != nil {
			return AgentResult{}, err
		}
		if !sameStrings(request.PermittedPaths, invocation.PermittedPaths) {
			return AgentResult{}, errors.New("agent report permitted paths do not match the invocation policy")
		}
	}
	path := reportPath(*invocation)
	var value report.Report
	if isTestInvocation {
		value, err = report.ReadEnvelope(path)
	} else {
		value, err = report.Read(path)
	}
	if err != nil {
		return AgentResult{}, err
	}
	validationContext := report.ValidationContext{
		InvocationID:   invocation.ID,
		RunID:          invocation.RunID,
		Harness:        invocation.Harness,
		Role:           invocation.Role,
		Stage:          string(invocation.Stage),
		PromptVersion:  invocation.PromptVersion,
		RoleKind:       string(roleDefinition.Kind),
		WorktreePath:   run.Worktree,
		PermittedPaths: invocation.PermittedPaths,
		CheckpointSHA:  run.CheckpointSHA,
	}
	inspector := s.worktreeInspector()
	if inspector == nil {
		return AgentResult{}, errors.New("worktree runtime does not support report inspection")
	}
	state, inspectErr := inspector.Inspect(ctx, run.Worktree)
	if inspectErr != nil {
		return AgentResult{}, fmt.Errorf("inspect worktree for agent report: %w", inspectErr)
	}
	testRevisionActive := isTestInvocation && run.TestObjection != nil
	observedChanges := append([]string(nil), state.ChangedPaths...)
	if testRevisionActive {
		observedChanges, err = testRevisionChangedPaths(state, run.TestRevisionBaseChangedPaths)
		if err != nil {
			return AgentResult{}, fmt.Errorf("compute test revision worktree changes: %w", err)
		}
	}
	validationContext.ObservedChanges = observedChanges
	validationContext.WorktreeObserved = true
	if run.CheckpointSHA != "" && state.HeadSHA != run.CheckpointSHA {
		return AgentResult{}, fmt.Errorf("agent report worktree HEAD %q does not match checkpoint %q", state.HeadSHA, run.CheckpointSHA)
	}
	if isReviewInvocation && len(state.ChangedPaths) != 0 {
		return AgentResult{}, fmt.Errorf("%s changed the immutable checkpoint worktree", invocation.Role)
	}
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return AgentResult{}, fmt.Errorf("decode specification packet for agent report: %w", err)
	}
	validationContext.RepositoryGuidancePaths = append([]string(nil), packet.RepositoryGuidancePaths...)
	validationContext.RepositoryGuidanceBound = invocation.Role == workflow.RoleStandardsReview && invocation.PromptVersion != "standards-review-v1"
	if isTestInvocation && !independentTestStageDeclared(packet) {
		return AgentResult{}, errors.New("test-stage reports are unavailable in advisory mode without a selected route; implementation owns TDD")
	}
	if err := validateImplementationOwnedSignals(value, *invocation, packet); err != nil {
		return AgentResult{}, err
	}
	if err := validateReviewRepairTestOwnerRouting(value, *invocation, *run, packet); err != nil {
		return AgentResult{}, err
	}
	validationContext.TestPaths = packet.RepositoryConfig.TestPolicy.TestPaths
	validationContext.TestInfrastructurePaths = packet.RepositoryConfig.TestPolicy.InfrastructurePaths
	if isReviewInvocation && value.ReviewHandoff != nil && value.ReviewHandoff.ReviewedSHA != run.CheckpointSHA {
		return AgentResult{}, &ReviewCheckpointMismatchError{
			Role:         invocation.Role,
			InvocationID: invocation.ID,
			Expected:     run.CheckpointSHA,
			Observed:     value.ReviewHandoff.ReviewedSHA,
		}
	}
	if err := report.Validate(value, validationContext); err != nil {
		if isUnverifiableTestReport(value, *invocation) {
			if testRevisionActive {
				return s.pauseUnverifiableTestRevisionReport(ctx, registration, runStore, run, invocation, value)
			}
			return s.pauseUnverifiableTestReport(ctx, registration, runStore, run, invocation, value)
		}
		return AgentResult{}, err
	}
	if isTestInvocation {
		if err := validateTestStageReportPolicy(value, packet.RepositoryConfig.TestPolicy); err != nil {
			return AgentResult{}, err
		}
	}
	implementationObjection := implementationTestObjection(value, *invocation)
	if value.TestObjectionResponse != nil && !testRevisionActive {
		return AgentResult{}, errors.New("test objection response requires an active implementation objection")
	}
	if implementationObjection {
		if err := validateImplementationTestObjection(value, *invocation, *run, packet); err != nil {
			return AgentResult{}, err
		}
	}
	automatedObjection, objectionGateReason := false, ""
	if implementationObjection {
		automatedObjection, objectionGateReason = s.automatedTestObjectionGate(ctx, registration, packet)
	}
	if testRevisionActive {
		if err := validateTestChangedPaths(observedChanges, packet.RepositoryConfig.TestPolicy); err != nil {
			return AgentResult{}, fmt.Errorf("test revision path ownership: %w", err)
		}
	} else {
		if invocation.Role == workflow.RoleImplementation && independentTestStageDeclared(packet) && !run.TestStageSkipped {
			if err := validateImplementationTestPaths(state.ChangedPaths, packet.RepositoryConfig.TestPolicy); err != nil {
				return AgentResult{}, err
			}
		}
		if err := validateProtectedTestPaths(run.Worktree, state, run.ProtectedTestPaths); err != nil {
			return AgentResult{}, err
		}
	}
	if value.Outcome == report.OutcomeNeedsClarification {
		if err := validateClarificationQuestions(packet, run.PendingQuestions, value.Questions); err != nil {
			return AgentResult{}, err
		}
	}
	if recorder, ok := runStore.(evaluationRecorder); ok {
		if err := recorder.EnsureEvaluationSummary(ctx, *run); err != nil {
			return AgentResult{}, fmt.Errorf("ensure local evaluation summary: %w", err)
		}
		if value.BudgetExhausted {
			if err := recorder.RecordEvaluationBudgetExhaustion(ctx, run.ID); err != nil {
				return AgentResult{}, fmt.Errorf("record local evaluation budget exhaustion: %w", err)
			}
		}
		for _, exemption := range value.Exemptions {
			if err := recorder.RecordEvaluationExemption(ctx, run.ID, store.EvaluationExemption(exemption)); err != nil {
				return AgentResult{}, fmt.Errorf("record local evaluation exemption: %w", err)
			}
		}
		for _, escalation := range value.Escalations {
			if err := recorder.RecordEvaluationEscalation(ctx, run.ID, store.EvaluationEscalationCategory(escalation)); err != nil {
				return AgentResult{}, fmt.Errorf("record local evaluation escalation: %w", err)
			}
		}
		for _, blocker := range value.Blockers {
			if err := recorder.RecordEvaluationBlocker(ctx, run.ID, store.EvaluationBlockerCategory(blocker)); err != nil {
				return AgentResult{}, fmt.Errorf("record local evaluation blocker: %w", err)
			}
		}
		if value.Usage != nil {
			if err := recorder.RecordEvaluationUsage(ctx, run.ID, invocation.ID, store.EvaluationUsage{
				Available:    value.Usage.Available,
				InputTokens:  value.Usage.InputTokens,
				OutputTokens: value.Usage.OutputTokens,
				TotalTokens:  value.Usage.TotalTokens,
				CostReported: value.Usage.CostReported,
				CostMicros:   value.Usage.CostMicros,
				Currency:     value.Usage.Currency,
			}); err != nil {
				return AgentResult{}, fmt.Errorf("record local evaluation usage: %w", err)
			}
		}
		if implementationObjection || (testRevisionActive && value.TestObjectionResponse != nil) {
			if err := recorder.RecordEvaluationEscalation(ctx, run.ID, store.EvaluationEscalationTestDispute); err != nil {
				return AgentResult{}, fmt.Errorf("record test objection escalation: %w", err)
			}
		}
		switch value.Outcome {
		case report.OutcomeNeedsClarification:
			if err := recorder.RecordEvaluationEscalation(ctx, run.ID, store.EvaluationEscalationClarification); err != nil {
				return AgentResult{}, fmt.Errorf("record local evaluation clarification: %w", err)
			}
		case report.OutcomeCannotProceed:
			if err := recorder.RecordEvaluationEscalation(ctx, run.ID, store.EvaluationEscalationBlocked); err != nil {
				return AgentResult{}, fmt.Errorf("record local evaluation blocker escalation: %w", err)
			}
		}
	}
	_, harnessRuntime, err := s.ensureAgentRuntime(registration.Cmux.SocketPath, config.Harness(invocation.Harness))
	if err != nil {
		return AgentResult{}, fmt.Errorf("ensure agent runtime: %w", err)
	}
	nativeSessionID := invocation.NativeSessionID
	if invocation.NativeSessionID != "" {
		if value.NativeSessionID != "" && value.NativeSessionID != invocation.NativeSessionID {
			return AgentResult{}, errors.New("agent report native session identifier does not match persisted invocation")
		}
	} else {
		nativeSessionID = value.NativeSessionID
	}
	if nativeSessionID == "" {
		return AgentResult{}, errors.New("accepted agent report must retain a native session identifier")
	}
	if _, journaled := runStore.(PendingEffectStore); journaled {
		acceptedInvocation := *invocation
		acceptedInvocation.NativeSessionID = nativeSessionID
		acceptedInvocation.Status = acceptedInvocationStatus(value.Outcome)
		acceptedInvocation.UpdatedAt = s.deps.Now().UTC()
		previousRun := *run
		nextRun := previousRun
		releaseActiveInvocation(&nextRun, invocation.ID)
		isTestReport := isTestInvocation
		isReviewReport := isReviewInvocation
		if implementationObjection {
			nextRun, err = projectImplementationTestObjection(previousRun, value, *invocation, packet, state, automatedObjection, objectionGateReason)
			if err != nil {
				return AgentResult{}, err
			}
		} else if !isTestReport && !isReviewReport {
			nextRun = agentReportRunProjection(previousRun, invocation.Stage, value)
		}
		nextRun.Revision = previousRun.Revision + 1
		if isTestReport || isReviewReport {
			nextRun.Revision = previousRun.Revision
		}
		nextRun.UpdatedAt = s.deps.Now().UTC()
		stopWorkerAfterReport := value.Outcome == report.OutcomeNeedsClarification || isReviewReport || implementationObjection
		acceptedInvocation, nextRun, err = s.acceptResultWithEffect(ctx, runStore, invocationStore, registration, harnessRuntime, harness.Session{
			InvocationID:    acceptedInvocation.ID,
			NativeSessionID: nativeSessionID,
			Surface:         invocationSurface(acceptedInvocation),
		}, acceptedInvocation, previousRun, nextRun, stopWorkerAfterReport, value)
		if err != nil {
			return AgentResult{}, err
		}
		*invocation = acceptedInvocation
		*run = nextRun
		if implementationObjection {
			return s.finishImplementationTestObjection(ctx, registration, runStore, run, invocation, value)
		}
		if isTestReport {
			return s.acceptTestStageReport(ctx, registration, runStore, run, invocation, value, state)
		}
		if isReviewReport {
			return s.acceptSpecificationReviewReport(ctx, registration, runStore, run, invocation, value)
		}
		if value.Outcome == report.OutcomeNeedsClarification {
			published, publicationErr := s.ensureClarificationPublication(ctx, registration, runStore, *run)
			if publicationErr != nil {
				return AgentResult{}, publicationErr
			}
			*run = published
		}
		return AgentResult{Invocation: *invocation, Report: value}, nil
	}
	if err := harnessRuntime.Finish(ctx, harness.Session{InvocationID: invocation.ID, NativeSessionID: nativeSessionID, Surface: invocationSurface(*invocation)}); err != nil {
		return AgentResult{}, fmt.Errorf("finish accepted harness session: %w", err)
	}
	if value.Outcome == report.OutcomeNeedsClarification {
		if err := s.lifecycleModule().stopRunWorker(ctx, workerIDForInvocation(*invocation)); err != nil {
			return AgentResult{}, err
		}
	}
	invocation.NativeSessionID = nativeSessionID
	invocation.Status = acceptedInvocationStatus(value.Outcome)
	invocation.UpdatedAt = s.deps.Now().UTC()
	if err := invocationStore.SaveInvocation(ctx, *invocation); err != nil {
		return AgentResult{}, fmt.Errorf("persist accepted invocation: %w", err)
	}
	previousRun := *run
	releaseActiveInvocation(run, invocation.ID)
	if implementationObjection {
		*run, err = projectImplementationTestObjection(previousRun, value, *invocation, packet, state, automatedObjection, objectionGateReason)
		if err != nil {
			return AgentResult{}, err
		}
		run.Revision = previousRun.Revision + 1
		run.UpdatedAt = s.deps.Now().UTC()
		if err := s.persistAgentRunState(ctx, registration, runStore, previousRun, *run); err != nil {
			return AgentResult{}, fmt.Errorf("persist implementation test objection: %w", err)
		}
		return s.finishImplementationTestObjection(ctx, registration, runStore, run, invocation, value)
	}
	if isTestInvocation {
		return s.acceptTestStageReport(ctx, registration, runStore, run, invocation, value, state)
	}
	if roleIsKind(*invocation, workflow.RoleKindReview) {
		return s.acceptSpecificationReviewReport(ctx, registration, runStore, run, invocation, value)
	}
	*run = agentReportRunProjection(previousRun, invocation.Stage, value)
	run.UpdatedAt = s.deps.Now().UTC()
	if err := s.persistAgentRunState(ctx, registration, runStore, previousRun, *run); err != nil {
		return AgentResult{}, fmt.Errorf("persist accepted agent state: %w", err)
	}
	if value.Outcome == report.OutcomeNeedsClarification {
		published, err := s.ensureClarificationPublication(ctx, registration, runStore, *run)
		if err != nil {
			return AgentResult{}, err
		}
		*run = published
	}
	return AgentResult{Invocation: *invocation, Report: value}, nil
}

// resumeAcceptedStageProjection completes the test- or review-specific run
// transition when result acceptance made the invocation terminal but a crash
// left the durable run active at the same stage. A run that already moved away
// from the invocation stage is the durable proof that no continuation remains.
func (s *Service) resumeAcceptedStageProjection(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run, invocation *store.Invocation, value report.Report) (AgentResult, bool, error) {
	if run == nil || run.Status != store.StatusActive || (run.Stage != invocation.Stage && !(roleIsKind(*invocation, workflow.RoleKindReview) && run.Stage == store.StageReview)) {
		return AgentResult{}, false, nil
	}
	if roleIsKind(*invocation, workflow.RoleKindTest) {
		inspector := s.worktreeInspector()
		if inspector == nil {
			return AgentResult{}, true, errors.New("worktree inspector is required to resume accepted test projection")
		}
		state, err := inspector.Inspect(ctx, run.Worktree)
		if err != nil {
			return AgentResult{}, true, fmt.Errorf("inspect worktree to resume accepted test projection: %w", err)
		}
		result, err := s.acceptTestStageReport(ctx, registration, runStore, run, invocation, value, state)
		return result, true, err
	}
	if roleIsKind(*invocation, workflow.RoleKindReview) {
		result, err := s.acceptSpecificationReviewReport(ctx, registration, runStore, run, invocation, value)
		return result, true, err
	}
	return AgentResult{}, false, nil
}

// addActiveInvocation adds one invocation to the durable activity projection.
func addActiveInvocation(run *store.Run, invocationID string) {
	if run == nil || strings.TrimSpace(invocationID) == "" {
		return
	}
	for _, activeID := range run.ActiveInvocationIDs {
		if activeID == invocationID {
			return
		}
	}
	run.ActiveInvocationIDs = append(run.ActiveInvocationIDs, invocationID)
}

// releaseActiveInvocation removes one invocation from the run's delegation
// projection without hiding concurrent reviewers that are still active.
func releaseActiveInvocation(run *store.Run, invocationID string) {
	if run == nil || strings.TrimSpace(invocationID) == "" {
		return
	}
	remaining := make([]string, 0, len(run.ActiveInvocationIDs))
	for _, activeID := range run.ActiveInvocationIDs {
		if activeID != invocationID {
			remaining = append(remaining, activeID)
		}
	}
	run.ActiveInvocationIDs = remaining
}

// clearActiveInvocations clears the current activity projection when a
// transition ends every visible invocation for the run.
func clearActiveInvocations(run *store.Run) {
	if run == nil {
		return
	}
	run.ActiveInvocationIDs = nil
}

// reviewCanBeAcceptedWhileWaiting permits the serialized event loop to apply
// a second review result after the first reviewer has already put the round in
// a human-waiting state. Non-review work remains blocked by that state.
func reviewCanBeAcceptedWhileWaiting(run store.Run, invocation store.Invocation) bool {
	if run.Stage != store.StageReview || run.Status != store.StatusWaitingForHuman || !roleIsKind(invocation, workflow.RoleKindReview) {
		return false
	}
	if containsString(run.ActiveInvocationIDs, invocation.ID) {
		return true
	}
	return false
}

// reviewHasBlockingResult reports whether either isolated reviewer has a
// concrete correctness, security, specification, or standards violation.
func reviewHasBlockingResult(run store.Run) bool {
	return (reviewRoleConfigured(run, workflow.RoleSpecificationReview) && run.SpecificationReview != nil && reviewHasBlockingFindingForRole(workflow.RoleSpecificationReview, run.SpecificationReview.Findings)) ||
		(reviewRoleConfigured(run, workflow.RoleStandardsReview) && run.StandardsReview != nil && reviewHasBlockingFindingForRole(workflow.RoleStandardsReview, run.StandardsReview.Findings))
}

// containsString reports whether a string occurs in a small coordinator-owned
// projection list.
func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// acceptedInvocationStatus maps a validated report outcome to its durable
// invocation lifecycle state, keeping result acceptance consistent across all
// visible roles.
func acceptedInvocationStatus(outcome report.Outcome) store.InvocationStatus {
	switch outcome {
	case report.OutcomeCompleted:
		return store.InvocationStatusCompleted
	case report.OutcomeNeedsClarification:
		return store.InvocationStatusWaitingForHuman
	case report.OutcomeCannotProceed:
		return store.InvocationStatusCannotProceed
	default:
		return store.InvocationStatusCannotProceed
	}
}

// agentReportRunProjection applies the declared workflow transition for a
// validated generic handoff in both journaled and legacy paths. An unreadable
// frozen packet or an undeclared transition fails the run at its invocation
// stage instead of guessing a route that could skip a selected stage.
func agentReportRunProjection(previous store.Run, invocationStage store.Stage, value report.Report) store.Run {
	next := previous
	// An accepted report ends the run's delegation to its invocation, so the
	// status projection stops implying that a harness is executing.
	clearActiveInvocations(&next)
	route, readable := routeForRun(previous)
	if !readable {
		next.Stage = invocationStage
		next.Status = store.StatusFailed
		next.PendingQuestions = nil
		return next
	}
	transition, err := workflow.DefaultRegistry().ResolveRouteReportTransition(route, invocationStage, value.Outcome)
	if err != nil {
		next.Stage = invocationStage
		next.Status = store.StatusFailed
		next.PendingQuestions = nil
		return next
	}
	next.Stage = transition.Stage
	next.Status = transition.Status
	switch value.Outcome {
	case report.OutcomeNeedsClarification:
		next.PendingQuestions = pendingQuestionsFromReport(value.Questions)
	case report.OutcomeCannotProceed:
		next.PendingQuestions = nil
	default:
		if value.Handoff != nil && len(value.Handoff.ProductionFilesChanged) != 0 {
			next.RoleHandoff = roleHandoffFromReport(*value.Handoff)
		}
		next.PendingQuestions = nil
	}
	next.ClarificationCommentID = ""
	next.ClarificationNotificationSent = false
	return next
}

// readAcceptedAgentReport returns the immutable report belonging to a terminal
// invocation. It makes repeated acceptance a read-only idempotent operation for
// the durable journal store without re-finishing its harness session. This
// function reads from the filesystem and should only be used as a fallback when
// the pending effect payload is unavailable.
func readAcceptedAgentReport(invocation store.Invocation) (report.Report, error) {
	path := reportPath(invocation)
	if roleIsKind(invocation, workflow.RoleKindTest) {
		return report.ReadEnvelope(path)
	}
	return report.Read(path)
}

// readAcceptedReportFromEffect extracts the accepted report from a result
// acceptance pending effect payload. This provides the immutable snapshot that
// was validated during acceptance, avoiding re-reading mutable report.json.
func readAcceptedReportFromEffect(pending store.PendingEffect) (report.Report, error) {
	if pending.Kind != store.PendingEffectKindResultAcceptance {
		return report.Report{}, fmt.Errorf("expected result_acceptance effect, got %s", pending.Kind)
	}
	var payload struct {
		AcceptedReport string `json:"accepted_report"`
	}
	if err := json.Unmarshal([]byte(pending.Payload), &payload); err != nil {
		return report.Report{}, fmt.Errorf("decode result acceptance payload: %w", err)
	}
	if strings.TrimSpace(payload.AcceptedReport) == "" {
		return report.Report{}, errors.New("result acceptance payload has no accepted report")
	}
	var value report.Report
	if err := json.Unmarshal([]byte(payload.AcceptedReport), &value); err != nil {
		return report.Report{}, fmt.Errorf("decode accepted report from effect: %w", err)
	}
	return value, nil
}

// RunAgent launches the visible agent and accepts a report when one is already
// present. Interactive callers normally use StartAgent and accept later.
func (s *Service) RunAgent(ctx context.Context, request AgentRequest) (AgentResult, error) {
	launch, err := s.StartAgent(ctx, request)
	if err != nil {
		return AgentResult{}, err
	}
	return s.AcceptAgentReport(ctx, AgentReportRequest{RunID: launch.Invocation.RunID, InvocationID: launch.Invocation.ID, PermittedPaths: request.PermittedPaths})
}

// normalizeAgentRequest fills the path default shared by automatic role
// selection without choosing a role before the active run is loaded.
func normalizeAgentRequest(request AgentRequest) AgentRequest {
	return request
}

// validateAgentRequest rejects roles and stages outside the factory-owned
// registry before external side effects occur.
func validateAgentRequest(request AgentRequest) error {
	registry := workflow.DefaultRegistry()
	if request.Role != "" {
		if _, exists := registry.Role(request.Role); !exists {
			return &PolicyRejection{Code: PolicyRejectionRoleUnavailable, Problem: fmt.Sprintf("agent role %q is not declared by the factory-owned workflow registry", request.Role)}
		}
	}
	if request.Stage != "" {
		if _, exists := registry.RoleForInvocationStage(request.Stage); !exists {
			return &PolicyRejection{Code: PolicyRejectionStageUnavailable, Problem: fmt.Sprintf("agent stage %q is not declared by the factory-owned workflow registry", request.Stage)}
		}
	}
	if request.Role != "" && request.Stage != "" {
		definition, _ := registry.Role(request.Role)
		if definition.Stage != request.Stage {
			return &PolicyRejection{Code: PolicyRejectionRoleStageMismatch, Problem: fmt.Sprintf("agent role %q belongs to stage %q, not %q", request.Role, definition.Stage, request.Stage)}
		}
	}
	if request.Model != "" && strings.ContainsAny(request.Model, "\x00\r\n ") {
		return errors.New("agent model contains unsafe characters")
	}
	if request.ReasoningEffort != "" && strings.ContainsAny(request.ReasoningEffort, "\x00\r\n ") {
		return errors.New("agent reasoning effort contains unsafe characters")
	}
	for harnessName, path := range map[string]string{"Codex": request.CodexAuthPath, "Claude": request.ClaudeAuthPath} {
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
			return fmt.Errorf("agent %s auth path must be absolute and free of control characters", harnessName)
		}
	}
	return nil
}

// AgentPolicy is the frozen repository selection resolved for one role. The
// three settings are chosen independently, so they travel together rather than
// as interchangeable bare strings.
type AgentPolicy struct {
	// Harness is the adapter that will run the invocation.
	Harness config.Harness
	// Model is the validated model selection.
	Model string
	// ReasoningEffort is the validated reasoning-effort selection. It is empty
	// when the role declares none.
	ReasoningEffort string
}

// resolveAgentPolicy applies the frozen repository harness, model, and
// reasoning-effort policy for one role. Each setting defaults to the role's
// declared option, and only a repository-declared override permission widens
// it. Every refusal is a typed policy rejection so the coordinator can report
// it in stable vocabulary.
func resolveAgentPolicy(repository config.RepositoryConfig, request AgentRequest) (AgentPolicy, error) {
	harnessName, err := resolveHarnessPolicy(repository, request)
	if err != nil {
		return AgentPolicy{}, err
	}
	model, err := resolveModelPolicy(repository, request)
	if err != nil {
		return AgentPolicy{}, err
	}
	effort, err := resolveReasoningEffortPolicy(repository, request)
	if err != nil {
		return AgentPolicy{}, err
	}
	return AgentPolicy{Harness: harnessName, Model: model, ReasoningEffort: effort}, nil
}

// resolveHarnessPolicy selects the role's harness and refuses an override the
// repository has not explicitly permitted.
func resolveHarnessPolicy(repository config.RepositoryConfig, request AgentRequest) (config.Harness, error) {
	declared := repository.RoleHarnessDefaults[request.Role]
	selected := request.Harness
	if selected == "" {
		selected = declared
	}
	if selected != config.HarnessCodex && selected != config.HarnessClaude {
		return "", &PolicyRejection{
			Code:    PolicyRejectionHarnessUnavailable,
			Problem: fmt.Sprintf("no supported harness policy for role %q", request.Role),
		}
	}
	if request.Harness != "" && request.Harness != declared && !hasOverride(repository.AllowedOverrides, config.OverrideHarness) {
		return "", &PolicyRejection{
			Code:    PolicyRejectionHarnessOverride,
			Problem: fmt.Sprintf("repository configuration does not allow harness override from %q to %q for role %q", declared, request.Harness, request.Role),
		}
	}
	return selected, nil
}

// resolveModelPolicy selects the role's model and refuses a value outside its
// declared options unless the repository permits model overrides.
func resolveModelPolicy(repository config.RepositoryConfig, request AgentRequest) (string, error) {
	options := repository.ModelOptions[request.Role]
	selected := request.Model
	if selected == "" && len(options) > 0 {
		selected = options[0]
	}
	if selected == "" {
		return "", &PolicyRejection{
			Code:    PolicyRejectionModelUnavailable,
			Problem: fmt.Sprintf("no model policy for role %q", request.Role),
		}
	}
	if !contains(options, selected) && !hasOverride(repository.AllowedOverrides, config.OverrideModel) {
		return "", &PolicyRejection{
			Code:    PolicyRejectionModelOverride,
			Problem: fmt.Sprintf("model %q is not a declared option for role %q", selected, request.Role),
		}
	}
	return selected, nil
}

// resolveReasoningEffortPolicy selects the role's reasoning effort. A role that
// declares no options accepts no selection unless the repository permits
// reasoning-effort overrides.
func resolveReasoningEffortPolicy(repository config.RepositoryConfig, request AgentRequest) (string, error) {
	options := repository.ReasoningEffortOptions[request.Role]
	selected := request.ReasoningEffort
	if selected == "" && len(options) > 0 {
		selected = options[0]
	}
	if selected != "" && !contains(options, selected) && !hasOverride(repository.AllowedOverrides, config.OverrideReasoningEffort) {
		return "", &PolicyRejection{
			Code:    PolicyRejectionReasoningEffortOverride,
			Problem: fmt.Sprintf("reasoning effort %q is not a declared option for role %q", selected, request.Role),
		}
	}
	return selected, nil
}

// hasOverride reports whether a repository policy explicitly permits a setting.
func hasOverride(values []config.OverrideName, wanted config.OverrideName) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// contains reports whether a string appears in a policy list.
func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// sameStrings compares policy lists in their coordinator-preserved order.
func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// validateAgentRunState enforces the visible role's legal predecessor stages
// before it starts or resumes a harness session.
func validateAgentRunState(run store.Run) error {
	if run.Status != store.StatusActive {
		return fmt.Errorf("cannot start visible agent from run status %q", run.Status)
	}
	registry := workflow.DefaultRegistry()
	if _, exists := registry.RoleForRunStage(run.Stage); !exists {
		if _, invocationStage := registry.RoleForInvocationStage(run.Stage); invocationStage {
			return nil
		}
		return fmt.Errorf("cannot start visible agent from run stage %q", run.Stage)
	}
	return nil
}

// selectAgentRole resolves an automatic request against the frozen run stage.
func selectAgentRole(run store.Run, request AgentRequest) (AgentRequest, error) {
	registry := workflow.DefaultRegistry()
	if request.Role == "" && request.Stage == "" {
		definition, exists := registry.RoleForRunStage(run.Stage)
		if !exists {
			return AgentRequest{}, fmt.Errorf("no factory-owned agent role is declared for run stage %q", run.Stage)
		}
		request.Role = definition.Name
		request.Stage = definition.Stage
	}
	if request.Role == "" {
		definition, exists := registry.RoleForInvocationStage(request.Stage)
		if !exists {
			return AgentRequest{}, fmt.Errorf("no factory-owned role owns invocation stage %q", request.Stage)
		}
		request.Role = definition.Name
	}
	if request.Stage == "" {
		definition, exists := registry.Role(request.Role)
		if !exists {
			return AgentRequest{}, &PolicyRejection{Code: PolicyRejectionRoleUnavailable, Problem: fmt.Sprintf("agent role %q is not declared by the factory-owned workflow registry", request.Role)}
		}
		request.Stage = definition.Stage
	}
	definition, exists := registry.Role(request.Role)
	if !exists {
		return AgentRequest{}, &PolicyRejection{Code: PolicyRejectionRoleUnavailable, Problem: fmt.Sprintf("agent role %q is not declared by the factory-owned workflow registry", request.Role)}
	}
	if definition.Stage != request.Stage {
		return AgentRequest{}, &PolicyRejection{Code: PolicyRejectionRoleStageMismatch, Problem: fmt.Sprintf("agent role %q belongs to stage %q, not %q", request.Role, definition.Stage, request.Stage)}
	}
	if !registry.CanStartFrom(request.Role, run.Stage) {
		return AgentRequest{}, fmt.Errorf("agent role %q cannot start from active run stage %q", request.Role, run.Stage)
	}
	return request, nil
}

// ensureTransitionBaseline prevents a checkpoint created during the check
// stage from re-entering test or implementation without a matching baseline
// projection for that exact checkpoint.
func (s *Service) ensureTransitionBaseline(ctx context.Context, runStore RunStore, run store.Run, request TransitionRequest) error {
	if request.Stage != store.StageTest && request.Stage != store.StageImplementation {
		return nil
	}
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return fmt.Errorf("validate baseline before %q transition: %w", request.Stage, err)
	}
	if run.Stage == store.StageClaim && request.Stage == store.StageImplementation && !independentTestStageDeclared(packet) {
		if err := s.ensureBaselineReady(ctx, runStore, run, packet); err != nil {
			return fmt.Errorf("cannot transition from claim to implementation without complete baseline results: %w", err)
		}
		return nil
	}
	if run.Stage != store.StageCheck {
		return nil
	}
	if err := s.ensureBaselineReadyAtCheckpoint(ctx, runStore, run, packet, run.CheckpointSHA); err != nil {
		return fmt.Errorf("cannot transition from check to %q at checkpoint %q without complete baseline results: %w", request.Stage, run.CheckpointSHA, err)
	}
	return nil
}

// persistAgentRunState uses the coordinator's GitHub label/comment transition
// when the claimed run has a status comment, retaining a direct-store fallback
// for legacy runs that predate that supervision record.
func (s *Service) persistAgentRunState(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, previous, next store.Run) error {
	checkpointChanged := previous.CheckpointSHA != "" && next.CheckpointSHA != previous.CheckpointSHA
	if checkpointChanged {
		if next.Revision <= previous.Revision {
			next.Revision = previous.Revision + 1
		}
		next.SpecificationReview = nil
		next.StandardsReview = nil
		if next.TestCheckpointSHA == "" || next.TestCheckpointSHA != next.CheckpointSHA {
			next.ReviewRepairPendingAttempt = 0
			next.ReviewRepairPacket = nil
		}
	}
	if next.SpecificationReview != nil && next.SpecificationReview.CheckpointSHA != next.CheckpointSHA {
		next.SpecificationReview = nil
	}
	if next.StandardsReview != nil && next.StandardsReview.CheckpointSHA != next.CheckpointSHA {
		next.StandardsReview = nil
	}
	invalidateResults := checkpointChanged
	if invalidateResults {
		_, atomicSupported := runStore.(atomicPacketTransitionStore)
		_, directSupported := runStore.(runResultInvalidator)
		invalidateResults = atomicSupported || directSupported
	}
	if next.StatusCommentID == "" || s.deps.GitHub == nil {
		if invalidateResults {
			next.UpdatedAt = s.deps.Now().UTC()
			if atomicStore, ok := runStore.(atomicPacketTransitionStore); ok {
				if err := atomicStore.SaveRunAndInvalidateResults(ctx, previous.Revision, next); err != nil {
					return fmt.Errorf("persist checkpoint state and invalidate results: %w", err)
				}
				return nil
			}
			if err := saveRunWithRetry(ctx, runStore, next); err != nil {
				return err
			}
			return invalidateRunResults(ctx, runStore, next.ID)
		}
		return saveRunWithRetry(ctx, runStore, next)
	}
	repository := github.Repository{Owner: registration.GitHub.Owner, Name: registration.GitHub.Repository}
	issue, err := s.deps.GitHub.Issue(ctx, repository, next.IssueNumber)
	if err != nil {
		return fmt.Errorf("read issue for agent state transition: %w", err)
	}
	if _, err := s.applyStateTransition(ctx, runStore, stateTransition{
		Repository:        repository,
		Issue:             issue,
		Previous:          previous,
		Next:              next,
		InvalidateResults: invalidateResults,
	}); err != nil {
		return err
	}
	return nil
}

// writeInvocationPacket atomically writes the worker-readable packet and leaves
// credentials outside its contents. The packet carries the frozen claim
// unchanged, so a repository-declared host path such as a cache location stays
// in it; the coordinator resolves those paths and the role prompt renders a
// projection that omits them.
func writeInvocationPacket(directory string, packet InvocationPacket) error {
	data, err := json.MarshalIndent(packet, "", "  ")
	if err != nil {
		return fmt.Errorf("encode invocation packet: %w", err)
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(directory, ".packet-*.tmp")
	if err != nil {
		return fmt.Errorf("create invocation packet: %w", err)
	}
	path := temporary.Name()
	defer func() { _ = os.Remove(path) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect invocation packet: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write invocation packet: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync invocation packet: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close invocation packet: %w", err)
	}
	if err := os.Rename(path, filepath.Join(directory, invocationPacketFileName)); err != nil {
		return fmt.Errorf("publish invocation packet: %w", err)
	}
	return nil
}
