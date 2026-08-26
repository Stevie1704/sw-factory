package factory

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/Stevie1704/sw-factory/internal/config"
)

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
// owning PID for a separate factory stop command to signal.
func acquireCoordinatorLock(path string) (*coordinatorLock, error) {
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
	if _, err := io.WriteString(file, strconv.Itoa(os.Getpid())+"\n"); err != nil {
		_ = releaseCoordinatorFile(file)
		return nil, fmt.Errorf("write coordinator lock owner: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = releaseCoordinatorFile(file)
		return nil, fmt.Errorf("sync coordinator lock owner: %w", err)
	}
	return &coordinatorLock{file: file}, nil
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
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return StopResult{}, fmt.Errorf("read coordinator lock owner: %w", err)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return StopResult{}, fmt.Errorf("read coordinator lock owner: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return StopResult{}, errors.New("coordinator lock has no valid owner PID")
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return StopResult{}, fmt.Errorf("find coordinator process: %w", err)
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return StopResult{}, fmt.Errorf("signal coordinator process %d: %w", pid, err)
	}
	return StopResult{Running: true, PID: pid}, nil
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
