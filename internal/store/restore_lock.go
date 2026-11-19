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

// RestoreWriteLock holds the SQLite write lock of a live store during a
// confirmed restore (ADR 0032). While it is held, no other connection, such
// as an already-open command inbox, can commit. A nil lock holds nothing:
// LockForRestore returns nil for a store that no writer can commit into.
type RestoreWriteLock struct {
	database *sql.DB
	conn     *sql.Conn
	// files are the handles that FileDigest opened. They stay open until
	// Release: closing any handle of the store file drops the POSIX lock
	// that SQLite holds on it for this process.
	files []*os.File
}

// LockForRestore takes the write lock of the store at path with BEGIN
// IMMEDIATE and holds it until Release. It writes nothing. A missing store
// file, a file that is not regular, or a damaged database gets no lock and
// no error, because no writer can commit into it. A store that another
// connection holds past the busy timeout returns an error for which
// IsTransient is true.
func LockForRestore(ctx context.Context, path string) (*RestoreWriteLock, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve operational store path: %w", err)
	}
	info, err := os.Lstat(absolutePath)
	if errors.Is(err, os.ErrNotExist) || (err == nil && !info.Mode().IsRegular()) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect operational store: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: absolutePath, RawQuery: "mode=rw&_pragma=busy_timeout(5000)"}).String()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open operational store for the restore lock: %w", err)
	}
	conn, err := database.Conn(ctx)
	if err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("open the restore lock connection: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		_ = database.Close()
		if isDamagedDatabase(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("take the store write lock for the restore: %w", err)
	}
	return &RestoreWriteLock{database: database, conn: conn}, nil
}

// FileDigest returns the size and the hexadecimal SHA-256 of one file, as
// the package-level FileDigest does, but keeps the handle open until
// Release, so the write lock stays held. A nil lock uses FileDigest.
func (l *RestoreWriteLock) FileDigest(path string) (int64, string, error) {
	if l == nil {
		return FileDigest(path)
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, "", fmt.Errorf("open %s for its checksum: %w", path, err)
	}
	l.files = append(l.files, file)
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return 0, "", fmt.Errorf("read %s for its checksum: %w", path, err)
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

// Release ends the transaction, which wrote nothing, closes the connection,
// and then closes the handles of FileDigest. A second call and a call on a
// nil lock do nothing.
func (l *RestoreWriteLock) Release() error {
	if l == nil || l.conn == nil {
		return nil
	}
	_, err := l.conn.ExecContext(context.Background(), "ROLLBACK")
	if err != nil {
		err = fmt.Errorf("end the restore lock transaction: %w", err)
	}
	err = errors.Join(err, l.conn.Close(), l.database.Close())
	for _, file := range l.files {
		err = errors.Join(err, file.Close())
	}
	l.conn, l.files = nil, nil
	return err
}
