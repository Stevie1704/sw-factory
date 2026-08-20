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
	defer opened.Close()

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
	if _, err := database.Exec(`CREATE TABLE schema_metadata (singleton INTEGER PRIMARY KEY, version INTEGER NOT NULL); INSERT INTO schema_metadata(singleton, version) VALUES (1, 99);`); err != nil {
		database.Close()
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
	if _, err := database.Exec(`CREATE TABLE schema_metadata (singleton INTEGER PRIMARY KEY, version INTEGER NOT NULL); INSERT INTO schema_metadata(singleton, version) VALUES (1, 0);`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer opened.Close()
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

func TestOpenRejectsAnEmptyPath(t *testing.T) {
	t.Parallel()

	_, err := store.Open(context.Background(), "")
	if err == nil {
		t.Fatal("Open() succeeded with an empty path")
	}
	if !strings.Contains(err.Error(), "path is required") {
		t.Fatalf("error = %v, want a path-required diagnostic", err)
	}
}

func TestOpenRejectsAPathThatIsADirectory(t *testing.T) {
	t.Parallel()

	privateParent := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(privateParent, 0o700); err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(privateParent, "factory.db")
	if err := os.Mkdir(storePath, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := store.Open(context.Background(), storePath)
	if err == nil {
		t.Fatal("Open() succeeded when the path is a directory")
	}
	if !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("error = %v, want a directory diagnostic", err)
	}
}

func TestStorePathReturnsTheResolvedAbsolutePath(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	want, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := opened.Path(); got != want {
		t.Fatalf("Path() = %q, want %q", got, want)
	}
}

func TestSaveRunRequiresAnID(t *testing.T) {
	t.Parallel()

	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	err = opened.SaveRun(context.Background(), store.Run{RepositoryPath: "/work/repository", Status: store.StatusActive})
	if err == nil || !strings.Contains(err.Error(), "id is required") {
		t.Fatalf("SaveRun() error = %v, want an id-required diagnostic", err)
	}
}

func TestSaveRunRequiresARepositoryPath(t *testing.T) {
	t.Parallel()

	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	err = opened.SaveRun(context.Background(), store.Run{ID: "run-1", Status: store.StatusActive})
	if err == nil || !strings.Contains(err.Error(), "repository path is required") {
		t.Fatalf("SaveRun() error = %v, want a repository-path-required diagnostic", err)
	}
}

func TestSaveRunRequiresAStatus(t *testing.T) {
	t.Parallel()

	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	err = opened.SaveRun(context.Background(), store.Run{ID: "run-1", RepositoryPath: "/work/repository"})
	if err == nil || !strings.Contains(err.Error(), "status is required") {
		t.Fatalf("SaveRun() error = %v, want a status-required diagnostic", err)
	}
}

func TestSaveRunUpdatesAnExistingRunInPlace(t *testing.T) {
	t.Parallel()

	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	initial := store.Run{
		ID:             "run-1",
		RepositoryPath: "/work/repository",
		Stage:          store.StageClaim,
		Status:         store.StatusActive,
		CreatedAt:      time.Unix(100, 0).UTC(),
		UpdatedAt:      time.Unix(100, 0).UTC(),
	}
	if err := opened.SaveRun(context.Background(), initial); err != nil {
		t.Fatalf("SaveRun() error = %v", err)
	}

	updated := initial
	updated.Stage = store.StageReady
	updated.Status = store.StatusComplete
	updated.UpdatedAt = time.Unix(500, 0).UTC()
	if err := opened.SaveRun(context.Background(), updated); err != nil {
		t.Fatalf("second SaveRun() error = %v", err)
	}

	got, err := opened.CurrentRun(context.Background())
	if err != nil {
		t.Fatalf("CurrentRun() error = %v", err)
	}
	if got != nil {
		t.Fatalf("CurrentRun() = %#v, want nil since the only run is now complete", got)
	}
}

func TestCurrentRunExcludesTerminalStatusesRegardlessOfRecency(t *testing.T) {
	t.Parallel()

	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	runs := []store.Run{
		{
			ID:             "run-old-active",
			RepositoryPath: "/work/repository",
			Status:         store.StatusActive,
			CreatedAt:      time.Unix(100, 0).UTC(),
			UpdatedAt:      time.Unix(100, 0).UTC(),
		},
		{
			ID:             "run-newer-waiting",
			RepositoryPath: "/work/repository",
			Status:         store.StatusWaitingForHuman,
			CreatedAt:      time.Unix(200, 0).UTC(),
			UpdatedAt:      time.Unix(200, 0).UTC(),
		},
		{
			ID:             "run-newest-complete",
			RepositoryPath: "/work/repository",
			Status:         store.StatusComplete,
			CreatedAt:      time.Unix(300, 0).UTC(),
			UpdatedAt:      time.Unix(300, 0).UTC(),
		},
	}
	for _, run := range runs {
		if err := opened.SaveRun(context.Background(), run); err != nil {
			t.Fatalf("SaveRun(%s) error = %v", run.ID, err)
		}
	}

	got, err := opened.CurrentRun(context.Background())
	if err != nil {
		t.Fatalf("CurrentRun() error = %v", err)
	}
	if got == nil || got.ID != "run-newer-waiting" {
		t.Fatalf("CurrentRun() = %#v, want the most recent non-terminal run", got)
	}
}

func TestCurrentRunReturnsThePersistedOperationalState(t *testing.T) {
	t.Parallel()

	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
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
