package store_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestStorePersistsRecoverableInvocationState verifies that surface and native
// session identities survive coordinator restart in the operational store.
func TestStorePersistsRecoverableInvocationState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	created := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	want := store.Invocation{
		ID:                      "inv-1",
		RunID:                   "run-1",
		Harness:                 "codex",
		Role:                    "implementation",
		Stage:                   store.StageImplementation,
		Model:                   "gpt-5",
		ReasoningEffort:         "medium",
		CredentialStoreID:       "/registered/repository",
		NativeSessionID:         "session-1",
		WorkspaceID:             "workspace-run",
		StatusSurfaceID:         "surface-status",
		RoleSurfaceID:           "surface-implementation",
		ImplementationSurfaceID: "surface-implementation",
		ChecksSurfaceID:         "surface-checks",
		InvocationDirectory:     "/tmp/invocation",
		ResultDirectory:         "/tmp/results",
		PermittedPaths:          []string{"internal/factory"},
		PromptVersion:           "implementation-v1",
		PromptCraftSourcePath:   "docs/factory/craft/implementation.md",
		PromptCraftSHA256:       strings.Repeat("a", 64),
		Status:                  store.InvocationStatusActive,
		LaunchVoided:            true,
		CreatedAt:               created,
		UpdatedAt:               created,
	}
	if err := opened.SaveInvocation(context.Background(), want); err != nil {
		t.Fatalf("SaveInvocation() error = %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	reopened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.Invocation(context.Background(), "run-1", "inv-1")
	if err != nil {
		t.Fatalf("Invocation() error = %v", err)
	}
	if got == nil || got.NativeSessionID != want.NativeSessionID || got.CredentialStoreID != want.CredentialStoreID || got.RoleSurfaceID != want.RoleSurfaceID || got.ImplementationSurfaceID != want.ImplementationSurfaceID || got.ResultDirectory != want.ResultDirectory || got.PromptCraftSourcePath != want.PromptCraftSourcePath || got.PromptCraftSHA256 != want.PromptCraftSHA256 || !got.LaunchVoided || len(got.PermittedPaths) != 1 || got.PermittedPaths[0] != "internal/factory" {
		t.Fatalf("Invocation() = %#v, want %#v", got, want)
	}
	if got.UpdatedAt.UTC() != created {
		t.Fatalf("Invocation().UpdatedAt = %s, want %s", got.UpdatedAt.UTC(), created)
	}
}

// TestStoreRejectsConflictingRoleSurfaceIdentities verifies legacy and
// role-neutral surface columns cannot persist contradictory handles.
func TestStoreRejectsConflictingRoleSurfaceIdentities(t *testing.T) {
	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = opened.Close() }()

	err = opened.SaveInvocation(context.Background(), store.Invocation{
		ID:                      "inv-conflicting-surface",
		RunID:                   "run-conflicting-surface",
		Harness:                 "codex",
		Role:                    "architecture",
		Stage:                   store.StageArchitecture,
		RoleSurfaceID:           "surface-role",
		ImplementationSurfaceID: "surface-implementation",
		Status:                  store.InvocationStatusActive,
	})
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("SaveInvocation() error = %v, want conflicting-surface rejection", err)
	}
}

// TestStoreRejectsAnInvocationForTheWrongRun verifies that invocation lookup
// cannot accidentally attach stale state to another active run.
func TestStoreRejectsAnInvocationForTheWrongRun(t *testing.T) {
	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = opened.Close() }()
	if err := opened.SaveInvocation(context.Background(), store.Invocation{
		ID: "inv-1", RunID: "run-1", Harness: "codex", Role: "implementation", Stage: store.StageImplementation,
		Status: store.InvocationStatusActive,
	}); err != nil {
		t.Fatalf("SaveInvocation() error = %v", err)
	}
	got, err := opened.Invocation(context.Background(), "run-2", "inv-1")
	if err != nil {
		t.Fatalf("Invocation() error = %v", err)
	}
	if got != nil {
		t.Fatalf("Invocation() = %#v, want no cross-run result", got)
	}
}

// TestStoreFindsTheActiveInvocation verifies the restart guard's store query
// ignores completed sessions.
func TestStoreFindsOnlyTheNewestActiveInvocation(t *testing.T) {
	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = opened.Close() }()
	created := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	for _, invocation := range []store.Invocation{
		{ID: "inv-complete", RunID: "run-1", Harness: "codex", Role: "implementation", Stage: store.StageImplementation, Status: store.InvocationStatusCompleted, CreatedAt: created, UpdatedAt: created},
		{ID: "inv-active-new", RunID: "run-1", Harness: "codex", Role: "implementation", Stage: store.StageImplementation, Status: store.InvocationStatusActive, CreatedAt: created, UpdatedAt: created.Add(2 * time.Minute)},
	} {
		if err := opened.SaveInvocation(context.Background(), invocation); err != nil {
			t.Fatalf("SaveInvocation(%s) error = %v", invocation.ID, err)
		}
	}
	active, err := opened.ActiveInvocation(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("ActiveInvocation() error = %v", err)
	}
	if active == nil || active.ID != "inv-active-new" {
		t.Fatalf("ActiveInvocation() = %#v, want newest active invocation", active)
	}
}

