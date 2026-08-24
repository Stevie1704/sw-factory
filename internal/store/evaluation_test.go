package store_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
)

const evaluationCheckpoint = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestEvaluationSummaryRecordsContentFreeRunAndAggregates verifies the local
// evaluation projection records workflow metadata without copying work content
// and produces useful aggregate counts and rates.
func TestEvaluationSummaryRecordsContentFreeRunAndAggregates(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()

	started := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	run := store.Run{ID: "run-evaluation", RepositoryPath: "/repo", Stage: store.StageClaim, Status: store.StatusActive, ImageDigest: "sha256:worker", CreatedAt: started, UpdatedAt: started}
	if err := opened.EnsureEvaluationSummary(ctx, run); err != nil {
		t.Fatalf("EnsureEvaluationSummary() error = %v", err)
	}
	if err := opened.SaveInvocation(ctx, store.Invocation{ID: "inv-evaluation", RunID: run.ID, Harness: "codex", Role: "implementation", Stage: store.StageImplementation, Model: "gpt-5", PromptVersion: "implementation-v1", Status: store.InvocationStatusCompleted, CreatedAt: started, UpdatedAt: started.Add(2 * time.Minute)}); err != nil {
		t.Fatalf("SaveInvocation() error = %v", err)
	}
	if err := opened.RecordEvaluationInvocation(ctx, run.ID, store.Invocation{ID: "inv-evaluation", RunID: run.ID, Harness: "codex", Role: "implementation", Stage: store.StageImplementation, Model: "gpt-5", PromptVersion: "implementation-v1"}, "sha256:worker", 1); err != nil {
		t.Fatalf("RecordEvaluationInvocation() error = %v", err)
	}
	if err := opened.SaveGateResults(ctx, []store.GateResult{{RunID: run.ID, CheckpointSHA: evaluationCheckpoint, Phase: store.GatePhaseCheckpoint, Ordinal: 0, GateName: "test", Outcome: store.GateOutcomePassed, Status: "success", Blocking: true}}); err != nil {
		t.Fatalf("SaveGateResults() error = %v", err)
	}
	if err := opened.RefreshEvaluationCounts(ctx, run.ID); err != nil {
		t.Fatalf("RefreshEvaluationCounts() error = %v", err)
	}
	if err := opened.RecordEvaluationStageTransition(ctx, run.ID, store.StageClaim, store.StageImplementation, started.Add(time.Minute)); err != nil {
		t.Fatalf("RecordEvaluationStageTransition() error = %v", err)
	}
	if err := opened.RecordEvaluationAttempt(ctx, run.ID, store.EvaluationAttemptCheckRepair); err != nil {
		t.Fatalf("RecordEvaluationAttempt() error = %v", err)
	}
	if err := opened.RecordEvaluationAttempt(ctx, run.ID, store.EvaluationAttemptReviewRevision); err != nil {
		t.Fatalf("RecordEvaluationAttempt() review error = %v", err)
	}
	if err := opened.RecordEvaluationExemption(ctx, run.ID, store.EvaluationExemptionTechnical); err != nil {
		t.Fatalf("RecordEvaluationExemption() error = %v", err)
	}
	if err := opened.RecordEvaluationEscalation(ctx, run.ID, store.EvaluationEscalationReviewFinding); err != nil {
		t.Fatalf("RecordEvaluationEscalation() error = %v", err)
	}
	if err := opened.RecordEvaluationBlocker(ctx, run.ID, store.EvaluationBlockerReview); err != nil {
		t.Fatalf("RecordEvaluationBlocker() first error = %v", err)
	}
	if err := opened.RecordEvaluationBlocker(ctx, run.ID, store.EvaluationBlockerReview); err != nil {
		t.Fatalf("RecordEvaluationBlocker() second error = %v", err)
	}
	if err := opened.RecordEvaluationBudgetExhaustion(ctx, run.ID); err != nil {
		t.Fatalf("RecordEvaluationBudgetExhaustion() error = %v", err)
	}
	if err := opened.RecordEvaluationUsage(ctx, run.ID, "inv-evaluation", store.EvaluationUsage{Available: true, InputTokens: 10, OutputTokens: 20, TotalTokens: 30, CostReported: true, CostMicros: 7, Currency: "USD"}); err != nil {
		t.Fatalf("RecordEvaluationUsage() error = %v", err)
	}
	finished := started.Add(5 * time.Minute)
	if err := opened.FinalizeEvaluation(ctx, run.ID, store.EvaluationOutcomeComplete, finished); err != nil {
		t.Fatalf("FinalizeEvaluation() error = %v", err)
	}

	got, err := opened.EvaluationSummary(ctx, run.ID)
	if err != nil {
		t.Fatalf("EvaluationSummary() error = %v", err)
	}
	if got == nil {
		t.Fatal("EvaluationSummary() = nil, want a persisted summary")
	}
	if got.Outcome != store.EvaluationOutcomeComplete || got.TotalWallTime != 5*time.Minute {
		t.Fatalf("summary outcome/wall time = %q/%s, want complete/5m", got.Outcome, got.TotalWallTime)
	}
	if got.InvocationCount != 1 || got.GateCount != 1 || got.CheckRepairCount != 1 || got.ReviewRevisionCount != 1 || !got.BudgetExhausted {
		t.Fatalf("summary counts = %#v, want invocation=1 gate=1 check=1 review=1", got)
	}
	if len(got.InvocationVersions) != 1 || got.InvocationVersions[0].WorkerVersion != "sha256:worker" {
		t.Fatalf("summary versions = %#v, want one content-free invocation version", got.InvocationVersions)
	}
	if !got.Usage.Available || got.Usage.TotalTokens != 30 || got.Usage.CostMicros != 7 {
		t.Fatalf("summary usage = %#v, want reported usage", got.Usage)
	}
	if len(got.RepeatedBlockers) != 1 || got.RepeatedBlockers[0].Count != 2 {
		t.Fatalf("summary blockers = %#v, want review count 2", got.RepeatedBlockers)
	}
	serialized, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(serialized), "issue text") || strings.Contains(string(serialized), "source contents") || strings.Contains(string(serialized), "command output") {
		t.Fatalf("evaluation summary contains work content: %s", serialized)
	}

	secondRun := store.Run{ID: "run-evaluation-failed", RepositoryPath: "/repo", Stage: store.StageClaim, Status: store.StatusActive, CreatedAt: started, UpdatedAt: started}
	if err := opened.EnsureEvaluationSummary(ctx, secondRun); err != nil {
		t.Fatal(err)
	}
	if err := opened.FinalizeEvaluation(ctx, secondRun.ID, store.EvaluationOutcomeFailed, started.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	aggregate, err := opened.AggregateEvaluation(ctx)
	if err != nil {
		t.Fatalf("AggregateEvaluation() error = %v", err)
	}
	if aggregate.RunCount != 2 || aggregate.OutcomeCounts[store.EvaluationOutcomeComplete] != 1 || aggregate.OutcomeCounts[store.EvaluationOutcomeFailed] != 1 {
		t.Fatalf("aggregate outcomes = %#v, want one complete and one failed", aggregate)
	}
	if aggregate.SuccessRate != 0.5 || aggregate.AverageTotalWallTime != 7*time.Minute+30*time.Second {
		t.Fatalf("aggregate rates/duration = %v/%s, want 0.5/7m30s", aggregate.SuccessRate, aggregate.AverageTotalWallTime)
	}
}

