package store_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
	_ "modernc.org/sqlite"
)

func TestOpenCreatesVersionedStoreWithNoActiveRun(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = opened.Close() }()

	if got := opened.SchemaVersion(); got != store.CurrentSchemaVersion {
		t.Fatalf("SchemaVersion() = %d, want %d", got, store.CurrentSchemaVersion)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("store permissions = %#o, want 0600", got)
	}
	current, err := opened.CurrentRun(context.Background())
	if err != nil {
		t.Fatalf("CurrentRun() error = %v", err)
	}
	if current != nil {
		t.Fatalf("CurrentRun() = %#v, want empty store", current)
	}
	backups, err := filepath.Glob(path + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 0 {
		t.Fatalf("fresh store backups = %v, want none", backups)
	}
}

// TestSchema33MigrationFoldsTheSingularActiveInvocationIntoTheSlice verifies
// schema 32 rows retain every active invocation when the singular projection is
// removed.
func TestSchema33MigrationFoldsTheSingularActiveInvocationIntoTheSlice(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("initial Open() error = %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("close initial store: %v", err)
	}

	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if !testOperationalRunsColumnExists(t, database, "active_invocation_id") {
		if _, err := database.ExecContext(t.Context(), "ALTER TABLE operational_runs ADD COLUMN active_invocation_id TEXT NOT NULL DEFAULT ''"); err != nil {
			_ = database.Close()
			t.Fatalf("add schema 32 active invocation column: %v", err)
		}
	}
	_, err = database.ExecContext(t.Context(), `
		INSERT INTO operational_runs (
			id, repository_path, issue_number, stage, status,
			active_invocation_id, active_invocation_ids, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);
		UPDATE schema_metadata SET version = 32 WHERE singleton = 1`,
		"run-schema-32", "/work/repository", 118, store.StageImplementation, store.StatusActive,
		"inv-singular", `["inv-existing"]`, "2026-08-01T00:00:00.000000000Z", "2026-08-01T00:00:01.000000000Z")
	if err != nil {
		_ = database.Close()
		t.Fatalf("write schema 32 fixture: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("schema 33 migration error = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if got := reopened.SchemaVersion(); got != 33 {
		t.Fatalf("SchemaVersion() = %d, want 33", got)
	}
	got, err := reopened.CurrentRun(context.Background())
	if err != nil {
		t.Fatalf("CurrentRun() error = %v", err)
	}
	if got == nil || len(got.ActiveInvocationIDs) != 2 || got.ActiveInvocationIDs[0] != "inv-singular" || got.ActiveInvocationIDs[1] != "inv-existing" {
		t.Fatalf("CurrentRun() active invocations = %#v, want singular value first", got)
	}

	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if testOperationalRunsColumnExists(t, database, "active_invocation_id") {
		t.Fatal("schema 33 still has the active_invocation_id column")
	}
}

// testOperationalRunsColumnExists reports whether a column exists in the
// migration fixture's operational_runs table.
func testOperationalRunsColumnExists(t *testing.T, database *sql.DB, wanted string) bool {
	t.Helper()
	rows, err := database.QueryContext(t.Context(), "PRAGMA table_info(operational_runs)")
	if err != nil {
		t.Fatalf("inspect operational_runs columns: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var (
		cid          int
		name         string
		columnType   string
		notNull      int
		defaultValue sql.NullString
		primaryKey   int
	)
	for rows.Next() {
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("scan operational_runs column: %v", err)
		}
		if name == wanted {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate operational_runs columns: %v", err)
	}
	return false
}

// TestPreviousBuildImplementationHandoffRowsDecodeIntoRoleHandoff verifies
// the existing implementation_handoff column remains the durable role-handoff
// source when the Run compatibility field is removed.
func TestPreviousBuildImplementationHandoffRowsDecodeIntoRoleHandoff(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("initial Open() error = %v", err)
	}
	if err := opened.SaveRun(context.Background(), store.Run{
		ID:             "run-legacy-handoff",
		RepositoryPath: "/work/repository",
		Stage:          store.StageImplementation,
		Status:         store.StatusActive,
		CreatedAt:      time.Unix(100, 0).UTC(),
		UpdatedAt:      time.Unix(200, 0).UTC(),
	}); err != nil {
		_ = opened.Close()
		t.Fatalf("seed run: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.ExecContext(t.Context(), `UPDATE operational_runs
		SET implementation_handoff = ?
		WHERE id = ?`,
		`{"change_summary":"written by schema 32","acceptance_mapping":[{"criterion":"legacy row","evidence":"stored JSON"}],"production_files_changed":["internal/store/store.go"],"focused_commands":["go test ./internal/store"]}`,
		"run-legacy-handoff")
	if err != nil {
		_ = database.Close()
		t.Fatalf("write previous-build handoff row: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.CurrentRun(context.Background())
	if err != nil {
		t.Fatalf("CurrentRun() error = %v", err)
	}
	if got == nil || got.RoleHandoff == nil || got.RoleHandoff.ChangeSummary != "written by schema 32" {
		t.Fatalf("CurrentRun() role handoff = %#v, want previous-build handoff", got)
	}
}

func TestOpenRejectsANewerSchemaVersion(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(t.Context(), `CREATE TABLE schema_metadata (singleton INTEGER PRIMARY KEY, version INTEGER NOT NULL); INSERT INTO schema_metadata(singleton, version) VALUES (1, 99);`); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = store.Open(context.Background(), path)
	var schemaErr *store.UnknownSchemaVersionError
	if !errors.As(err, &schemaErr) {
		t.Fatalf("error = %v, want UnknownSchemaVersionError", err)
	}
	if schemaErr.Version != 99 {
		t.Fatalf("schema error = %#v", schemaErr)
	}
}

func TestOpenRejectsAnExistingEmptyDatabaseFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := store.Open(context.Background(), path)
	var unversionedErr *store.UnversionedDatabaseError
	if !errors.As(err, &unversionedErr) {
		t.Fatalf("error = %v, want UnversionedDatabaseError", err)
	}
}

func TestOpenRejectsANonPrivateExistingStoreDirectory(t *testing.T) {
	t.Parallel()

	directory := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := store.Open(context.Background(), filepath.Join(directory, "factory.db"))
	if err == nil {
		t.Fatal("Open() succeeded in a non-private directory")
	}
	if !strings.Contains(err.Error(), "not private") {
		t.Fatalf("error = %v, want private-directory diagnostic", err)
	}
}

func TestOpenBacksUpBeforeApplyingAnOlderMigration(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(t.Context(), `CREATE TABLE schema_metadata (singleton INTEGER PRIMARY KEY, version INTEGER NOT NULL); INSERT INTO schema_metadata(singleton, version) VALUES (1, 0);`); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = opened.Close() }()
	if got := opened.SchemaVersion(); got != store.CurrentSchemaVersion {
		t.Fatalf("SchemaVersion() = %d, want %d", got, store.CurrentSchemaVersion)
	}
	backups, err := filepath.Glob(path + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("backup files = %v, want one backup", backups)
	}
}

// TestStoreMigratesSchemaThreeAndReadsLegacyEmptyPermittedPaths verifies the
// invocation column migration remains compatible with rows whose old default
// was an empty string rather than JSON.
func TestStoreMigratesSchemaThreeAndReadsLegacyEmptyPermittedPaths(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	legacySchema := `
		CREATE TABLE schema_metadata (singleton INTEGER PRIMARY KEY, version INTEGER NOT NULL);
		INSERT INTO schema_metadata(singleton, version) VALUES (1, 3);
		CREATE TABLE operational_runs (
			id TEXT PRIMARY KEY,
			repository_path TEXT NOT NULL,
			issue_number INTEGER NOT NULL,
			stage TEXT NOT NULL,
			status TEXT NOT NULL,
			branch TEXT NOT NULL DEFAULT '',
			worktree TEXT NOT NULL DEFAULT '',
			checkpoint_sha TEXT NOT NULL DEFAULT '',
			image_digest TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			coordinator TEXT NOT NULL DEFAULT '',
			status_comment_id TEXT NOT NULL DEFAULT '',
			specification_packet TEXT NOT NULL DEFAULT ''
		);
		CREATE UNIQUE INDEX one_active_run_per_repository
			ON operational_runs (repository_path)
			WHERE status NOT IN ('complete', 'cancelled', 'failed');`
	if _, err := database.ExecContext(t.Context(), legacySchema); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() migration error = %v", err)
	}
	if got := opened.SchemaVersion(); got != store.CurrentSchemaVersion {
		t.Fatalf("SchemaVersion() = %d, want %d", got, store.CurrentSchemaVersion)
	}
	if err := opened.SaveInvocation(context.Background(), store.Invocation{
		ID: "inv-migrated", RunID: "run-migrated", Harness: "codex", Role: "implementation", Stage: store.StageImplementation, Status: store.InvocationStatusCompleted,
	}); err != nil {
		_ = opened.Close()
		t.Fatalf("SaveInvocation() error = %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(t.Context(), "UPDATE invocations SET permitted_paths = '' WHERE id = 'inv-migrated'"); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	invocation, err := reopened.Invocation(context.Background(), "run-migrated", "inv-migrated")
	if err != nil {
		t.Fatalf("Invocation() error = %v", err)
	}
	if invocation == nil || len(invocation.PermittedPaths) != 0 {
		t.Fatalf("Invocation() = %#v, want an invocation with empty permitted paths", invocation)
	}
}

func TestOpenReopensAnExistingCurrentVersionStoreWithoutCreatingABackup(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	first, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	wanted := store.Run{
		ID:             "run-1",
		RepositoryPath: "/work/repository",
		Stage:          store.StagePreflight,
		Status:         store.StatusActive,
	}
	if err := first.SaveRun(context.Background(), wanted); err != nil {
		t.Fatalf("SaveRun() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reopened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() (reopen) error = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if got := reopened.SchemaVersion(); got != store.CurrentSchemaVersion {
		t.Fatalf("SchemaVersion() = %d, want %d", got, store.CurrentSchemaVersion)
	}
	current, err := reopened.CurrentRun(context.Background())
	if err != nil {
		t.Fatalf("CurrentRun() error = %v", err)
	}
	if current == nil || current.ID != "run-1" {
		t.Fatalf("CurrentRun() = %#v, want the persisted run to survive reopening", current)
	}
	backups, err := filepath.Glob(path + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 0 {
		t.Fatalf("backups after reopening a current-version store = %v, want none", backups)
	}
}

// TestGateResultsSurviveReopenAndRemainKeyedByPhaseAndCheckpoint verifies
// deterministic results cannot be reused after the evaluated commit changes.
func TestGateResultsSurviveReopenAndRemainKeyedByPhaseAndCheckpoint(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	firstSHA := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	secondSHA := "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"
	if err := opened.SaveGateResults(context.Background(), []store.GateResult{
		{RunID: "run-gates", CheckpointSHA: firstSHA, Phase: store.GatePhaseBaseline, Ordinal: 0, GateName: "format", Outcome: store.GateOutcomePassed, Status: "success", Blocking: true, SetupFingerprint: "deps-a"},
		{RunID: "run-gates", CheckpointSHA: firstSHA, Phase: store.GatePhaseCheckpoint, Ordinal: 0, GateName: "format", Outcome: store.GateOutcomeFailed, Status: "failure", Blocking: true, SetupFingerprint: "deps-a"},
		{RunID: "run-gates", CheckpointSHA: secondSHA, Phase: store.GatePhaseCheckpoint, Ordinal: 0, GateName: "format", Outcome: store.GateOutcomePassed, Status: "success", Blocking: true, SetupFingerprint: "deps-b"},
	}); err != nil {
		_ = opened.Close()
		t.Fatalf("SaveGateResults() error = %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.GateResults(context.Background(), "run-gates", store.GatePhaseCheckpoint, secondSHA)
	if err != nil {
		t.Fatalf("GateResults() error = %v", err)
	}
	if len(got) != 1 || got[0].GateName != "format" || got[0].Outcome != store.GateOutcomePassed || got[0].SetupFingerprint != "deps-b" {
		t.Fatalf("GateResults() = %#v, want only the second checkpoint result", got)
	}
}

// TestRunTerminalProjectionSurvivesStoreReopen verifies merge and lifecycle
// metadata remain available to status after a terminal transition.
func TestRunTerminalProjectionSurvivesStoreReopen(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	run := store.Run{
		ID:                        "run-terminal",
		RepositoryPath:            "/work/repository",
		IssueNumber:               42,
		Stage:                     store.StageReady,
		Status:                    store.StatusComplete,
		PullRequestNumber:         17,
		PullRequestURL:            "https://github.com/example/project/pull/17",
		MergeCommitSHA:            "0123456789abcdef0123456789abcdef0123456789",
		LifecycleReason:           "pull request #17 merged",
		LifecycleNotificationSent: true,
	}
	if err := opened.SaveRun(context.Background(), run); err != nil {
		_ = opened.Close()
		t.Fatalf("SaveRun() error = %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	latest, err := reopened.LatestRun(context.Background())
	if err != nil {
		t.Fatalf("LatestRun() error = %v", err)
	}
	if latest == nil || latest.MergeCommitSHA != run.MergeCommitSHA || latest.LifecycleReason != run.LifecycleReason || !latest.LifecycleNotificationSent {
		t.Fatalf("LatestRun() = %#v, want terminal lifecycle projection", latest)
	}
}

func TestOpenRejectsAnOperationalStorePathThatIsADirectory(t *testing.T) {
	t.Parallel()

	directory := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	nestedDirectoryAsFile := filepath.Join(directory, "factory.db")
	if err := os.MkdirAll(nestedDirectoryAsFile, 0o700); err != nil {
		t.Fatal(err)
	}

	_, err := store.Open(context.Background(), nestedDirectoryAsFile)
	if err == nil {
		t.Fatal("Open() succeeded for a store path that is a directory")
	}
	if !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("error = %v, want a directory diagnostic", err)
	}
}

func TestOpenRejectsAnEmptyPath(t *testing.T) {
	t.Parallel()

	_, err := store.Open(context.Background(), "")
	if err == nil {
		t.Fatal("Open() succeeded for an empty path")
	}
}

func TestStoreCloseIsSafeOnANilReceiver(t *testing.T) {
	t.Parallel()

	var opened *store.Store
	if err := opened.Close(); err != nil {
		t.Fatalf("Close() on a nil *Store error = %v, want nil", err)
	}
}

func TestStorePathReturnsTheResolvedAbsolutePath(t *testing.T) {
	t.Parallel()

	relative := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(relative, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(relative, "factory.db")
	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = opened.Close() }()
	want, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := opened.Path(); got != want {
		t.Fatalf("Path() = %q, want %q", got, want)
	}
}

func TestSaveRunRejectsMissingRequiredFields(t *testing.T) {
	t.Parallel()

	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()

	tests := []struct {
		name string
		run  store.Run
	}{
		{name: "missing id", run: store.Run{RepositoryPath: "/work/repository", Status: store.StatusActive}},
		{name: "missing repository path", run: store.Run{ID: "run-1", Status: store.StatusActive}},
		{name: "missing status", run: store.Run{ID: "run-1", RepositoryPath: "/work/repository"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := opened.SaveRun(context.Background(), tc.run); err == nil {
				t.Fatalf("SaveRun(%#v) succeeded, want an error", tc.run)
			}
		})
	}
}

func TestSaveRunDefaultsTimestampsWhenUnset(t *testing.T) {
	t.Parallel()

	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()

	before := time.Now().UTC()
	run := store.Run{ID: "run-1", RepositoryPath: "/work/repository", Status: store.StatusActive}
	if err := opened.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun() error = %v", err)
	}
	after := time.Now().UTC()

	got, err := opened.CurrentRun(context.Background())
	if err != nil {
		t.Fatalf("CurrentRun() error = %v", err)
	}
	if got == nil {
		t.Fatal("CurrentRun() = nil, want the saved run")
	}
	if got.CreatedAt.Before(before) || got.CreatedAt.After(after) {
		t.Fatalf("CreatedAt = %v, want it between %v and %v", got.CreatedAt, before, after)
	}
	if !got.UpdatedAt.Equal(got.CreatedAt) {
		t.Fatalf("UpdatedAt = %v, want it to default to CreatedAt = %v", got.UpdatedAt, got.CreatedAt)
	}
}

// TestRunPersistsTheProtectedTestHandoffProjection verifies the separate base
// and test checkpoints, handoff, exemption, and path hashes survive SQLite
// round trips.
func TestRunPersistsTheProtectedTestHandoffProjection(t *testing.T) {
	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	want := store.Run{
		ID:                 "run-test-projection",
		RepositoryPath:     "/work/repository",
		Stage:              store.StageImplementation,
		Status:             store.StatusActive,
		CheckpointSHA:      strings.Repeat("b", 64),
		BaseCheckpointSHA:  strings.Repeat("a", 64),
		TestCheckpointSHA:  strings.Repeat("b", 64),
		TestHandoff:        &store.TestHandoff{AcceptanceCoverage: []store.TestAcceptanceCoverage{{Criterion: "criterion", Evidence: "red test"}}, ChangedFiles: []string{"internal/factory/agent_test.go"}, FocusedTestCommand: "go test ./internal/factory -run TestBehavior", ExpectedFailureReason: "expected behavior assertion", ObservedFailureEvidence: []store.TestFailureEvidence{{Kind: "exit_code", Detail: "1"}}},
		TestExemption:      &store.TestExemption{Kind: "technical", Justification: "test runtime unavailable during pilot"},
		ProtectedTestPaths: []store.ProtectedTestPath{{Path: "internal/factory/agent_test.go", SHA256: strings.Repeat("c", 64)}},
		TestStageSkipped:   true,
		CheckRepairBudget:  1,
	}
	if err := opened.SaveRun(context.Background(), want); err != nil {
		t.Fatalf("SaveRun() error = %v", err)
	}
	got, err := opened.LatestRun(context.Background())
	if err != nil {
		t.Fatalf("LatestRun() error = %v", err)
	}
	if got == nil || got.BaseCheckpointSHA != want.BaseCheckpointSHA || got.TestCheckpointSHA != want.TestCheckpointSHA || got.TestHandoff == nil || got.TestExemption == nil || len(got.ProtectedTestPaths) != 1 || !got.TestStageSkipped {
		t.Fatalf("LatestRun() = %#v, want persisted test projection", got)
	}
}

// TestRunPersistsTheTestObjectionProjection verifies an objection, its native
// test-session identity, bounded history, and pre-existing worktree hashes
// survive a SQLite restart.
func TestRunPersistsTheTestObjectionProjection(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	want := store.Run{
		ID:                           "run-objection-projection",
		RepositoryPath:               "/work/repository",
		Stage:                        store.StageTest,
		Status:                       store.StatusActive,
		CheckpointSHA:                strings.Repeat("b", 64),
		TestInvocationID:             "inv-test",
		TestRevisionAttempts:         1,
		TestRevisionBudget:           2,
		TestRevisionHistory:          []store.TestRevision{{Attempt: 1, Outcome: store.TestRevisionPending}},
		TestObjection:                &store.TestObjection{Test: "internal/factory/agent_test.go:TestBehavior", Claim: "the assertion is too narrow", Evidence: "public behavior is correct", InvocationID: "inv-test"},
		TestRevisionBaseChangedPaths: []store.ProtectedTestPath{{Path: "internal/factory/agent.go", SHA256: strings.Repeat("c", 64)}},
	}
	if err := opened.SaveRun(context.Background(), want); err != nil {
		_ = opened.Close()
		t.Fatalf("SaveRun() error = %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.CurrentRun(context.Background())
	if err != nil {
		t.Fatalf("CurrentRun() error = %v", err)
	}
	if got == nil || got.TestInvocationID != want.TestInvocationID || got.TestRevisionAttempts != 1 || got.TestRevisionBudget != 2 || len(got.TestRevisionHistory) != 1 || got.TestObjection == nil || got.TestObjection.Claim != want.TestObjection.Claim || len(got.TestRevisionBaseChangedPaths) != 1 || got.TestRevisionBaseChangedPaths[0].SHA256 != strings.Repeat("c", 64) {
		t.Fatalf("CurrentRun() = %#v, want persisted objection projection", got)
	}
}

// TestRunPersistsRoleAndSpecificationReviewProjections verifies the
// reviewer packet's durable handoffs survive the schema-20 restart boundary.
func TestRunPersistsRoleAndSpecificationReviewProjections(t *testing.T) {
	t.Parallel()

	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	checkpoint := strings.Repeat("b", 64)
	want := store.Run{
		ID:             "run-review-projection",
		RepositoryPath: "/work/repository",
		Stage:          store.StageReview,
		Status:         store.StatusWaitingForHuman,
		CheckpointSHA:  checkpoint,
		RoleHandoff: &store.RoleHandoff{
			ChangeSummary:          "implemented the requested review behavior",
			AcceptanceMapping:      []store.HandoffAcceptance{{Criterion: "criterion", Evidence: "focused test"}},
			ProductionFilesChanged: []string{"internal/factory/review.go"},
			FocusedCommands:        []string{"go test ./internal/factory"},
			KnownLimitations:       []string{"advisory naming note"},
		},
		SpecificationReview: &store.SpecificationReview{
			CheckpointSHA: checkpoint,
			Findings: []store.ReviewFinding{{
				Location: "internal/factory/review.go:1", Claim: "claim", Evidence: "evidence", Severity: "advisory", Category: "scope",
				SuggestedResolution: "resolution", SuggestedOwner: "implementation",
			}},
		},
	}
	if err := opened.SaveRun(context.Background(), want); err != nil {
		t.Fatalf("SaveRun() error = %v", err)
	}
	got, err := opened.CurrentRun(context.Background())
	if err != nil {
		t.Fatalf("CurrentRun() error = %v", err)
	}
	if got == nil || got.RoleHandoff == nil || got.SpecificationReview == nil || got.SpecificationReview.CheckpointSHA != checkpoint || got.SpecificationReview.Findings[0].SuggestedOwner != "implementation" {
		t.Fatalf("CurrentRun() = %#v, want role handoff and review projections", got)
	}
}

// TestRunRejectsAReviewProjectionForAnOlderCheckpoint verifies stale findings
// cannot be persisted or reused after a new immutable checkpoint is selected.
func TestRunRejectsAReviewProjectionForAnOlderCheckpoint(t *testing.T) {
	t.Parallel()

	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	run := store.Run{
		ID:             "run-stale-review",
		RepositoryPath: "/work/repository",
		Stage:          store.StageReview,
		Status:         store.StatusWaitingForHuman,
		CheckpointSHA:  strings.Repeat("b", 64),
		SpecificationReview: &store.SpecificationReview{
			CheckpointSHA: strings.Repeat("a", 64),
		},
	}
	if err := opened.SaveRun(context.Background(), run); err == nil || !strings.Contains(err.Error(), "match the run checkpoint") {
		t.Fatalf("SaveRun() error = %v, want stale-review rejection", err)
	}
}

// TestStorePersistsCheckRepairBudget verifies the retry ceiling, consumed
// attempt count, and in-flight reservation survive the restart boundary.
func TestStorePersistsCheckRepairBudget(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	run := store.Run{
		ID:                        "run-repair-budget",
		RepositoryPath:            "/work/repository",
		Stage:                     store.StageImplementation,
		Status:                    store.StatusActive,
		CheckRepairAttempts:       2,
		CheckRepairBudget:         3,
		CheckRepairPendingAttempt: 3,
		CreatedAt:                 time.Unix(100, 0).UTC(),
		UpdatedAt:                 time.Unix(200, 0).UTC(),
	}
	if err := opened.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun() error = %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	reopened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.CurrentRun(context.Background())
	if err != nil {
		t.Fatalf("CurrentRun() error = %v", err)
	}
	if got == nil || got.CheckRepairAttempts != 2 || got.CheckRepairBudget != 3 || got.CheckRepairPendingAttempt != 3 {
		t.Fatalf("CurrentRun() = %#v, want repair attempts 2, pending attempt 3, and budget 3", got)
	}
}

// TestStorePersistsReviewRepairState verifies the bounded review-repair
// packet, attempt history, and current reservation survive a restart.
func TestStorePersistsReviewRepairState(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	run := store.Run{
		ID:                         "run-review-repair",
		RepositoryPath:             "/work/repository",
		Stage:                      store.StageImplementation,
		Status:                     store.StatusActive,
		ReviewRepairAttempts:       1,
		ReviewRepairBudget:         2,
		ReviewRepairPendingAttempt: 2,
		ReviewRepairHistory: []store.ReviewRepairAttempt{{
			Attempt: 1, Outcome: store.ReviewRepairStarted,
			BlockerKeys: []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		}},
		ReviewRepairPacket: &store.ReviewRepairPacket{
			Version: 1, RunID: "run-review-repair", CheckpointSHA: strings.Repeat("b", 64), Attempt: 2, Budget: 2,
			Findings: []store.ReviewRepairFinding{{ReviewerRole: "spec_review", Finding: store.ReviewFinding{Location: "internal/factory/review.go:1", Claim: "behavior is incomplete", Evidence: "the completion branch omits the required transition", Severity: "blocker", Category: "correctness", SuggestedResolution: "apply the required transition before returning", SuggestedOwner: "implementation"}}},
		},
		CheckpointSHA:     strings.Repeat("b", 64),
		BaseCheckpointSHA: strings.Repeat("c", 64),
		CreatedAt:         time.Unix(100, 0).UTC(), UpdatedAt: time.Unix(200, 0).UTC(),
	}
	if err := opened.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun() error = %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	reopened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.CurrentRun(context.Background())
	if err != nil {
		t.Fatalf("CurrentRun() error = %v", err)
	}
	if got == nil || got.ReviewRepairAttempts != 1 || got.ReviewRepairBudget != 2 || got.ReviewRepairPendingAttempt != 2 || len(got.ReviewRepairHistory) != 1 || got.ReviewRepairPacket == nil || got.ReviewRepairPacket.Findings[0].ReviewerRole != "spec_review" {
		t.Fatalf("CurrentRun() = %#v, want review-repair state preserved", got)
	}
}

// TestStoreRejectsIncompleteReviewRepairFindings verifies repair packets use
// the same complete nested finding contract as exact-checkpoint reviews.
func TestStoreRejectsIncompleteReviewRepairFindings(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = opened.Close() }()
	run := store.Run{
		ID:                 "run-incomplete-review-repair",
		RepositoryPath:     "/work/repository",
		Stage:              store.StageImplementation,
		Status:             store.StatusActive,
		ReviewRepairBudget: 1,
		ReviewRepairPacket: &store.ReviewRepairPacket{
			Version: 1, RunID: "run-incomplete-review-repair", CheckpointSHA: strings.Repeat("b", 64), Attempt: 1, Budget: 1,
			Findings: []store.ReviewRepairFinding{{
				ReviewerRole: "spec_review",
				Finding: store.ReviewFinding{
					Location: "internal/factory/review.go:1", Claim: "behavior is incomplete", Severity: "blocker", Category: "correctness", SuggestedResolution: "apply the required transition", SuggestedOwner: "implementation",
				},
			}},
		},
		CheckpointSHA: strings.Repeat("b", 64),
		CreatedAt:     time.Unix(100, 0).UTC(), UpdatedAt: time.Unix(200, 0).UTC(),
	}
	if err := opened.SaveRun(context.Background(), run); err == nil || !strings.Contains(err.Error(), "evidence is required") {
		t.Fatalf("SaveRun() error = %v, want incomplete review-repair finding refusal", err)
	}
}

// TestStorePersistsTheActiveInvocationDelegation verifies the durable marker
// that lets a status projection separate an active run from an active harness
// invocation without reading the invocation table.
func TestStorePersistsTheActiveInvocationDelegation(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	run := store.Run{
		ID:                  "run-delegation",
		RepositoryPath:      "/work/repository",
		Stage:               store.StageImplementation,
		Status:              store.StatusActive,
		ActiveInvocationIDs: []string{"inv-delegation"},
		CreatedAt:           time.Unix(100, 0).UTC(),
		UpdatedAt:           time.Unix(200, 0).UTC(),
	}
	if err := opened.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun() error = %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	reopened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.CurrentRun(context.Background())
	if err != nil {
		t.Fatalf("CurrentRun() error = %v", err)
	}
	if got == nil || len(got.ActiveInvocationIDs) != 1 || got.ActiveInvocationIDs[0] != "inv-delegation" {
		t.Fatalf("CurrentRun() = %#v, want the persisted active invocation identity", got)
	}
	run.ActiveInvocationIDs = nil
	run.UpdatedAt = time.Unix(300, 0).UTC()
	if err := reopened.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun() clearing error = %v", err)
	}
	cleared, err := reopened.CurrentRun(context.Background())
	if err != nil {
		t.Fatalf("CurrentRun() after clearing error = %v", err)
	}
	if cleared == nil || len(cleared.ActiveInvocationIDs) != 0 {
		t.Fatalf("CurrentRun() = %#v, want the delegation cleared", cleared)
	}
}

func TestSaveRunUpsertsAnExistingRunByID(t *testing.T) {
	t.Parallel()

	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()

	initial := store.Run{
		ID:             "run-1",
		RepositoryPath: "/work/repository",
		Stage:          store.StageClaim,
		Status:         store.StatusActive,
		CreatedAt:      time.Unix(100, 0).UTC(),
		UpdatedAt:      time.Unix(100, 0).UTC(),
	}
	if err := opened.SaveRun(context.Background(), initial); err != nil {
		t.Fatalf("SaveRun() initial error = %v", err)
	}

	updated := initial
	updated.Stage = store.StageReview
	updated.Status = store.StatusWaitingForHuman
	updated.PullRequestNumber = 17
	updated.PullRequestURL = "https://github.com/example/project/pull/17"
	updated.CreatedAt = time.Time{}
	updated.UpdatedAt = time.Unix(200, 0).UTC()
	if err := opened.SaveRun(context.Background(), updated); err != nil {
		t.Fatalf("SaveRun() update error = %v", err)
	}

	got, err := opened.CurrentRun(context.Background())
	if err != nil {
		t.Fatalf("CurrentRun() error = %v", err)
	}
	if got == nil {
		t.Fatal("CurrentRun() = nil, want the upserted run")
	}
	if got.Stage != store.StageReview || got.Status != store.StatusWaitingForHuman {
		t.Fatalf("CurrentRun() = %#v, want the updated stage and status", got)
	}
	if !got.UpdatedAt.Equal(updated.UpdatedAt) {
		t.Fatalf("UpdatedAt = %v, want %v", got.UpdatedAt, updated.UpdatedAt)
	}
	if !got.CreatedAt.Equal(initial.CreatedAt) {
		t.Fatalf("CreatedAt = %v, want original %v", got.CreatedAt, initial.CreatedAt)
	}
	if got.PullRequestNumber != updated.PullRequestNumber || got.PullRequestURL != updated.PullRequestURL {
		t.Fatalf("pull request identity = #%d %q, want #%d %q", got.PullRequestNumber, got.PullRequestURL, updated.PullRequestNumber, updated.PullRequestURL)
	}
}

func TestCurrentRunExcludesTerminalStatusesAndReturnsTheMostRecentlyUpdatedRun(t *testing.T) {
	t.Parallel()

	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()

	runs := []store.Run{
		{ID: "run-complete", RepositoryPath: "/work/repository", Status: store.StatusComplete, UpdatedAt: time.Unix(400, 0).UTC(), CreatedAt: time.Unix(400, 0).UTC()},
		{ID: "run-cancelled", RepositoryPath: "/work/repository", Status: store.StatusCancelled, UpdatedAt: time.Unix(300, 0).UTC(), CreatedAt: time.Unix(300, 0).UTC()},
		{ID: "run-failed", RepositoryPath: "/work/repository", Status: store.StatusFailed, UpdatedAt: time.Unix(500, 0).UTC(), CreatedAt: time.Unix(500, 0).UTC()},
		{ID: "run-waiting", RepositoryPath: "/work/waiting-repository", Status: store.StatusWaitingForHarness, UpdatedAt: time.Unix(100, 0).UTC(), CreatedAt: time.Unix(100, 0).UTC()},
		{ID: "run-active", RepositoryPath: "/work/repository", Status: store.StatusActive, UpdatedAt: time.Unix(200, 0).UTC(), CreatedAt: time.Unix(200, 0).UTC()},
	}
	for _, run := range runs {
		if err := opened.SaveRun(context.Background(), run); err != nil {
			t.Fatalf("SaveRun(%q) error = %v", run.ID, err)
		}
	}

	got, err := opened.CurrentRun(context.Background())
	if err != nil {
		t.Fatalf("CurrentRun() error = %v", err)
	}
	if got == nil || got.ID != "run-active" {
		t.Fatalf("CurrentRun() = %#v, want the most recently updated non-terminal run (run-active)", got)
	}
}

func TestUnknownSchemaVersionErrorMessageIncludesBothVersions(t *testing.T) {
	t.Parallel()

	err := &store.UnknownSchemaVersionError{Version: 42, Supported: store.CurrentSchemaVersion}
	got := err.Error()
	if !strings.Contains(got, "42") {
		t.Fatalf("Error() = %q, want it to mention the found version", got)
	}
}

func TestUnversionedDatabaseErrorUnwrapsTheUnderlyingError(t *testing.T) {
	t.Parallel()

	underlying := errors.New("boom")
	err := &store.UnversionedDatabaseError{Path: "/tmp/factory.db", Err: underlying}
	if !errors.Is(err, underlying) {
		t.Fatalf("errors.Is(err, underlying) = false, want true")
	}
}

func TestCurrentRunReturnsThePersistedOperationalState(t *testing.T) {
	t.Parallel()

	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	wanted := store.Run{
		ID:             "run-1",
		RepositoryPath: "/work/repository",
		IssueNumber:    3,
		Stage:          "preflight",
		Status:         "active",
		Branch:         "factory/run-1",
		Worktree:       "/worktrees/run-1",
		CheckpointSHA:  "",
		CreatedAt:      time.Unix(100, 0).UTC(),
		UpdatedAt:      time.Unix(200, 0).UTC(),
	}
	if err := opened.SaveRun(context.Background(), wanted); err != nil {
		t.Fatalf("SaveRun() error = %v", err)
	}
	got, err := opened.CurrentRun(context.Background())
	if err != nil {
		t.Fatalf("CurrentRun() error = %v", err)
	}
	if got == nil || got.ID != wanted.ID || got.Status != wanted.Status || !got.UpdatedAt.Equal(wanted.UpdatedAt) {
		t.Fatalf("CurrentRun() = %#v, want %#v", got, wanted)
	}
}

// TestCurrentRunPersistsSpecificationAndStatusCommentIdentity verifies the
// frozen packet and editable-comment identity survive reopening SQLite.
func TestCurrentRunPersistsSpecificationAndStatusCommentIdentity(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	wanted := store.Run{
		ID:                            "run-claim",
		RepositoryPath:                "/work/repository",
		IssueNumber:                   42,
		Stage:                         store.StageClaim,
		Status:                        store.StatusActive,
		Branch:                        "factory/run-claim",
		Worktree:                      "/worktrees/run-claim",
		CheckpointSHA:                 "0123456789abcdef",
		ImageDigest:                   "sha256:worker",
		Coordinator:                   "host-a",
		StatusCommentID:               "12345",
		SpecificationPacket:           `{"version":1,"issue":{"number":42}}`,
		PendingQuestions:              []store.PendingQuestion{{ID: "clarification-1", Prompt: "Which behavior is intended?"}},
		ClarificationCommentID:        "67890",
		ClarificationNotificationSent: true,
		CreatedAt:                     time.Unix(100, 0).UTC(),
		UpdatedAt:                     time.Unix(200, 0).UTC(),
	}
	if err := opened.SaveRun(context.Background(), wanted); err != nil {
		_ = opened.Close()
		t.Fatalf("SaveRun() error = %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.CurrentRun(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("CurrentRun() = nil, want persisted run")
	}
	if got.Coordinator != wanted.Coordinator || got.StatusCommentID != wanted.StatusCommentID || got.SpecificationPacket != wanted.SpecificationPacket || len(got.PendingQuestions) != 1 || got.ClarificationCommentID != wanted.ClarificationCommentID || !got.ClarificationNotificationSent {
		t.Fatalf("persisted claim metadata = %#v, want coordinator/comment/packet/clarification state from %#v", got, wanted)
	}
}

// TestSchemaOneMigrationPreservesClaimMetadata verifies schema 1 databases
// receive the claim columns before the current store is reopened.
func TestSchemaOneMigrationPreservesClaimMetadata(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.ExecContext(t.Context(), `CREATE TABLE schema_metadata (singleton INTEGER PRIMARY KEY, version INTEGER NOT NULL);
		INSERT INTO schema_metadata(singleton, version) VALUES (1, 1);
		CREATE TABLE operational_runs (
			id TEXT PRIMARY KEY,
			repository_path TEXT NOT NULL,
			issue_number INTEGER NOT NULL,
			stage TEXT NOT NULL,
			status TEXT NOT NULL,
			branch TEXT NOT NULL DEFAULT '',
			worktree TEXT NOT NULL DEFAULT '',
			checkpoint_sha TEXT NOT NULL DEFAULT '',
			image_digest TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`)
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() migration error = %v", err)
	}
	wanted := store.Run{
		ID:                  "run-migrated",
		RepositoryPath:      "/work/repository",
		IssueNumber:         42,
		Stage:               store.StageClaim,
		Status:              store.StatusActive,
		Coordinator:         "host-a",
		StatusCommentID:     "12345",
		SpecificationPacket: `{"version":1,"issue":{"number":42}}`,
		CreatedAt:           time.Unix(100, 0).UTC(),
		UpdatedAt:           time.Unix(200, 0).UTC(),
	}
	if err := opened.SaveRun(context.Background(), wanted); err != nil {
		_ = opened.Close()
		t.Fatalf("SaveRun() after migration error = %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() after migration error = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.CurrentRun(context.Background())
	if err != nil {
		t.Fatalf("CurrentRun() error = %v", err)
	}
	if got == nil {
		t.Fatal("CurrentRun() = nil, want migrated run")
	}
	if got.Coordinator != wanted.Coordinator || got.StatusCommentID != wanted.StatusCommentID || got.SpecificationPacket != wanted.SpecificationPacket {
		t.Fatalf("migrated claim metadata = %#v, want %#v", got, wanted)
	}
}

// TestSchemaTwoMigrationReconcilesDuplicateActiveRuns verifies migration 3
// keeps the newest legacy run and makes older duplicates terminal before the
// active-run uniqueness index is created.
func TestSchemaTwoMigrationReconcilesDuplicateActiveRuns(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.ExecContext(t.Context(), `CREATE TABLE schema_metadata (singleton INTEGER PRIMARY KEY, version INTEGER NOT NULL);
		INSERT INTO schema_metadata(singleton, version) VALUES (1, 2);
		CREATE TABLE operational_runs (
			id TEXT PRIMARY KEY,
			repository_path TEXT NOT NULL,
			issue_number INTEGER NOT NULL,
			stage TEXT NOT NULL,
			status TEXT NOT NULL,
			branch TEXT NOT NULL DEFAULT '',
			worktree TEXT NOT NULL DEFAULT '',
			checkpoint_sha TEXT NOT NULL DEFAULT '',
			image_digest TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			coordinator TEXT NOT NULL DEFAULT '',
			status_comment_id TEXT NOT NULL DEFAULT '',
			specification_packet TEXT NOT NULL DEFAULT ''
		);
		INSERT INTO operational_runs (id, repository_path, issue_number, stage, status, created_at, updated_at)
		VALUES ('run-old', '/work/repository', 42, 'claim', 'active', '2026-08-20T10:00:00.1Z', '2026-08-20T10:00:00.1Z');
		INSERT INTO operational_runs (id, repository_path, issue_number, stage, status, created_at, updated_at)
		VALUES ('run-new', '/work/repository', 42, 'implementation', 'waiting_for_harness', '2026-08-20T10:00:00.12Z', '2026-08-20T10:00:00.12Z')`)
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	opened, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open() migration error = %v", err)
	}
	current, err := opened.CurrentRun(t.Context())
	if err != nil {
		_ = opened.Close()
		t.Fatalf("CurrentRun() error = %v", err)
	}
	wantUpdatedAt := time.Date(2026, 8, 20, 10, 0, 0, 120_000_000, time.UTC)
	if current == nil || current.ID != "run-new" || current.Status != store.StatusWaitingForHarness || !current.UpdatedAt.Equal(wantUpdatedAt) {
		_ = opened.Close()
		t.Fatalf("CurrentRun() = %#v, want newest .12Z run to remain active", current)
	}
	// Verify LatestRun also returns the correct run after timestamp normalization
	latest, err := opened.LatestRun(t.Context())
	if err != nil {
		_ = opened.Close()
		t.Fatalf("LatestRun() error = %v", err)
	}
	if latest == nil || latest.ID != "run-new" || !latest.UpdatedAt.Equal(wantUpdatedAt) {
		_ = opened.Close()
		t.Fatalf("LatestRun() = %#v, want run-new with normalized timestamp after migration", latest)
	}
	current.Status = store.StatusComplete
	if err := opened.SaveRun(t.Context(), *current); err != nil {
		_ = opened.Close()
		t.Fatalf("SaveRun() terminal update error = %v", err)
	}
	remaining, err := opened.CurrentRun(t.Context())
	if err != nil {
		_ = opened.Close()
		t.Fatalf("CurrentRun() after terminal update error = %v", err)
	}
	if remaining != nil {
		_ = opened.Close()
		t.Fatalf("CurrentRun() = %#v, want no legacy duplicate after reconciliation", remaining)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestSaveRunEnforcesOneActiveRunPerRepository verifies the store-level
// invariant that protects concurrent claimers from creating two active runs.
func TestSaveRunEnforcesOneActiveRunPerRepository(t *testing.T) {
	t.Parallel()

	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	base := store.Run{RepositoryPath: "/work/repository", Stage: store.StageClaim, Status: store.StatusActive}
	if err := opened.SaveRun(context.Background(), store.Run{ID: "run-1", RepositoryPath: base.RepositoryPath, Stage: base.Stage, Status: base.Status}); err != nil {
		t.Fatal(err)
	}
	if err := opened.SaveRun(context.Background(), store.Run{ID: "run-2", RepositoryPath: base.RepositoryPath, Stage: base.Stage, Status: base.Status}); err == nil {
		t.Fatal("SaveRun() accepted a second active run for the repository")
	}
	if err := opened.SaveRun(context.Background(), store.Run{ID: "run-2", RepositoryPath: base.RepositoryPath, Stage: base.Stage, Status: store.StatusFailed}); err != nil {
		t.Fatalf("SaveRun() terminal run error = %v", err)
	}
}

// TestRunTimestampOrderingUsesSubsecondPrecision verifies latest-run queries
// preserve chronological order when timestamps share the same second.
func TestRunTimestampOrderingUsesSubsecondPrecision(t *testing.T) {
	t.Parallel()

	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	base := time.Date(2026, 8, 20, 10, 11, 12, 0, time.UTC)
	for _, run := range []store.Run{
		{ID: "run-earlier", RepositoryPath: "/work/repository-a", Stage: store.StageClaim, Status: store.StatusComplete, UpdatedAt: base.Add(120 * time.Millisecond)},
		{ID: "run-later", RepositoryPath: "/work/repository-b", Stage: store.StageClaim, Status: store.StatusComplete, UpdatedAt: base.Add(900 * time.Millisecond)},
	} {
		if err := opened.SaveRun(context.Background(), run); err != nil {
			t.Fatal(err)
		}
	}
	got, err := opened.LatestRun(context.Background())
	if err != nil {
		t.Fatalf("LatestRun() error = %v", err)
	}
	if got == nil || got.ID != "run-later" {
		t.Fatalf("LatestRun() = %#v, want run-later", got)
	}
}

// TestLatestRunIncludesTerminalState verifies status can report the last
// claimed run after the active workflow has ended.
func TestLatestRunIncludesTerminalState(t *testing.T) {
	t.Parallel()

	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	wanted := store.Run{
		ID:             "run-complete",
		RepositoryPath: "/work/repository",
		IssueNumber:    42,
		Stage:          store.StageReady,
		Status:         store.StatusComplete,
		Branch:         "factory/run-complete",
		Worktree:       "/worktrees/run-complete",
		CreatedAt:      time.Unix(100, 0).UTC(),
		UpdatedAt:      time.Unix(200, 0).UTC(),
	}
	if err := opened.SaveRun(context.Background(), wanted); err != nil {
		t.Fatal(err)
	}
	got, err := opened.LatestRun(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != wanted.ID || got.Status != store.StatusComplete || got.Branch != wanted.Branch || got.Worktree != wanted.Worktree {
		t.Fatalf("LatestRun() = %#v, want terminal run %#v", got, wanted)
	}
}

// TestStorePersistsTheHumanReviewWatermarkAndPacket verifies the replay-safe
// GitHub review identity and the unbounded human repair packet survive a
// coordinator restart, and that the packet still rejects a missing identity.
func TestStorePersistsTheHumanReviewWatermarkAndPacket(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	finding := store.ReviewRepairFinding{ReviewerRole: "human", Finding: store.ReviewFinding{
		Location: "internal/factory/polling.go:42", Claim: "this branch never releases the queue",
		Evidence: "GitHub review 4001 submitted by alice", Severity: "blocker", Category: "specification",
		SuggestedResolution: "release queue ownership on the terminal transition", SuggestedOwner: "implementation",
	}}
	run := store.Run{
		ID:                      "run-human-review",
		RepositoryPath:          "/work/repository",
		Stage:                   store.StageImplementation,
		Status:                  store.StatusActive,
		ReviewRepairBudget:      2,
		ProcessedReviewID:       "4001",
		ProcessedReviewRevision: 7,
		Revision:                7,
		ReviewRepairPacket: &store.ReviewRepairPacket{
			Version: 1, Source: store.ReviewRepairSourceHuman, ReviewID: "4001",
			RunID: "run-human-review", CheckpointSHA: strings.Repeat("b", 64), Budget: 2,
			Findings: []store.ReviewRepairFinding{finding},
		},
		CheckpointSHA: strings.Repeat("b", 64),
		CreatedAt:     time.Unix(100, 0).UTC(), UpdatedAt: time.Unix(200, 0).UTC(),
	}
	if err := opened.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun() error = %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	reopened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.CurrentRun(context.Background())
	if err != nil {
		t.Fatalf("CurrentRun() error = %v", err)
	}
	if got == nil || got.ProcessedReviewID != "4001" || got.ProcessedReviewRevision != 7 {
		t.Fatalf("CurrentRun() = %#v, want the persisted human review watermark", got)
	}
	if got.ReviewRepairPacket == nil || got.ReviewRepairPacket.Source != store.ReviewRepairSourceHuman || got.ReviewRepairPacket.ReviewID != "4001" {
		t.Fatalf("CurrentRun() packet = %#v, want the human repair packet", got.ReviewRepairPacket)
	}

	anonymous := run
	anonymous.ID = "run-anonymous-human-review"
	packet := *run.ReviewRepairPacket
	packet.RunID = anonymous.ID
	packet.ReviewID = ""
	anonymous.ReviewRepairPacket = &packet
	if err := reopened.SaveRun(context.Background(), anonymous); err == nil || !strings.Contains(err.Error(), "must identify its GitHub review") {
		t.Fatalf("SaveRun() error = %v, want a refusal of an unidentified human repair packet", err)
	}
}
