package store_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestSnapshotRollsBackAHotJournal verifies step 2 of the consistent
// snapshot in ADR 0032: a hot rollback journal from an interrupted writer
// does not refuse the snapshot. The read-write open without migration rolls
// it back, and the snapshot holds the committed state, not the interrupted
// write.
func TestSnapshotRollsBackAHotJournal(t *testing.T) {
	t.Parallel()
	path := hotJournalStore(t)
	destination := filepath.Join(privateDirectory(t), "store.db")

	if _, err := store.Snapshot(context.Background(), path, destination, nil); err != nil {
		t.Fatalf("Snapshot() error = %v, want the hot journal rolled back", err)
	}
	if _, err := os.Stat(path + "-journal"); !os.IsNotExist(err) {
		t.Fatalf("the hot journal remains after the snapshot: %v", err)
	}
	if got := runStatus(t, openRaw(t, destination), "run-1"); got != string(store.StatusComplete) {
		t.Fatalf("snapshot run status = %q, want the committed %q", got, store.StatusComplete)
	}
}

// hotJournalStore returns a store file with a hot rollback journal: the
// crash image of a writer that stopped inside a transaction. It copies the
// store and its -journal while a transaction that spilled its pages to the
// file is open, then rolls the original back.
func hotJournalStore(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	source := quiescentStore(t, completedRun("run-1", 7))
	database, err := sql.Open("sqlite", source+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(delete)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	conn, err := database.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	for _, statement := range []string{
		"PRAGMA cache_size = 2",
		"BEGIN IMMEDIATE",
		"UPDATE operational_runs SET status = 'active' WHERE id = 'run-1'",
		"CREATE TABLE interrupted_write(payload BLOB)",
		"INSERT INTO interrupted_write SELECT randomblob(4096) FROM (WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 200) SELECT i FROM n)",
	} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	image := filepath.Join(privateDirectory(t), "factory.db")
	for _, suffix := range []string{"", "-journal"} {
		content, err := os.ReadFile(source + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(image+suffix, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	return image
}
