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

// CurrentSchemaVersion is the latest operational-store schema understood by
// this binary.
const CurrentSchemaVersion = 3

// runTimestampLayout keeps serialized run timestamps fixed-width so SQLite
// text ordering matches chronological ordering for sub-second timestamps.
const runTimestampLayout = "2006-01-02T15:04:05.000000000Z07:00"

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

// Run is the persisted operational identity and current state of one run.
type Run struct {
	ID                  string
	RepositoryPath      string
	IssueNumber         int
	Stage               Stage
	Status              Status
	Branch              string
	Worktree            string
	CheckpointSHA       string
	ImageDigest         string
	Coordinator         string
	StatusCommentID     string
	SpecificationPacket string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type Store struct {
	db            *sql.DB
	path          string
	schemaVersion int
}

// Open opens the operational store at path, creating or migrating its database as needed.
// Open opens or creates an operational store at path, applying supported schema migrations as needed.
// It requires a private parent directory and rejects empty databases, unsupported schema versions,
// and databases with missing or unreadable schema metadata. Existing databases are backed up before migration.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("operational store path is required")
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve operational store path: %w", err)
	}
	storeDirectory := filepath.Dir(absolutePath)
	if err := os.MkdirAll(storeDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create operational store directory: %w", err)
	}
	if info, err := os.Stat(storeDirectory); err != nil {
		return nil, fmt.Errorf("inspect operational store directory: %w", err)
	} else if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("operational store directory %q is not private; expected no group or other permissions", storeDirectory)
	}
	existed, empty, err := databaseState(absolutePath)
	if err != nil {
		return nil, err
	}
	if existed && empty {
		return nil, &UnversionedDatabaseError{Path: absolutePath, Err: errors.New("database file is empty")}
	}
	database, err := openConfiguredDatabase(ctx, absolutePath)
	if err != nil {
		return nil, err
	}
	closeOnError := true
	defer func() {
		if closeOnError && database != nil {
			_ = database.Close()
		}
	}()
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
			database, err = openConfiguredDatabase(ctx, absolutePath)
			if err != nil {
				return nil, fmt.Errorf("reopen operational store for migration: %w", err)
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

// CurrentRun returns the newest non-terminal run, if one is active.
func (s *Store) CurrentRun(ctx context.Context) (*Run, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, repository_path, issue_number, stage, status, branch, worktree,
		       checkpoint_sha, image_digest, coordinator, status_comment_id,
		       specification_packet, created_at, updated_at
		FROM operational_runs
		WHERE status NOT IN (?, ?, ?)
		ORDER BY updated_at DESC
		LIMIT 1`, StatusComplete, StatusCancelled, StatusFailed)
	return scanRun(row)
}

// LatestRun returns the most recently updated claimed run, including terminal
// runs so status can show the last known branch and worktree.
func (s *Store) LatestRun(ctx context.Context) (*Run, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, repository_path, issue_number, stage, status, branch, worktree,
		       checkpoint_sha, image_digest, coordinator, status_comment_id,
		       specification_packet, created_at, updated_at
		FROM operational_runs
		ORDER BY updated_at DESC
		LIMIT 1`)
	return scanRun(row)
}

// scanRun decodes one operational run row and its RFC3339 timestamps.
func scanRun(row *sql.Row) (*Run, error) {
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
		&run.Coordinator,
		&run.StatusCommentID,
		&run.SpecificationPacket,
		&createdAt,
		&updatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read run row: %w", err)
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

// SaveRun validates and upserts one operational run record.
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
			checkpoint_sha, image_digest, coordinator, status_comment_id,
			specification_packet, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			repository_path = excluded.repository_path,
			issue_number = excluded.issue_number,
			stage = excluded.stage,
			status = excluded.status,
			branch = excluded.branch,
			worktree = excluded.worktree,
			checkpoint_sha = excluded.checkpoint_sha,
			image_digest = excluded.image_digest,
			coordinator = excluded.coordinator,
			status_comment_id = excluded.status_comment_id,
			specification_packet = excluded.specification_packet,
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
		run.Coordinator,
		run.StatusCommentID,
		run.SpecificationPacket,
		run.CreatedAt.UTC().Format(runTimestampLayout),
		run.UpdatedAt.UTC().Format(runTimestampLayout),
	)
	if err != nil {
		return fmt.Errorf("save run: %w", err)
	}
	return nil
}

// databaseState reports whether the path identifies an existing database file and whether that file is empty.
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

// openConfiguredDatabase opens the SQLite database at path, restricts its file permissions,
// limits it to a single connection, and applies the store configuration. It closes the
// database and returns an error if setup fails.
func openConfiguredDatabase(ctx context.Context, path string) (*sql.DB, error) {
	database, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open operational store: %w", err)
	}
	if err := database.PingContext(ctx); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("open operational store: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("protect operational store: %w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	if err := configure(ctx, database); err != nil {
		_ = database.Close()
		return nil, err
	}
	return database, nil
}

// configure applies the SQLite settings required by the operational store.
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

// initializeMetadata creates the schema metadata table and initializes its version to zero.
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

// readSchemaVersion reads the schema version recorded in the database metadata.
// It returns an UnversionedDatabaseError if the metadata cannot be read.
func readSchemaVersion(ctx context.Context, database *sql.DB, path string) (int, error) {
	var version int
	if err := database.QueryRowContext(ctx, "SELECT version FROM schema_metadata WHERE singleton = 1").Scan(&version); err != nil {
		return 0, &UnversionedDatabaseError{Path: path, Err: err}
	}
	return version, nil
}

