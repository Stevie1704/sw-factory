package factory

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// summaryCheckpoint is the reviewed checkpoint of the planner fixtures.
const summaryCheckpoint = "0123456789abcdef0123456789abcdef01234567"

// readinessRunWithPullRequestWriter returns a run whose complete review round
// passed at summaryCheckpoint and whose packet declares the PR writer.
func readinessRunWithPullRequestWriter(t *testing.T) store.Run {
	t.Helper()
	roles := []string{workflow.RoleSpecificationReview, workflow.RoleStandardsReview, workflow.RolePullRequestWriter}
	policy := config.RepositoryConfig{RoleHarnessDefaults: map[string]config.Harness{}, ModelOptions: map[string][]string{}, Timeouts: config.TimeoutConfig{Agent: "1h"}}
	for _, role := range roles {
		policy.RoleHarnessDefaults[role] = config.HarnessCodex
		policy.ModelOptions[role] = []string{"gpt-5"}
	}
	packetData, err := json.Marshal(SpecificationPacket{Version: specificationPacketVersion, RepositoryConfig: policy})
	if err != nil {
		t.Fatalf("marshal packet: %v", err)
	}
	return store.Run{
		ID: "run-summary", Stage: store.StageReview, Status: store.StatusActive,
		CheckpointSHA:       summaryCheckpoint,
		SpecificationReview: &store.SpecificationReview{CheckpointSHA: summaryCheckpoint},
		StandardsReview:     &store.StandardsReview{CheckpointSHA: summaryCheckpoint},
		SpecificationPacket: string(packetData),
	}
}

// TestProgressionStepRunsThePullRequestWriterBeforeReadiness verifies the
// planner starts, accepts, ends, or waits for the writer before it finalizes
// readiness, and starts it again for a new hand-off checkpoint.
func TestProgressionStepRunsThePullRequestWriterBeforeReadiness(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	writer := &store.Invocation{ID: "inv-writer", Role: workflow.RolePullRequestWriter, Stage: workflow.StagePullRequestSummary, Status: store.InvocationStatusActive, CreatedAt: now.Add(-10 * time.Minute)}
	expired := *writer
	expired.CreatedAt = now.Add(-2 * time.Hour)
	settled := readinessRunWithPullRequestWriter(t)
	settled.PullRequestSummary = &store.PullRequestSummary{CheckpointSHA: summaryCheckpoint, Status: store.PullRequestSummaryWritten, Body: "text"}
	earlierHandOff := readinessRunWithPullRequestWriter(t)
	earlierHandOff.PullRequestSummary = &store.PullRequestSummary{CheckpointSHA: strings.Repeat("a", 40), Status: store.PullRequestSummaryWritten, Body: "old"}
	unconfigured := progressionRunWithConcurrentReviews(t)
	unconfigured.Stage = store.StageReview
	unconfigured.CheckpointSHA = summaryCheckpoint
	unconfigured.SpecificationReview = &store.SpecificationReview{CheckpointSHA: summaryCheckpoint}
	unconfigured.StandardsReview = &store.StandardsReview{CheckpointSHA: summaryCheckpoint}
	pending := readinessRunWithPullRequestWriter(t)

	tests := map[string]struct {
		state    progressionState
		wantKind progressionActionKind
		wantStop progressionOutcome
	}{
		"start writer":            {state: progressionState{Run: &pending}, wantKind: progressionActionStartPullRequestWriter},
		"new hand-off checkpoint": {state: progressionState{Run: &earlierHandOff}, wantKind: progressionActionStartPullRequestWriter},
		"accept summary": {
			state:    progressionState{Run: &pending, ActiveInvocations: []*store.Invocation{writer}, ReadyInvocationIDs: map[string]bool{writer.ID: true}},
			wantKind: progressionActionAcceptPullRequestSummary,
		},
		"end expired writer": {
			state:    progressionState{Run: &pending, ActiveInvocations: []*store.Invocation{&expired}, Now: now, AgentTimeout: time.Hour},
			wantKind: progressionActionAbandonPullRequestWriter,
		},
		"wait for writer": {
			state:    progressionState{Run: &pending, ActiveInvocations: []*store.Invocation{writer}, Now: now, AgentTimeout: time.Hour},
			wantStop: progressionInvocationActive,
		},
		"settled summary":     {state: progressionState{Run: &settled}, wantKind: progressionActionRetryReviewReadiness},
		"writer unconfigured": {state: progressionState{Run: &unconfigured}, wantKind: progressionActionRetryReviewReadiness},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			action, stop := progressionStep(test.state, workflow.DefaultRegistry())
			if test.wantStop != "" {
				if stop == nil || stop.Outcome != test.wantStop {
					t.Fatalf("progressionStep() = %#v, %#v; want stop %q", action, stop, test.wantStop)
				}
				return
			}
			if stop != nil {
				t.Fatalf("progressionStep() stopped with %#v, want %s", stop, test.wantKind)
			}
			if action.kind != test.wantKind {
				t.Fatalf("action kind = %q, want %q", action.kind, test.wantKind)
			}
			if err := action.validate(); err != nil {
				t.Fatalf("action.validate() error = %v", err)
			}
		})
	}
}

