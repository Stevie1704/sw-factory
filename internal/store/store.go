package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

const CurrentSchemaVersion = 1

type Stage string

const (
	StageClaim          Stage = "claim"
	StagePreflight      Stage = "preflight"
	StageTest           Stage = "test"
	StageImplementation Stage = "implementation"
	StageCheck          Stage = "check"
	StageDraftPR        Stage = "draft_pr"
	StageReview         Stage = "review"
	StageReady          Stage = "ready"
)

type Status string

const (
	StatusActive            Status = "active"
	StatusWaitingForHuman   Status = "waiting_for_human"
	StatusWaitingForHarness Status = "waiting_for_harness"
	StatusFailed            Status = "failed"
	StatusCancelled         Status = "cancelled"
	StatusComplete          Status = "complete"
)

type UnknownSchemaVersionError struct {
	Version   int
	Supported int
}

func (e *UnknownSchemaVersionError) Error() string {
	return fmt.Sprintf("operational store schema version %d is newer than supported version %d", e.Version, e.Supported)
}

type UnversionedDatabaseError struct {
	Path string
	Err  error
}

func (e *UnversionedDatabaseError) Error() string {
	return fmt.Sprintf("operational store %q has no readable schema version: %v", e.Path, e.Err)
}

func (e *UnversionedDatabaseError) Unwrap() error { return e.Err }

type Run struct {
	ID             string
	RepositoryPath string
	IssueNumber    int
	Stage          Stage
	Status         Status
	Branch         string
	Worktree       string
	CheckpointSHA  string
	ImageDigest    string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type Store struct {
	db            *sql.DB
	path          string
	schemaVersion int
}

func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("operational store path is required")
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve operational store path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o700); err != nil {
		return nil, fmt.Errorf("create operational store directory: %w", err)
	}
	existed, empty, err := databaseState(absolutePath)
	if err != nil {
		return nil, err
	}
	if existed && empty {
		return nil, &UnversionedDatabaseError{Path: absolutePath, Err: errors.New("database file is empty")}
	}
	database, err := sql.Open("sqlite", absolutePath)
	if err != nil {
		return nil, fmt.Errorf("open operational store: %w", err)
	}
	if err := database.PingContext(ctx); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("open operational store: %w", err)
	}
	if err := os.Chmod(absolutePath, 0o600); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("protect operational store: %w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = database.Close()
		}
	}()
	if err := configure(ctx, database); err != nil {
		return nil, err
	}
	if !existed {
		if err := initializeMetadata(ctx, database); err != nil {
			return nil, err
		}
	}
	version, err := readSchemaVersion(ctx, database, absolutePath)
	if err != nil {
		return nil, err
	}
	if version > CurrentSchemaVersion {
		return nil, &UnknownSchemaVersionError{Version: version, Supported: CurrentSchemaVersion}
	}
	if version < CurrentSchemaVersion {
		if existed {
			if err := database.Close(); err != nil {
				return nil, fmt.Errorf("close store before migration: %w", err)
			}
			if _, err := backupDatabase(absolutePath); err != nil {
				return nil, err
			}
			database, err = sql.Open("sqlite", absolutePath)
			if err != nil {
				return nil, fmt.Errorf("reopen operational store for migration: %w", err)
			}
			database.SetMaxOpenConns(1)
			database.SetMaxIdleConns(1)
			if err := configure(ctx, database); err != nil {
				return nil, err
			}
		}
		if err := migrate(ctx, database, version); err != nil {
			return nil, err
		}
		version = CurrentSchemaVersion
	}
	closeOnError = false
	return &Store{db: database, path: absolutePath, schemaVersion: version}, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Path() string { return s.path }

func (s *Store) SchemaVersion() int { return s.schemaVersion }

