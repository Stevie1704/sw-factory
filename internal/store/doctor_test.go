package store_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/store"
	_ "modernc.org/sqlite"
)

// TestStartupCheckReadsTheVersionedOperationalStore verifies SQLite diagnosis
// delegates schema and permission ownership to the read-only store opener.
func TestStartupCheckReadsTheVersionedOperationalStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "factory.db")
	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("Open().Close() error = %v", err)
	}
	result := doctor.Run(context.Background(), store.StartupCheck(path)).Results[0]
	if result.Status != doctor.StatusPassed {
		t.Fatalf("SQLite result = %#v, want passed", result)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("diagnosed store disappeared: %v", err)
	}
}

// TestStartupCheckDoesNotCreateOrMigrateTheOperationalStore verifies diagnosis
// remains observational and leaves missing store state for normal startup.
func TestStartupCheckDoesNotCreateOrMigrateTheOperationalStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "factory.db")
	result := doctor.Run(context.Background(), store.StartupCheck(path)).Results[0]
	if result.Status != doctor.StatusFailed {
		t.Fatalf("SQLite result = %#v, want failed for a missing store", result)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("diagnosis created an operational store; stat error = %v", err)
	}
}

// TestStartupCheckDoesNotCreateAMigrationBackup verifies an older store is
// reported for normal migration without being changed by diagnosis.
func TestStartupCheckDoesNotCreateAMigrationBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "factory.db")
	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("Open().Close() error = %v", err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(context.Background(), "UPDATE schema_metadata SET version = 0 WHERE singleton = 1"); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	result := doctor.Run(context.Background(), store.StartupCheck(path)).Results[0]
	if result.Status != doctor.StatusFailed {
		t.Fatalf("SQLite result = %#v, want migration failure", result)
	}
	backups, err := filepath.Glob(path + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 0 {
		t.Fatalf("diagnosis created migration backups: %v", backups)
	}
}

// TestStartupCheckDoesNotExposeSQLiteErrors verifies the CLI-facing result is
// bounded even when the underlying store reports a path-specific failure.
func TestStartupCheckDoesNotExposeSQLiteErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "factory.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	result := doctor.Run(context.Background(), store.StartupCheck(path)).Results[0]
	if result.Status != doctor.StatusFailed {
		t.Fatalf("SQLite result = %#v, want failed", result)
	}
	if strings.Contains(result.Problem+result.Action, "factory.db") {
		t.Fatalf("SQLite result exposed a path: %#v", result)
	}
}
