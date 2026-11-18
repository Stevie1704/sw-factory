package factory

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// migrationRegistration returns a registration whose store holds one run
// with the given status at the previous schema version.
func migrationRegistration(t *testing.T, status store.Status) config.RepositoryRegistration {
	t.Helper()
	directory := t.TempDir()
	registration := config.RepositoryRegistration{
		Path:                filepath.Join(directory, "repository"),
		OperationalDataPath: filepath.Join(directory, "data", "factory.db"),
	}
	opened, err := store.Open(context.Background(), registration.OperationalDataPath)
	if err != nil {
		t.Fatal(err)
	}
	run := store.Run{ID: "run-1", RepositoryPath: registration.Path, IssueNumber: 1, Stage: store.StageClaim, Status: status, CreatedAt: time.Unix(100, 0).UTC(), UpdatedAt: time.Unix(100, 0).UTC()}
	if err := opened.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := setSchemaVersion(registration.OperationalDataPath, store.CurrentSchemaVersion-1); err != nil {
		t.Fatal(err)
	}
	return registration
}

// TestMigrateStoreMigratesAQuiescentStore verifies the explicit migration
// entry point on an idle store and its reported provenance.
func TestMigrateStoreMigratesAQuiescentStore(t *testing.T) {
	t.Parallel()
	registration := migrationRegistration(t, store.StatusComplete)

	result, err := MigrateStore(context.Background(), registration)
	if err != nil {
		t.Fatalf("MigrateStore() error = %v", err)
	}
	if len(result.Refusals) != 0 || result.Migration.ToVersion != store.CurrentSchemaVersion || result.Migration.Backup == "" {
		t.Fatalf("result = %+v, want a migration with a backup", result)
	}
}

// TestMigrateStoreRefusesActiveWorkAndALiveCoordinator verifies that the
// migration never runs while a run is active or a coordinator holds the
// lock, and leaves the store schema unchanged.
func TestMigrateStoreRefusesActiveWorkAndALiveCoordinator(t *testing.T) {
	t.Parallel()
	active := migrationRegistration(t, store.StatusActive)
	result, err := MigrateStore(context.Background(), active)
	if err != nil || len(result.Refusals) == 0 || result.Refusals[0].Code != "run.active" {
		t.Fatalf("active result = %+v, %v; want a run.active refusal", result, err)
	}
	assertSchema(t, active.OperationalDataPath, store.CurrentSchemaVersion-1)

	idle := migrationRegistration(t, store.StatusComplete)
	owner, err := acquireCoordinatorLock(coordinatorLockPath(idle), LockHolderCoordinator)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.release() }()
	result, err = MigrateStore(context.Background(), idle)
	if err != nil || len(result.Refusals) == 0 || result.Refusals[0].Code != "coordinator.running" {
		t.Fatalf("locked result = %+v, %v; want a coordinator.running refusal", result, err)
	}
	assertSchema(t, idle.OperationalDataPath, store.CurrentSchemaVersion-1)
}

// TestMigrateStoreRollsBackAnInterruptedWriteFirst verifies that a rollback
// journal is resolved before quiescence is checked.
func TestMigrateStoreRollsBackAnInterruptedWriteFirst(t *testing.T) {
	t.Parallel()
	registration := migrationRegistration(t, store.StatusComplete)
	if err := os.WriteFile(registration.OperationalDataPath+"-journal", []byte("interrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := MigrateStore(context.Background(), registration)
	if err != nil || len(result.Refusals) != 0 || result.Migration.ToVersion != store.CurrentSchemaVersion {
		t.Fatalf("result = %+v, %v; want a migration after the rollback", result, err)
	}
}

// assertSchema fails unless the store records the given schema version.
func assertSchema(t *testing.T, path string, want int) {
	t.Helper()
	inspection, err := store.InspectForUpgrade(context.Background(), path)
	if err != nil || inspection.SchemaVersion != want {
		t.Fatalf("schema = %d, %v; want %d", inspection.SchemaVersion, err, want)
	}
}

// setSchemaVersion rewrites the recorded schema version of a store fixture.
func setSchemaVersion(path string, version int) error {
	opened, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer func() { _ = opened.Close() }()
	_, err = opened.Exec("UPDATE schema_metadata SET version = ?", version)
	return err
}

// TestMigrateStoreRefusesAnActiveSessionRole verifies that a session-role
// invocation that the previous release still owns stops the migration even
// when no run is active.
func TestMigrateStoreRefusesAnActiveSessionRole(t *testing.T) {
	t.Parallel()
	registration := migrationRegistration(t, store.StatusComplete)
	database, err := sql.Open("sqlite", registration.OperationalDataPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Exec(`INSERT INTO ideation_invocations (session_id, id, role, trigger_id, session_revision, context_revision, harness, prompt_version, prompt_sha256, worker_id, status, created_at, updated_at)
		VALUES ('session-1', 'session-inv-1', 'ideation', 'turn-1', 1, 'context-1', 'codex', 'ideation-v1', 'digest', 'worker-1', 'active', '2026-11-01T00:00:00Z', '2026-11-01T00:00:00Z')`)
	if closeErr := database.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}

	result, err := MigrateStore(context.Background(), registration)
	if err != nil || len(result.Refusals) != 1 || result.Refusals[0].Code != "session_invocation.active" {
		t.Fatalf("result = %+v, %v; want a session_invocation.active refusal", result, err)
	}
	assertSchema(t, registration.OperationalDataPath, store.CurrentSchemaVersion-1)
}
