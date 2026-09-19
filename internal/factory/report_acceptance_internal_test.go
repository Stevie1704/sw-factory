package factory

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// acceptanceWorktreeInspector returns one already observed worktree state for
// gather-phase tests.
type acceptanceWorktreeInspector struct {
	state gitadapter.WorktreeState
}

// Inspect returns the configured worktree observation without touching Git.
func (inspector acceptanceWorktreeInspector) Inspect(context.Context, string) (gitadapter.WorktreeState, error) {
	return inspector.state, nil
}

// admissionSnapshot builds a gathered snapshot directly, without a store, an
// adapter, or a context, so an admission rejection is reproducible from the
// run, the invocation, the report, and the observed worktree alone.
func admissionSnapshot(t *testing.T, role string, stage store.Stage, mutate func(*AcceptanceSnapshot)) AcceptanceSnapshot {
	t.Helper()
	roleDefinition, declared := workflow.DefaultRegistry().Role(role)
	if !declared {
		t.Fatalf("role %q is not declared", role)
	}
	snapshot := AcceptanceSnapshot{
		Run: store.Run{
			ID:            "run-1",
			Stage:         stage,
			Status:        store.StatusActive,
			Worktree:      t.TempDir(),
			CheckpointSHA: strings.Repeat("a", 40),
		},
		Invocation: store.Invocation{
			ID:            "inv-1",
			RunID:         "run-1",
			Role:          role,
			Stage:         stage,
			Harness:       string(config.HarnessClaude),
			PromptVersion: role + "-v1",
		},
		Role: roleDefinition,
		Report: report.Report{
			SchemaVersion: report.SchemaVersion,
			InvocationID:  "inv-1",
			RunID:         "run-1",
			Harness:       string(config.HarnessClaude),
			Role:          role,
			Stage:         string(stage),
			Outcome:       report.OutcomeCompleted,
			Summary:       "work is complete",
			ReportedAt:    time.Unix(1700000000, 0).UTC(),
		},
		Worktree: gitadapter.WorktreeState{HeadSHA: strings.Repeat("a", 40)},
	}
	if mutate != nil {
		mutate(&snapshot)
	}
	return snapshot
}

// TestAdmitReportRejectsAWorktreeThatLeftTheCheckpoint preserves the immutable
// checkpoint rejection in the pure admission phase.
func TestAdmitReportRejectsAWorktreeThatLeftTheCheckpoint(t *testing.T) {
	snapshot := admissionSnapshot(t, workflow.RoleImplementation, store.StageImplementation, func(snapshot *AcceptanceSnapshot) {
		snapshot.Worktree.HeadSHA = strings.Repeat("b", 40)
	})
	_, err := AdmitReport(snapshot)
	if err == nil || !strings.Contains(err.Error(), "does not match checkpoint") {
		t.Fatalf("want checkpoint rejection, got %v", err)
	}
}

// TestAdmitReportRejectsAReviewerThatChangedTheWorktree preserves reviewer
// read-only ownership in the pure admission phase.
func TestAdmitReportRejectsAReviewerThatChangedTheWorktree(t *testing.T) {
	snapshot := admissionSnapshot(t, workflow.RoleSpecificationReview, store.StageReview, func(snapshot *AcceptanceSnapshot) {
		snapshot.Worktree.ChangedPaths = []string{"internal/factory/agent.go"}
	})
	_, err := AdmitReport(snapshot)
	if err == nil || !strings.Contains(err.Error(), "changed the immutable checkpoint worktree") {
		t.Fatalf("want immutable worktree rejection, got %v", err)
	}
}

// TestAdmitReportRejectsATestReportWithoutADeclaredTestStage verifies frozen
// route ownership without consulting repository adapters.
func TestAdmitReportRejectsATestReportWithoutADeclaredTestStage(t *testing.T) {
	snapshot := admissionSnapshot(t, workflow.RoleTest, store.StageTest, nil)
	_, err := AdmitReport(snapshot)
	if err == nil || !strings.Contains(err.Error(), "test-stage reports are unavailable in advisory mode") {
		t.Fatalf("want advisory-mode rejection, got %v", err)
	}
}

// TestAdmitReportRejectsAReviewOfAnotherCheckpointWithATypedError verifies the
// exact-checkpoint rejection retains its typed identity.
func TestAdmitReportRejectsAReviewOfAnotherCheckpointWithATypedError(t *testing.T) {
	snapshot := admissionSnapshot(t, workflow.RoleSpecificationReview, store.StageReview, func(snapshot *AcceptanceSnapshot) {
		snapshot.Report.ReviewHandoff = &report.ReviewHandoff{ReviewedSHA: strings.Repeat("c", 40)}
	})
	_, err := AdmitReport(snapshot)
	var mismatch *ReviewCheckpointMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("want *ReviewCheckpointMismatchError, got %v", err)
	}
	if mismatch.Expected != strings.Repeat("a", 40) || mismatch.Observed != strings.Repeat("c", 40) {
		t.Fatalf("unexpected mismatch detail %+v", mismatch)
	}
}

