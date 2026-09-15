package store_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
)

const (
	reviewUnitTestBaseSHA       = "1111111111111111111111111111111111111111111111111111111111111111"
	reviewUnitTestCheckpointSHA = "2222222222222222222222222222222222222222222222222222222222222222"
)

// TestReviewManifestAndUnitResultsAreIdempotent verifies the normalized review
// projection preserves one immutable manifest and rejects conflicting results.
func TestReviewManifestAndUnitResultsAreIdempotent(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()

	round := testReviewRound()
	unit := testReviewUnit(round.ID)
	if err := opened.SaveReviewManifest(ctx, round, []store.ReviewUnit{unit}); err != nil {
		t.Fatalf("SaveReviewManifest() error = %v", err)
	}
	if err := opened.SaveReviewManifest(ctx, round, []store.ReviewUnit{unit}); err != nil {
		t.Fatalf("SaveReviewManifest() repeated error = %v", err)
	}
	authorized := round
	authorized.AuthorizedMaxUnits = 4
	authorized.Status = store.ReviewRoundStatusAwaitingAuthorization
	if err := opened.SaveReviewRound(ctx, authorized); err != nil {
		t.Fatalf("SaveReviewRound() after authorization metadata change = %v", err)
	}
	units, err := opened.ReviewUnits(ctx, round.ID)
	if err != nil {
		t.Fatalf("ReviewUnits() error = %v", err)
	}
	if len(units) != 1 || units[0].UnitID != unit.UnitID {
		t.Fatalf("ReviewUnits() = %#v, want one stable unit", units)
	}

	result := store.ReviewUnitResult{
		RoundID:       round.ID,
		Role:          "spec_review",
		UnitID:        unit.UnitID,
		InvocationID:  "inv-review-unit",
		CheckpointSHA: round.CheckpointSHA,
		Outcome:       store.ReviewUnitOutcomeCompleted,
		Summary:       "unit completed",
		Findings: []store.ReviewFinding{{
			Location:            "src/main.go:1",
			Claim:               "the changed line is correct",
			Evidence:            "the exact unit diff contains the guarded branch",
			Severity:            "advisory",
			Category:            "correctness",
			SuggestedResolution: "retain the implementation",
			SuggestedOwner:      "implementation",
			UnitID:              unit.UnitID,
		}},
	}
	if err := opened.SaveReviewUnitResult(ctx, result); err != nil {
		t.Fatalf("SaveReviewUnitResult() error = %v", err)
	}
	if err := opened.SaveReviewUnitResult(ctx, result); err != nil {
		t.Fatalf("SaveReviewUnitResult() repeated error = %v", err)
	}
	conflict := result
	conflict.Summary = "different accepted result"
	if err := opened.SaveReviewUnitResult(ctx, conflict); err == nil {
		t.Fatal("SaveReviewUnitResult() conflicting error = nil, want idempotency conflict")
	}
}