func (s *Store) CurrentRun(ctx context.Context) (*Run, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, repository_path, issue_number, stage, status, branch, worktree,
		       checkpoint_sha, image_digest, created_at, updated_at
		FROM operational_runs
		WHERE status NOT IN (?, ?, ?)
		ORDER BY updated_at DESC
		LIMIT 1`, StatusComplete, StatusCancelled, StatusFailed)
	var run Run
	var createdAt, updatedAt string
	if err := row.Scan(
		&run.ID,
		&run.RepositoryPath,
		&run.IssueNumber,
		&run.Stage,
		&run.Status,
		&run.Branch,
		&run.Worktree,
		&run.CheckpointSHA,
		&run.ImageDigest,
		&createdAt,
		&updatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read current run: %w", err)
	}
	var err error
	run.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse run created_at: %w", err)
	}
	run.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return nil, fmt.Errorf("parse run updated_at: %w", err)
	}
	return &run, nil
}

func (s *Store) SaveRun(ctx context.Context, run Run) error {
	if run.ID == "" {
		return errors.New("run id is required")
	}
	if run.RepositoryPath == "" {
		return errors.New("run repository path is required")
	}
	if run.Status == "" {
		return errors.New("run status is required")
	}
	if run.CreatedAt.IsZero() {
		run.CreatedAt = time.Now().UTC()
	}
	if run.UpdatedAt.IsZero() {
		run.UpdatedAt = run.CreatedAt
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO operational_runs (
			id, repository_path, issue_number, stage, status, branch, worktree,
			checkpoint_sha, image_digest, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			repository_path = excluded.repository_path,
			issue_number = excluded.issue_number,
			stage = excluded.stage,
			status = excluded.status,
			branch = excluded.branch,
			worktree = excluded.worktree,
			checkpoint_sha = excluded.checkpoint_sha,
			image_digest = excluded.image_digest,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at`,
		run.ID,
		run.RepositoryPath,
		run.IssueNumber,
		run.Stage,
		run.Status,
		run.Branch,
		run.Worktree,
		run.CheckpointSHA,
		run.ImageDigest,
		run.CreatedAt.UTC().Format(time.RFC3339Nano),
		run.UpdatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("save run: %w", err)
	}
	return nil
}

func databaseState(path string) (bool, bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("inspect operational store: %w", err)
	}
	if info.IsDir() {
		return false, false, fmt.Errorf("operational store path %q is a directory", path)
	}
	return true, info.Size() == 0, nil
}

func configure(ctx context.Context, database *sql.DB) error {
	for _, statement := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = DELETE",
	} {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure SQLite: %w", err)
		}
	}
	return nil
}

func initializeMetadata(ctx context.Context, database *sql.DB) error {
	if _, err := database.ExecContext(ctx, `CREATE TABLE schema_metadata (
		singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
		version INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema metadata: %w", err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO schema_metadata(singleton, version) VALUES (1, 0)`); err != nil {
		return fmt.Errorf("initialize schema metadata: %w", err)
	}
	return nil
}

func readSchemaVersion(ctx context.Context, database *sql.DB, path string) (int, error) {
	var version int
	if err := database.QueryRowContext(ctx, "SELECT version FROM schema_metadata WHERE singleton = 1").Scan(&version); err != nil {
		return 0, &UnversionedDatabaseError{Path: path, Err: err}
	}
	return version, nil
}

func migrate(ctx context.Context, database *sql.DB, from int) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin store migration: %w", err)
	}
	defer tx.Rollback()
	version := from
	for version < CurrentSchemaVersion {
		switch version + 1 {
		case 1:
			if _, err := tx.ExecContext(ctx, `CREATE TABLE operational_runs (
				id TEXT PRIMARY KEY,
				repository_path TEXT NOT NULL,
				issue_number INTEGER NOT NULL,
				stage TEXT NOT NULL,
				status TEXT NOT NULL,
				branch TEXT NOT NULL DEFAULT '',
				worktree TEXT NOT NULL DEFAULT '',
				checkpoint_sha TEXT NOT NULL DEFAULT '',
				image_digest TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL
			)`); err != nil {
				return fmt.Errorf("apply store migration 1: %w", err)
			}
		default:
			return fmt.Errorf("no migration registered for schema version %d", version+1)
		}
		version++
		if _, err := tx.ExecContext(ctx, "UPDATE schema_metadata SET version = ? WHERE singleton = 1", version); err != nil {
			return fmt.Errorf("record store schema version %d: %w", version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit store migration: %w", err)
	}
	return nil
}

func backupDatabase(path string) (string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		suffix := time.Now().UTC().Format("20060102T150405.000000000Z07:00")
		if attempt > 0 {
			suffix += "-" + strconv.Itoa(attempt)
		}
		backupPath := path + ".bak-" + suffix
		if _, err := os.Stat(backupPath); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect store backup path: %w", err)
		}
		input, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("open store for backup: %w", err)
		}
		output, err := os.OpenFile(backupPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			input.Close()
			return "", fmt.Errorf("create store backup: %w", err)
		}
		_, copyErr := io.Copy(output, input)
		if copyErr == nil {
			copyErr = output.Sync()
		}
		closeOutputErr := output.Close()
		closeInputErr := input.Close()
		if copyErr != nil {
			_ = os.Remove(backupPath)
			return "", fmt.Errorf("copy store backup: %w", copyErr)
		}
		if closeOutputErr != nil {
			return "", fmt.Errorf("close store backup: %w", closeOutputErr)
		}
		if closeInputErr != nil {
			return "", fmt.Errorf("close source store after backup: %w", closeInputErr)
		}
		return backupPath, nil
	}
	return "", errors.New("could not choose a unique store backup path")
}
