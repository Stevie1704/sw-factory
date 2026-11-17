package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// MinimumUpgradeSchemaVersion is the oldest operational-store schema that the
// read-only upgrade inspection reads. From this version on, the store has the
// run, invocation, and pending-effect tables that prove quiescence.
const MinimumUpgradeSchemaVersion = 21

// UpgradeInspection is the read-only view of an operational store that an
// upgrade plan needs. Its run facts are set only when Supported is true.
type UpgradeInspection struct {
	// Path is the absolute store path.
	Path string
	// Exists reports that the store file exists.
	Exists bool
	// SchemaVersion is the recorded schema version of an existing store.
	SchemaVersion int
	// InterruptedWrite reports a rollback journal beside the store: a writer
	// stopped during a transaction, and only a read-write open rolls it back.
	InterruptedWrite bool
	// ActiveRuns lists the IDs of the runs with a non-terminal status.
	ActiveRuns []string
	// ActiveInvocations counts the invocations that are still active.
	ActiveInvocations int
	// PendingEffects counts the journaled external effects.
	PendingEffects int
	// Backups lists the pre-migration backups of this exact store.
	Backups []string
}

// Supported reports whether this build can inspect and migrate the store.
func (i UpgradeInspection) Supported() bool {
	return i.Exists && i.SchemaVersion >= MinimumUpgradeSchemaVersion && i.SchemaVersion <= CurrentSchemaVersion
}

// Migration describes what one explicit migration did.
type Migration struct {
	// FromVersion and ToVersion are the schema versions before and after.
	FromVersion int `json:"from_version"`
	ToVersion   int `json:"to_version"`
	// Backup is the pre-migration backup path, or empty when nothing migrated.
	Backup string `json:"backup,omitempty"`
}

// InspectForUpgrade reads the schema version and the active work of an
// existing store through a narrow read-only path that also accepts an older
// supported schema. It never creates, migrates, backs up, or changes the
// store. A missing store is reported, not an error.
func InspectForUpgrade(ctx context.Context, path string) (UpgradeInspection, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return UpgradeInspection{}, fmt.Errorf("resolve operational store path: %w", err)
	}
	inspection := UpgradeInspection{Path: absolutePath}
	if _, err := os.Lstat(absolutePath); errors.Is(err, os.ErrNotExist) {
		return inspection, nil
	}
	if _, err := checkExistingStoreFile(absolutePath); err != nil {
		return inspection, err
	}
	inspection.Exists = true
	if inspection.Backups, err = migrationBackups(absolutePath); err != nil {
		return inspection, err
	}
	if journal, err := os.Stat(absolutePath + "-journal"); err == nil && journal.Size() > 0 {
		inspection.InterruptedWrite = true
		return inspection, nil
	}
	database, err := openReadOnlyDatabase(ctx, absolutePath)
	if err != nil {
		return inspection, err
	}
	defer func() { _ = database.Close() }()
	if inspection.SchemaVersion, err = readSchemaVersion(ctx, database, absolutePath); err != nil {
		return inspection, err
	}
	if !inspection.Supported() {
		return inspection, nil
	}
	return inspection, readActiveWork(ctx, database, &inspection)
}

// readActiveWork reads the facts that decide whether the store is quiescent.
func readActiveWork(ctx context.Context, database *sql.DB, inspection *UpgradeInspection) error {
	rows, err := database.QueryContext(ctx, "SELECT id FROM operational_runs WHERE status NOT IN (?, ?, ?) ORDER BY id", StatusComplete, StatusCancelled, StatusFailed)
	if err != nil {
		return fmt.Errorf("read active runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("read active runs: %w", err)
		}
		inspection.ActiveRuns = append(inspection.ActiveRuns, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read active runs: %w", err)
	}
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM invocations WHERE status = ?", InvocationStatusActive).Scan(&inspection.ActiveInvocations); err != nil {
		return fmt.Errorf("read active invocations: %w", err)
	}
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM pending_effects").Scan(&inspection.PendingEffects); err != nil {
		return fmt.Errorf("read pending effects: %w", err)
	}
	return nil
}

// migrationBackups lists the pre-migration backups whose name proves they
// belong to the store at path.
func migrationBackups(path string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("list operational store backups: %w", err)
	}
	prefix := filepath.Base(path) + ".bak-"
	var backups []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) && entry.Type().IsRegular() {
			backups = append(backups, filepath.Join(filepath.Dir(path), entry.Name()))
		}
	}
	sort.Strings(backups)
	return backups, nil
}

// Migrate is the explicit store migration entry point. It opens the store
// read-write, which rolls back an interrupted transaction, then backs up and
// migrates an older supported schema in one transaction. A current store is
// left unchanged. The caller must hold the coordinator lock.
func Migrate(ctx context.Context, path string) (Migration, error) {
	opened, migration, err := openMigrating(ctx, path)
	if err != nil {
		return Migration{}, err
	}
	if err := opened.Close(); err != nil {
		return Migration{}, fmt.Errorf("close operational store after migration: %w", err)
	}
	return migration, nil
}

// RollBackInterruptedWrite opens an existing store read-write without a
// migration, so SQLite rolls back the transaction of an interrupted writer
// from its journal. The caller must hold the coordinator lock.
func RollBackInterruptedWrite(ctx context.Context, path string) error {
	absolutePath, err := checkExistingStoreFile(path)
	if err != nil {
		return err
	}
	database, err := openConfiguredDatabase(ctx, absolutePath)
	if err != nil {
		return err
	}
	_, readErr := readSchemaVersion(ctx, database, absolutePath)
	if closeErr := database.Close(); readErr == nil && closeErr != nil {
		return fmt.Errorf("close operational store after rollback: %w", closeErr)
	}
	return readErr
}
