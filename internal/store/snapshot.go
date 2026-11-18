package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
)

// Querier is a read view of one database connection. The connection that
// holds the write lock during a snapshot is one; a read-only store is
// another.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// SnapshotCheck runs on the connection that holds the SQLite write lock,
// before the snapshot. An error ends the snapshot before anything is
// written, and Snapshot returns it unchanged.
type SnapshotCheck func(ctx context.Context, view Querier) error

// SnapshotFile describes one verified snapshot file.
type SnapshotFile struct {
	// Size is the file size in bytes.
	Size int64
	// SHA256 is the hexadecimal SHA-256 of the file content.
	SHA256 string
	// SchemaVersion is the schema version that the snapshot records.
	SchemaVersion int
}

// SnapshotInspection is what a verified snapshot file holds.
type SnapshotInspection struct {
	// File identifies the verified file.
	File SnapshotFile
	// Watermarks are the highest identities that the snapshot knows.
	Watermarks Watermarks
	// KnownRuns names the external objects of every run in the snapshot.
	KnownRuns []KnownRun
}

// vacuumInto writes the online snapshot of the database of conn to
// destination. It is a parameter so a test can inject a failure, such as a
// full disk.
type vacuumInto func(ctx context.Context, conn *sql.Conn, destination string) error

// Snapshot takes the online snapshot of ADR 0032 from the current store at
// source to the new file destination. Connection A takes the write lock with
// BEGIN IMMEDIATE and runs check; connection B runs VACUUM INTO while A holds
// the lock, so no other writer can commit between the check and the
// snapshot. A wrote nothing and ends with ROLLBACK. The snapshot gets mode
// 0600 at once and passes an integrity check before Snapshot returns.
//
// Snapshot reads the schema version read-only first and refuses an older or
// newer schema with *MigrationRequiredError or *UnknownSchemaVersionError,
// without opening the store read-write. A nil check checks nothing. On any
// error, the destination does not exist. The caller holds the coordinator
// lock, and the destination directory must be private.
func Snapshot(ctx context.Context, source, destination string, check SnapshotCheck) (SnapshotInspection, error) {
	return snapshot(ctx, source, destination, check, vacuumOnConnection)
}

// snapshot is Snapshot with an exchangeable VACUUM INTO step.
func snapshot(ctx context.Context, source, destination string, check SnapshotCheck, vacuum vacuumInto) (SnapshotInspection, error) {
	path, err := checkSnapshotSource(ctx, source)
	if err != nil {
		return SnapshotInspection{}, err
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return SnapshotInspection{}, fmt.Errorf("snapshot destination %q already exists or is not readable: %v", destination, err)
	}
	if err := lockedVacuum(ctx, path, destination, check, vacuum); err != nil {
		_ = os.Remove(destination)
		return SnapshotInspection{}, err
	}
	inspection, err := protectAndInspect(ctx, destination)
	if err != nil {
		_ = os.Remove(destination)
		return SnapshotInspection{}, err
	}
	return inspection, nil
}

// checkSnapshotSource returns the absolute path of a current store, read
// through the read-only path so an older store is never opened read-write.
func checkSnapshotSource(ctx context.Context, source string) (string, error) {
	path, err := checkExistingStoreFile(source)
	if err != nil {
		return "", err
	}
	database, err := openReadOnlyDatabase(ctx, path)
	if err != nil {
		return "", err
	}
	version, err := readSchemaVersion(ctx, database, path)
	if closeErr := database.Close(); err == nil && closeErr != nil {
		err = fmt.Errorf("close operational store after the schema read: %w", closeErr)
	}
	if err != nil {
		return "", err
	}
	if version > CurrentSchemaVersion {
		return "", &UnknownSchemaVersionError{Version: version, Supported: CurrentSchemaVersion}
	}
	if version < CurrentSchemaVersion {
		return "", &MigrationRequiredError{Version: version, Supported: CurrentSchemaVersion}
	}
	return path, nil
}

