package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestStoreCleanupUsesTerminalRetentionAndPreservesEvaluationSummaries
// verifies the seven-day boundary and the separation between operational and
// content-free evaluation projections.
func TestStoreCleanupUsesTerminalRetentionAndPreservesEvaluationSummaries(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, time.January, 20, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-7 * 24 * time.Hour)
	opened, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()

	boundary := store.Run{
		ID:             "run-boundary",
		RepositoryPath: "/repo/boundary",
		Stage:          store.StageReady,
		Status:         store.StatusComplete,
		Branch:         "factory/run-boundary",
		Worktree:       "/worktrees/run-boundary",
		TerminalAt:     cutoff,
		CreatedAt:      cutoff.Add(-time.Hour),
		UpdatedAt:      cutoff,
	}
	recent := store.Run{
		ID:             "run-recent",
		RepositoryPath: "/repo/recent",
		Stage:          store.StageReady,
		Status:         store.StatusFailed,
		Branch:         "factory/run-recent",
		Worktree:       "/worktrees/run-recent",
		TerminalAt:     cutoff.Add(time.Minute),
		CreatedAt:      cutoff,
		UpdatedAt:      cutoff.Add(time.Minute),
	}
	active := store.Run{
		ID:             "run-active",
		RepositoryPath: "/repo/active",
		Stage:          store.StageImplementation,
		Status:         store.StatusActive,
		Branch:         "factory/run-active",
		Worktree:       "/worktrees/run-active",
		CreatedAt:      cutoff.Add(-time.Hour),
		UpdatedAt:      cutoff.Add(-time.Hour),
	}
	for _, run := range []store.Run{boundary, recent, active} {
		if err := opened.SaveRun(ctx, run); err != nil {
			t.Fatalf("SaveRun(%s) error = %v", run.ID, err)
		}
	}
	if err := opened.SaveInvocation(ctx, store.Invocation{
		ID:                  "inv-boundary",
		RunID:               boundary.ID,
		Harness:             "codex",
		Role:                "implementation",
		Stage:               store.StageImplementation,
		Status:              store.InvocationStatusCompleted,
		InvocationDirectory: "/worktrees/.factory-agents/run-boundary/inv-boundary/packet",
		ResultDirectory:     "/worktrees/.factory-agents/run-boundary/inv-boundary/results",
		CreatedAt:           cutoff,
		UpdatedAt:           cutoff,
	}); err != nil {
		t.Fatal(err)
	}
	if err := opened.SavePendingEffect(ctx, store.PendingEffect{
		RunID:     boundary.ID,
		ID:        "effect-boundary",
		Kind:      store.PendingEffectKindCheckpoint,
		Payload:   `{"run_id":"run-boundary"}`,
		CreatedAt: cutoff,
		UpdatedAt: cutoff,
	}); err != nil {
		t.Fatal(err)
	}
	if err := opened.SaveGateResults(ctx, []store.GateResult{{
		RunID:         boundary.ID,
		CheckpointSHA: strings.Repeat("a", 40),
		Phase:         store.GatePhaseCheckpoint,
		Ordinal:       0,
		GateName:      "test",
		Outcome:       store.GateOutcomePassed,
		Status:        "success",
		Blocking:      true,
		CreatedAt:     cutoff,
		UpdatedAt:     cutoff,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := opened.EnsureEvaluationSummary(ctx, boundary); err != nil {
		t.Fatal(err)
	}
	if err := opened.FinalizeEvaluation(ctx, boundary.ID, store.EvaluationOutcomeComplete, cutoff); err != nil {
		t.Fatal(err)
	}

	candidates, err := opened.ListCleanupCandidates(ctx, now.Add(-7*24*time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Run.ID != boundary.ID {
		t.Fatalf("ListCleanupCandidates() = %#v, want only boundary run", candidates)
	}
	if candidates[0].PendingEffect == nil || len(candidates[0].Invocations) != 1 {
		t.Fatalf("cleanup candidate = %#v, want pending effect and invocation history", candidates[0])
	}

	deleted, err := opened.DeleteCleanupRun(ctx, boundary.ID, now.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Runs != 1 || deleted.Invocations != 1 || deleted.GateResults != 1 || deleted.PendingEffects != 1 {
		t.Fatalf("DeleteCleanupRun() = %#v, want all operational rows removed", deleted)
	}
	if summary, err := opened.EvaluationSummary(ctx, boundary.ID); err != nil {
		t.Fatal(err)
	} else if summary == nil || summary.Outcome != store.EvaluationOutcomeComplete {
		t.Fatalf("EvaluationSummary() = %#v, want retained summary", summary)
	}
	if invocation, err := opened.Invocation(ctx, boundary.ID, "inv-boundary"); err != nil {
		t.Fatal(err)
	} else if invocation != nil {
		t.Fatalf("Invocation() = %#v, want deleted invocation", invocation)
	}

	if _, err := opened.DeleteCleanupRun(ctx, recent.ID, now.Add(-7*24*time.Hour)); err == nil {
		t.Fatal("DeleteCleanupRun() removed a recent terminal run")
	} else {
		var notEligible *store.CleanupNotEligibleError
		if !errors.As(err, &notEligible) {
			t.Fatalf("DeleteCleanupRun() error = %v, want CleanupNotEligibleError", err)
		}
	}
	if repeated, err := opened.DeleteCleanupRun(ctx, boundary.ID, now.Add(-7*24*time.Hour)); err != nil {
		t.Fatal(err)
	} else if repeated.Runs != 0 {
		t.Fatalf("repeated DeleteCleanupRun() = %#v, want idempotent no-op", repeated)
	}
}