// TestAdmitReportRejectsAnInvalidReport verifies schema validation remains an
// admission concern.
func TestAdmitReportRejectsAnInvalidReport(t *testing.T) {
	snapshot := admissionSnapshot(t, workflow.RoleImplementation, store.StageImplementation, func(snapshot *AcceptanceSnapshot) {
		snapshot.Report.ReportedAt = time.Time{}
	})
	_, err := AdmitReport(snapshot)
	if err == nil || !strings.Contains(err.Error(), "reported_at is required") {
		t.Fatalf("want schema rejection, got %v", err)
	}
}

// TestAdmitReportParksAnUnverifiableTestReportInsteadOfRejectingIt preserves
// the human-disposition exit for unverifiable test evidence.
func TestAdmitReportParksAnUnverifiableTestReportInsteadOfRejectingIt(t *testing.T) {
	snapshot := admissionSnapshot(t, workflow.RoleTest, store.StageTest, func(snapshot *AcceptanceSnapshot) {
		snapshot.Packet.RepositoryConfig.TestPolicy.Mode = config.TestModeRequired
		snapshot.Packet.Route = workflow.RouteAcceptance
		snapshot.Report.ReportedAt = time.Time{}
	})
	outcome, err := AdmitReport(snapshot)
	if err != nil {
		t.Fatalf("want an unverifiable admission, got %v", err)
	}
	if outcome != AcceptanceOutcomeUnverifiable {
		t.Fatalf("want the report parked as unverifiable, got %q", outcome)
	}
}

// TestAdmitAcceptanceIdentityRejectsAStageMismatch verifies invocation-stage
// ownership before the report snapshot is gathered.
func TestAdmitAcceptanceIdentityRejectsAStageMismatch(t *testing.T) {
	snapshot := admissionSnapshot(t, workflow.RoleImplementation, store.StageImplementation, func(snapshot *AcceptanceSnapshot) {
		snapshot.Invocation.Stage = store.StageTest
	})
	err := admitAcceptanceIdentity(snapshot.Run, snapshot.Invocation, snapshot.Role, AgentReportRequest{})
	if err == nil || !strings.Contains(err.Error(), "does not match active run stage") {
		t.Fatalf("want stage rejection, got %v", err)
	}
}

// TestAdmitAcceptanceIdentityRejectsAPathPolicyThatDiffersFromTheInvocation
// verifies request paths cannot widen the frozen invocation policy.
func TestAdmitAcceptanceIdentityRejectsAPathPolicyThatDiffersFromTheInvocation(t *testing.T) {
	snapshot := admissionSnapshot(t, workflow.RoleImplementation, store.StageImplementation, func(snapshot *AcceptanceSnapshot) {
		snapshot.Invocation.PermittedPaths = []string{"internal/"}
	})
	err := admitAcceptanceIdentity(snapshot.Run, snapshot.Invocation, snapshot.Role, AgentReportRequest{PermittedPaths: []string{"cmd/"}})
	if err == nil || !strings.Contains(err.Error(), "permitted paths do not match the invocation policy") {
		t.Fatalf("want path policy rejection, got %v", err)
	}
}

// completedImplementationSnapshot is an admissible implementation report. The
// rejection cases below start from it, so each one differs from an accepted
// report in exactly the field the rejection is about.
func completedImplementationSnapshot(t *testing.T, mutate func(*AcceptanceSnapshot)) AcceptanceSnapshot {
	t.Helper()
	return admissionSnapshot(t, workflow.RoleImplementation, store.StageImplementation, func(snapshot *AcceptanceSnapshot) {
		snapshot.Report.Handoff = &report.Handoff{
			ChangeSummary:          "the coordinator now sequences report acceptance",
			AcceptanceMapping:      []report.AcceptanceMapping{{Criterion: "accept a report", Evidence: "go test ./internal/factory/"}},
			ProductionFilesChanged: []string{"internal/factory/agent.go"},
			FocusedCommands:        []string{"go test ./internal/factory/"},
		}
		snapshot.Worktree.ChangedPaths = []string{"internal/factory/agent.go"}
		snapshot.ObservedChanges = []string{"internal/factory/agent.go"}
		if mutate != nil {
			mutate(snapshot)
		}
	})
}

