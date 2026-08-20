package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
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
	current, err := opened.CurrentRun(context.Background())
	if err != nil {
		t.Fatalf("CurrentRun() error = %v", err)
	}
	if current != nil {
		t.Fatalf("CurrentRun() = %#v, want empty store", current)
	}
}

func TestOpenRejectsANewerSchemaVersion(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "factory.db")
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

func TestOpenBacksUpBeforeApplyingAnOlderMigration(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "factory.db")
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

func TestCurrentRunReturnsThePersistedOperationalState(t *testing.T) {
	t.Parallel()

	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "factory.db"))
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
