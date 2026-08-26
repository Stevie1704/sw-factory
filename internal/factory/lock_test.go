package factory

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
)

// TestCoordinatorLockPreventsASecondOwner verifies the host lock is enforced
// by the kernel and becomes reusable after the first owner releases it.
func TestCoordinatorLockPreventsASecondOwner(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state", "factory.lock")
	first, err := acquireCoordinatorLock(path)
	if err != nil {
		t.Fatalf("first acquireCoordinatorLock() error = %v", err)
	}
	defer func() { _ = first.release() }()
	if _, err := acquireCoordinatorLock(path); !errors.Is(err, ErrCoordinatorAlreadyRunning) {
		t.Fatalf("second acquireCoordinatorLock() error = %v, want ErrCoordinatorAlreadyRunning", err)
	}
	if err := first.release(); err != nil {
		t.Fatalf("release() error = %v", err)
	}
	second, err := acquireCoordinatorLock(path)
	if err != nil {
		t.Fatalf("reacquireCoordinatorLock() error = %v", err)
	}
	if err := second.release(); err != nil {
		t.Fatalf("second release() error = %v", err)
	}
}

// TestRequestCoordinatorStopReportsNoOwner verifies an idle stop is safe and
// leaves no misleading success result for the CLI.
func TestRequestCoordinatorStopReportsNoOwner(t *testing.T) {
	t.Parallel()

	result, err := requestCoordinatorStop(filepath.Join(t.TempDir(), "missing.lock"))
	if !errors.Is(err, ErrCoordinatorNotRunning) {
		t.Fatalf("requestCoordinatorStop() error = %v, want ErrCoordinatorNotRunning", err)
	}
	if result.Running || result.PID != 0 {
		t.Fatalf("stop result = %#v, want no running coordinator", result)
	}
}

// TestCoordinatorLockPathUsesTheRepositoryIdentity verifies separate host
// configurations for one checkout cannot bypass the coordinator lock by
// choosing different operational-store paths.
func TestCoordinatorLockPathUsesTheRepositoryIdentity(t *testing.T) {
	t.Parallel()

	first := config.RepositoryRegistration{
		Path:                filepath.Join(t.TempDir(), "repository"),
		OperationalDataPath: filepath.Join(t.TempDir(), "first.db"),
	}
	second := first
	second.OperationalDataPath = filepath.Join(t.TempDir(), "second.db")
	if got, want := coordinatorLockPath(first), coordinatorLockPath(second); got != want {
		t.Fatalf("lock paths = %q and %q, want shared path for one repository", got, want)
	}
}

// TestCoordinatorLockRejectsSymlinkedLockFiles verifies lock ownership and
// stop inspection cannot follow a path replacement into an unrelated file.
func TestCoordinatorLockRejectsSymlinkedLockFiles(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "coordinator.lock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireCoordinatorLock(link); err == nil {
		t.Fatal("acquireCoordinatorLock() followed a symlink")
	}
	if _, err := requestCoordinatorStop(link); err == nil {
		t.Fatal("requestCoordinatorStop() followed a symlink")
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "123\n" {
		t.Fatalf("symlink target content = %q, want unchanged PID marker", content)
	}
}
