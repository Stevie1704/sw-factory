package store_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// quiescentStore creates a current store that holds only the given terminal
// run.
func quiescentStore(t *testing.T, run store.Run) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun() error = %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// completedRun returns a terminal run with one pull request.
func completedRun(id string, pullRequest int) store.Run {
	run := claimedRun(nil)
	run.ID = id
	run.IssueNumber = pullRequest + 100
	run.Stage = store.StageReady
	run.Status = store.StatusComplete
	run.PullRequestNumber = pullRequest
	run.PullRequestURL = "https://github.com/example/service/pull/" + strconv.Itoa(pullRequest)
	return run
}

// privateDirectory returns a new 0700 directory, as a checkpoint needs.
func privateDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

// openRaw opens a SQLite file outside the store API and closes it at the end
// of the test.
func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

// runStatus reads the status of one run.
func runStatus(t *testing.T, database *sql.DB, id string) string {
	t.Helper()
	var status string
	if err := database.QueryRow("SELECT status FROM operational_runs WHERE id = ?", id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}
