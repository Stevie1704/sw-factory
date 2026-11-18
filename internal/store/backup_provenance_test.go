package store_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// migratingReleaseForTest names the build that migrates the store in these tests.
var migratingReleaseForTest = store.ReleaseIdentity{Version: "v9.9.9", Revision: "0123abc"}

// provenanceForTest is the documented content of <store>.bak-<timestamp>.json.
type provenanceForTest struct {
	SourceSchemaVersion int `json:"source_schema_version"`
	TargetSchemaVersion int `json:"target_schema_version"`
	MigratingRelease    struct {
		Version  string `json:"version"`
		Revision string `json:"revision"`
	} `json:"migrating_release"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// TestPreMigrationBackupIsAVerifiedOnlineSnapshotOfTheOlderSchema verifies that
// the backup of an older store is a private snapshot that passes the integrity
// check and still records the older schema it was taken from.
func TestPreMigrationBackupIsAVerifiedOnlineSnapshotOfTheOlderSchema(t *testing.T) {
	t.Parallel()
	path := storeFixture(t, store.CurrentSchemaVersion-1, "INSERT INTO operational_runs (id, status) VALUES ('run-1', 'complete')")

	backup, err := store.BackupDatabaseForTest(path, migratingReleaseForTest)
	if err != nil {
		t.Fatalf("BackupDatabaseForTest() error = %v", err)
	}
	info, err := os.Stat(backup)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %v, %v; want 0600", info, err)
	}
	inspection, err := store.InspectSnapshot(context.Background(), backup)
	if err != nil {
		t.Fatalf("InspectSnapshot(backup) error = %v", err)
	}
	if inspection.File.SchemaVersion != store.CurrentSchemaVersion-1 {
		t.Fatalf("backup schema = %d, want the older schema %d", inspection.File.SchemaVersion, store.CurrentSchemaVersion-1)
	}
}

// TestPreMigrationBackupWaitsForAnUncommittedWriter verifies that a migration
// while another connection holds an uncommitted write takes its snapshot after
// that writer commits, so the backup passes the integrity check and holds the
// committed row.
func TestPreMigrationBackupWaitsForAnUncommittedWriter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := storeFixture(t, store.CurrentSchemaVersion-1)
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	conn, err := writer.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO operational_runs (id, status) VALUES ('run-open', 'active')"); err != nil {
		t.Fatal(err)
	}
	commitErr := make(chan error, 1)
	go func() {
		time.Sleep(200 * time.Millisecond)
		_, err := conn.ExecContext(context.WithoutCancel(ctx), "COMMIT")
		commitErr <- err
	}()

	backup, err := store.BackupDatabaseForTest(path, migratingReleaseForTest)
	if err != nil {
		t.Fatalf("BackupDatabaseForTest() error = %v", err)
	}
	if err := <-commitErr; err != nil {
		t.Fatalf("writer COMMIT error = %v", err)
	}
	if _, err := store.InspectSnapshot(ctx, backup); err != nil {
		t.Fatalf("InspectSnapshot(backup) error = %v", err)
	}
	reader, err := sql.Open("sqlite", backup)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	var count int
	if err := reader.QueryRowContext(ctx, "SELECT COUNT(*) FROM operational_runs WHERE id = 'run-open'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("backup rows for run-open = %d, %v; want the committed row", count, err)
	}
}

// TestPreMigrationBackupWritesProvenanceBesideTheSnapshot verifies that the
// provenance file names the source and target schema, the migrating release,
// and the SHA-256 and size of the backup bytes.
func TestPreMigrationBackupWritesProvenanceBesideTheSnapshot(t *testing.T) {
	t.Parallel()
	path := storeFixture(t, store.CurrentSchemaVersion-1)

	backup, err := store.BackupDatabaseForTest(path, migratingReleaseForTest)
	if err != nil {
		t.Fatalf("BackupDatabaseForTest() error = %v", err)
	}
	info, err := os.Stat(backup + ".json")
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("provenance mode = %v, %v; want 0600", info, err)
	}
	raw, err := os.ReadFile(backup + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var provenance provenanceForTest
	if err := json.Unmarshal(raw, &provenance); err != nil {
		t.Fatalf("provenance is not JSON: %v", err)
	}
	backupBytes, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(backupBytes)
	if provenance.SourceSchemaVersion != store.CurrentSchemaVersion-1 || provenance.TargetSchemaVersion != store.CurrentSchemaVersion {
		t.Fatalf("provenance schemas = %d -> %d, want %d -> %d", provenance.SourceSchemaVersion, provenance.TargetSchemaVersion, store.CurrentSchemaVersion-1, store.CurrentSchemaVersion)
	}
	if provenance.MigratingRelease.Version != "v9.9.9" || provenance.MigratingRelease.Revision != "0123abc" {
		t.Fatalf("provenance release = %+v, want the migrating release", provenance.MigratingRelease)
	}
	if provenance.SHA256 != hex.EncodeToString(digest[:]) || provenance.Size != int64(len(backupBytes)) {
		t.Fatalf("provenance digest = %s/%d, want %x/%d", provenance.SHA256, provenance.Size, digest, len(backupBytes))
	}
}

// TestInspectForUpgradeListsBackupsButNotTheirProvenance verifies that the
// provenance file of a backup is not counted as a second backup.
func TestInspectForUpgradeListsBackupsButNotTheirProvenance(t *testing.T) {
	t.Parallel()
	path := quiescentStore(t, completedRun("run-1", 7))
	backup := path + ".bak-20260101T000000.000000000Z"
	for _, name := range []string{backup, backup + ".json"} {
		if err := os.WriteFile(name, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	inspection, err := store.InspectForUpgrade(context.Background(), path)
	if err != nil || len(inspection.Backups) != 1 || inspection.Backups[0] != backup {
		t.Fatalf("InspectForUpgrade() backups = %v, %v; want only %s", inspection.Backups, err, backup)
	}
}
