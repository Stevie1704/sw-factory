package store_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// appendProcessEnvironment names the store into which the test binary,
// started again by TestRestoreWriteLockBlocksAnInboxOfAnotherProcess, appends
// one command.
const appendProcessEnvironment = "FACTORY_TEST_APPEND_STORE"

// TestAppendProcess is the other process of the restore lock test. It runs
// only when the environment names a store: it appends one command through a
// command inbox and exits 3 when the store is busy.
func TestAppendProcess(t *testing.T) {
	path := os.Getenv(appendProcessEnvironment)
	if path == "" {
		t.Skip("runs only as the other process of the restore lock test")
	}
	store.SetInboxBusyTimeoutForTest(50 * time.Millisecond)
	inbox, err := store.OpenCommandInbox(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inbox.Close() }()
	if _, _, err := inbox.AcceptIdeationCommand(t.Context(), commandForTest("request-1", "session-1", 1)); store.IsTransient(err) {
		os.Exit(3)
	} else if err != nil {
		t.Fatal(err)
	}
}

// appendInOtherProcess appends one command to the store at path from a new
// process and reports whether the store was busy.
func appendInOtherProcess(t *testing.T, path string) (busy bool) {
	t.Helper()
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAppendProcess$", "-test.count=1")
	command.Env = append(os.Environ(), appendProcessEnvironment+"="+path)
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 3 {
		return true
	}
	if err != nil {
		t.Fatalf("append process: %v\n%s", err, output)
	}
	return false
}

// TestRestoreWriteLockBlocksAnInboxOfAnotherProcess verifies that the
// restore write lock stops the command inbox of another process from
// committing, also after the lock read the size and SHA-256 of the live
// store: its handle stays open, because a close of any handle of the file
// drops the POSIX lock of SQLite for the whole process. After the release,
// the inbox commits again.
func TestRestoreWriteLockBlocksAnInboxOfAnotherProcess(t *testing.T) {
	t.Parallel()
	opened, path := openIdeationStoreForTest(t)
	createIdeationSessionForTest(t, opened, "session-1")
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	lock, err := store.LockForRestore(t.Context(), path)
	if err != nil || lock == nil {
		t.Fatalf("LockForRestore() = %v, %v; want a held lock", lock, err)
	}
	size, digest, err := lock.FileDigest(path)
	if err != nil || size == 0 || len(digest) != 64 {
		t.Fatalf("lock.FileDigest() = %d, %q, %v", size, digest, err)
	}
	if !appendInOtherProcess(t, path) {
		t.Fatal("another process appended under the restore write lock")
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("second Release() error = %v", err)
	}
	if appendInOtherProcess(t, path) {
		t.Fatal("the store stays busy after the release")
	}
}

// TestLockForRestoreOfAMissingOrDamagedStore verifies that a missing or
// damaged store needs no write lock: no writer can commit into it.
func TestLockForRestoreOfAMissingOrDamagedStore(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(privateDirectory(t), "factory.db")
	damaged := filepath.Join(privateDirectory(t), "factory.db")
	if err := os.WriteFile(damaged, []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"missing": missing, "damaged": damaged} {
		lock, err := store.LockForRestore(t.Context(), path)
		if err != nil || lock != nil {
			t.Fatalf("%s: LockForRestore() = %v, %v; want no lock", name, lock, err)
		}
		if err := lock.Release(); err != nil {
			t.Fatalf("%s: Release() of no lock = %v", name, err)
		}
		if _, err := os.Lstat(path + "-journal"); !os.IsNotExist(err) {
			t.Fatalf("%s: LockForRestore() created a journal: %v", name, err)
		}
	}
}

// TestCommandInboxDoesNotCommitIntoAMovedStore verifies the SQLite
// behaviour that restore relies on after the preserve renames: an
// already-open inbox refuses to write a store file that was moved away
// (SQLITE_READONLY_DBMOVED), so its command never goes only into the
// preserved original.
func TestCommandInboxDoesNotCommitIntoAMovedStore(t *testing.T) {
	t.Parallel()
	opened, path := openIdeationStoreForTest(t)
	session := createIdeationSessionForTest(t, opened, "session-1")
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	inbox, err := store.OpenCommandInbox(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inbox.Close() })
	preserved := filepath.Join(filepath.Dir(path), "preserved.db")
	if err := os.Rename(path, preserved); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(preserved)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := inbox.AcceptIdeationCommand(t.Context(), commandForTest("request-1", session.ID, 1)); err == nil {
		t.Fatal("append into a moved store succeeded")
	}
	after, err := os.ReadFile(preserved)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("the moved store changed: %v", err)
	}
}
