package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
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

// TestSupervisorHeartbeatRoundTripsAndExpires verifies the durable liveness
// projection remains readable after restart and becomes stale at its expiry.
func TestSupervisorHeartbeatRoundTripsAndExpires(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	wanted := store.SupervisorHeartbeat{
		Coordinator: "host-a",
		PID:         1234,
		StartedAt:   now,
		RenewedAt:   now.Add(time.Minute),
		ExpiresAt:   now.Add(6 * time.Minute),
	}
	if err := opened.SaveSupervisorHeartbeat(t.Context(), wanted); err != nil {
		_ = opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restarted.Close() }()
	got, err := restarted.ReadSupervisorHeartbeat(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Coordinator != wanted.Coordinator || got.PID != wanted.PID || !got.StartedAt.Equal(wanted.StartedAt) || !got.RenewedAt.Equal(wanted.RenewedAt) || !got.ExpiresAt.Equal(wanted.ExpiresAt) {
		t.Fatalf("heartbeat = %#v, want %#v", got, wanted)
	}
	if !got.Live(now.Add(2 * time.Minute)) {
		t.Fatal("heartbeat is not live before expiry")
	}
	if got.Live(wanted.ExpiresAt) {
		t.Fatal("heartbeat is live at its expiry")
	}
}

// TestSupervisorHeartbeatRecordsAStoppedCoordinator verifies that a clean
// coordinator stop can record a heartbeat that expires at its renewal: the
// coordinator stays observable and is not live, while an expiry before the
// renewal is still refused.
func TestSupervisorHeartbeatRecordsAStoppedCoordinator(t *testing.T) {
	t.Parallel()

	opened, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	stoppedAt := time.Date(2026, 9, 17, 10, 5, 0, 0, time.UTC)
	stopped := store.SupervisorHeartbeat{Coordinator: "host-a", PID: 1234, StartedAt: stoppedAt.Add(-time.Hour), RenewedAt: stoppedAt, ExpiresAt: stoppedAt}
	if err := opened.SaveSupervisorHeartbeat(t.Context(), stopped); err != nil {
		t.Fatalf("SaveSupervisorHeartbeat(stopped) error = %v", err)
	}
	got, err := opened.ReadSupervisorHeartbeat(t.Context())
	if err != nil || got == nil || got.Coordinator != "host-a" || got.Live(stoppedAt) {
		t.Fatalf("heartbeat = %#v, %v; want the stopped coordinator, not live", got, err)
	}
	stopped.ExpiresAt = stoppedAt.Add(-time.Second)
	if err := opened.SaveSupervisorHeartbeat(t.Context(), stopped); err == nil {
		t.Fatal("SaveSupervisorHeartbeat() accepted an expiry before the renewal")
	}
}

// TestSupervisorHeartbeatAllowsWallClockRollback verifies a host clock step
// does not make a valid renewal unpersistable merely because it predates the
// process start timestamp.
func TestSupervisorHeartbeatAllowsWallClockRollback(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()

	startedAt := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	heartbeat := store.SupervisorHeartbeat{
		Coordinator: "host-a",
		PID:         1234,
		StartedAt:   startedAt,
		RenewedAt:   startedAt.Add(-3 * time.Second),
		ExpiresAt:   startedAt.Add(5 * time.Minute),
	}
	if err := opened.SaveSupervisorHeartbeat(t.Context(), heartbeat); err != nil {
		t.Fatalf("SaveSupervisorHeartbeat() error = %v, want clock rollback to be accepted", err)
	}
}

func TestCurrentInvocationSchemaContainsNoTerminalProjection(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	for _, retired := range []string{"workspace_id", "status_surface_id", "role_surface_id", "implementation_surface_id", "checks_surface_id", "attach_required"} {
		if testInvocationColumnExists(t, database, retired) {
			t.Fatalf("current invocations table still contains retired column %q", retired)
		}
	}
	for _, retired := range []string{"lifecycle_notification_sent", "ready_notification_sent", "clarification_notification_sent"} {
		if testOperationalRunsColumnExists(t, database, retired) {
			t.Fatalf("current operational_runs table still contains retired column %q", retired)
		}
	}
	var notificationTables int
	if err := database.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'lifecycle_notifications'`).Scan(&notificationTables); err != nil {
		t.Fatalf("inspect lifecycle notification table: %v", err)
	}
	if notificationTables != 0 {
		t.Fatal("current schema still contains retired lifecycle_notifications table")
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

// testInvocationColumnExists reports whether a column exists in the
// migration fixture's invocations table.
func testInvocationColumnExists(t *testing.T, database *sql.DB, wanted string) bool {
	t.Helper()
	rows, err := database.QueryContext(t.Context(), "PRAGMA table_info(invocations)")
	if err != nil {
		t.Fatalf("inspect invocations columns: %v", err)
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
			t.Fatalf("scan invocations column: %v", err)
		}
		if name == wanted {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate invocations columns: %v", err)
	}
	return false
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
		ID:                "run-terminal",
		RepositoryPath:    "/work/repository",
		IssueNumber:       42,
		Stage:             store.StageReady,
		Status:            store.StatusComplete,
		PullRequestNumber: 17,
		PullRequestURL:    "https://github.com/example/project/pull/17",
		MergeCommitSHA:    "0123456789abcdef0123456789abcdef0123456789",
		LifecycleReason:   "pull request #17 merged",
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
	if latest == nil || latest.MergeCommitSHA != run.MergeCommitSHA || latest.LifecycleReason != run.LifecycleReason {
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
		ID:                                  "run-test-projection",
		RepositoryPath:                      "/work/repository",
		Stage:                               store.StageImplementation,
		Status:                              store.StatusActive,
		CheckpointSHA:                       strings.Repeat("b", 64),
		BaseCheckpointSHA:                   strings.Repeat("a", 64),
		AcceptedImplementationCheckpointSHA: strings.Repeat("d", 64),
		TestCheckpointSHA:                   strings.Repeat("b", 64),
		TestHandoff:                         &store.TestHandoff{AcceptanceCoverage: []store.TestAcceptanceCoverage{{Criterion: "criterion", Evidence: "red test"}}, ChangedFiles: []string{"internal/factory/agent_test.go"}, FocusedTestCommand: "go test ./internal/factory -run TestBehavior", ExpectedFailureReason: "expected behavior assertion", ObservedFailureEvidence: []store.TestFailureEvidence{{Kind: "exit_code", Detail: "1"}}},
		TestExemption:                       &store.TestExemption{Kind: "technical", Justification: "test runtime unavailable during pilot"},
		ProtectedTestPaths:                  []store.ProtectedTestPath{{Path: "internal/factory/agent_test.go", SHA256: strings.Repeat("c", 64)}},
		TestStageSkipped:                    true,
		CheckRepairBudget:                   1,
	}
	if err := opened.SaveRun(context.Background(), want); err != nil {
		t.Fatalf("SaveRun() error = %v", err)
	}
	got, err := opened.LatestRun(context.Background())
	if err != nil {
		t.Fatalf("LatestRun() error = %v", err)
	}
	if got == nil || got.BaseCheckpointSHA != want.BaseCheckpointSHA || got.AcceptedImplementationCheckpointSHA != want.AcceptedImplementationCheckpointSHA || got.TestCheckpointSHA != want.TestCheckpointSHA || got.TestHandoff == nil || got.TestExemption == nil || len(got.ProtectedTestPaths) != 1 || !got.TestStageSkipped {
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
		SpecificationReview: &store.SpecificationReview{Outcome: "completed",
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

// TestRunPersistsIncompleteReviewDispositionAndFindingsAcrossRestart verifies
// an interrupted review axis retains its outcome context and findings in the
// durable projection.
func TestRunPersistsIncompleteReviewDispositionAndFindingsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := strings.Repeat("c", 64)
	want := store.Run{
		ID: "run-incomplete-review", RepositoryPath: "/work/repository", Stage: store.StageReview,
		Status: store.StatusWaitingForHuman, CheckpointSHA: checkpoint,
		SpecificationReview: &store.SpecificationReview{
			CheckpointSHA: checkpoint,
			Outcome:       "cannot_proceed",
			Summary:       "review stopped after establishing a blocker",
			Evidence:      []store.ReviewEvidence{{Kind: "artifact", Detail: "the required artifact is unavailable"}},
			Findings: []store.ReviewFinding{{
				Location: "internal/factory/review.go:42", Claim: "the behavior is incorrect", Evidence: "the observed behavior fails",
				Severity: "blocker", Category: "correctness", SuggestedResolution: "repair the behavior", SuggestedOwner: "implementation",
			}},
		},
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
	if got == nil || got.SpecificationReview == nil || got.SpecificationReview.Outcome != "cannot_proceed" || got.SpecificationReview.Summary != want.SpecificationReview.Summary || len(got.SpecificationReview.Findings) != 1 || len(got.SpecificationReview.Evidence) != 1 {
		t.Fatalf("CurrentRun() = %#v, want incomplete review disposition and finding", got)
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
		SpecificationReview: &store.SpecificationReview{Outcome: "completed",
			CheckpointSHA: strings.Repeat("a", 64),
		},
	}
	if err := opened.SaveRun(context.Background(), run); err == nil || !strings.Contains(err.Error(), "match the run checkpoint") {
		t.Fatalf("SaveRun() error = %v, want stale-review rejection", err)
	}
}

// TestStorePersistsCheckRepairBudget verifies the retry budget, consumed
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
		ReviewRepairPacket: &store.ReviewRepairPacket{Source: "factory",
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

// TestStorePersistsRepositoryChosenRetryBudgets verifies the frozen per-run
// budgets are the only bound the store applies. Review-repair and
// test-revision budgets, and both attempt histories, far above every
// previously hard-coded ceiling must survive a restart unchanged.
func TestStorePersistsRepositoryChosenRetryBudgets(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	history := make([]store.ReviewRepairAttempt, 0, 6)
	for attempt := 1; attempt <= 6; attempt++ {
		history = append(history, store.ReviewRepairAttempt{Attempt: attempt, Outcome: store.ReviewRepairStarted})
	}
	revisions := make([]store.TestRevision, 0, 4)
	for attempt := 1; attempt <= 4; attempt++ {
		revisions = append(revisions, store.TestRevision{Attempt: attempt, Outcome: store.TestRevisionRejected})
	}
	run := store.Run{
		ID:                   "run-repository-budgets",
		RepositoryPath:       "/work/repository",
		Stage:                store.StageImplementation,
		Status:               store.StatusActive,
		CheckRepairBudget:    7,
		ReviewRepairAttempts: 6,
		ReviewRepairBudget:   12,
		ReviewRepairHistory:  history,
		TestRevisionAttempts: 4,
		TestRevisionBudget:   9,
		TestRevisionHistory:  revisions,
		CheckpointSHA:        strings.Repeat("b", 64),
		BaseCheckpointSHA:    strings.Repeat("c", 64),
		CreatedAt:            time.Unix(100, 0).UTC(), UpdatedAt: time.Unix(200, 0).UTC(),
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
	if got == nil || got.CheckRepairBudget != 7 || got.ReviewRepairBudget != 12 || got.TestRevisionBudget != 9 || len(got.ReviewRepairHistory) != 6 || len(got.TestRevisionHistory) != 4 {
		t.Fatalf("CurrentRun() = %#v, want repository-chosen budgets preserved", got)
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
		ReviewRepairPacket: &store.ReviewRepairPacket{Source: "factory",
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
		ID:                     "run-claim",
		RepositoryPath:         "/work/repository",
		IssueNumber:            42,
		Stage:                  store.StageClaim,
		Status:                 store.StatusActive,
		Branch:                 "factory/run-claim",
		Worktree:               "/worktrees/run-claim",
		CheckpointSHA:          "0123456789abcdef",
		ImageDigest:            "sha256:worker",
		Coordinator:            "host-a",
		StatusCommentID:        "12345",
		SpecificationPacket:    `{"version":1,"issue":{"number":42}}`,
		PendingQuestions:       []store.PendingQuestion{{ID: "clarification-1", Prompt: "Which behavior is intended?"}},
		ClarificationCommentID: "67890",
		CreatedAt:              time.Unix(100, 0).UTC(),
		UpdatedAt:              time.Unix(200, 0).UTC(),
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
	if got.Coordinator != wanted.Coordinator || got.StatusCommentID != wanted.StatusCommentID || got.SpecificationPacket != wanted.SpecificationPacket || len(got.PendingQuestions) != 1 || got.ClarificationCommentID != wanted.ClarificationCommentID {
		t.Fatalf("persisted claim metadata = %#v, want coordinator/comment/packet/clarification state from %#v", got, wanted)
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
// maintainer event identity and the unbounded human repair packet survive a
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
			Version: 1, Source: store.ReviewRepairSourceHuman, SourceEventKind: store.ReviewRepairEventPullRequestReview, SourceEventID: "4001",
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
	if got.ReviewRepairPacket == nil || got.ReviewRepairPacket.Source != store.ReviewRepairSourceHuman || got.ReviewRepairPacket.SourceEventID != "4001" {
		t.Fatalf("CurrentRun() packet = %#v, want the human repair packet", got.ReviewRepairPacket)
	}

	anonymous := run
	anonymous.ID = "run-anonymous-human-review"
	packet := *run.ReviewRepairPacket
	packet.RunID = anonymous.ID
	packet.SourceEventID = ""
	anonymous.ReviewRepairPacket = &packet
	if err := reopened.SaveRun(context.Background(), anonymous); err == nil || !strings.Contains(err.Error(), "must identify its maintainer event") {
		t.Fatalf("SaveRun() error = %v, want a refusal of an unidentified human repair packet", err)
	}
}

// TestReviewRepairPacketSourceEventReadsEveryPersistedShape verifies durable
// replay preserves provenance for a pull-request review, a supervision
// command. Pre-baseline review identities do not confer provenance.
func TestReviewRepairPacketSourceEventReadsEveryPersistedShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		encoded      string
		wantKind     store.ReviewRepairSourceEvent
		wantIdentity string
	}{
		{
			name:         "legacy review identity only",
			encoded:      `{"version":1,"source":"human","review_id":"4001"}`,
			wantKind:     "",
			wantIdentity: "",
		},
		{
			name:         "pull request review event",
			encoded:      `{"version":1,"source":"human","source_event_kind":"pull_request_review","source_event_id":"4001"}`,
			wantKind:     store.ReviewRepairEventPullRequestReview,
			wantIdentity: "4001",
		},
		{
			name:         "supervision command event",
			encoded:      `{"version":1,"source":"human","source_event_kind":"supervision_command","source_event_id":"5539017380"}`,
			wantKind:     store.ReviewRepairEventSupervisionCommand,
			wantIdentity: "5539017380",
		},
		{
			name:         "factory packet has no maintainer event",
			encoded:      `{"version":1,"attempt":1}`,
			wantKind:     "",
			wantIdentity: "",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			var packet store.ReviewRepairPacket
			if err := json.Unmarshal([]byte(testCase.encoded), &packet); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			kind, identity := packet.SourceEvent()
			if kind != testCase.wantKind || identity != testCase.wantIdentity {
				t.Fatalf("SourceEvent() = %q/%q, want %q/%q", kind, identity, testCase.wantKind, testCase.wantIdentity)
			}
		})
	}
}

// TestStorePersistsACommandSourcedHumanRepairPacket verifies a packet whose
// provenance is a supervision comment survives a coordinator restart with its
// source kind and identity.
func TestStorePersistsACommandSourcedHumanRepairPacket(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = opened.Close() }()
	finding := store.ReviewRepairFinding{ReviewerRole: "human", Finding: store.ReviewFinding{
		Location: "pull request #140", Claim: "validate permitted paths before adoption",
		Evidence: "GitHub comment 5539017380 submitted by alice", Severity: "blocker", Category: "specification",
		SuggestedResolution: "validate permitted paths before adoption", SuggestedOwner: "implementation",
	}}
	run := store.Run{
		ID:                 "run-command-repair",
		RepositoryPath:     "/work/repository",
		Stage:              store.StageImplementation,
		Status:             store.StatusActive,
		ReviewRepairBudget: 2,
		Revision:           3,
		ReviewRepairPacket: &store.ReviewRepairPacket{
			Version: 1, Source: store.ReviewRepairSourceHuman,
			SourceEventKind: store.ReviewRepairEventSupervisionCommand, SourceEventID: "5539017380",
			RunID: "run-command-repair", CheckpointSHA: strings.Repeat("c", 64), Budget: 2,
			Findings: []store.ReviewRepairFinding{finding},
		},
		CheckpointSHA: strings.Repeat("c", 64),
		CreatedAt:     time.Unix(100, 0).UTC(), UpdatedAt: time.Unix(200, 0).UTC(),
	}
	if err := opened.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun() error = %v", err)
	}
	got, err := opened.CurrentRun(context.Background())
	if err != nil {
		t.Fatalf("CurrentRun() error = %v", err)
	}
	if got == nil || got.ReviewRepairPacket == nil {
		t.Fatalf("CurrentRun() = %#v, want the persisted command repair packet", got)
	}
	kind, identity := got.ReviewRepairPacket.SourceEvent()
	if kind != store.ReviewRepairEventSupervisionCommand || identity != "5539017380" {
		t.Fatalf("SourceEvent() = %q/%q, want the supervision command provenance", kind, identity)
	}
}
