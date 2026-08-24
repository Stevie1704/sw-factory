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
		NativeSessionID:         "session-1",
		WorkspaceID:             "workspace-run",
		StatusSurfaceID:         "surface-status",
		ImplementationSurfaceID: "surface-implementation",
		ChecksSurfaceID:         "surface-checks",
		InvocationDirectory:     "/tmp/invocation",
		ResultDirectory:         "/tmp/results",
		PermittedPaths:          []string{"internal/factory"},
		PromptVersion:           "implementation-v1",
		Status:                  store.InvocationStatusActive,
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
	if got == nil || got.NativeSessionID != want.NativeSessionID || got.ImplementationSurfaceID != want.ImplementationSurfaceID || got.ResultDirectory != want.ResultDirectory || len(got.PermittedPaths) != 1 || got.PermittedPaths[0] != "internal/factory" {
		t.Fatalf("Invocation() = %#v, want %#v", got, want)
	}
	if got.UpdatedAt.UTC() != created {
		t.Fatalf("Invocation().UpdatedAt = %s, want %s", got.UpdatedAt.UTC(), created)
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