// TestAdmitReportSelectsTheHandoffOutcomeForACompletedImplementationReport
// verifies ordinary implementation completion selects the declared handoff.
func TestAdmitReportSelectsTheHandoffOutcomeForACompletedImplementationReport(t *testing.T) {
	outcome, err := AdmitReport(completedImplementationSnapshot(t, nil))
	if err != nil {
		t.Fatalf("want an admitted report, got %v", err)
	}
	if outcome != AcceptanceOutcomeHandoff {
		t.Fatalf("want the handoff outcome, got %q", outcome)
	}
}

// TestAdmitReportRejectsAReportThatChangedAProtectedTestPath verifies gathered
// path identities retain independent-test ownership.
func TestAdmitReportRejectsAReportThatChangedAProtectedTestPath(t *testing.T) {
	snapshot := completedImplementationSnapshot(t, func(snapshot *AcceptanceSnapshot) {
		snapshot.Run.ProtectedTestPaths = []store.ProtectedTestPath{{Path: "internal/factory/agent_test.go", SHA256: strings.Repeat("d", 64)}}
	})
	_, err := AdmitReport(snapshot)
	if err == nil || !strings.Contains(err.Error(), "protected test path") {
		t.Fatalf("want a protected test path rejection, got %v", err)
	}
}

// TestAdmitReportRejectsAClarificationWithoutAQuestion verifies clarification
// retains its bounded structured-report contract.
func TestAdmitReportRejectsAClarificationWithoutAQuestion(t *testing.T) {
	snapshot := completedImplementationSnapshot(t, func(snapshot *AcceptanceSnapshot) {
		snapshot.Report.Outcome = report.OutcomeNeedsClarification
		snapshot.Report.Handoff = nil
	})
	_, err := AdmitReport(snapshot)
	if err == nil || !strings.Contains(err.Error(), "at least one question") {
		t.Fatalf("want a clarification rejection, got %v", err)
	}
}

// TestAdmitAcceptanceIdentityRejectsAnUnsafePathPolicy verifies request path
// validation happens before acceptance reads the report.
func TestAdmitAcceptanceIdentityRejectsAnUnsafePathPolicy(t *testing.T) {
	snapshot := completedImplementationSnapshot(t, nil)
	err := admitAcceptanceIdentity(snapshot.Run, snapshot.Invocation, snapshot.Role, AgentReportRequest{PermittedPaths: []string{"../escape"}})
	if err == nil {
		t.Fatal("want an unsafe permitted path rejection")
	}
}

// TestAdmitAcceptanceIdentityRejectsATerminalRun verifies only an active run
// can admit a first report acceptance.
func TestAdmitAcceptanceIdentityRejectsATerminalRun(t *testing.T) {
	snapshot := completedImplementationSnapshot(t, func(snapshot *AcceptanceSnapshot) {
		snapshot.Run.Status = store.StatusFailed
	})
	err := admitAcceptanceIdentity(snapshot.Run, snapshot.Invocation, snapshot.Role, AgentReportRequest{})
	if err == nil {
		t.Fatal("want a run-state rejection")
	}
}

// TestGatherDefersSpecificationPacketErrorsToAdmission verifies gather captures
// a corrupt packet without changing the historical rejection order.
func TestGatherDefersSpecificationPacketErrorsToAdmission(t *testing.T) {
	snapshot := completedImplementationSnapshot(t, func(snapshot *AcceptanceSnapshot) {
		snapshot.Run.SpecificationPacket = "{not-json"
		snapshot.Run.Worktree = t.TempDir()
		snapshot.Invocation.ResultDirectory = t.TempDir()
	})
	if _, err := report.WriteAtomicForInvocation(snapshot.Invocation.ResultDirectory, snapshot.Invocation.ID, snapshot.Report); err != nil {
		t.Fatalf("write report: %v", err)
	}
	module := reportAcceptance{worktree: acceptanceWorktreeInspector{state: snapshot.Worktree}}
	gathered, err := module.gather(context.Background(), config.RepositoryRegistration{}, snapshot.Run, snapshot.Invocation, snapshot.Role)
	if err != nil {
		t.Fatalf("gather() error = %v", err)
	}
	if gathered.PacketError == nil || !strings.Contains(gathered.PacketError.Error(), "decode specification packet") {
		t.Fatalf("packet error = %v, want deferred decode error", gathered.PacketError)
	}
	_, err = AdmitReport(gathered)
	if err == nil || !strings.Contains(err.Error(), "decode specification packet") {
		t.Fatalf("AdmitReport() error = %v, want deferred decode error", err)
	}
}