// lockedVacuum runs the check on connection A under BEGIN IMMEDIATE and the
// snapshot on connection B while A holds the lock.
func lockedVacuum(ctx context.Context, path, destination string, check SnapshotCheck, vacuum vacuumInto) (err error) {
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=rw&_pragma=busy_timeout(5000)"}).String()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open operational store for the snapshot: %w", err)
	}
	defer func() { _ = database.Close() }()
	database.SetMaxOpenConns(2)
	lock, err := database.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open the snapshot lock connection: %w", err)
	}
	defer func() { _ = lock.Close() }()
	if _, err := lock.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("take the store write lock: %w", err)
	}
	defer func() {
		if _, rollbackErr := lock.ExecContext(context.WithoutCancel(ctx), "ROLLBACK"); err == nil && rollbackErr != nil {
			err = fmt.Errorf("release the store write lock: %w", rollbackErr)
		}
	}()
	if check != nil {
		if err := check(ctx, lock); err != nil {
			return err
		}
	}
	reader, err := database.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open the snapshot connection: %w", err)
	}
	defer func() { _ = reader.Close() }()
	return vacuum(ctx, reader, destination)
}

// vacuumOnConnection writes the snapshot with VACUUM INTO.
func vacuumOnConnection(ctx context.Context, conn *sql.Conn, destination string) error {
	if _, err := conn.ExecContext(ctx, "VACUUM INTO ?", destination); err != nil {
		return fmt.Errorf("snapshot the operational store: %w", err)
	}
	return nil
}

// protectAndInspect restricts a new snapshot file to 0600, syncs it, and
// inspects it.
func protectAndInspect(ctx context.Context, path string) (SnapshotInspection, error) {
	if err := os.Chmod(path, 0o600); err != nil {
		return SnapshotInspection{}, fmt.Errorf("protect the snapshot: %w", err)
	}
	file, err := os.Open(path)
	if err != nil {
		return SnapshotInspection{}, fmt.Errorf("open the snapshot: %w", err)
	}
	syncErr := file.Sync()
	if closeErr := file.Close(); syncErr == nil {
		syncErr = closeErr
	}
	if syncErr != nil {
		return SnapshotInspection{}, fmt.Errorf("sync the snapshot: %w", syncErr)
	}
	return InspectSnapshot(ctx, path)
}

// InspectSnapshot verifies one snapshot file that no process writes: it runs
// PRAGMA integrity_check, reads the schema version, and computes the
// SHA-256. It opens the file immutable and read-only, so it creates no
// sidecar and changes nothing.
func InspectSnapshot(ctx context.Context, path string) (SnapshotInspection, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return SnapshotInspection{}, fmt.Errorf("resolve snapshot path: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: absolutePath, RawQuery: "mode=ro&immutable=1"}).String()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return SnapshotInspection{}, fmt.Errorf("open snapshot: %w", err)
	}
	defer func() { _ = database.Close() }()
	var integrity string
	if err := database.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return SnapshotInspection{}, fmt.Errorf("check snapshot integrity: %w", err)
	}
	if integrity != "ok" {
		return SnapshotInspection{}, fmt.Errorf("snapshot integrity check failed: %s", integrity)
	}
	inspection := SnapshotInspection{}
	if inspection.File.SchemaVersion, err = readSchemaVersion(ctx, database, absolutePath); err != nil {
		return SnapshotInspection{}, err
	}
	if inspection.File.Size, inspection.File.SHA256, err = fileDigest(absolutePath); err != nil {
		return SnapshotInspection{}, err
	}
	// Only the current schema has known tables; a caller compares the
	// schema version of another snapshot and refuses it.
	if inspection.File.SchemaVersion != CurrentSchemaVersion {
		return inspection, nil
	}
	if inspection.Watermarks, err = ReadWatermarks(ctx, database); err != nil {
		return SnapshotInspection{}, err
	}
	if inspection.KnownRuns, err = ReadKnownRuns(ctx, database); err != nil {
		return SnapshotInspection{}, err
	}
	return inspection, nil
}

// fileDigest returns the size and the hexadecimal SHA-256 of one file.
func fileDigest(path string) (int64, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, "", fmt.Errorf("open %s for its checksum: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return 0, "", fmt.Errorf("read %s for its checksum: %w", path, err)
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}
