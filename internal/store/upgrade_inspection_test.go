package store_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// olderStoreFixture writes a reduced store of an older supported schema with
// the tables that upgrade inspection reads, and returns its path.
func olderStoreFixture(t *testing.T, version int, statements ...string) string {
	t.Helper()
	path := filepath.Join(privateDirectory(t), "factory.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	schema := []string{
		"CREATE TABLE schema_metadata (singleton INTEGER PRIMARY KEY CHECK (singleton = 1), version INTEGER NOT NULL)",
		"CREATE TABLE operational_runs (id TEXT PRIMARY KEY, status TEXT NOT NULL)",
		"CREATE TABLE invocations (id TEXT PRIMARY KEY, run_id TEXT NOT NULL, status TEXT NOT NULL)",
		"CREATE TABLE pending_effects (run_id TEXT PRIMARY KEY, effect_id TEXT NOT NULL)",
	}
	for _, statement := range append(schema, statements...) {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if _, err := database.Exec("INSERT INTO schema_metadata (singleton, version) VALUES (1, ?)", version); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fileState returns the content digest and modification time of a file, so
// a test can prove that an inspection wrote nothing.
func fileState(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return info.ModTime().String() + string(sum[:])
}

// TestInspectForUpgradeReadsAnOlderStoreWithoutMigration verifies the narrow
// read-only path: an older supported schema reports its version, its active
// work, and its migration backups, and the file does not change.
func TestInspectForUpgradeReadsAnOlderStoreWithoutMigration(t *testing.T) {
	t.Parallel()
	path := olderStoreFixture(t, 40,
		"INSERT INTO operational_runs (id, status) VALUES ('run-done', 'complete'), ('run-open', 'waiting_for_human')",
		"INSERT INTO invocations (id, run_id, status) VALUES ('inv-1', 'run-open', 'active')",
		"INSERT INTO pending_effects (run_id, effect_id) VALUES ('run-open', 'effect-1')",
	)
	if err := os.WriteFile(path+".bak-20260101T000000.000000000Z", []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := fileState(t, path)

	inspection, err := store.InspectForUpgrade(context.Background(), path)
	if err != nil {
		t.Fatalf("InspectForUpgrade() error = %v", err)
	}
	if !inspection.Exists || inspection.SchemaVersion != 40 || !inspection.Supported() {
		t.Fatalf("inspection = %+v, want an existing, supported schema 40", inspection)
	}
	if !slices.Equal(inspection.ActiveRuns, []string{"run-open"}) || inspection.ActiveInvocations != 1 || inspection.PendingEffects != 1 {
		t.Fatalf("active work = %v runs, %d invocations, %d effects; want run-open, 1, 1", inspection.ActiveRuns, inspection.ActiveInvocations, inspection.PendingEffects)
	}
	if len(inspection.Backups) != 1 || filepath.Base(inspection.Backups[0]) != "factory.db.bak-20260101T000000.000000000Z" {
		t.Fatalf("backups = %v, want the one migration backup", inspection.Backups)
	}
	if after := fileState(t, path); after != before {
		t.Fatal("inspection changed the store file")
	}
	if _, err := os.Stat(path + "-journal"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspection left a journal: %v", err)
	}
}

// TestInspectForUpgradeReportsSchemaLimits verifies that a newer schema and
// a schema below the inspectable minimum are reported without reading run
// tables, and that a missing store is not an error.
func TestInspectForUpgradeReportsSchemaLimits(t *testing.T) {
	t.Parallel()
	newer, err := store.InspectForUpgrade(context.Background(), olderStoreFixture(t, store.CurrentSchemaVersion+1))
	if err != nil || newer.Supported() || newer.SchemaVersion != store.CurrentSchemaVersion+1 {
		t.Fatalf("newer inspection = %+v, %v; want an unsupported newer schema", newer, err)
	}
	tooOld, err := store.InspectForUpgrade(context.Background(), olderStoreFixture(t, store.MinimumUpgradeSchemaVersion-1))
	if err != nil || tooOld.Supported() {
		t.Fatalf("old inspection = %+v, %v; want an unsupported old schema", tooOld, err)
	}
	missing, err := store.InspectForUpgrade(context.Background(), filepath.Join(privateDirectory(t), "factory.db"))
	if err != nil || missing.Exists {
		t.Fatalf("missing inspection = %+v, %v; want absence", missing, err)
	}
}

// TestInspectForUpgradeReportsAnInterruptedWrite verifies that a rollback
// journal beside the store is reported, because a read-only reader cannot
// roll it back.
func TestInspectForUpgradeReportsAnInterruptedWrite(t *testing.T) {
	t.Parallel()
	path := quiescentStore(t, completedRun("run-1", 7))
	if err := os.WriteFile(path+"-journal", []byte("hot"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspection, err := store.InspectForUpgrade(context.Background(), path)
	if err != nil {
		t.Fatalf("InspectForUpgrade() error = %v", err)
	}
	if !inspection.InterruptedWrite {
		t.Fatalf("inspection = %+v, want an interrupted write", inspection)
	}
}

// TestMigrateReportsTheBackupProvenance verifies the explicit migration entry
// point: it migrates an older store, keeps the automatic backup, and names
// it; a current store is left unchanged.
func TestMigrateReportsTheBackupProvenance(t *testing.T) {
	t.Parallel()
	path := quiescentStore(t, completedRun("run-1", 7))
	database := openRaw(t, path)
	if _, err := database.Exec("UPDATE schema_metadata SET version = 51"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	migration, err := store.Migrate(context.Background(), path)
	if err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if migration.FromVersion != 51 || migration.ToVersion != store.CurrentSchemaVersion || migration.Backup == "" {
		t.Fatalf("migration = %+v, want 51 to current with a backup", migration)
	}
	if _, err := os.Stat(migration.Backup); err != nil {
		t.Fatalf("backup %s: %v", migration.Backup, err)
	}

	again, err := store.Migrate(context.Background(), path)
	if err != nil || again.FromVersion != store.CurrentSchemaVersion || again.Backup != "" {
		t.Fatalf("second Migrate() = %+v, %v; want no change and no backup", again, err)
	}
}

// TestRollBackInterruptedWriteClearsTheJournal verifies that the read-write
// recovery open resolves a rollback journal without migrating, so a later
// read-only inspection can read the store again.
func TestRollBackInterruptedWriteClearsTheJournal(t *testing.T) {
	t.Parallel()
	path := quiescentStore(t, completedRun("run-1", 7))
	if err := os.WriteFile(path+"-journal", []byte("interrupted transaction"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.RollBackInterruptedWrite(context.Background(), path); err != nil {
		t.Fatalf("RollBackInterruptedWrite() error = %v", err)
	}
	inspection, err := store.InspectForUpgrade(context.Background(), path)
	if err != nil || inspection.InterruptedWrite || inspection.SchemaVersion != store.CurrentSchemaVersion {
		t.Fatalf("inspection after rollback = %+v, %v; want a readable current store", inspection, err)
	}
}