// TestAdmitReportPreservesCheckpointErrorsBeforePacketErrors covers the
// multi-failure precedence required to keep rejection behavior equivalent.
func TestAdmitReportPreservesCheckpointErrorsBeforePacketErrors(t *testing.T) {
	tests := []struct {
		name   string
		value  AcceptanceSnapshot
		wanted string
	}{
		{
			name: "head mismatch",
			value: admissionSnapshot(t, workflow.RoleImplementation, store.StageImplementation, func(snapshot *AcceptanceSnapshot) {
				snapshot.Worktree.HeadSHA = strings.Repeat("b", 40)
			}),
			wanted: "does not match checkpoint",
		},
		{
			name: "reviewer changed worktree",
			value: admissionSnapshot(t, workflow.RoleSpecificationReview, store.StageReview, func(snapshot *AcceptanceSnapshot) {
				snapshot.Worktree.ChangedPaths = []string{"internal/factory/agent.go"}
			}),
			wanted: "changed the immutable checkpoint worktree",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.value.PacketError = errors.New("decode specification packet for agent report: corrupt")
			_, err := AdmitReport(test.value)
			if err == nil || !strings.Contains(err.Error(), test.wanted) {
				t.Fatalf("AdmitReport() error = %v, want %q before packet error", err, test.wanted)
			}
		})
	}
}

// TestAdmitReportUsesGatheredProtectedPathIdentities verifies admission does
// not revisit a worktree after gather has captured its protected paths.
func TestAdmitReportUsesGatheredProtectedPathIdentities(t *testing.T) {
	digest := strings.Repeat("d", 64)
	snapshot := completedImplementationSnapshot(t, func(snapshot *AcceptanceSnapshot) {
		snapshot.Run.Worktree = t.TempDir() + "/missing"
		snapshot.Run.ProtectedTestPaths = []store.ProtectedTestPath{{Path: "internal/factory/agent_test.go", SHA256: digest}}
		snapshot.ObservedProtectedTestPaths = []store.ProtectedTestPath{{Path: "internal/factory/agent_test.go", SHA256: digest}}
	})
	outcome, err := AdmitReport(snapshot)
	if err != nil {
		t.Fatalf("AdmitReport() read the worktree after gather: %v", err)
	}
	if outcome != AcceptanceOutcomeHandoff {
		t.Fatalf("outcome = %q, want %q", outcome, AcceptanceOutcomeHandoff)
	}
}

// TestGatherCapturesTheObjectionGateDecision verifies the policy read and test
// path identities become immutable inputs to projection.
func TestGatherCapturesTheObjectionGateDecision(t *testing.T) {
	snapshot := completedImplementationSnapshot(t, func(snapshot *AcceptanceSnapshot) {
		snapshot.Run.Worktree = t.TempDir()
		snapshot.Invocation.ResultDirectory = t.TempDir()
		snapshot.Report.Handoff.TestObjections = []report.TestObjection{{Test: "TestContract", Claim: "the contract is stale", Evidence: "focused test evidence"}}
	})
	packet := SpecificationPacket{
		Version: specificationPacketVersion,
		Route:   workflow.RouteAcceptance,
		RepositoryConfig: config.RepositoryConfig{
			TestPolicy:  config.TestPolicy{Mode: config.TestModeRequired},
			RetryLimits: config.RetryLimits{TestRevision: 1},
		},
	}
	packetData, err := json.Marshal(packet)
	if err != nil {
		t.Fatalf("marshal packet: %v", err)
	}
	snapshot.Run.SpecificationPacket = string(packetData)
	if _, err := report.WriteAtomicForInvocation(snapshot.Invocation.ResultDirectory, snapshot.Invocation.ID, snapshot.Report); err != nil {
		t.Fatalf("write report: %v", err)
	}
	registration := config.RepositoryRegistration{Path: "/registered/repository"}
	gateCalled := false
	module := reportAcceptance{
		worktree: acceptanceWorktreeInspector{state: snapshot.Worktree},
		objectionGate: func(actual SpecificationPacket) (bool, string) {
			gateCalled = true
			if actual.RepositoryConfig.TestPolicy.Mode != config.TestModeRequired {
				t.Fatalf("test policy mode = %q, want required", actual.RepositoryConfig.TestPolicy.Mode)
			}
			return true, ""
		},
	}
	gathered, err := module.gather(context.Background(), registration, snapshot.Run, snapshot.Invocation, snapshot.Role)
	if err != nil {
		t.Fatalf("gather() error = %v", err)
	}
	if !gateCalled || !gathered.AutomatedObjection {
		t.Fatalf("gathered objection decision = %t, gate called = %t", gathered.AutomatedObjection, gateCalled)
	}
	if len(gathered.ObjectionBasePaths) != len(snapshot.Worktree.ChangedPaths) {
		t.Fatalf("gathered base paths = %v, want one identity per changed path", gathered.ObjectionBasePaths)
	}
}