// TestEvaluationSummaryCountsRepeatedGateRuns verifies retries against one
// exact checkpoint increase execution evidence even when result storage is an
// idempotent upsert.
func TestEvaluationSummaryCountsRepeatedGateRuns(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	run := store.Run{ID: "run-repeated-gates", RepositoryPath: "/repo", Stage: store.StageCheck, Status: store.StatusActive}
	if err := opened.EnsureEvaluationSummary(ctx, run); err != nil {
		t.Fatalf("EnsureEvaluationSummary() error = %v", err)
	}
	result := store.GateResult{RunID: run.ID, CheckpointSHA: evaluationCheckpoint, Phase: store.GatePhaseCheckpoint, Ordinal: 0, GateName: "test", Outcome: store.GateOutcomeFailed, Status: "failure", Blocking: true}
	if err := opened.SaveGateResults(ctx, []store.GateResult{result}); err != nil {
		t.Fatalf("SaveGateResults() error = %v", err)
	}
	if err := opened.RefreshEvaluationCounts(ctx, run.ID); err != nil {
		t.Fatalf("RefreshEvaluationCounts() error = %v", err)
	}
	if err := opened.RecordEvaluationGateRun(ctx, run.ID, 1); err != nil {
		t.Fatalf("RecordEvaluationGateRun() error = %v", err)
	}
	if err := opened.RefreshEvaluationCounts(ctx, run.ID); err != nil {
		t.Fatalf("RefreshEvaluationCounts() after repeat error = %v", err)
	}
	got, err := opened.EvaluationSummary(ctx, run.ID)
	if err != nil {
		t.Fatalf("EvaluationSummary() error = %v", err)
	}
	if got == nil || got.GateCount != 2 {
		t.Fatalf("EvaluationSummary() = %#v, want two gate executions for one result row", got)
	}
}

