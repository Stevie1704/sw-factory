package factory

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
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
		// Only the first unit owns the non-text change, as the manifest
		// invariant requires exactly one primary assignment per file.
		PrimaryNonTextFiles: nonTextOwnerFor(ordinal),
		ChangedLines:        1,
	}
}

// nonTextOwnerFor gives the renamed fixture file a single owning unit.
func nonTextOwnerFor(ordinal int) []string {
	if ordinal != 1 {
		return nil
	}
	return []string{"src/main.go"}
}

// TestMissingReviewUnitFillsThePersistedConcurrencyCeiling verifies a
// partitioned round schedules several units at once, up to the concurrency
// frozen when the round was created, instead of running one unit at a time.
// ADR 0012 makes concurrency a host capacity bound, not a serialization rule.
func TestMissingReviewUnitFillsThePersistedConcurrencyCeiling(t *testing.T) {
	t.Parallel()

	run := progressionRunWithConcurrentReviews(t)
	run.Stage = store.StageReview
	run.Status = store.StatusActive
	run.CheckpointSHA = reviewResumeCheckpointSHA
	units := []store.ReviewUnit{
		{UnitID: "unit-001", Ordinal: 1},
		{UnitID: "unit-002", Ordinal: 2},
	}
	specification := activeReviewInvocation(workflow.RoleSpecificationReview, store.StageReview, "unit-001")
	standards := activeReviewInvocation(workflow.RoleStandardsReview, workflow.StageStandardsReview, "unit-001")

	for _, test := range []struct {
		name        string
		concurrency int
		active      []*store.Invocation
		results     map[string][]store.ReviewUnitResult
		wantRole    string
		wantUnit    string
	}{
		{
			name:        "idle round starts the first unit",
			concurrency: 2,
			wantRole:    workflow.RoleSpecificationReview,
			wantUnit:    "unit-001",
		},
		{
			name:        "one live reviewer still admits the other axis",
			concurrency: 2,
			active:      []*store.Invocation{specification},
			wantRole:    workflow.RoleStandardsReview,
			wantUnit:    "unit-001",
		},
		{
			name:        "a filled ceiling schedules nothing",
			concurrency: 2,
			active:      []*store.Invocation{specification, standards},
		},
		{
			name:        "a wider ceiling advances to the next unit",
			concurrency: 4,
			active:      []*store.Invocation{specification, standards},
			wantRole:    workflow.RoleSpecificationReview,
			wantUnit:    "unit-002",
		},
		{
			name:        "an accepted result frees its axis for the next unit",
			concurrency: 2,
			active:      []*store.Invocation{standards},
			results: map[string][]store.ReviewUnitResult{
				workflow.RoleSpecificationReview: {{UnitID: "unit-001", Outcome: store.ReviewUnitOutcomeCompleted}},
			},
			wantRole: workflow.RoleSpecificationReview,
			wantUnit: "unit-002",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := progressionState{
				Run:               &run,
				ActiveInvocations: test.active,
				ReviewRound:       &store.ReviewRound{ID: "rr-1", Status: store.ReviewRoundStatusActive, Concurrency: test.concurrency},
				ReviewUnits:       units,
				ReviewUnitResults: test.results,
			}
			role, unitID, ok := missingReviewUnit(state, workflow.DefaultRegistry())
			if test.wantUnit == "" {
				if ok {
					t.Fatalf("missingReviewUnit() = %q/%q/true, want no assignment", role, unitID)
				}
				return
			}
			if !ok || role != test.wantRole || unitID != test.wantUnit {
				t.Fatalf("missingReviewUnit() = %q/%q/%t, want %q/%q/true", role, unitID, ok, test.wantRole, test.wantUnit)
			}
		})
	}
}

// activeReviewInvocation builds a live review invocation that already owns one
// manifest assignment.
func activeReviewInvocation(role string, stage store.Stage, unitID string) *store.Invocation {
	return &store.Invocation{
		ID:            "inv-" + role + "-" + unitID,
		Role:          role,
		Stage:         stage,
		Status:        store.InvocationStatusActive,
		ReviewRoundID: "rr-1",
		ReviewUnitID:  unitID,
	}
}