// migrate applies supported schema migrations from the specified version through the current schema version in a single transaction. It records each completed migration and leaves the database unchanged if a migration fails.
func migrate(ctx context.Context, database *sql.DB, from int) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin store migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
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
		case 2:
			for _, statement := range []string{
				"ALTER TABLE operational_runs ADD COLUMN coordinator TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN status_comment_id TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN specification_packet TEXT NOT NULL DEFAULT ''",
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("apply store migration 2: %w", err)
				}
			}
		case 3:
			if err := reconcileDuplicateRuns(ctx, tx); err != nil {
				return fmt.Errorf("reconcile runs before store migration 3: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX one_active_run_per_repository
				ON operational_runs (repository_path)
				WHERE status NOT IN ('complete', 'cancelled', 'failed')`); err != nil {
				return fmt.Errorf("apply store migration 3: %w", err)
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

// reconcileDuplicateRuns keeps the newest non-terminal run per repository
// while migrating legacy rows whose timestamp text may have variable-width
// fractional seconds. Go's RFC3339Nano parser provides chronological ordering
// at nanosecond precision before older duplicates are marked failed.
func reconcileDuplicateRuns(ctx context.Context, tx *sql.Tx) error {
	type runCandidate struct {
		id        string
		updatedAt time.Time
	}
	// First, normalize all legacy timestamps to the canonical fixed-width format
	// so that subsequent SQL ORDER BY operations on created_at/updated_at produce
	// correct chronological results via lexical string comparison.
	allRows, err := tx.QueryContext(ctx, `SELECT id, created_at, updated_at FROM operational_runs`)
	if err != nil {
		return fmt.Errorf("list runs for timestamp normalization: %w", err)
	}
	type timestampUpdate struct {
		id        string
		createdAt string
		updatedAt string
	}
	updates := make([]timestampUpdate, 0)
	for allRows.Next() {
		var id, createdAt, updatedAt string
		if err := allRows.Scan(&id, &createdAt, &updatedAt); err != nil {
			_ = allRows.Close()
			return fmt.Errorf("scan run for timestamp normalization: %w", err)
		}
		parsedCreatedAt, err := time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			_ = allRows.Close()
			return fmt.Errorf("parse run created_at for normalization: %w", err)
		}
		parsedUpdatedAt, err := time.Parse(time.RFC3339Nano, updatedAt)
		if err != nil {
			_ = allRows.Close()
			return fmt.Errorf("parse run updated_at for normalization: %w", err)
		}
		normalizedCreatedAt := parsedCreatedAt.Format(runTimestampLayout)
		normalizedUpdatedAt := parsedUpdatedAt.Format(runTimestampLayout)
		if createdAt != normalizedCreatedAt || updatedAt != normalizedUpdatedAt {
			updates = append(updates, timestampUpdate{
				id:        id,
				createdAt: normalizedCreatedAt,
				updatedAt: normalizedUpdatedAt,
			})
		}
	}
	if err := allRows.Err(); err != nil {
		_ = allRows.Close()
		return fmt.Errorf("read runs for timestamp normalization: %w", err)
	}
	if err := allRows.Close(); err != nil {
		return fmt.Errorf("close runs for timestamp normalization: %w", err)
	}
	for _, update := range updates {
		if _, err := tx.ExecContext(ctx, `UPDATE operational_runs SET created_at = ?, updated_at = ? WHERE id = ?`,
			update.createdAt, update.updatedAt, update.id); err != nil {
			return fmt.Errorf("normalize timestamps for run %q: %w", update.id, err)
		}
	}
	// Now reconcile duplicates using the normalized timestamps
	rows, err := tx.QueryContext(ctx, `
		SELECT id, repository_path, updated_at
		FROM operational_runs
		WHERE status NOT IN (?, ?, ?)`, StatusComplete, StatusCancelled, StatusFailed)
	if err != nil {
		return fmt.Errorf("list non-terminal runs: %w", err)
	}
	newestByRepository := make(map[string]runCandidate)
	duplicates := make([]string, 0)
	for rows.Next() {
		var id, repositoryPath, updatedAt string
		if err := rows.Scan(&id, &repositoryPath, &updatedAt); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan run for reconciliation: %w", err)
		}
		parsedUpdatedAt, err := time.Parse(time.RFC3339Nano, updatedAt)
		if err != nil {
			_ = rows.Close()
			return fmt.Errorf("parse run updated_at for reconciliation: %w", err)
		}
		candidate := runCandidate{id: id, updatedAt: parsedUpdatedAt}
		current, exists := newestByRepository[repositoryPath]
		if !exists || parsedUpdatedAt.After(current.updatedAt) || (parsedUpdatedAt.Equal(current.updatedAt) && id > current.id) {
			if exists {
				duplicates = append(duplicates, current.id)
			}
			newestByRepository[repositoryPath] = candidate
		} else {
			duplicates = append(duplicates, candidate.id)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read runs for reconciliation: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close runs for reconciliation: %w", err)
	}
	for _, id := range duplicates {
		if _, err := tx.ExecContext(ctx, "UPDATE operational_runs SET status = ? WHERE id = ?", StatusFailed, id); err != nil {
			return fmt.Errorf("mark duplicate run %q failed: %w", id, err)
		}
	}
	return nil
}

// backupDatabase creates a private 0600 backup of the database and returns its path.
// It removes an incomplete backup when copying fails.
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
			_ = input.Close()
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
