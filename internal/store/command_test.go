package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestRunPersistsTheProcessedCommandWatermarkAcrossRestart verifies that a
// coordinator restart cannot make an already processed comment eligible again.
func TestRunPersistsTheProcessedCommandWatermarkAcrossRestart(t *testing.T) {
	t.Parallel()

	databasePath := filepath.Join(t.TempDir(), "factory.db")
	if err := os.Chmod(filepath.Dir(databasePath), 0o700); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	createdAt := time.Date(2026, 8, 23, 10, 11, 12, 0, time.UTC)
	run := store.Run{
		ID:                       "run-command",
		RepositoryPath:           "/repo",
		IssueNumber:              8,
		Stage:                    store.StageImplementation,
		Status:                   store.StatusActive,
		Revision:                 4,
		ProcessedCommentID:       "comment-42",
		ProcessedCommentRevision: 4,
		LastCommandName:          "status",
		LastCommandOutcome:       "accepted",
		LastCommandMessage:       "status refreshed",
		HarnessOverride:          "codex",
		CreatedAt:                createdAt,
		UpdatedAt:                createdAt,
	}

	opened, err := store.Open(context.Background(), databasePath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := opened.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun() error = %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	restarted, err := store.Open(context.Background(), databasePath)
	if err != nil {
		t.Fatalf("Open() after restart error = %v", err)
	}
	defer func() { _ = restarted.Close() }()
	latest, err := restarted.LatestRun(context.Background())
	if err != nil {
		t.Fatalf("LatestRun() error = %v", err)
	}
	if latest == nil {
		t.Fatal("LatestRun() = nil, want persisted run")
	}
	if latest.Revision != run.Revision || latest.ProcessedCommentID != run.ProcessedCommentID || latest.ProcessedCommentRevision != run.ProcessedCommentRevision {
		t.Fatalf("watermark = revision %d/comment %q/comment revision %d, want %d/%q/%d", latest.Revision, latest.ProcessedCommentID, latest.ProcessedCommentRevision, run.Revision, run.ProcessedCommentID, run.ProcessedCommentRevision)
	}
	if latest.LastCommandName != run.LastCommandName || latest.LastCommandOutcome != run.LastCommandOutcome || latest.LastCommandMessage != run.LastCommandMessage || latest.HarnessOverride != run.HarnessOverride {
		t.Fatalf("command state = %#v, want %#v", latest, run)
	}
}

// TestSaveRunIfRevisionRejectsAStaleCommand verifies the SQLite compare-and-set
// refuses a second coordinator that read an older run revision.
func TestSaveRunIfRevisionRejectsAStaleCommand(t *testing.T) {
	t.Parallel()

	databasePath := filepath.Join(t.TempDir(), "factory.db")
	if err := os.Chmod(filepath.Dir(databasePath), 0o700); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	opened, err := store.Open(context.Background(), databasePath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = opened.Close() }()
	initial := store.Run{ID: "run-cas", RepositoryPath: "/repo", Stage: store.StageImplementation, Status: store.StatusActive, Revision: 2}
	if err := opened.SaveRun(context.Background(), initial); err != nil {
		t.Fatalf("SaveRun() error = %v", err)
	}
	stale := initial
	stale.Revision = 3
	stale.ProcessedCommentID = "comment-stale"
	if err := opened.SaveRunIfRevision(context.Background(), 1, stale); !errors.Is(err, store.ErrRevisionConflict) {
		t.Fatalf("SaveRunIfRevision() error = %v, want ErrRevisionConflict", err)
	}
	if err := opened.SaveRunIfRevision(context.Background(), 2, stale); err != nil {
		t.Fatalf("SaveRunIfRevision() current error = %v", err)
	}
}
