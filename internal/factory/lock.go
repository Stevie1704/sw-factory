package factory

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/version"
)

// LockHolder names the kind of command that holds the coordinator lock.
type LockHolder string

const (
	// LockHolderCoordinator is a polling coordinator started by factory start.
	LockHolderCoordinator LockHolder = "coordinator"
	// LockHolderStoreMigration is factory upgrade migrate-store.
	LockHolderStoreMigration LockHolder = "store_migration"
	// LockHolderReset is a confirmed factory reset.
	LockHolderReset LockHolder = "reset"
	// LockHolderCheckpoint is factory checkpoint create.
	LockHolderCheckpoint LockHolder = "checkpoint"
)

// lockRecord is the content of the lock file: one JSON record with the
// owner PID, what holds the lock, and the build that runs it.
type lockRecord struct {
	PID      int        `json:"pid"`
	Holder   LockHolder `json:"holder"`
	Version  string     `json:"version"`
	Revision string     `json:"revision"`
}

// errInvalidLockRecord reports a lock file whose content is not a complete
// lock record.
var errInvalidLockRecord = errors.New("coordinator lock record is not valid: it needs pid, holder, version, and revision")

// CoordinatorLockRecord is the observed state of one coordinator lock.
type CoordinatorLockRecord struct {
	// Held reports that a live process holds the lock.
	Held bool
	// Holder, Version, and Revision identify the holder of a held lock.
	Holder   LockHolder
	Version  string
	Revision string
}

// ErrCoordinatorAlreadyRunning reports that another process owns the
// repository's host lock.
var ErrCoordinatorAlreadyRunning = errors.New("factory coordinator is already running")

// ErrCoordinatorNotRunning reports that no process currently owns the host
// lock targeted by a stop request.
var ErrCoordinatorNotRunning = errors.New("factory coordinator is not running")

// StopResult reports whether a stop signal was sent to a coordinator.
type StopResult struct {
	// Running is true when a live lock owner received the stop signal.
	Running bool
	// PID is the signalled process identifier, or zero when no coordinator ran.
	PID int
}

// coordinatorLock is a kernel-backed repository ownership lock held for the
// lifetime of one polling coordinator process.
type coordinatorLock struct {
	file *os.File
}

// coordinatorLockPath returns a stable private lock keyed by the canonical
// registered checkout path. Keeping the lock outside both the checkout and
// operational store makes separate host configurations for one repository
// share ownership without modifying repository contents.
func coordinatorLockPath(registration config.RepositoryRegistration) string {
	repositoryPath := filepath.Clean(registration.Path)
	if absolute, err := filepath.Abs(repositoryPath); err == nil {
		repositoryPath = absolute
	}
	if resolved, err := filepath.EvalSymlinks(repositoryPath); err == nil {
		repositoryPath = resolved
	}
	identity := sha256.Sum256([]byte(repositoryPath))
	return filepath.Join(os.TempDir(), "sw-factory", "coordinators", fmt.Sprintf("%x.lock", identity))
}

