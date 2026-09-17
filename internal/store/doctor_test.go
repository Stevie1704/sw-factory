package store_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestSupervisorStartupCheckDistinguishesLiveAndMissingHeartbeats verifies a
// healthy coordinator passes while an absent coordinator remains visible as a
// non-blocking warning.
func TestSupervisorStartupCheckDistinguishesLiveAndMissingHeartbeats(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state", "factory.db")
	opened, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	missing := store.SupervisorStartupCheck(path, now)(t.Context())
	if missing.Status != doctor.StatusWarning || !strings.Contains(missing.Problem, "no live supervisor") {
		t.Fatalf("missing heartbeat result = %#v, want a no-live warning", missing)
	}

	opened, err = store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.SaveSupervisorHeartbeat(t.Context(), store.SupervisorHeartbeat{
		Coordinator: "host-a",
		PID:         1234,
		StartedAt:   now,
		RenewedAt:   now,
		ExpiresAt:   now.Add(time.Minute),
	}); err != nil {
		_ = opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	live := store.SupervisorStartupCheck(path, now.Add(30*time.Second))(t.Context())
	if live.Status != doctor.StatusPassed {
		t.Fatalf("live heartbeat result = %#v, want passed", live)
	}
	notHeld := store.SupervisorStartupCheckWithLock(path, now.Add(30*time.Second), func() bool { return false })(t.Context())
	if notHeld.Status != doctor.StatusWarning || !strings.Contains(notHeld.Problem, "no live supervisor") {
		t.Fatalf("unheld live heartbeat result = %#v, want a no-live warning", notHeld)
	}
}

// TestSupervisorStartupCheckNamesAnActiveRunWithoutALiveHeartbeat verifies
// the warning explains why a non-terminal run can appear to be progressing.
func TestSupervisorStartupCheckNamesAnActiveRunWithoutALiveHeartbeat(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state", "factory.db")
	opened, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.SaveRun(t.Context(), store.Run{
		ID:             "run-paused",
		RepositoryPath: "/repo",
		IssueNumber:    193,
		Stage:          store.StageCheck,
		Status:         store.StatusWaitingForHarness,
		CreatedAt:      time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC),
		UpdatedAt:      time.Date(2026, 9, 17, 9, 1, 0, 0, time.UTC),
	}); err != nil {
		_ = opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	result := store.SupervisorStartupCheck(path, time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC))(t.Context())
	if result.Status != doctor.StatusWarning || !strings.Contains(result.Problem, "run-paused") || !strings.Contains(result.Action, "factory start") {
		t.Fatalf("active-run heartbeat result = %#v, want an actionable run-specific warning", result)
	}
}
