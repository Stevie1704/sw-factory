package factory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/reviewunits"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

const (
	reviewResumeBaseSHA       = "1111111111111111111111111111111111111111111111111111111111111111"
	reviewResumeCheckpointSHA = "2222222222222222222222222222222222222222222222222222222222222222"
)

// TestReviewResumeAgentRequestSelectsTheNextPersistedAxisUnit verifies manual
// recovery advances within one axis before entering the next configured axis.
func TestReviewResumeAgentRequestSelectsTheNextPersistedAxisUnit(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()

	run := progressionRunWithConcurrentReviews(t)
	run.Stage = store.StageReview
	run.BaseCheckpointSHA = reviewResumeBaseSHA
	run.CheckpointSHA = reviewResumeCheckpointSHA
	diff := []byte("diff --git a/src/main.go b/src/main.go\nindex 1111111..2222222 100644\n--- a/src/main.go\n+++ b/src/main.go\n@@ -1 +1 @@\n-old\n+new\n" +
		"diff --git a/src/util.go b/src/util.go\nindex 3333333..4444444 100644\n--- a/src/util.go\n+++ b/src/util.go\n@@ -1 +1 @@\n-old\n+new\n")
	diffPath := filepath.Join(t.TempDir(), "review.diff")
	if err := os.WriteFile(diffPath, diff, 0o600); err != nil {
		t.Fatalf("write review diff: %v", err)
	}
	manifest, err := reviewunits.Build(diff, run.ID, run.BaseCheckpointSHA, run.CheckpointSHA, reviewunits.Policy{
		SchemaVersion: reviewunits.SchemaVersion,
		PolicyVersion: reviewunits.PolicyVersion,
		MaxUnitBytes:  180,
		MaxUnits:      4,
		ContextLines:  10,
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	round := store.ReviewRound{
		ID:                reviewRoundID(run, manifest.DiffSHA256),
		RunID:             run.ID,
		BaseCheckpointSHA: run.BaseCheckpointSHA,
		CheckpointSHA:     run.CheckpointSHA,
		DiffPath:          diffPath,
		DiffBytes:         int64(manifest.DiffBytes),
		DiffSHA256:        manifest.DiffSHA256,
		ManifestSHA256:    manifest.ManifestSHA256,
		SchemaVersion:     manifest.SchemaVersion,
		PolicyVersion:     manifest.PolicyVersion,
		MaxUnitBytes:      manifest.MaxUnitBytes,
		MaxUnits:          manifest.MaxUnits,
		ContextLines:      manifest.ContextLines,
		Concurrency:       2,
		Status:            store.ReviewRoundStatusActive,
	}
	units := reviewUnitsFromManifest(round.ID, manifest)
	if len(units) != 2 {
		t.Fatalf("Build() produced %d units, want two", len(units))
	}
	if err := opened.SaveReviewManifest(ctx, round, units); err != nil {
		t.Fatalf("SaveReviewManifest() error = %v", err)
	}
	if err := opened.SaveReviewUnitResult(ctx, store.ReviewUnitResult{
		RoundID:       round.ID,
		Role:          workflow.RoleSpecificationReview,
		UnitID:        "unit-001",
		InvocationID:  "inv-spec-001",
		CheckpointSHA: run.CheckpointSHA,
		Outcome:       store.ReviewUnitOutcomeCompleted,
	}); err != nil {
		t.Fatalf("SaveReviewUnitResult() error = %v", err)
	}

	request, err := reviewResumeAgentRequest(ctx, opened, run)
	if err != nil {
		t.Fatalf("reviewResumeAgentRequest() error = %v", err)
	}
	if request.Role != workflow.RoleSpecificationReview || request.Stage != store.StageReview || request.ReviewUnitID != "unit-002" {
		t.Fatalf("resume request = %#v, want specification unit-002", request)
	}

	if err := opened.SaveReviewUnitResult(ctx, store.ReviewUnitResult{
		RoundID:       round.ID,
		Role:          workflow.RoleSpecificationReview,
		UnitID:        "unit-002",
		InvocationID:  "inv-spec-002",
		CheckpointSHA: run.CheckpointSHA,
		Outcome:       store.ReviewUnitOutcomeCompleted,
	}); err != nil {
		t.Fatalf("SaveReviewUnitResult() second error = %v", err)
	}
	request, err = reviewResumeAgentRequest(ctx, opened, run)
	if err != nil {
		t.Fatalf("reviewResumeAgentRequest() second error = %v", err)
	}
	if request.Role != workflow.RoleStandardsReview || request.Stage != workflow.StageStandardsReview || request.ReviewUnitID != "unit-001" {
		t.Fatalf("resume request = %#v, want standards unit-001", request)
	}
}

// TestAggregateReviewUnitResultsKeepsAxesSeparate verifies partial progress is
// not exposed as an aggregate and a terminal failure on one axis does not
// contaminate the other axis.
func TestAggregateReviewUnitResultsKeepsAxesSeparate(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()

	run := progressionRunWithConcurrentReviews(t)
	run.Stage = store.StageReview
	run.CheckpointSHA = reviewResumeCheckpointSHA
	round := store.ReviewRound{
		ID:                "rr-aggregate-test",
		RunID:             run.ID,
		BaseCheckpointSHA: reviewResumeBaseSHA,
		CheckpointSHA:     run.CheckpointSHA,
		DiffPath:          "/repo/review.diff",
		DiffBytes:         8,
		DiffSHA256:        strings.Repeat("a", 64),
		ManifestSHA256:    strings.Repeat("b", 64),
		SchemaVersion:     1,
		PolicyVersion:     "review-units-v1",
		MaxUnitBytes:      64 << 10,
		MaxUnits:          4,
		ContextLines:      10,
		Concurrency:       2,
		Status:            store.ReviewRoundStatusActive,
	}
	units := []store.ReviewUnit{
		resumeReviewUnit(round.ID, "unit-001", 1, 0),
		resumeReviewUnit(round.ID, "unit-002", 2, 4),
	}
	if err := opened.SaveReviewManifest(ctx, round, units); err != nil {
		t.Fatalf("SaveReviewManifest() error = %v", err)
	}

	specificationResult := func(unitID, invocationID string) store.ReviewUnitResult {
		return store.ReviewUnitResult{RoundID: round.ID, Role: workflow.RoleSpecificationReview, UnitID: unitID, InvocationID: invocationID, CheckpointSHA: run.CheckpointSHA, Outcome: store.ReviewUnitOutcomeCompleted}
	}
	if err := opened.SaveReviewUnitResult(ctx, specificationResult("unit-001", "inv-spec-001")); err != nil {
		t.Fatalf("SaveReviewUnitResult() partial error = %v", err)
	}
	if aggregate, complete, aggregateErr := aggregateReviewUnitResults(ctx, opened, round, workflow.RoleSpecificationReview, run.CheckpointSHA); aggregateErr != nil || complete || aggregate != nil {
		t.Fatalf("partial specification aggregate = %#v/%t/%v, want nil/false/nil", aggregate, complete, aggregateErr)
	}
	if err := opened.SaveReviewUnitResult(ctx, specificationResult("unit-002", "inv-spec-002")); err != nil {
		t.Fatalf("SaveReviewUnitResult() complete error = %v", err)
	}
	specification, complete, err := aggregateReviewUnitResults(ctx, opened, round, workflow.RoleSpecificationReview, run.CheckpointSHA)
	if err != nil || !complete || specification == nil || specification.Outcome != store.ReviewUnitOutcomeCompleted {
		t.Fatalf("specification aggregate = %#v/%t/%v, want completed aggregate", specification, complete, err)
	}

	cannotProceed := store.ReviewUnitResult{RoundID: round.ID, Role: workflow.RoleStandardsReview, UnitID: "unit-001", InvocationID: "inv-standards-001", CheckpointSHA: run.CheckpointSHA, Outcome: store.ReviewUnitOutcomeCannotProceed, Evidence: []store.ReviewEvidence{{Kind: "infrastructure", Detail: "standards source unavailable"}}}
	if err := opened.SaveReviewUnitResult(ctx, cannotProceed); err != nil {
		t.Fatalf("SaveReviewUnitResult() standards failure error = %v", err)
	}
	if aggregate, complete, aggregateErr := aggregateReviewUnitResults(ctx, opened, round, workflow.RoleStandardsReview, run.CheckpointSHA); aggregateErr != nil || complete || aggregate != nil {
		t.Fatalf("partial standards aggregate = %#v/%t/%v, want nil/false/nil", aggregate, complete, aggregateErr)
	}
	if err := opened.SaveReviewUnitResult(ctx, store.ReviewUnitResult{RoundID: round.ID, Role: workflow.RoleStandardsReview, UnitID: "unit-002", InvocationID: "inv-standards-002", CheckpointSHA: run.CheckpointSHA, Outcome: store.ReviewUnitOutcomeCompleted}); err != nil {
		t.Fatalf("SaveReviewUnitResult() standards completion error = %v", err)
	}
	standards, complete, err := aggregateReviewUnitResults(ctx, opened, round, workflow.RoleStandardsReview, run.CheckpointSHA)
	if err != nil || !complete || standards == nil || standards.Outcome != store.ReviewUnitOutcomeCannotProceed || len(standards.Evidence) != 1 {
		t.Fatalf("standards aggregate = %#v/%t/%v, want cannot-proceed aggregate", standards, complete, err)
	}
	terminal, err := reviewManifestTerminal(ctx, opened, round, run)
	if err != nil || !terminal {
		t.Fatalf("reviewManifestTerminal() = %t/%v, want true after both axes", terminal, err)
	}
}

// resumeReviewUnit returns a small valid unit fixture for restart selection.
func resumeReviewUnit(roundID, unitID string, ordinal, startByte int) store.ReviewUnit {
	return store.ReviewUnit{
		RoundID:       roundID,
		UnitID:        unitID,
		Ordinal:       ordinal,
		WorkloadBytes: 4,
		DiffSHA256:    strings.Repeat("c", 64),
		Segments:      []store.ReviewUnitSegment{{StartByte: startByte, EndByte: startByte + 4}},
		PrimaryRanges: []store.ReviewUnitRange{{Path: "src/main.go", Hunk: 1, Side: "new", StartLine: ordinal, EndLine: ordinal}},
		PrimaryFiles:  []string{"src/main.go"},
		ChangedLines:  1,
	}
}