// TestMergePullRequestSummarySectionKeepsOtherText verifies the section goes
// above the coordinator section, a later hand-off replaces it, an empty
// section removes it, and text outside both marker pairs never changes.
func TestMergePullRequestSummarySectionKeepsOtherText(t *testing.T) {
	coordinator := generatedPullRequestStart + "\nfactory\n" + generatedPullRequestEnd
	original := "Human intro\n\n" + coordinator + "\n\nHuman footer"
	first := renderPullRequestSummarySection(store.PullRequestSummary{CheckpointSHA: summaryCheckpoint, Status: store.PullRequestSummaryWritten, Body: "first summary"})
	second := renderPullRequestSummarySection(store.PullRequestSummary{CheckpointSHA: strings.Repeat("b", 40), Status: store.PullRequestSummaryWritten, Body: "second summary"})

	merged := mergePullRequestSummarySection(original, first)
	if want := "Human intro\n\n" + first + "\n\n" + coordinator + "\n\nHuman footer"; merged != want {
		t.Fatalf("first merge = %q, want %q", merged, want)
	}
	replaced := mergePullRequestSummarySection(merged, second)
	if strings.Contains(replaced, "first summary") || strings.Count(replaced, pullRequestSummaryStart) != 1 || replaced != strings.Replace(merged, first, second, 1) {
		t.Fatalf("second hand-off body = %q, want exactly the new section", replaced)
	}
	if removed := mergePullRequestSummarySection(replaced, ""); removed != original {
		t.Fatalf("removed section body = %q, want the original body", removed)
	}
	if unchanged := mergePullRequestSummarySection(original, ""); unchanged != original {
		t.Fatalf("body without a section = %q, want it unchanged", unchanged)
	}
}

// TestPullRequestSummaryFromReportKeepsOnlyAUsableBody verifies a completed
// body becomes a written summary, and a non-completed report or a forged
// coordinator marker or a missing template section becomes a failed summary.
func TestPullRequestSummaryFromReportKeepsOnlyAUsableBody(t *testing.T) {
	invocation := store.Invocation{ID: "inv-writer"}
	body := "## Summary\n\nAdds a role.\n\n### evidence\n\nGates passed.\n\n## Merge Danger ##\n\n**Door:** two-way"
	written := pullRequestSummaryFromReport(invocation, report.Report{Outcome: report.OutcomeCompleted, PullRequestSummary: &report.PullRequestSummary{Body: "\n" + body + "\n"}}, summaryCheckpoint)
	if written.Status != store.PullRequestSummaryWritten || written.Body != body || written.CheckpointSHA != summaryCheckpoint {
		t.Fatalf("written summary = %#v, want the trimmed body for the checkpoint", written)
	}
	for name, value := range map[string]report.Report{
		"cannot proceed":  {Outcome: report.OutcomeCannotProceed},
		"forged marker":   {Outcome: report.OutcomeCompleted, PullRequestSummary: &report.PullRequestSummary{Body: body + "\n" + generatedPullRequestEnd}},
		"missing section": {Outcome: report.OutcomeCompleted, PullRequestSummary: &report.PullRequestSummary{Body: "## Summary\n\ntext\n\n## Merge Danger\n\nlow; Evidence is inline"}},
	} {
		t.Run(name, func(t *testing.T) {
			failed := pullRequestSummaryFromReport(invocation, value, summaryCheckpoint)
			if failed.Status != store.PullRequestSummaryFailed || failed.Body != "" || failed.Reason == "" {
				t.Fatalf("summary = %#v, want a failed summary with a reason", failed)
			}
		})
	}
}

// TestMergePullRequestSummarySectionIgnoresAnOrphanMarker verifies a start
// marker left without its end marker never makes a later merge replace the
// human text that follows it.
func TestMergePullRequestSummarySectionIgnoresAnOrphanMarker(t *testing.T) {
	coordinator := generatedPullRequestStart + "\nfactory\n" + generatedPullRequestEnd
	section := renderPullRequestSummarySection(store.PullRequestSummary{CheckpointSHA: summaryCheckpoint, Status: store.PullRequestSummaryWritten, Body: "summary"})
	orphaned := pullRequestSummaryStart + "\nHuman text a person kept\n\n" + coordinator
	first := mergePullRequestSummarySection(orphaned, section)
	second := mergePullRequestSummarySection(first, section)
	for _, body := range []string{first, second} {
		if !strings.Contains(body, "Human text a person kept") || strings.Count(body, "summary\n"+pullRequestSummaryEnd) != 1 {
			t.Fatalf("merged body = %q, want the human text kept and one section", body)
		}
	}
}