// TestEvaluationDispositionHashesTheOpaqueFindingIdentity verifies a human
// disposition is attached to a category while the finding identity itself is
// never persisted as content.
func TestEvaluationDispositionHashesTheOpaqueFindingIdentity(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	run := store.Run{ID: "run-disposition", RepositoryPath: "/repo", Stage: store.StageReview, Status: store.StatusWaitingForHuman}
	if err := opened.EnsureEvaluationSummary(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := opened.RecordEvaluationEscalation(ctx, run.ID, store.EvaluationEscalationReviewFinding); err != nil {
		t.Fatal(err)
	}
	const findingText = "review finding text must not be retained"
	disposition, err := opened.SaveEvaluationDisposition(ctx, store.EvaluationDispositionRequest{RunID: run.ID, EventID: findingText, Category: store.EvaluationEscalationReviewFinding, Disposition: store.EvaluationDispositionAdvisory})
	if err != nil {
		t.Fatalf("SaveEvaluationDisposition() error = %v", err)
	}
	if disposition.EventIDHash == "" || disposition.EventIDHash == findingText {
		t.Fatalf("disposition event identity = %#v, want a non-content hash", disposition)
	}
	if disposition.Disposition != store.EvaluationDispositionAdvisory {
		t.Fatalf("disposition = %#v, want advisory", disposition)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), findingText) {
		t.Fatal("operational database retained the finding text")
	}
}

// TestEvaluationUsageIsExplicitlyUnavailableUntilReported verifies the store
// does not manufacture usage or cost when a harness provides no measurement.
func TestEvaluationUsageIsExplicitlyUnavailableUntilReported(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	if err := opened.EnsureEvaluationSummary(ctx, store.Run{ID: "run-no-usage", RepositoryPath: "/repo", Stage: store.StageClaim, Status: store.StatusActive}); err != nil {
		t.Fatal(err)
	}
	got, err := opened.EvaluationSummary(ctx, "run-no-usage")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Usage.Available || got.Usage.UnavailableReason != store.EvaluationUsageNotReported {
		t.Fatalf("usage = %#v, want explicitly unavailable/not_reported", got.Usage)
	}
}

// TestAggregateEvaluationReportsMixedCurrencyCostUnavailable verifies cost is
// never combined across currencies even when each harness report is reliable.
func TestAggregateEvaluationReportsMixedCurrencyCostUnavailable(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	for _, fixture := range []struct {
		runID    string
		currency string
	}{
		{runID: "run-usd", currency: "USD"},
		{runID: "run-eur", currency: "EUR"},
	} {
		run := store.Run{ID: fixture.runID, RepositoryPath: "/repo/" + fixture.runID, Stage: store.StageClaim, Status: store.StatusActive}
		if err := opened.EnsureEvaluationSummary(ctx, run); err != nil {
			t.Fatal(err)
		}
		if err := opened.RecordEvaluationUsage(ctx, fixture.runID, "inv-"+fixture.runID, store.EvaluationUsage{Available: true, CostReported: true, CostMicros: 1, Currency: fixture.currency}); err != nil {
			t.Fatal(err)
		}
	}
	aggregate, err := opened.AggregateEvaluation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if aggregate.Usage.Available || aggregate.Usage.UnavailableReason != store.EvaluationUsageMixedCurrency || aggregate.Usage.CostReported {
		t.Fatalf("aggregate usage = %#v, want mixed-currency cost unavailable", aggregate.Usage)
	}
}