// TestProjectAcceptanceKeepsDurabilityStrategiesEquivalent verifies one pure
// projection owns review results, objection context, and revision policy for
// both journaled and direct commitment.
func TestProjectAcceptanceKeepsDurabilityStrategiesEquivalent(t *testing.T) {
	now := time.Unix(1700000100, 0).UTC()
	packetData, err := json.Marshal(SpecificationPacket{Version: specificationPacketVersion})
	if err != nil {
		t.Fatalf("marshal packet: %v", err)
	}
	handoff := completedImplementationSnapshot(t, func(snapshot *AcceptanceSnapshot) {
		snapshot.Run.SpecificationPacket = string(packetData)
	})
	testStage := admissionSnapshot(t, workflow.RoleTest, store.StageTest, nil)
	review := admissionSnapshot(t, workflow.RoleSpecificationReview, store.StageReview, func(snapshot *AcceptanceSnapshot) {
		snapshot.Report.ReviewHandoff = &report.ReviewHandoff{ReviewedSHA: snapshot.Run.CheckpointSHA}
	})
	objection := completedImplementationSnapshot(t, func(snapshot *AcceptanceSnapshot) {
		snapshot.Run.Worktree = t.TempDir() + "/missing"
		snapshot.Run.TestInvocationID = "inv-test"
		snapshot.Run.TestHandoff = &store.TestHandoff{}
		snapshot.Run.ProtectedTestPaths = []store.ProtectedTestPath{{Path: "contract_test.go", SHA256: strings.Repeat("a", 64)}}
		snapshot.Packet.Route = workflow.RouteAcceptance
		snapshot.Packet.RepositoryConfig.RetryLimits.TestRevision = 1
		snapshot.Report.Handoff.TestObjections = []report.TestObjection{{Test: "TestContract", Claim: "the contract is stale", Evidence: "focused test evidence"}}
		snapshot.ObjectionBasePaths = []store.ProtectedTestPath{{Path: "internal/factory/agent.go", SHA256: strings.Repeat("b", 64)}}
	})
	tests := []struct {
		name              string
		snapshot          AcceptanceSnapshot
		outcome           AcceptanceOutcome
		directRevision    int64
		journaledRevision int64
	}{
		{name: "handoff", snapshot: handoff, outcome: AcceptanceOutcomeHandoff, directRevision: 7, journaledRevision: 8},
		{name: "test stage", snapshot: testStage, outcome: AcceptanceOutcomeTestStage, directRevision: 7, journaledRevision: 7},
		{name: "review", snapshot: review, outcome: AcceptanceOutcomeReview, directRevision: 7, journaledRevision: 7},
		{name: "test objection", snapshot: objection, outcome: AcceptanceOutcomeTestObjection, directRevision: 8, journaledRevision: 8},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.snapshot.Run.Revision = 7
			test.snapshot.Run.ActiveInvocationIDs = []string{test.snapshot.Invocation.ID, "inv-concurrent"}
			projection := projectAcceptance(test.snapshot, test.outcome, now)
			if projection.Error != nil {
				t.Fatalf("projectAcceptance() error = %v", projection.Error)
			}
			if projection.Next.Revision != test.directRevision || projection.JournaledNext.Revision != test.journaledRevision {
				t.Fatalf("revisions direct/journaled = %d/%d, want %d/%d", projection.Next.Revision, projection.JournaledNext.Revision, test.directRevision, test.journaledRevision)
			}
			direct, journaled := projection.Next, projection.JournaledNext
			direct.Revision, journaled.Revision = 0, 0
			direct.UpdatedAt, journaled.UpdatedAt = time.Time{}, time.Time{}
			if !reflect.DeepEqual(direct, journaled) {
				t.Fatalf("logical projections differ:\ndirect: %#v\njournaled: %#v", direct, journaled)
			}
			if test.outcome == AcceptanceOutcomeReview && projection.Next.SpecificationReview == nil {
				t.Fatal("direct projection deferred the review result to dispatch")
			}
			if test.outcome == AcceptanceOutcomeTestObjection && !reflect.DeepEqual(projection.Next.TestRevisionBaseChangedPaths, test.snapshot.ObjectionBasePaths) {
				t.Fatalf("objection base paths = %v, want gathered %v", projection.Next.TestRevisionBaseChangedPaths, test.snapshot.ObjectionBasePaths)
			}
		})
	}
}
