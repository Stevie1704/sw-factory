package factory

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

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

func TestAdmitReportRejectsAWorktreeThatLeftTheCheckpoint(t *testing.T) {
	snapshot := admissionSnapshot(t, workflow.RoleImplementation, store.StageImplementation, func(snapshot *AcceptanceSnapshot) {
		snapshot.Worktree.HeadSHA = strings.Repeat("b", 40)
	})
	_, err := AdmitReport(snapshot)
	if err == nil || !strings.Contains(err.Error(), "does not match checkpoint") {
		t.Fatalf("want checkpoint rejection, got %v", err)
	}
}

func TestAdmitReportRejectsAReviewerThatChangedTheWorktree(t *testing.T) {
	snapshot := admissionSnapshot(t, workflow.RoleSpecificationReview, store.StageReview, func(snapshot *AcceptanceSnapshot) {
		snapshot.Worktree.ChangedPaths = []string{"internal/factory/agent.go"}
	})
	_, err := AdmitReport(snapshot)
	if err == nil || !strings.Contains(err.Error(), "changed the immutable checkpoint worktree") {
		t.Fatalf("want immutable worktree rejection, got %v", err)
	}
}

func TestAdmitReportRejectsATestReportWithoutADeclaredTestStage(t *testing.T) {
	snapshot := admissionSnapshot(t, workflow.RoleTest, store.StageTest, nil)
	_, err := AdmitReport(snapshot)
	if err == nil || !strings.Contains(err.Error(), "test-stage reports are unavailable in advisory mode") {
		t.Fatalf("want advisory-mode rejection, got %v", err)
	}
}

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

func TestAdmitReportRejectsAnInvalidReport(t *testing.T) {
	snapshot := admissionSnapshot(t, workflow.RoleImplementation, store.StageImplementation, func(snapshot *AcceptanceSnapshot) {
		snapshot.Report.ReportedAt = time.Time{}
	})
	_, err := AdmitReport(snapshot)
	if err == nil || !strings.Contains(err.Error(), "reported_at is required") {
		t.Fatalf("want schema rejection, got %v", err)
	}
}

func TestAdmitReportParksAnUnverifiableTestReportInsteadOfRejectingIt(t *testing.T) {
	snapshot := admissionSnapshot(t, workflow.RoleTest, store.StageTest, func(snapshot *AcceptanceSnapshot) {
		snapshot.Packet.RepositoryConfig.TestPolicy.Mode = config.TestModeRequired
		snapshot.Packet.Route = workflow.RouteAcceptance
		snapshot.Report.ReportedAt = time.Time{}
	})
	admission, err := AdmitReport(snapshot)
	if err != nil {
		t.Fatalf("want an unverifiable admission, got %v", err)
	}
	if !admission.Unverifiable {
		t.Fatal("want the report parked as unverifiable")
	}
}

func TestAdmitAcceptanceIdentityRejectsAStageMismatch(t *testing.T) {
	snapshot := admissionSnapshot(t, workflow.RoleImplementation, store.StageImplementation, func(snapshot *AcceptanceSnapshot) {
		snapshot.Invocation.Stage = store.StageTest
	})
	err := admitAcceptanceIdentity(snapshot.Run, snapshot.Invocation, snapshot.Role, AgentReportRequest{})
	if err == nil || !strings.Contains(err.Error(), "does not match active run stage") {
		t.Fatalf("want stage rejection, got %v", err)
	}
}

func TestAdmitAcceptanceIdentityRejectsAPathPolicyThatDiffersFromTheInvocation(t *testing.T) {
	snapshot := admissionSnapshot(t, workflow.RoleImplementation, store.StageImplementation, func(snapshot *AcceptanceSnapshot) {
		snapshot.Invocation.PermittedPaths = []string{"internal/"}
	})
	err := admitAcceptanceIdentity(snapshot.Run, snapshot.Invocation, snapshot.Role, AgentReportRequest{PermittedPaths: []string{"cmd/"}})
	if err == nil || !strings.Contains(err.Error(), "permitted paths do not match the invocation policy") {
		t.Fatalf("want path policy rejection, got %v", err)
	}
}