// TestReviewUnitTelemetryStaysContentFree verifies local evaluation exposes
// bounded workload, invocation, finding, and incomplete-result measurements.
func TestReviewUnitTelemetryStaysContentFree(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()

	started := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	run := store.Run{ID: "run-review-telemetry", RepositoryPath: "/repo", Stage: store.StageReview, Status: store.StatusActive, CreatedAt: started, UpdatedAt: started}
	if err := opened.EnsureEvaluationSummary(ctx, run); err != nil {
		t.Fatalf("EnsureEvaluationSummary() error = %v", err)
	}
	round := testReviewRound()
	round.RunID = run.ID
	unit := testReviewUnit(round.ID)
	if err := opened.SaveReviewManifest(ctx, round, []store.ReviewUnit{unit}); err != nil {
		t.Fatalf("SaveReviewManifest() error = %v", err)
	}

	for _, invocation := range []store.Invocation{
		{ID: "inv-review-spec", RunID: run.ID, Harness: "codex", Role: "spec_review", Stage: store.StageReview, Status: store.InvocationStatusCompleted, ReviewRoundID: round.ID, ReviewUnitID: unit.UnitID, CreatedAt: started, UpdatedAt: started.Add(2 * time.Minute)},
		{ID: "inv-review-standards", RunID: run.ID, Harness: "codex", Role: "standards_review", Stage: store.StageReview, Status: store.InvocationStatusCompleted, ReviewRoundID: round.ID, ReviewUnitID: unit.UnitID, CreatedAt: started.Add(time.Minute), UpdatedAt: started.Add(4 * time.Minute)},
	} {
		if err := opened.SaveInvocation(ctx, invocation); err != nil {
			t.Fatalf("SaveInvocation(%q) error = %v", invocation.ID, err)
		}
		if err := opened.RecordEvaluationInvocation(ctx, run.ID, invocation, "sha256:worker", 1); err != nil {
			t.Fatalf("RecordEvaluationInvocation(%q) error = %v", invocation.ID, err)
		}
	}
	result := store.ReviewUnitResult{
		RoundID:       round.ID,
		Role:          "spec_review",
		UnitID:        unit.UnitID,
		InvocationID:  "inv-review-spec",
		CheckpointSHA: round.CheckpointSHA,
		Outcome:       store.ReviewUnitOutcomeCompleted,
		Findings: []store.ReviewFinding{{
			Location:            "src/main.go:1",
			Claim:               "the implementation is correct",
			Evidence:            "bounded review evidence",
			Severity:            "blocker",
			Category:            "correctness",
			SuggestedResolution: "keep the change",
			SuggestedOwner:      "implementation",
			UnitID:              unit.UnitID,
		}},
	}
	if err := opened.SaveReviewUnitResult(ctx, result); err != nil {
		t.Fatalf("SaveReviewUnitResult() error = %v", err)
	}
	got, err := opened.EvaluationSummary(ctx, run.ID)
	if err != nil {
		t.Fatalf("EvaluationSummary() error = %v", err)
	}
	if got == nil {
		t.Fatal("EvaluationSummary() = nil, want a summary")
	}
	if got.ReviewDiffBytes != round.DiffBytes || got.ReviewUnitCount != 1 || got.ReviewLargestUnitBytes != unit.WorkloadBytes || got.ReviewInvocationCount != 2 || got.ReviewFindingCounts["blocker"] != 1 {
		t.Fatalf("review telemetry = %#v, want workload/invocation/finding measurements", got)
	}
	if got.ReviewDuration != 5*time.Minute {
		t.Fatalf("review duration = %s, want 5m", got.ReviewDuration)
	}
	if got.ReviewIncompleteUnitCount != 0 || got.ReviewCannotProceedUnitCount != 0 || got.ReviewOverBudgetCount != 0 {
		t.Fatalf("review incomplete telemetry = %#v, want zero", got)
	}
}

// testReviewRound returns a valid persisted review-round identity for store
// tests without depending on factory packet or filesystem construction.
func testReviewRound() store.ReviewRound {
	return store.ReviewRound{
		ID:                 "rr-store-test",
		RunID:              "run-store-review",
		BaseCheckpointSHA:  reviewUnitTestBaseSHA,
		CheckpointSHA:      reviewUnitTestCheckpointSHA,
		DiffPath:           "/repo/.factory-agents/run-store-review/review-round/review.diff",
		DiffBytes:          12,
		DiffSHA256:         strings.Repeat("a", 64),
		ManifestSHA256:     strings.Repeat("b", 64),
		SchemaVersion:      1,
		PolicyVersion:      "review-units-v1",
		MaxUnitBytes:       64 << 10,
		MaxUnits:           4,
		ContextLines:       10,
		Concurrency:        2,
		AuthorizedMaxUnits: 0,
		Status:             store.ReviewRoundStatusPending,
	}
}

// testReviewUnit returns one valid primary assignment for a store test round.
func testReviewUnit(roundID string) store.ReviewUnit {
	return store.ReviewUnit{
		RoundID:             roundID,
		UnitID:              "unit-001",
		Ordinal:             1,
		WorkloadBytes:       4,
		DiffSHA256:          strings.Repeat("c", 64),
		Segments:            []store.ReviewUnitSegment{{StartByte: 0, EndByte: 4}},
		PrimaryRanges:       []store.ReviewUnitRange{{Path: "src/main.go", Hunk: 1, Side: "new", StartLine: 1, EndLine: 1}},
		PrimaryNonTextFiles: []string{"src/main.go"},
		ChangedLines:        1,
	}
}
