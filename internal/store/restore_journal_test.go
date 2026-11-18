package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// storeWithRestoreJournal creates a current store and a restore journal
// beside it, and returns the store path.
func storeWithRestoreJournal(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "factory.db")
	opened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.RestoreJournalPath(path), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// assertRestoreInProgress fails unless err is a *RestoreInProgressError that
// names the restore command.
func assertRestoreInProgress(t *testing.T, opener string, err error) {
	t.Helper()
	var restore *store.RestoreInProgressError
	if !errors.As(err, &restore) || !strings.Contains(err.Error(), "factory checkpoint restore") {
		t.Fatalf("%s error = %v, want a *RestoreInProgressError that names factory checkpoint restore", opener, err)
	}
}

// TestStoreOpenersRefuseWhileARestoreJournalExists verifies that no store
// opener, including the read-only diagnosis opener, opens a store whose
// files a restore may have split.
func TestStoreOpenersRefuseWhileARestoreJournalExists(t *testing.T) {
	t.Parallel()
	path := storeWithRestoreJournal(t)
	ctx := context.Background()

	_, err := store.Open(ctx, path)
	assertRestoreInProgress(t, "Open", err)
	_, err = store.OpenReadOnly(ctx, path)
	assertRestoreInProgress(t, "OpenReadOnly", err)
	_, err = store.OpenCommandInbox(ctx, path)
	assertRestoreInProgress(t, "OpenCommandInbox", err)
	_, err = store.InspectForUpgrade(ctx, path)
	assertRestoreInProgress(t, "InspectForUpgrade", err)
	_, err = store.Snapshot(ctx, path, filepath.Join(t.TempDir(), "snapshot.db"), nil)
	assertRestoreInProgress(t, "Snapshot", err)
}

// TestStoreOpenRefusesARestoreJournalWithoutAStore verifies that Open does
// not create a new store while a restore journal exists, because the live
// store may be in the restore work directory.
func TestStoreOpenRefusesARestoreJournalWithoutAStore(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "factory.db")
	if err := os.WriteFile(store.RestoreJournalPath(path), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := store.Open(context.Background(), path)
	assertRestoreInProgress(t, "Open", err)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Open created a store while a restore journal exists: %v", err)
	}
}

// TestStartupCheckReportsARestoreInProgress verifies that factory doctor
// reports the restore journal with its own failure code and names the
// restore command.
func TestStartupCheckReportsARestoreInProgress(t *testing.T) {
	t.Parallel()
	path := storeWithRestoreJournal(t)
	result := doctor.Run(context.Background(), store.StartupCheck(path)).Results[0]
	if result.Status != doctor.StatusFailed || result.Code != "store.sqlite.restore_in_progress" || !strings.Contains(result.Action, "factory checkpoint restore") {
		t.Fatalf("SQLite result = %#v, want store.sqlite.restore_in_progress with the restore command", result)
	}
}
