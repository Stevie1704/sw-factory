package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	effectkernel "github.com/Stevie1704/sw-factory/internal/effect"
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
	payload, err := effectkernel.ReadResultAcceptance(pending)
	if err != nil {
		return report.Report{}, err
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

// readAcceptedReviewReportFromEffect extracts the immutable review invocation
// and report snapshot from a result-acceptance effect. Legacy payloads without
// the snapshot fall back to the invocation artifact retained on disk.
func readAcceptedReviewReportFromEffect(pending store.PendingEffect) (store.Invocation, report.Report, bool, error) {
	payload, err := effectkernel.ReadResultAcceptance(pending)
	if err != nil {
		return store.Invocation{}, report.Report{}, false, err
	}
	if !roleIsKind(payload.Invocation, workflow.RoleKindReview) {
		return payload.Invocation, report.Report{}, false, nil
	}
	if strings.TrimSpace(payload.AcceptedReport) == "" {
		value, readErr := readAcceptedAgentReport(payload.Invocation)
		if readErr != nil {
			return store.Invocation{}, report.Report{}, true, fmt.Errorf("read accepted review report from invocation artifact: %w", readErr)
		}
		return payload.Invocation, value, true, nil
	}
	var value report.Report
	if err := json.Unmarshal([]byte(payload.AcceptedReport), &value); err != nil {
		return store.Invocation{}, report.Report{}, true, fmt.Errorf("decode accepted review report from effect: %w", err)
	}
	return payload.Invocation, value, true, nil
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
		if err := ensureBaselineReadyForLaunch(ctx, runStore, run, packet); err != nil {
			return fmt.Errorf("cannot transition from claim to implementation without complete baseline results: %w", err)
		}
		return nil
	}
	if run.Stage != store.StageCheck {
		return nil
	}
	if err := ensureBaselineReadyAtCheckpointForLaunch(ctx, runStore, run, packet, run.CheckpointSHA); err != nil {
		return fmt.Errorf("cannot transition from check to %q at checkpoint %q without complete baseline results: %w", request.Stage, run.CheckpointSHA, err)
	}
	return nil
}
