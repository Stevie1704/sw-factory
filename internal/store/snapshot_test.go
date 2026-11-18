package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestSnapshotHoldsTheCheckedStateWhileAWriterWaits verifies the online
// snapshot sequence of ADR 0032: the check runs on the connection that holds
// BEGIN IMMEDIATE, no other writer can commit before the snapshot, and the
// snapshot is a private, verified copy of the checked state.
func TestSnapshotHoldsTheCheckedStateWhileAWriterWaits(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"delete", "wal"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			path := quiescentStore(t, completedRun("run-1", 7))
			if _, err := openRaw(t, path).ExecContext(ctx, "PRAGMA journal_mode = "+mode); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(privateDirectory(t), "store.db")
			writerFailed := false
			check := func(ctx context.Context, view store.Querier) error {
				var status string
				if err := view.QueryRowContext(ctx, "SELECT status FROM operational_runs WHERE id = 'run-1'").Scan(&status); err != nil {
					return err
				}
				writer, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(100)")
				if err != nil {
					return err
				}
				defer func() { _ = writer.Close() }()
				_, err = writer.ExecContext(ctx, "UPDATE operational_runs SET status = ? WHERE id = 'run-1'", store.StatusActive)
				writerFailed = err != nil
				return nil
			}

			snapshot, err := store.Snapshot(ctx, path, destination, check)
			if err != nil {
				t.Fatalf("Snapshot() error = %v", err)
			}
			if !writerFailed {
				t.Fatal("a concurrent writer committed while the snapshot held the write lock")
			}
			info, err := os.Stat(destination)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("snapshot mode = %v, %v; want 0600", info, err)
			}
			if got := runStatus(t, openRaw(t, destination), "run-1"); got != string(store.StatusComplete) {
				t.Fatalf("snapshot run status = %q, want the checked %q", got, store.StatusComplete)
			}
			content, err := os.ReadFile(destination)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(content)
			if snapshot.File.SHA256 != hex.EncodeToString(sum[:]) || snapshot.File.Size != int64(len(content)) || snapshot.File.SchemaVersion != store.CurrentSchemaVersion {
				t.Fatalf("snapshot file = %+v, want the SHA-256, size, and schema of %s", snapshot.File, destination)
			}
		})
	}
}

// TestSnapshotRefusalWritesNothing verifies that a check that refuses ends
// the snapshot before VACUUM INTO, so no destination file exists.
func TestSnapshotRefusalWritesNothing(t *testing.T) {
	t.Parallel()
	path := quiescentStore(t, completedRun("run-1", 7))
	destination := filepath.Join(privateDirectory(t), "store.db")
	refused := errors.New("not quiescent")

	_, err := store.Snapshot(context.Background(), path, destination, func(context.Context, store.Querier) error { return refused })
	if !errors.Is(err, refused) {
		t.Fatalf("Snapshot() error = %v, want the check refusal", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("destination exists after a refusal: %v", err)
	}
}

// TestSnapshotRefusesAnOlderSchemaWithoutChangingTheStore verifies that a
// snapshot never migrates and never opens an older store read-write.
func TestSnapshotRefusesAnOlderSchemaWithoutChangingTheStore(t *testing.T) {
	t.Parallel()
	path := quiescentStore(t, completedRun("run-1", 7))
	if _, err := openRaw(t, path).Exec("UPDATE schema_metadata SET version = ?", store.CurrentSchemaVersion-1); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(privateDirectory(t), "store.db")

	_, err = store.Snapshot(context.Background(), path, destination, nil)
	var migration *store.MigrationRequiredError
	if !errors.As(err, &migration) {
		t.Fatalf("Snapshot() error = %v, want a MigrationRequiredError", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("the store bytes changed: %v", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("destination exists after a refusal: %v", err)
	}
}

// TestSnapshotRemovesAPartialFileWhenTheDiskIsFull verifies that a failed
// VACUUM INTO, such as a full disk, leaves no destination file and does not
// change the store.
func TestSnapshotRemovesAPartialFileWhenTheDiskIsFull(t *testing.T) {
	t.Parallel()
	path := quiescentStore(t, completedRun("run-1", 7))
	destination := filepath.Join(privateDirectory(t), "store.db")
	full := errors.New("database or disk is full")
	vacuum := func(context.Context, *sql.Conn, string) error {
		if err := os.WriteFile(destination, []byte("partial"), 0o644); err != nil {
			return err
		}
		return full
	}

	_, err := store.SnapshotWithVacuumForTest(context.Background(), path, destination, nil, vacuum)
	if !errors.Is(err, full) {
		t.Fatalf("Snapshot() error = %v, want the disk-full error", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("a partial snapshot remains: %v", err)
	}
	if got := runStatus(t, openRaw(t, path), "run-1"); got != string(store.StatusComplete) {
		t.Fatalf("store run status = %q after a failed snapshot", got)
	}
}

// TestSnapshotExcludesAnInboxAppendThatWaitsForTheLock verifies that a
// session command appended during the snapshot waits for the write lock and
// commits after it, so the snapshot holds the checked, empty inbox.
func TestSnapshotExcludesAnInboxAppendThatWaitsForTheLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := quiescentStore(t, completedRun("run-1", 7))
	destination := filepath.Join(privateDirectory(t), "store.db")
	appended := make(chan error, 1)
	check := func(ctx context.Context, view store.Querier) error {
		go func() {
			inbox, err := store.OpenCommandInbox(ctx, path)
			if err != nil {
				appended <- err
				return
			}
			defer func() { _ = inbox.Close() }()
			_, _, err = inbox.AcceptIdeationCommand(ctx, commandForTest("request-1", "session-1", 1))
			appended <- err
		}()
		// Give the append time to reach BEGIN IMMEDIATE and wait for the lock.
		time.Sleep(200 * time.Millisecond)
		return nil
	}

	if _, err := store.Snapshot(ctx, path, destination, check); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if err := <-appended; err != nil {
		t.Fatalf("the waiting inbox append failed: %v", err)
	}
	if got := commandCount(t, openRaw(t, destination)); got != 0 {
		t.Fatalf("snapshot holds %d session commands, want the checked 0", got)
	}
	if got := commandCount(t, openRaw(t, path)); got != 1 {
		t.Fatalf("store holds %d session commands after the snapshot, want 1", got)
	}
}

// commandCount counts the session commands of one store file.
func commandCount(t *testing.T, database *sql.DB) int {
	t.Helper()
	var count int
	if err := database.QueryRow("SELECT COUNT(*) FROM ideation_commands").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
