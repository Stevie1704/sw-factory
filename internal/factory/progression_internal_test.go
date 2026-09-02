package factory

import (
	"encoding/json"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// TestProgressionStepPlansReportAcceptanceFromObservedReadiness verifies the
// planner consumes readiness observed by the coordinator without inspecting
// the invocation result directory itself.
func TestProgressionStepPlansReportAcceptanceFromObservedReadiness(t *testing.T) {
	run := store.Run{ID: "run-1", Stage: store.StageImplementation, Status: store.StatusActive}
	invocation := &store.Invocation{ID: "inv-1"}

	action, stop := progressionStep(progressionState{
		Run:                &run,
		ActiveInvocations:  []*store.Invocation{invocation},
		ReadyInvocationIDs: map[string]bool{"inv-1": true},
	}, workflow.DefaultRegistry())
	if stop != nil {
		t.Fatalf("progressionStep() stopped with %#v, want report acceptance", stop)
	}
	if action.kind != progressionActionAcceptAgentReport {
		t.Fatalf("planned action kind = %q, want %q", action.kind, progressionActionAcceptAgentReport)
	}
	if action.name != "accept agent report" {
		t.Fatalf("planned action name = %q, want unchanged operator-facing name", action.name)
	}
	if action.runID != run.ID || action.invocationID != invocation.ID {
		t.Fatalf("planned action operands = %#v, want run %q and invocation %q", action, run.ID, invocation.ID)
	}
}

// TestProgressionStepPlansEveryCoordinatorCommand verifies each legal planner
// branch returns a validated typed command while preserving its transition
// name and required operands.
func TestProgressionStepPlansEveryCoordinatorCommand(t *testing.T) {
	registry := workflow.DefaultRegistry()
	runWithReviews := progressionRunWithConcurrentReviews(t)
	runWithMissingReview := runWithReviews
	runWithMissingReview.ID = "run-missing-review"
	runWithMissingReview.Stage = store.StageReview
	runWithMissingReview.CheckpointSHA = "checkpoint"
	runWithMissingReview.SpecificationReview = &store.SpecificationReview{CheckpointSHA: "checkpoint"}
	tests := []struct {
		name           string
		state          progressionState
		wantKind       progressionActionKind
		wantName       string
		wantInvocation string
		wantRole       string
		wantStage      store.Stage
	}{
		{
			name:     "baseline",
			state:    progressionState{Run: &store.Run{ID: "run-baseline", Stage: store.StageClaim, Status: store.StatusActive}},
			wantKind: progressionActionRunBaseline,
			wantName: "run baseline",
		},
		{
			name:     "start agent",
			state:    progressionState{Run: &store.Run{ID: "run-agent", Stage: store.StageImplementation, Status: store.StatusActive}},
			wantKind: progressionActionStartAgent,
			wantName: "start agent",
		},
		{
			name:      "start missing review",
			state:     progressionState{Run: &runWithMissingReview},
			wantKind:  progressionActionStartAgent,
			wantName:  "start missing standards_review review",
			wantRole:  workflow.RoleStandardsReview,
			wantStage: workflow.StageStandardsReview,
		},
		{
			name: "accept report",
			state: progressionState{
				Run:                &store.Run{ID: "run-report", Stage: store.StageImplementation, Status: store.StatusActive},
				ActiveInvocations:  []*store.Invocation{{ID: "inv-report"}},
				ReadyInvocationIDs: map[string]bool{"inv-report": true},
			},
			wantKind:       progressionActionAcceptAgentReport,
			wantName:       "accept agent report",
			wantInvocation: "inv-report",
		},
		{
			name: "create draft pull request",
			state: progressionState{
				Run: &store.Run{ID: "run-draft", Stage: store.StageCheck, Status: store.StatusActive},
			},
			wantKind: progressionActionCreateDraftPullRequest,
			wantName: "create draft pull request",
		},
		{
			name:     "start review round",
			state:    progressionState{Run: &runWithReviews},
			wantKind: progressionActionStartReviewRound,
			wantName: "start concurrent reviews",
		},
		{
			name:     "retry review readiness",
			state:    progressionState{Run: &store.Run{ID: "run-readiness", Stage: store.StageReview, Status: store.StatusActive}},
			wantKind: progressionActionRetryReviewReadiness,
			wantName: "finalize pull-request readiness",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			action, stop := progressionStep(test.state, registry)
			if stop != nil {
				t.Fatalf("progressionStep() stopped with %#v, want %s", stop, test.wantKind)
			}
			if action.kind != test.wantKind || action.name != test.wantName {
				t.Fatalf("planned action = %#v, want kind=%q name=%q", action, test.wantKind, test.wantName)
			}
			if action.runID == "" {
				t.Fatal("planned action has no run id")
			}
			if action.invocationID != test.wantInvocation {
				t.Fatalf("planned invocation id = %q, want %q", action.invocationID, test.wantInvocation)
			}
			if action.role != test.wantRole || action.stage != test.wantStage {
				t.Fatalf("planned role/stage = %q/%q, want %q/%q", action.role, action.stage, test.wantRole, test.wantStage)
			}
			if err := action.validate(); err != nil {
				t.Fatalf("planned action validation error = %v", err)
			}
		})
	}
}

// TestProgressionActionValidationRejectsInvalidKindAndOperands verifies the
// closed command set rejects malformed commands before coordinator dispatch.
func TestProgressionActionValidationRejectsInvalidKindAndOperands(t *testing.T) {
	tests := []progressionAction{
		{kind: progressionActionKind("invented"), name: "invented", runID: "run-1"},
		{kind: progressionActionAcceptAgentReport, name: "accept agent report", runID: "run-1", role: workflow.RoleTest, invocationID: "inv-1"},
		{kind: progressionActionStartAgent, name: "start agent", runID: "run-1", role: workflow.RoleTest},
	}

	for index, action := range tests {
		if err := action.validate(); err == nil {
			t.Fatalf("action %d validation error = nil, want typed invalid-action error", index)
		} else if _, ok := err.(*progressionActionError); !ok {
			t.Fatalf("action %d validation error type = %T, want *progressionActionError", index, err)
		}
	}
}

// progressionRunWithConcurrentReviews builds a frozen packet that enables
// both factory-owned review roles for planner branch coverage.
func progressionRunWithConcurrentReviews(t *testing.T) store.Run {
	t.Helper()
	packetData, err := json.Marshal(SpecificationPacket{
		Version: specificationPacketVersion,
		RepositoryConfig: config.RepositoryConfig{
			RoleHarnessDefaults: map[string]config.Harness{
				workflow.RoleSpecificationReview: config.HarnessCodex,
				workflow.RoleStandardsReview:     config.HarnessCodex,
			},
			ModelOptions: map[string][]string{
				workflow.RoleSpecificationReview: {"gpt-5"},
				workflow.RoleStandardsReview:     {"gpt-5"},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal concurrent review packet: %v", err)
	}
	return store.Run{
		ID:                  "run-reviews",
		Stage:               store.StageDraftPR,
		Status:              store.StatusActive,
		SpecificationPacket: string(packetData),
	}
}
