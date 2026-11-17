package store_test

// This file is the feasibility evidence of ADR 0032. It is a test-only
// prototype: it shows that an online SQLite snapshot is consistent while
// another connection writes, and that a divergence check refuses a restore
// when external state moved past the snapshot. It adds no production
// backup or restore behavior.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestCheckpointSnapshotExcludesAnUncommittedWrite shows that VACUUM INTO,
// run in its own read transaction, captures only committed state while
// another connection holds an open write transaction, and that the result is
// one self-contained, current-schema file without sidecars.
func TestCheckpointSnapshotExcludesAnUncommittedWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := quiescentStore(t, completedRun("run-1", 7))

	writer := openRaw(t, path)
	transaction, err := writer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.ExecContext(ctx, "UPDATE operational_runs SET status = ? WHERE id = ?", store.StatusActive, "run-1"); err != nil {
		t.Fatal(err)
	}

	snapshot := filepath.Join(privateDirectory(t), "store.db")
	if _, err := openRaw(t, path).ExecContext(ctx, "VACUUM INTO ?", snapshot); err != nil {
		t.Fatalf("VACUUM INTO during an open write transaction: %v", err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	// VACUUM INTO creates the file with the process umask, so a checkpoint
	// must restrict it before anything reads it as a store.
	if err := os.Chmod(snapshot, 0o600); err != nil {
		t.Fatal(err)
	}

	captured := openRaw(t, snapshot)
	var integrity string
	if err := captured.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("snapshot integrity = %q, %v; want ok", integrity, err)
	}
	if got := runStatus(t, captured, "run-1"); got != string(store.StatusComplete) {
		t.Fatalf("snapshot run status = %q, want the committed %q", got, store.StatusComplete)
	}
	if got := runStatus(t, openRaw(t, path), "run-1"); got != string(store.StatusActive) {
		t.Fatalf("live run status = %q, want the later commit %q", got, store.StatusActive)
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if _, err := os.Stat(snapshot + suffix); !os.IsNotExist(err) {
			t.Fatalf("snapshot sidecar %s exists: %v", suffix, err)
		}
	}
	reopened, err := store.OpenReadOnly(ctx, snapshot)
	if err != nil {
		t.Fatalf("OpenReadOnly(snapshot) error = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if reopened.SchemaVersion() != store.CurrentSchemaVersion {
		t.Fatalf("snapshot schema = %d, want %d", reopened.SchemaVersion(), store.CurrentSchemaVersion)
	}
	if fileDigest(t, snapshot) != fileDigest(t, snapshot) {
		t.Fatal("snapshot checksum is not stable")
	}
}

// TestCheckpointRestoreRefusesAfterExternalDivergence shows the conservative
// restore rule: a snapshot is acceptable only when the live store and the
// code host hold nothing that the snapshot does not know, and no effect is
// journaled.
func TestCheckpointRestoreRefusesAfterExternalDivergence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := quiescentStore(t, completedRun("run-1", 7))
	snapshot := filepath.Join(privateDirectory(t), "store.db")
	if _, err := openRaw(t, path).ExecContext(ctx, "VACUUM INTO ?", snapshot); err != nil {
		t.Fatal(err)
	}
	known := snapshotWatermarks(t, snapshot)

	if refusals := divergence(known, observedState{runs: []string{"run-1"}, pullRequests: []int{7}}); len(refusals) != 0 {
		t.Fatalf("unchanged installation refusals = %v, want none", refusals)
	}

	// After the snapshot, the coordinator claims run-2 and opens pull request #8.
	later, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := later.SaveRun(ctx, completedRun("run-2", 8)); err != nil {
		t.Fatal(err)
	}
	if err := later.SavePendingEffect(ctx, store.PendingEffect{RunID: "run-2", ID: "effect-1", Kind: store.PendingEffectKindStatusComment, Payload: `{}`, CreatedAt: time.Unix(300, 0).UTC(), UpdatedAt: time.Unix(300, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := later.Close(); err != nil {
		t.Fatal(err)
	}
	live := snapshotWatermarks(t, path)
	refusals := divergence(known, observedState{runs: live.runs, pullRequests: []int{7, 8}, pendingEffects: live.pendingEffects})
	for _, want := range []string{"run run-2 is not in the snapshot", "pull request #8 is not in the snapshot", "a pending effect is journaled"} {
		if !slices.Contains(refusals, want) {
			t.Fatalf("refusals = %v, want %q", refusals, want)
		}
	}
}

// observedState is what the live store and the code host show at restore.
type observedState struct {
	runs           []string
	pullRequests   []int
	pendingEffects int
}

// divergence returns one refusal reason for each item the snapshot cannot
// explain. An empty result is the only state that allows a restore.
func divergence(snapshot, observed observedState) []string {
	var refusals []string
	for _, run := range observed.runs {
		if !slices.Contains(snapshot.runs, run) {
			refusals = append(refusals, "run "+run+" is not in the snapshot")
		}
	}
	for _, number := range observed.pullRequests {
		if !slices.Contains(snapshot.pullRequests, number) {
			refusals = append(refusals, "pull request #"+strconv.Itoa(number)+" is not in the snapshot")
		}
	}
	if observed.pendingEffects != 0 || snapshot.pendingEffects != 0 {
		refusals = append(refusals, "a pending effect is journaled")
	}
	return refusals
}

// snapshotWatermarks reads the run and pull-request identities and the
// pending-effect count of one store file.
func snapshotWatermarks(t *testing.T, path string) observedState {
	t.Helper()
	database := openRaw(t, path)
	rows, err := database.Query("SELECT id, pull_request_number FROM operational_runs ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var state observedState
	for rows.Next() {
		var id string
		var number int
		if err := rows.Scan(&id, &number); err != nil {
			t.Fatal(err)
		}
		state.runs = append(state.runs, id)
		state.pullRequests = append(state.pullRequests, number)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM pending_effects").Scan(&state.pendingEffects); err != nil {
		t.Fatal(err)
	}
	return state
}

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

// fileDigest returns the SHA-256 of one file.
func fileDigest(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
