package store_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// storeFixture writes a reduced store with the given schema version and the
// tables that upgrade inspection reads, and returns its path.
func storeFixture(t *testing.T, version int, statements ...string) string {
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
		"CREATE TABLE ideation_invocations (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, status TEXT NOT NULL)",
		"CREATE TABLE ideation_commands (sequence INTEGER PRIMARY KEY AUTOINCREMENT, state TEXT NOT NULL)",
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

// TestInspectForUpgradeReadsActiveWorkWithoutChange verifies the read-only
// path: a current store reports each kind of work that fails the migration
// gate and its migration backups, and the file does not change.
func TestInspectForUpgradeReadsActiveWorkWithoutChange(t *testing.T) {
	t.Parallel()
	path := storeFixture(t, store.CurrentSchemaVersion,
		"INSERT INTO operational_runs (id, status) VALUES ('run-done', 'complete'), ('run-open', 'waiting_for_human')",
		"INSERT INTO invocations (id, run_id, status) VALUES ('inv-1', 'run-open', 'active')",
		"INSERT INTO pending_effects (run_id, effect_id) VALUES ('run-open', 'effect-1')",
		"INSERT INTO ideation_invocations (id, session_id, status) VALUES ('session-inv-1', 'session-1', 'active'), ('session-inv-0', 'session-0', 'accepted')",
		"INSERT INTO ideation_commands (state) VALUES ('accepted'), ('completed')",
	)
	if err := os.WriteFile(path+".bak-20260101T000000.000000000Z", []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := fileState(t, path)

	inspection, err := store.InspectForUpgrade(context.Background(), path)
	if err != nil {
		t.Fatalf("InspectForUpgrade() error = %v", err)
	}
	if !inspection.Supported() || !slices.Equal(inspection.ActiveRuns, []string{"run-open"}) {
		t.Fatalf("inspection = %+v, want a supported store with run-open active", inspection)
	}
	var codes []string
	for _, finding := range inspection.ActivityFindings() {
		codes = append(codes, finding.Code)
	}
	if !slices.Equal(codes, []string{"run.active", "invocation.active", "effect.pending", "session_invocation.active", "session_command.queued"}) {
		t.Fatalf("activity findings = %v, want every kind of active work", codes)
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

// TestInspectForUpgradeReportsSchemaLimits verifies that a newer and an
// older schema are reported as unsupported without reading run tables, and
// that a missing store is not an error.
func TestInspectForUpgradeReportsSchemaLimits(t *testing.T) {
	t.Parallel()
	newer, err := store.InspectForUpgrade(context.Background(), storeFixture(t, store.CurrentSchemaVersion+1))
	if err != nil || newer.Supported() || newer.SchemaVersion != store.CurrentSchemaVersion+1 {
		t.Fatalf("newer inspection = %+v, %v; want an unsupported newer schema", newer, err)
	}
	older, err := store.InspectForUpgrade(context.Background(), storeFixture(t, store.CurrentSchemaVersion-1))
	if err != nil || older.Supported() || older.SchemaFinding() == nil || older.SchemaFinding().Code != "store.schema_unsupported" {
		t.Fatalf("older inspection = %+v, %v; want store.schema_unsupported", older, err)
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

// TestMigrateLeavesACurrentStoreUnchanged verifies that the explicit
// migration entry point is a typed no-op on a current store.
func TestMigrateLeavesACurrentStoreUnchanged(t *testing.T) {
	t.Parallel()
	path := quiescentStore(t, completedRun("run-1", 7))
	before := fileState(t, path)

	migration, err := store.Migrate(context.Background(), path)
	if err != nil || migration != (store.Migration{FromVersion: store.CurrentSchemaVersion, ToVersion: store.CurrentSchemaVersion}) {
		t.Fatalf("Migrate() = %+v, %v; want a no-op at the current schema", migration, err)
	}
	if fileState(t, path) != before {
		t.Fatal("Migrate() changed a current store")
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

// TestOpenRefusesAnOlderStoreAndNamesTheMigrationEntryPoint verifies that
// the ordinary opener never migrates an existing store: it refuses an older
// schema with a typed error, writes no backup, and leaves the schema.
func TestOpenRefusesAnOlderStoreAndNamesTheMigrationEntryPoint(t *testing.T) {
	t.Parallel()
	path := quiescentStore(t, completedRun("run-1", 7))
	database := openRaw(t, path)
	if _, err := database.Exec("UPDATE schema_metadata SET version = ?", store.CurrentSchemaVersion-1); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := store.Open(context.Background(), path)
	var required *store.MigrationRequiredError
	if !errors.As(err, &required) || required.Version != store.CurrentSchemaVersion-1 || !strings.Contains(err.Error(), "factory upgrade migrate-store") {
		t.Fatalf("Open(older store) error = %v, want a MigrationRequiredError that names factory upgrade migrate-store", err)
	}
	inspection, inspectErr := store.InspectForUpgrade(context.Background(), path)
	if inspectErr != nil || inspection.SchemaVersion != store.CurrentSchemaVersion-1 || len(inspection.Backups) != 0 {
		t.Fatalf("store after refusal = %+v, %v; want the older schema and no backup", inspection, inspectErr)
	}
}

// openMigrated migrates an older store fixture through the explicit
// migration entry point, then opens it as every other caller does.
func openMigrated(ctx context.Context, path string) (*store.Store, error) {
	if _, err := store.Migrate(ctx, path); err != nil {
		return nil, err
	}
	return store.Open(ctx, path)
}