// TestStorePersistsConcurrentReviewInvocations verifies the store permits one
// active invocation per review role while preserving both identities across a
// reopen.
func TestStorePersistsConcurrentReviewInvocations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	created := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	for _, invocation := range []store.Invocation{
		{ID: "inv-spec", RunID: "run-reviews", Harness: "codex", Role: "spec_review", Stage: store.StageReview, Status: store.InvocationStatusActive, CreatedAt: created, UpdatedAt: created},
		{ID: "inv-standards", RunID: "run-reviews", Harness: "codex", Role: "standards_review", Stage: store.Stage("standards_review"), Status: store.InvocationStatusActive, CreatedAt: created, UpdatedAt: created.Add(time.Minute)},
	} {
		if err := opened.SaveInvocation(context.Background(), invocation); err != nil {
			t.Fatalf("SaveInvocation(%s) error = %v", invocation.ID, err)
		}
	}
	active, err := opened.ActiveInvocations(context.Background(), "run-reviews")
	if err != nil {
		t.Fatalf("ActiveInvocations() error = %v", err)
	}
	if len(active) != 2 || active[0].ID != "inv-spec" || active[1].ID != "inv-standards" {
		t.Fatalf("ActiveInvocations() = %#v, want both review roles in order", active)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	reopened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if err := reopened.SaveInvocation(context.Background(), store.Invocation{
		ID: "inv-spec-duplicate", RunID: "run-reviews", Harness: "codex", Role: "spec_review", Stage: store.StageReview, Status: store.InvocationStatusActive,
	}); err == nil || !strings.Contains(err.Error(), "active invocation already exists") {
		t.Fatalf("duplicate specification invocation error = %v, want role-scoped uniqueness", err)
	}
}

// TestStoreFindsTheLatestInvocationForRepair verifies terminal history lookup
// returns the most recently updated implementation session, not only active
// rows.
func TestStoreFindsTheLatestInvocationForRepair(t *testing.T) {
	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = opened.Close() }()
	created := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	for _, invocation := range []store.Invocation{
		{ID: "inv-old", RunID: "run-repair", Harness: "codex", Role: "implementation", Stage: store.StageImplementation, NativeSessionID: "session-old", Status: store.InvocationStatusCompleted, CreatedAt: created, UpdatedAt: created},
		{ID: "inv-latest", RunID: "run-repair", Harness: "codex", Role: "implementation", Stage: store.StageImplementation, NativeSessionID: "session-latest", Status: store.InvocationStatusCompleted, CreatedAt: created, UpdatedAt: created.Add(time.Minute)},
		{ID: "inv-other", RunID: "other-run", Harness: "codex", Role: "implementation", Stage: store.StageImplementation, NativeSessionID: "session-other", Status: store.InvocationStatusCompleted, CreatedAt: created, UpdatedAt: created.Add(2 * time.Minute)},
	} {
		if err := opened.SaveInvocation(context.Background(), invocation); err != nil {
			t.Fatalf("SaveInvocation(%s) error = %v", invocation.ID, err)
		}
	}
	got, err := opened.LatestInvocation(context.Background(), "run-repair")
	if err != nil {
		t.Fatalf("LatestInvocation() error = %v", err)
	}
	if got == nil || got.ID != "inv-latest" || got.NativeSessionID != "session-latest" {
		t.Fatalf("LatestInvocation() = %#v, want latest terminal implementation session", got)
	}
}

// TestStoreFindsInvocationHistoryRegardlessOfStatus verifies startup can
// distinguish a clean claim from a run that already attempted an invocation.
func TestStoreFindsInvocationHistoryRegardlessOfStatus(t *testing.T) {
	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = opened.Close() }()
	for _, status := range []store.InvocationStatus{
		store.InvocationStatusCompleted,
		store.InvocationStatusWaitingForHuman,
		store.InvocationStatusCannotProceed,
	} {
		if err := opened.SaveInvocation(context.Background(), store.Invocation{
			ID: "inv-" + string(status), RunID: "run-history", Harness: "codex", Role: "implementation", Stage: store.StageClaim, Status: status,
		}); err != nil {
			t.Fatalf("SaveInvocation(%s) error = %v", status, err)
		}
	}
	found, err := opened.HasInvocation(context.Background(), "run-history")
	if err != nil {
		t.Fatalf("HasInvocation() error = %v", err)
	}
	if !found {
		t.Fatal("HasInvocation() = false, want invocation history")
	}
	found, err = opened.HasInvocation(context.Background(), "run-without-history")
	if err != nil {
		t.Fatalf("HasInvocation(empty) error = %v", err)
	}
	if found {
		t.Fatal("HasInvocation(empty) = true, want no invocation history")
	}
}