// TestAggregateReviewUnitResultsFailsOnARoleOwnedBlockingFinding verifies the
// ADR 0012 failure branch: once every unit of an axis is terminal, one
// role-owned blocking finding fails that axis and leaves the other axis clean,
// while the round becomes terminal so repair may route.
func TestAggregateReviewUnitResultsFailsOnARoleOwnedBlockingFinding(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()

	run := progressionRunWithConcurrentReviews(t)
	run.Stage = store.StageReview
	run.CheckpointSHA = reviewResumeCheckpointSHA
	round := aggregateTestRound(run, "rr-blocking-test")
	units := []store.ReviewUnit{
		resumeReviewUnit(round.ID, "unit-001", 1, 0),
		resumeReviewUnit(round.ID, "unit-002", 2, 4),
	}
	if err := opened.SaveReviewManifest(ctx, round, units); err != nil {
		t.Fatalf("SaveReviewManifest() error = %v", err)
	}

	blocker := store.ReviewFinding{
		Location: "src/main.go:1", Claim: "the handler drops the error", Evidence: "src/main.go:1",
		Severity: "blocker", Category: "correctness",
		SuggestedResolution: "return the error", SuggestedOwner: workflow.RoleImplementation,
		UnitID: "unit-001",
	}
	results := []store.ReviewUnitResult{
		{Role: workflow.RoleSpecificationReview, UnitID: "unit-001", InvocationID: "inv-spec-001", Findings: []store.ReviewFinding{blocker}},
		{Role: workflow.RoleSpecificationReview, UnitID: "unit-002", InvocationID: "inv-spec-002"},
		{Role: workflow.RoleStandardsReview, UnitID: "unit-001", InvocationID: "inv-std-001"},
		{Role: workflow.RoleStandardsReview, UnitID: "unit-002", InvocationID: "inv-std-002"},
	}
	for _, result := range results {
		result.RoundID = round.ID
		result.CheckpointSHA = run.CheckpointSHA
		result.Outcome = store.ReviewUnitOutcomeCompleted
		if err := opened.SaveReviewUnitResult(ctx, result); err != nil {
			t.Fatalf("SaveReviewUnitResult(%s/%s) error = %v", result.Role, result.UnitID, err)
		}
	}

	specification, complete, err := aggregateReviewUnitResults(ctx, opened, round, workflow.RoleSpecificationReview, run.CheckpointSHA)
	if err != nil || !complete || specification == nil {
		t.Fatalf("specification aggregate = %#v/%t/%v, want a complete aggregate", specification, complete, err)
	}
	if specification.Outcome != store.ReviewUnitOutcomeCompleted {
		t.Fatalf("specification outcome = %q, want a completed axis carrying its blocker", specification.Outcome)
	}
	if len(specification.Findings) != 1 || specification.Findings[0].UnitID != "unit-001" {
		t.Fatalf("specification findings = %#v, want one finding attributed to unit-001", specification.Findings)
	}
	if !reviewHasBlockingFindingForRole(workflow.RoleSpecificationReview, specification.Findings) {
		t.Fatal("specification axis does not report its own blocking finding")
	}

	standards, complete, err := aggregateReviewUnitResults(ctx, opened, round, workflow.RoleStandardsReview, run.CheckpointSHA)
	if err != nil || !complete || standards == nil {
		t.Fatalf("standards aggregate = %#v/%t/%v, want a complete aggregate", standards, complete, err)
	}
	if len(standards.Findings) != 0 {
		t.Fatalf("standards findings = %#v, want the axes to stay separate", standards.Findings)
	}

	run.SpecificationReview = &store.SpecificationReview{CheckpointSHA: run.CheckpointSHA, Outcome: specification.Outcome, Findings: specification.Findings}
	run.StandardsReview = &store.StandardsReview{CheckpointSHA: run.CheckpointSHA, Outcome: standards.Outcome}
	if !reviewHasBlockingResult(run) {
		t.Fatal("reviewHasBlockingResult() = false, want the round to fail and route repair")
	}
	terminal, err := reviewManifestTerminal(ctx, opened, round, run)
	if err != nil || !terminal {
		t.Fatalf("reviewManifestTerminal() = %t/%v, want true before repair may start", terminal, err)
	}
}

