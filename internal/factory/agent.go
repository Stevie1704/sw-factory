package factory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/prompt"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// InvocationStore is the operational-store seam required by harness invocation
// launch and structured report acceptance.
type InvocationStore interface {
	RunStore
	SaveInvocation(context.Context, store.Invocation) error
	Invocation(context.Context, string, string) (*store.Invocation, error)
}

// InvocationHistoryStore is the operational-store seam used to distinguish a
// completed claim from a run that has ever attempted a harness invocation.
type InvocationHistoryStore interface {
	HasInvocation(context.Context, string) (bool, error)
}

// AgentRequest selects one coordinator-owned role invocation.
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
	// ReviewUnitID optionally selects one persisted exact-checkpoint review unit.
	// Empty lets the coordinator choose the next unassigned unit for the role.
	ReviewUnitID string
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
	// ReviewRoundID identifies the immutable review round carried by this packet.
	ReviewRoundID string `json:"review_round_id,omitempty"`
	// ReviewUnitID identifies the exact manifest unit carried by this packet.
	ReviewUnitID string `json:"review_unit_id,omitempty"`
	// Continuation records that this invocation continued a harness session that
	// already held the role's first prompt, so a rebuilt prompt keeps carrying
	// only what changed.
	Continuation bool `json:"continuation,omitempty"`
}

const (
	// invocationPacketMinimumSupportedVersion identifies the oldest append-only
	// packet shape retained for restart recovery.
	invocationPacketMinimumSupportedVersion = 1
	// reviewDiffArtifactPacketVersion identifies the first packet shape whose
	// review invocations require the persisted review.diff artifact.
	reviewDiffArtifactPacketVersion = 11
	// invocationPacketVersion identifies the read-only invocation packet shape.
	// Version eleven records the review.diff artifact identity and the exact
	// review round and unit assignment.
	invocationPacketVersion = 11
	// invocationPacketFileName is the stable worker-visible packet filename.
	invocationPacketFileName = "specification.json"
)

// StartAgent prepares the frozen invocation packet, starts the pinned worker,
// creates the durable run projections, and launches the selected factory role
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
	result, err = s.startAgentWithStore(ctx, registration, runStore, run, request)
	if err == nil || !credentialProjectionCaptureLimit(err) || run == nil {
		return result, err
	}
	_, pauseErr := s.lifecycleModule().pauseForCaptureLimit(ctx, registration, runStore, *run, credentialProjectionHarness(err))
	return result, errors.Join(err, pauseErr)
}

// AcceptAgentReport reads only the invocation report file, validates its
// identity and observed worktree state, and then lets the coordinator decide
// the resulting workflow status. Store opening and command locking remain
// coordinator concerns; every phase after them belongs to the module.
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
	return s.acceptanceModule(acceptanceEvaluationRecorderForRunStore(runStore)).Accept(ctx, ReportAcceptanceRequest{
		Registration: registration,
		RunStore:     runStore,
		Run:          run,
		Request:      request,
	})
}

// RunAgent launches the harness invocation and accepts a report when one is already
// present. Long-running callers normally use StartAgent and accept later.
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
		if err := ensureBaselineReadyForLaunch(ctx, s.checkpointFileReader(), runStore, run, packet); err != nil {
			return fmt.Errorf("cannot transition from claim to implementation without complete baseline results: %w", err)
		}
		return nil
	}
	if run.Stage != store.StageCheck {
		return nil
	}
	if err := ensureBaselineReadyAtCheckpointForLaunch(ctx, s.checkpointFileReader(), runStore, run, packet, run.CheckpointSHA); err != nil {
		return fmt.Errorf("cannot transition from check to %q at checkpoint %q without complete baseline results: %w", request.Stage, run.CheckpointSHA, err)
	}
	return nil
}