// TestStoreInvalidatesSupersededResultsRetainsBaseline verifies a packet
// revision supersedes prior invocation work and checkpoint results without
// discarding the independent baseline evaluation.
func TestStoreInvalidatesSupersededResultsRetainsBaseline(t *testing.T) {
	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = opened.Close() }()
	for _, status := range []store.InvocationStatus{
		store.InvocationStatusActive,
		store.InvocationStatusCompleted,
		store.InvocationStatusWaitingForHuman,
		store.InvocationStatusCannotProceed,
	} {
		if err := opened.SaveInvocation(context.Background(), store.Invocation{
			ID: "inv-invalidate-" + string(status), RunID: "run-invalidate", Harness: "codex", Role: "implementation", Stage: store.StageImplementation, Status: status,
		}); err != nil {
			t.Fatalf("SaveInvocation(%s) error = %v", status, err)
		}
	}
	if err := opened.SaveInvocation(context.Background(), store.Invocation{
		ID: "inv-other-run", RunID: "run-other", Harness: "codex", Role: "implementation", Stage: store.StageImplementation, Status: store.InvocationStatusActive,
	}); err != nil {
		t.Fatalf("SaveInvocation(other) error = %v", err)
	}
	baselineSHA := strings.Repeat("a", 40)
	checkpointSHA := strings.Repeat("b", 40)
	if err := opened.SaveGateResults(context.Background(), []store.GateResult{
		{RunID: "run-invalidate", CheckpointSHA: baselineSHA, Phase: store.GatePhaseBaseline, Ordinal: 0, GateName: "baseline", Outcome: store.GateOutcomePassed, Status: "success"},
		{RunID: "run-invalidate", CheckpointSHA: checkpointSHA, Phase: store.GatePhaseCheckpoint, Ordinal: 0, GateName: "checkpoint", Outcome: store.GateOutcomePassed, Status: "success"},
	}); err != nil {
		t.Fatalf("SaveGateResults() error = %v", err)
	}
	if err := opened.InvalidateRunResults(context.Background(), "run-invalidate"); err != nil {
		t.Fatalf("InvalidateRunResults() error = %v", err)
	}
	for _, status := range []store.InvocationStatus{
		store.InvocationStatusActive,
		store.InvocationStatusCompleted,
		store.InvocationStatusWaitingForHuman,
	} {
		got, err := opened.Invocation(context.Background(), "run-invalidate", "inv-invalidate-"+string(status))
		if err != nil {
			t.Fatalf("Invocation(%s) error = %v", status, err)
		}
		if got == nil || got.Status != store.InvocationStatusSuperseded {
			t.Fatalf("Invocation(%s) = %#v, want superseded", status, got)
		}
	}
	unchanged, err := opened.Invocation(context.Background(), "run-invalidate", "inv-invalidate-"+string(store.InvocationStatusCannotProceed))
	if err != nil {
		t.Fatal(err)
	}
	if unchanged == nil || unchanged.Status != store.InvocationStatusSuperseded {
		t.Fatalf("cannot-proceed invocation = %#v, want superseded", unchanged)
	}
	other, err := opened.Invocation(context.Background(), "run-other", "inv-other-run")
	if err != nil {
		t.Fatal(err)
	}
	if other == nil || other.Status != store.InvocationStatusActive {
		t.Fatalf("other-run invocation = %#v, want unchanged active", other)
	}
	baseline, err := opened.GateResults(context.Background(), "run-invalidate", store.GatePhaseBaseline, baselineSHA)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := opened.GateResults(context.Background(), "run-invalidate", store.GatePhaseCheckpoint, checkpointSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(baseline) != 1 || len(checkpoint) != 0 {
		t.Fatalf("remaining gate results = baseline=%#v checkpoint=%#v, want baseline only", baseline, checkpoint)
	}
}

// TestStoreRejectsTwoActiveInvocationsForOneRun verifies the database index,
// rather than only the coordinator lookup, owns active-invocation uniqueness.
func TestStoreRejectsTwoActiveInvocationsForOneRun(t *testing.T) {
	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = opened.Close() }()
	base := store.Invocation{RunID: "run-unique", Harness: "codex", Role: "implementation", Stage: store.StageImplementation, Status: store.InvocationStatusActive}
	base.ID = "inv-first"
	if err := opened.SaveInvocation(context.Background(), base); err != nil {
		t.Fatalf("SaveInvocation(first) error = %v", err)
	}
	base.ID = "inv-second"
	if err := opened.SaveInvocation(context.Background(), base); err == nil || !strings.Contains(err.Error(), "active invocation already exists") {
		t.Fatalf("SaveInvocation(second) error = %v, want authoritative active-uniqueness error", err)
	}
}