// acquireCoordinatorLock takes a non-blocking advisory lock and records the
// owning PID for a separate factory stop command to signal, and the holder
// identity for an upgrade plan to observe.
func acquireCoordinatorLock(path string, holder LockHolder) (*coordinatorLock, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("coordinator lock path is required")
	}
	if err := ensurePrivateLockDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	file, err := openPrivateLockFile(path, true)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if lockWouldBlock(err) {
			return nil, ErrCoordinatorAlreadyRunning
		}
		return nil, fmt.Errorf("acquire coordinator lock: %w", err)
	}
	if err := file.Truncate(0); err != nil {
		_ = releaseCoordinatorFile(file)
		return nil, fmt.Errorf("clear coordinator lock owner: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = releaseCoordinatorFile(file)
		return nil, fmt.Errorf("seek coordinator lock owner: %w", err)
	}
	if _, err := io.WriteString(file, newLockRecord(holder)); err != nil {
		_ = releaseCoordinatorFile(file)
		return nil, fmt.Errorf("write coordinator lock owner: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = releaseCoordinatorFile(file)
		return nil, fmt.Errorf("sync coordinator lock owner: %w", err)
	}
	return &coordinatorLock{file: file}, nil
}

// newLockRecord returns the encoded lock record of this process.
func newLockRecord(holder LockHolder) string {
	build := version.Current()
	encoded, _ := json.Marshal(lockRecord{PID: os.Getpid(), Holder: holder, Version: build.Version, Revision: build.Revision})
	return string(encoded) + "\n"
}

// ObserveCoordinatorLock reports, without taking or changing the lock,
// whether a live process holds the coordinator lock of one registration and
// what that process is. A held lock with an invalid record is an error.
func ObserveCoordinatorLock(registration config.RepositoryRegistration) (CoordinatorLockRecord, error) {
	path := coordinatorLockPath(registration)
	held, err := coordinatorLockHeld(path)
	if err != nil || !held {
		return CoordinatorLockRecord{}, err
	}
	record, err := readLockRecord(path)
	if err != nil {
		return CoordinatorLockRecord{}, err
	}
	return CoordinatorLockRecord{Held: true, Holder: record.Holder, Version: record.Version, Revision: record.Revision}, nil
}

// readLockRecord reads and validates the record of an existing lock file
// without taking the lock.
func readLockRecord(path string) (lockRecord, error) {
	file, err := openPrivateLockFile(path, false)
	if err != nil {
		return lockRecord{}, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(file)
	if err != nil {
		return lockRecord{}, fmt.Errorf("read coordinator lock owner: %w", err)
	}
	var record lockRecord
	if json.Unmarshal(data, &record) != nil || record.PID <= 0 || record.Holder == "" || record.Version == "" || record.Revision == "" {
		return lockRecord{}, errInvalidLockRecord
	}
	return record, nil
}

// release unlocks and closes one coordinator lock, retaining its path as a
// reusable lock inode for later coordinator starts.
func (lock *coordinatorLock) release() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	file := lock.file
	lock.file = nil
	return releaseCoordinatorFile(file)
}

// requestCoordinatorStop signals the current kernel lock owner without
// deleting the lock file or touching the active run's workflow state.
func requestCoordinatorStop(path string) (StopResult, error) {
	if strings.TrimSpace(path) == "" {
		return StopResult{}, errors.New("coordinator lock path is required")
	}
	file, err := openPrivateLockFile(path, false)
	if errors.Is(err, os.ErrNotExist) {
		return StopResult{}, ErrCoordinatorNotRunning
	}
	if err != nil {
		return StopResult{}, err
	}
	defer func() { _ = file.Close() }()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		return StopResult{}, ErrCoordinatorNotRunning
	} else if !lockWouldBlock(err) {
		return StopResult{}, fmt.Errorf("inspect coordinator lock: %w", err)
	}
	record, err := readLockRecord(path)
	if err != nil {
		return StopResult{}, err
	}
	pid := record.PID
	process, err := os.FindProcess(pid)
	if err != nil {
		return StopResult{}, fmt.Errorf("find coordinator process: %w", err)
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return StopResult{}, fmt.Errorf("signal coordinator process %d: %w", pid, err)
	}
	return StopResult{Running: true, PID: pid}, nil
}

// coordinatorLockHeld reports whether a live coordinator currently owns the
// repository lock. It never creates the lock inode, signals the owner, or
// changes the lock's contents, so a read-only reset preview can call it. An
// absent lock file means no coordinator is running.
func coordinatorLockHeld(path string) (bool, error) {
	if strings.TrimSpace(path) == "" {
		return false, errors.New("coordinator lock path is required")
	}
	file, err := openPrivateLockFile(path, false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = file.Close() }()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		return false, nil
	} else if !lockWouldBlock(err) {
		return false, fmt.Errorf("inspect coordinator lock: %w", err)
	}
	return true, nil
}

// removeCoordinatorLock removes one proven-unlocked coordinator lock inode.
// An already-absent lock is a successful result. The shared parent directory
// is retained because it belongs to every repository on the host.
func removeCoordinatorLock(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove coordinator lock: %w", err)
	}
	return nil
}

// ensurePrivateLockDirectory creates and validates the lock's parent without
// permitting group or other-user access to the PID marker.
func ensurePrivateLockDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create coordinator lock directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect coordinator lock directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("coordinator lock directory %q is not private", path)
	}
	return nil
}

// openPrivateLockFile opens a regular lock inode without following a final
// symlink, preventing a lock path replacement from redirecting PID writes or
// stop reads to an unrelated file.
func openPrivateLockFile(path string, create bool) (*os.File, error) {
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW
	if create {
		flags |= unix.O_CREAT
	}
	fd, err := unix.Open(path, flags, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open coordinator lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open coordinator lock returned an invalid file")
	}
	if err := validatePrivateLockFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// validatePrivateLockFile verifies that an existing PID marker is a private
// regular file before its advisory lock is used.
func validatePrivateLockFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect coordinator lock: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("coordinator lock is not a private regular file")
	}
	return nil
}

// lockWouldBlock recognizes the platform's non-blocking advisory-lock result.
func lockWouldBlock(err error) bool {
	return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN)
}

// releaseCoordinatorFile unlocks and closes a lock file while retaining the
// inode for later starts.
func releaseCoordinatorFile(file *os.File) error {
	if file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(file.Fd()), unix.LOCK_UN)
	closeErr := file.Close()
	if unlockErr != nil && closeErr != nil {
		return errors.Join(unlockErr, closeErr)
	}
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
