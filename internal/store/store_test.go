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
		ID:                  "run-claim",
		RepositoryPath:      "/work/repository",
		IssueNumber:         42,
		Stage:               store.StageClaim,
		Status:              store.StatusActive,
		Branch:              "factory/run-claim",
		Worktree:            "/worktrees/run-claim",
		CheckpointSHA:       "0123456789abcdef",
		ImageDigest:         "sha256:worker",
		Coordinator:         "host-a",
		StatusCommentID:     "12345",
		SpecificationPacket: `{"version":1,"issue":{"number":42}}`,
		CreatedAt:           time.Unix(100, 0).UTC(),
		UpdatedAt:           time.Unix(200, 0).UTC(),
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
	if got.Coordinator != wanted.Coordinator || got.StatusCommentID != wanted.StatusCommentID || got.SpecificationPacket != wanted.SpecificationPacket {
		t.Fatalf("persisted claim metadata = %#v, want coordinator/comment/packet from %#v", got, wanted)
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
