package checkpoint_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/checkpoint"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// liveSidecars are the sidecars that the restore fixture places beside the
// live store. Their bytes are arbitrary: restore never opens them.
var liveSidecars = []string{"-journal", "-wal", "-shm"}

// restoreFixture is a live store with three sidecars and a sealed checkpoint
// of another store.
type restoreFixture struct {
	storePath  string
	checkpoint string
	manifest   checkpoint.Manifest
	original   map[string][]byte
}

// newRestoreFixture writes the live file set and a sealed checkpoint.
func newRestoreFixture(t *testing.T) restoreFixture {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := restoreFixture{storePath: filepath.Join(directory, "factory.db"), checkpoint: sealedCheckpoint(t), original: map[string][]byte{}}
	manifest, err := checkpoint.Verify(context.Background(), fixture.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	fixture.manifest = manifest
	fixture.original["factory.db"] = []byte("the original live store bytes")
	for _, suffix := range liveSidecars {
		fixture.original["factory.db"+suffix] = []byte("the original sidecar " + suffix)
	}
	for name, content := range fixture.original {
		if err := os.WriteFile(filepath.Join(directory, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}

// plan returns the restore plan of the fixture.
func (f restoreFixture) plan(t *testing.T) checkpoint.RestorePlan {
	t.Helper()
	plan, err := checkpoint.NewRestorePlan(f.checkpoint, f.manifest, f.storePath)
	if err != nil {
		t.Fatalf("NewRestorePlan() error = %v", err)
	}
	return plan
}

// liveFiles returns the bytes of every file in the store directory, except
// the restore work directory.
func (f restoreFixture) liveFiles(t *testing.T) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(f.storePath))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		content, err := os.ReadFile(filepath.Join(filepath.Dir(f.storePath), entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = content
	}
	return files
}

// assertOriginal fails unless the live file set is byte for byte the
// original one and no restore file remains.
func (f restoreFixture) assertOriginal(t *testing.T, point string) {
	t.Helper()
	live := f.liveFiles(t)
	if len(live) != len(f.original) {
		t.Fatalf("%s: live files = %v, want exactly the original file set", point, keys(live))
	}
	for name, content := range f.original {
		if !bytes.Equal(live[name], content) {
			t.Fatalf("%s: %s differs from the original", point, name)
		}
	}
	if _, err := os.Stat(f.plan(t).WorkDirectory); !os.IsNotExist(err) {
		t.Fatalf("%s: the work directory remains after a rollback: %v", point, err)
	}
}

// assertReplaced fails unless the live store is the verified replacement,
// no live sidecar remains, and the complete original is preserved.
func (f restoreFixture) assertReplaced(t *testing.T, point string) {
	t.Helper()
	live := f.liveFiles(t)
	snapshot, err := os.ReadFile(filepath.Join(f.checkpoint, checkpoint.StoreFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || !bytes.Equal(live["factory.db"], snapshot) {
		t.Fatalf("%s: live files = %v, want only the replacement store", point, keys(live))
	}
	original := filepath.Join(f.plan(t).WorkDirectory, "original")
	for name, content := range f.original {
		preserved, err := os.ReadFile(filepath.Join(original, name))
		if err != nil || !bytes.Equal(preserved, content) {
			t.Fatalf("%s: the preserved original %s = %v; want the original bytes", point, name, err)
		}
	}
}

// keys returns the sorted names of a file map.
func keys(files map[string][]byte) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// TestApplyRestoreReplacesTheStoreAndPreservesTheOriginal verifies a
// complete restore: the replacement is live, the original file set is in
// original/, and no restore journal remains.
func TestApplyRestoreReplacesTheStoreAndPreservesTheOriginal(t *testing.T) {
	t.Parallel()
	fixture := newRestoreFixture(t)
	plan := fixture.plan(t)
	if plan.LiveFiles[0] != "factory.db" || len(plan.LiveFiles) != 4 {
		t.Fatalf("live files = %v, want the store first, then each sidecar", plan.LiveFiles)
	}
	if err := checkpoint.ApplyRestore(context.Background(), plan, checkpoint.Factory{}); err != nil {
		t.Fatalf("ApplyRestore() error = %v", err)
	}
	fixture.assertReplaced(t, "complete")
	if _, err := os.Stat(store.RestoreJournalPath(fixture.storePath)); !os.IsNotExist(err) {
		t.Fatalf("the restore journal remains: %v", err)
	}
	info, err := os.Stat(plan.WorkDirectory)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("work directory = %v, %v; want mode 0700", info, err)
	}
}

// TestApplyRestoreWithoutALiveStore verifies a restore into a store
// directory that has no live file.
func TestApplyRestoreWithoutALiveStore(t *testing.T) {
	t.Parallel()
	fixture := newRestoreFixture(t)
	for name := range fixture.original {
		if err := os.Remove(filepath.Join(filepath.Dir(fixture.storePath), name)); err != nil {
			t.Fatal(err)
		}
	}
	plan := fixture.plan(t)
	if len(plan.LiveFiles) != 0 {
		t.Fatalf("live files = %v, want none", plan.LiveFiles)
	}
	if err := checkpoint.ApplyRestore(context.Background(), plan, checkpoint.Factory{}); err != nil {
		t.Fatalf("ApplyRestore() error = %v", err)
	}
	if _, err := store.OpenReadOnly(context.Background(), fixture.storePath); err != nil {
		t.Fatalf("OpenReadOnly() after the restore error = %v", err)
	}
}

// TestApplyRestoreRollsBackAReplacementThatDoesNotVerify verifies that a
// replacement that differs from the manifest is never left live.
func TestApplyRestoreRollsBackAReplacementThatDoesNotVerify(t *testing.T) {
	t.Parallel()
	fixture := newRestoreFixture(t)
	plan := fixture.plan(t)
	plan.Replacement.SHA256 = "0000"
	if err := checkpoint.ApplyRestore(context.Background(), plan, checkpoint.Factory{}); err == nil {
		t.Fatal("ApplyRestore() of a replacement that does not verify succeeded")
	}
	fixture.assertOriginal(t, "failed verification")
	if _, err := os.Stat(store.RestoreJournalPath(fixture.storePath)); !os.IsNotExist(err) {
		t.Fatalf("the restore journal remains after the rollback: %v", err)
	}
}

// errCrash simulates a process crash: the restore stops at once and runs no
// cleanup.
var errCrash = errors.New("simulated crash")

// TestRestoreContinuationAfterACrashAtEachPoint crashes a restore before and
// after each journal write and each rename, including between two sidecar
// renames, and continues it. The result is the unchanged original file set
// or the verified replacement, never a mix.
func TestRestoreContinuationAfterACrashAtEachPoint(t *testing.T) {
	t.Parallel()
	var points []string
	recorder := newRestoreFixture(t)
	if err := checkpoint.ApplyRestoreWithCrash(context.Background(), recorder.plan(t), checkpoint.Factory{}, func(point string) error {
		points = append(points, point)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"journal planned", "journal preserving",
		"before preserve factory.db", "after preserve factory.db",
		"after preserve factory.db-journal", "before preserve factory.db-wal",
		"journal replacing", "before replace factory.db", "after replace factory.db",
		"journal verifying", "journal verified",
	} {
		if !slices.Contains(points, required) {
			t.Fatalf("crash points = %v, want %q", points, required)
		}
	}
	// From the verifying state on, the continuation repeats the verification
	// of a complete replacement, so it completes the restore.
	verifying := slices.Index(points, "journal verifying")
	for index, point := range points {
		fixture := newRestoreFixture(t)
		err := checkpoint.ApplyRestoreWithCrash(context.Background(), fixture.plan(t), checkpoint.Factory{}, func(current string) error {
			if current == point {
				return errCrash
			}
			return nil
		})
		if !errors.Is(err, errCrash) {
			t.Fatalf("%s: ApplyRestore() = %v, want the simulated crash", point, err)
		}
		if _, err := store.OpenReadOnly(context.Background(), fixture.storePath); !errors.As(err, new(*store.RestoreInProgressError)) {
			t.Fatalf("%s: OpenReadOnly() after the crash = %v, want a restore in progress", point, err)
		}
		continuation, err := checkpoint.ContinueRestore(context.Background(), fixture.storePath)
		if err != nil {
			t.Fatalf("%s: ContinueRestore() error = %v", point, err)
		}
		if _, err := os.Stat(store.RestoreJournalPath(fixture.storePath)); !os.IsNotExist(err) {
			t.Fatalf("%s: the restore journal remains after the continuation: %v", point, err)
		}
		if index >= verifying {
			if continuation.Result != checkpoint.ContinuationCompleted {
				t.Fatalf("%s: continuation = %+v, want completed", point, continuation)
			}
			fixture.assertReplaced(t, point)
			continue
		}
		if continuation.Result != checkpoint.ContinuationRolledBack {
			t.Fatalf("%s: continuation = %+v, want rolled back", point, continuation)
		}
		fixture.assertOriginal(t, point)
	}
}

// TestContinueRestoreWithoutAJournal verifies that a continuation without
// a journal reports that nothing was to continue.
func TestContinueRestoreWithoutAJournal(t *testing.T) {
	t.Parallel()
	fixture := newRestoreFixture(t)
	if _, err := checkpoint.ContinueRestore(context.Background(), fixture.storePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ContinueRestore() error = %v, want os.ErrNotExist", err)
	}
}

// TestRestoreContinuationRollsBackAReplacementThatNoLongerVerifies crashes
// a restore in the verifying state, changes the replacement, and verifies
// that the continuation puts the original file set back.
func TestRestoreContinuationRollsBackAReplacementThatNoLongerVerifies(t *testing.T) {
	t.Parallel()
	fixture := newRestoreFixture(t)
	err := checkpoint.ApplyRestoreWithCrash(context.Background(), fixture.plan(t), checkpoint.Factory{}, func(point string) error {
		if point == "journal verifying" {
			return errCrash
		}
		return nil
	})
	if !errors.Is(err, errCrash) {
		t.Fatalf("ApplyRestore() = %v, want the simulated crash", err)
	}
	if err := os.WriteFile(fixture.storePath, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	continuation, err := checkpoint.ContinueRestore(context.Background(), fixture.storePath)
	if err != nil || continuation.Result != checkpoint.ContinuationRolledBack {
		t.Fatalf("ContinueRestore() = %+v, %v; want rolled back", continuation, err)
	}
	fixture.assertOriginal(t, "changed replacement")
}

// crashAt returns a crash point that stops at point and passes every other
// point.
func crashAt(point string) func(string) error {
	return func(current string) error {
		if current == point {
			return errCrash
		}
		return nil
	}
}

// interruptBeforeRollback crashes a restore of the fixture at point, so the
// next continuation rolls back. For the verifying state it also changes the
// replacement, so the repeated verification fails.
func interruptBeforeRollback(t *testing.T, fixture restoreFixture, point string) {
	t.Helper()
	if err := checkpoint.ApplyRestoreWithCrash(context.Background(), fixture.plan(t), checkpoint.Factory{}, crashAt(point)); !errors.Is(err, errCrash) {
		t.Fatalf("ApplyRestore() = %v, want the simulated crash at %s", err, point)
	}
	if point == "journal verifying" {
		if err := os.WriteFile(fixture.storePath, []byte("changed"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRestoreContinuationAfterACrashInsideTheRollback crashes the rollback
// of a continuation at each of its points, including between two sidecar
// renames, and continues again. The result is always the unchanged
// original file set.
func TestRestoreContinuationAfterACrashInsideTheRollback(t *testing.T) {
	t.Parallel()
	for _, interrupted := range []string{"after replace factory.db", "journal verifying"} {
		var points []string
		recorder := newRestoreFixture(t)
		interruptBeforeRollback(t, recorder, interrupted)
		if _, err := checkpoint.ContinueRestoreWithCrash(context.Background(), recorder.storePath, func(point string) error {
			points = append(points, point)
			return nil
		}); err != nil {
			t.Fatalf("%s: ContinueRestore() error = %v", interrupted, err)
		}
		for _, required := range []string{"rollback removed replacement", "after rollback factory.db", "after rollback factory.db-journal", "before rollback factory.db-wal"} {
			if !slices.Contains(points, required) {
				t.Fatalf("%s: rollback crash points = %v, want %q", interrupted, points, required)
			}
		}
		for _, point := range points {
			fixture := newRestoreFixture(t)
			interruptBeforeRollback(t, fixture, interrupted)
			if _, err := checkpoint.ContinueRestoreWithCrash(context.Background(), fixture.storePath, crashAt(point)); !errors.Is(err, errCrash) {
				t.Fatalf("%s, %s: ContinueRestore() = %v, want the simulated crash", interrupted, point, err)
			}
			continuation, err := checkpoint.ContinueRestore(context.Background(), fixture.storePath)
			if err != nil || continuation.Result != checkpoint.ContinuationRolledBack {
				t.Fatalf("%s, %s: second ContinueRestore() = %+v, %v; want rolled back", interrupted, point, continuation, err)
			}
			fixture.assertOriginal(t, interrupted+", "+point)
		}
	}
}

// TestApplyRestoreRefusesASidecarThatIsNotOfTheReplacement places a stale
// sidecar at the live path before and after the replacement moves into
// place. The replacement is one file without sidecars, so a sidecar at the
// live path belongs to another store and could apply to the replacement.
// The restore fails and the original file set is back.
func TestApplyRestoreRefusesASidecarThatIsNotOfTheReplacement(t *testing.T) {
	t.Parallel()
	for _, point := range []string{"journal replacing", "after replace factory.db"} {
		for _, suffix := range liveSidecars {
			fixture := newRestoreFixture(t)
			err := checkpoint.ApplyRestoreWithCrash(context.Background(), fixture.plan(t), checkpoint.Factory{}, func(current string) error {
				if current == point {
					return os.WriteFile(fixture.storePath+suffix, []byte("a stale sidecar"), 0o600)
				}
				return nil
			})
			if err == nil {
				t.Fatalf("%s, %s: ApplyRestore() with a stale sidecar succeeded", point, suffix)
			}
			fixture.assertOriginal(t, point+", "+suffix)
		}
	}
}

// TestApplyRestoreKeepsAnUnlistedSidecarAwayFromTheReplacement verifies a
// sidecar that appears after the plan listed the live files: the
// replacement never goes live beside it, and restore does not remove it.
func TestApplyRestoreKeepsAnUnlistedSidecarAwayFromTheReplacement(t *testing.T) {
	t.Parallel()
	fixture := newRestoreFixture(t)
	for _, suffix := range liveSidecars {
		if err := os.Remove(fixture.storePath + suffix); err != nil {
			t.Fatal(err)
		}
		delete(fixture.original, "factory.db"+suffix)
	}
	plan := fixture.plan(t)
	stale := []byte("a stale write-ahead log")
	if err := os.WriteFile(fixture.storePath+"-wal", stale, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.ApplyRestore(context.Background(), plan, checkpoint.Factory{}); err == nil {
		t.Fatal("ApplyRestore() beside an unlisted sidecar succeeded")
	}
	fixture.original["factory.db-wal"] = stale
	fixture.assertOriginal(t, "unlisted sidecar")
}

// TestRestoreContinuationRefusesASidecarThatIsNotOfTheReplacement crashes
// a restore in the verifying state, places a stale sidecar at the live
// path, and verifies that the continuation rolls back.
func TestRestoreContinuationRefusesASidecarThatIsNotOfTheReplacement(t *testing.T) {
	t.Parallel()
	fixture := newRestoreFixture(t)
	if err := checkpoint.ApplyRestoreWithCrash(context.Background(), fixture.plan(t), checkpoint.Factory{}, crashAt("journal verifying")); !errors.Is(err, errCrash) {
		t.Fatalf("ApplyRestore() = %v, want the simulated crash", err)
	}
	if err := os.WriteFile(fixture.storePath+"-shm", []byte("a stale shared memory index"), 0o600); err != nil {
		t.Fatal(err)
	}
	continuation, err := checkpoint.ContinueRestore(context.Background(), fixture.storePath)
	if err != nil || continuation.Result != checkpoint.ContinuationRolledBack {
		t.Fatalf("ContinueRestore() = %+v, %v; want rolled back", continuation, err)
	}
	fixture.assertOriginal(t, "stale sidecar while verifying")
}