// aggregateTestRound builds a valid persisted round for aggregation fixtures.
func aggregateTestRound(run store.Run, id string) store.ReviewRound {
	return store.ReviewRound{
		ID:                id,
		RunID:             run.ID,
		BaseCheckpointSHA: reviewResumeBaseSHA,
		CheckpointSHA:     run.CheckpointSHA,
		DiffPath:          "/repo/review.diff",
		DiffBytes:         8,
		DiffSHA256:        strings.Repeat("a", 64),
		ManifestSHA256:    strings.Repeat("b", 64),
		SchemaVersion:     reviewunits.SchemaVersion,
		PolicyVersion:     reviewunits.PolicyVersion,
		MaxUnitBytes:      reviewunits.DefaultMaxUnitBytes,
		MaxUnits:          reviewunits.DefaultMaxUnits,
		ContextLines:      reviewDiffContextLines,
		Concurrency:       2,
		Status:            store.ReviewRoundStatusActive,
	}
}

// TestOverBudgetReviewRoundLaunchesNothingUntilAuthorized verifies the ADR 0012
// over-budget disposition end to end: a manifest above the normal fan-out is
// persisted whole, schedules no unit while it awaits authorization, and
// resumes from unit-001 once a maintainer authorizes that exact manifest.
func TestOverBudgetReviewRoundLaunchesNothingUntilAuthorized(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()

	run := progressionRunWithConcurrentReviews(t)
	run.Stage = store.StageReview
	run.Status = store.StatusWaitingForHuman
	run.CheckpointSHA = reviewResumeCheckpointSHA
	round := aggregateTestRound(run, "rr-authorization-test")
	round.Status = store.ReviewRoundStatusAwaitingAuthorization
	round.DiffBytes = 20
	units := make([]store.ReviewUnit, 0, 5)
	for ordinal := 1; ordinal <= 5; ordinal++ {
		units = append(units, resumeReviewUnit(round.ID, "unit-00"+strconv.Itoa(ordinal), ordinal, (ordinal-1)*4))
	}
	if len(units) <= round.MaxUnits {
		t.Fatalf("manifest has %d units, want more than the %d-unit normal fan-out", len(units), round.MaxUnits)
	}
	if err := opened.SaveReviewManifest(ctx, round, units); err != nil {
		t.Fatalf("SaveReviewManifest() error = %v", err)
	}

	state := progressionState{Run: &run, ReviewRound: &round, ReviewUnits: units}
	if role, unitID, ok := missingReviewUnit(state, workflow.DefaultRegistry()); ok {
		t.Fatalf("missingReviewUnit() = %q/%q/true, want nothing scheduled while awaiting authorization", role, unitID)
	}

	if err := opened.AuthorizeReviewRound(ctx, round.ID, len(units)); err != nil {
		t.Fatalf("AuthorizeReviewRound() error = %v", err)
	}
	authorized, err := opened.ReviewRound(ctx, run.ID, run.CheckpointSHA)
	if err != nil || authorized == nil {
		t.Fatalf("ReviewRound() = %#v/%v, want the authorized round", authorized, err)
	}
	if authorized.Status != store.ReviewRoundStatusPending || authorized.AuthorizedMaxUnits != len(units) {
		t.Fatalf("authorized round = %q/%d, want pending/%d", authorized.Status, authorized.AuthorizedMaxUnits, len(units))
	}
	if authorized.ManifestSHA256 != round.ManifestSHA256 || authorized.MaxUnits != round.MaxUnits {
		t.Fatal("authorization changed the frozen manifest identity or its unit workload policy")
	}

	state.ReviewRound = authorized
	role, unitID, ok := missingReviewUnit(state, workflow.DefaultRegistry())
	if !ok || role != workflow.RoleSpecificationReview || unitID != "unit-001" {
		t.Fatalf("missingReviewUnit() = %q/%q/%t, want the first unit after authorization", role, unitID, ok)
	}
}
