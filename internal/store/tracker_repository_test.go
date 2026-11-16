package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// frozenRepository is a validated Azure Repos identity of one claim.
var frozenRepository = store.TrackerRepository{
	Organization: "contoso",
	ProjectID:    "6f9b0c5e-2a41-4d8e-9a43-1c2f5e7d8a90",
	Project:      "Factory Pilot",
	RepositoryID: "3b1f6a2e-8c4d-4e5f-9a6b-7c8d9e0f1a2b",
	Repository:   "service",
}

// claimedRun returns an active run with the given frozen repository.
func claimedRun(repository *store.TrackerRepository) store.Run {
	return store.Run{
		ID:                "run-azure",
		RepositoryPath:    "/work/service",
		IssueNumber:       101,
		Stage:             store.StageClaim,
		Status:            store.StatusActive,
		TrackerRepository: repository,
		CreatedAt:         time.Unix(100, 0).UTC(),
		UpdatedAt:         time.Unix(100, 0).UTC(),
	}
}

// TestRunPersistsTheFrozenTrackerRepositoryAcrossReopen verifies that the
// repository identity validated at claim survives a restart.
func TestRunPersistsTheFrozenTrackerRepositoryAcrossReopen(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	identity := frozenRepository
	if err := opened.SaveRun(context.Background(), claimedRun(&identity)); err != nil {
		t.Fatalf("SaveRun() error = %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.CurrentRun(context.Background())
	if err != nil || got == nil {
		t.Fatalf("CurrentRun() = %v, %v", got, err)
	}
	if got.TrackerRepository == nil || *got.TrackerRepository != frozenRepository {
		t.Fatalf("TrackerRepository = %+v, want %+v", got.TrackerRepository, frozenRepository)
	}
}

// TestRunWithoutATrackerRepositoryReadsAsNil verifies that GitHub, local
// Markdown, and az rest runs keep no frozen repository.
func TestRunWithoutATrackerRepositoryReadsAsNil(t *testing.T) {
	t.Parallel()

	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	if err := opened.SaveRun(context.Background(), claimedRun(nil)); err != nil {
		t.Fatalf("SaveRun() error = %v", err)
	}
	got, err := opened.CurrentRun(context.Background())
	if err != nil || got == nil || got.TrackerRepository != nil {
		t.Fatalf("CurrentRun() = %+v, %v; want a run without a tracker repository", got, err)
	}
}

// TestFrozenTrackerRepositoryCannotBeRedirected verifies that once a run is
// frozen to a repository, no later save can change or clear it, so a tag
// edit cannot redirect active work; an unchanged save still succeeds.
func TestFrozenTrackerRepositoryCannotBeRedirected(t *testing.T) {
	t.Parallel()

	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	identity := frozenRepository
	run := claimedRun(&identity)
	if err := opened.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun() error = %v", err)
	}
	unchanged := run
	unchanged.Stage = store.StageReview
	unchanged.UpdatedAt = time.Unix(200, 0).UTC()
	if err := opened.SaveRun(context.Background(), unchanged); err != nil {
		t.Fatalf("SaveRun() with the same repository error = %v", err)
	}
	other := frozenRepository
	other.RepositoryID = "9d8c7b6a-5f4e-4d3c-8b2a-1f0e9d8c7b6a"
	other.Repository = "web"
	for name, repository := range map[string]*store.TrackerRepository{"other repository": &other, "cleared": nil} {
		redirected := unchanged
		redirected.TrackerRepository = repository
		if err := opened.SaveRun(context.Background(), redirected); err == nil {
			t.Fatalf("SaveRun() %s error = nil, want a refused redirect", name)
		}
		current, err := opened.CurrentRun(context.Background())
		if err != nil || current.TrackerRepository == nil || *current.TrackerRepository != frozenRepository {
			t.Fatalf("after %s: TrackerRepository = %+v, %v; want it unchanged", name, current.TrackerRepository, err)
		}
	}
}

// TestSaveRunRejectsAnIncompleteTrackerRepository verifies that a frozen
// identity must name every field.
func TestSaveRunRejectsAnIncompleteTrackerRepository(t *testing.T) {
	t.Parallel()

	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	incomplete := frozenRepository
	incomplete.RepositoryID = ""
	if err := opened.SaveRun(context.Background(), claimedRun(&incomplete)); err == nil {
		t.Fatal("SaveRun() error = nil for a tracker repository without an ID")
	}
}