// TestAggregateEvaluationReportsPartialCostUnavailable verifies aggregate cost
// is not presented as complete when one usage-bearing run omitted cost.
func TestAggregateEvaluationReportsPartialCostUnavailable(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	for _, fixture := range []struct {
		runID string
		usage store.EvaluationUsage
	}{
		{runID: "run-cost", usage: store.EvaluationUsage{Available: true, TotalTokens: 10, CostReported: true, CostMicros: 1, Currency: "USD"}},
		{runID: "run-token-only", usage: store.EvaluationUsage{Available: true, TotalTokens: 20}},
	} {
		run := store.Run{ID: fixture.runID, RepositoryPath: "/repo/" + fixture.runID, Stage: store.StageClaim, Status: store.StatusActive}
		if err := opened.EnsureEvaluationSummary(ctx, run); err != nil {
			t.Fatal(err)
		}
		if err := opened.RecordEvaluationUsage(ctx, fixture.runID, "inv-"+fixture.runID, fixture.usage); err != nil {
			t.Fatal(err)
		}
	}
	perRun, err := opened.EvaluationSummary(ctx, "run-token-only")
	if err != nil {
		t.Fatal(err)
	}
	if perRun == nil || perRun.Usage.UnavailableReason != store.EvaluationUsagePartialCost {
		t.Fatalf("per-run token-only usage = %#v, want partial cost unavailable", perRun)
	}
	aggregate, err := opened.AggregateEvaluation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !aggregate.Usage.Available || aggregate.Usage.CostReported || aggregate.Usage.UnavailableReason != store.EvaluationUsagePartialCost || aggregate.Usage.CostMicros != 0 {
		t.Fatalf("aggregate usage = %#v, want partial cost unavailable with token totals", aggregate.Usage)
	}
}

// TestEvaluationSummaryRejectsUnsafeMetadata verifies version fields cannot
// become a side channel for issue text or other work content.
func TestEvaluationSummaryRejectsUnsafeMetadata(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()

	if err := opened.EnsureEvaluationSummary(ctx, store.Run{ID: "run-unsafe", RepositoryPath: "/repo", Stage: store.StageClaim, Status: store.StatusActive, ImageDigest: "issue text"}); err == nil {
		t.Fatal("EnsureEvaluationSummary() error = nil, want unsafe worker metadata rejection")
	}
	if err := opened.SaveEvaluationSummary(ctx, store.EvaluationSummary{
		RunID:   "run-unsafe",
		Outcome: store.EvaluationOutcomeActive,
		InvocationVersions: []store.EvaluationInvocationVersion{{
			Model: "source contents",
		}},
	}); err == nil {
		t.Fatal("SaveEvaluationSummary() error = nil, want unsafe invocation metadata rejection")
	}
}

// TestDeleteEvaluationSummariesBeforeOnlyRemovesExplicitlySelectedTerminalData
// verifies retention deletion is deliberate, bounded, and does not touch active
// summaries.
func TestDeleteEvaluationSummariesBeforeOnlyRemovesExplicitlySelectedTerminalData(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	old := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for _, fixture := range []struct {
		id     string
		status store.EvaluationOutcome
		at     time.Time
	}{
		{id: "run-old", status: store.EvaluationOutcomeComplete, at: old},
		{id: "run-new", status: store.EvaluationOutcomeComplete, at: old.Add(48 * time.Hour)},
		{id: "run-active", status: store.EvaluationOutcomeActive, at: old},
	} {
		run := store.Run{ID: fixture.id, RepositoryPath: "/repo/" + fixture.id, Stage: store.StageClaim, Status: store.StatusActive, CreatedAt: fixture.at, UpdatedAt: fixture.at}
		if err := opened.EnsureEvaluationSummary(ctx, run); err != nil {
			t.Fatal(err)
		}
		if fixture.status != store.EvaluationOutcomeActive {
			if err := opened.FinalizeEvaluation(ctx, fixture.id, fixture.status, fixture.at.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
	}
	deleted, err := opened.DeleteEvaluationSummariesBefore(ctx, old.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("DeleteEvaluationSummariesBefore() error = %v", err)
	}
	if deleted.Summaries != 1 {
		t.Fatalf("deleted summaries = %#v, want one old terminal summary", deleted)
	}
	for _, id := range []string{"run-old", "run-new", "run-active"} {
		got, err := opened.EvaluationSummary(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		wantPresent := id != "run-old"
		if (got != nil) != wantPresent {
			t.Fatalf("summary %q present=%t, want %t", id, got != nil, wantPresent)
		}
	}
}
