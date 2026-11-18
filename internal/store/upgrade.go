package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// MinimumUpgradeSchemaVersion is the oldest operational-store schema that an
// upgrade accepts. It is a literal on purpose: it moves only by a deliberate
// decision, never as a side effect of a new CurrentSchemaVersion, so the next
// release still accepts the store of this one.
const MinimumUpgradeSchemaVersion = 53

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
	// ActiveSessionInvocations counts the session-role invocations that are
	// still active; each one owns a worker.
	ActiveSessionInvocations int
	// UnreleasedSessionInvocations counts the ended session-role
	// invocations whose worker and snapshot are not proved released: the
	// role worker can still run.
	UnreleasedSessionInvocations int
	// QueuedSessionCommands counts the session commands that the
	// coordinator has not completed.
	QueuedSessionCommands int
	// WorkerProcesses lists every process that the store launched in a
	// worker; the caller checks with Docker whether one of them still runs.
	WorkerProcesses []WorkerProcess
	// Backups lists the pre-migration backups of this exact store.
	Backups []string
}

// Supported reports whether this build can inspect and migrate the store:
// its schema lies from MinimumUpgradeSchemaVersion to CurrentSchemaVersion.
func (i UpgradeInspection) Supported() bool {
	return i.Exists && i.SchemaVersion >= MinimumUpgradeSchemaVersion && i.SchemaVersion <= CurrentSchemaVersion
}

// UpgradeFinding is one stable reason why a store must not be migrated now.
// The upgrade plan and the migration entry point report the same findings.
type UpgradeFinding struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// SchemaFinding returns the finding of a store that is missing, newer than
// the current schema, or older than the minimum upgrade schema, or nil.
func (i UpgradeInspection) SchemaFinding() *UpgradeFinding {
	switch {
	case !i.Exists:
		return &UpgradeFinding{Code: "store.missing", Detail: "no operational store exists at " + i.Path}
	case i.SchemaVersion > CurrentSchemaVersion:
		return &UpgradeFinding{Code: "store.schema_newer", Detail: fmt.Sprintf("the store has schema %d; this build supports %d", i.SchemaVersion, CurrentSchemaVersion)}
	case !i.Supported():
		return &UpgradeFinding{Code: "store.schema_unsupported", Detail: fmt.Sprintf("the store has schema %d; this build accepts %d to %d", i.SchemaVersion, MinimumUpgradeSchemaVersion, CurrentSchemaVersion)}
	}
	return nil
}

// ActivityFindings returns one finding per kind of active work that the
// migration gate refuses: runs, invocations, journaled effects, active or
// unreleased session-role invocations, and queued session commands.
func (i UpgradeInspection) ActivityFindings() []UpgradeFinding {
	var findings []UpgradeFinding
	if len(i.ActiveRuns) != 0 {
		findings = append(findings, UpgradeFinding{Code: "run.active", Detail: "active runs: " + strings.Join(i.ActiveRuns, ", ")})
	}
	if i.ActiveInvocations != 0 {
		findings = append(findings, UpgradeFinding{Code: "invocation.active", Detail: fmt.Sprintf("%d invocations are active", i.ActiveInvocations)})
	}
	if i.PendingEffects != 0 {
		findings = append(findings, UpgradeFinding{Code: "effect.pending", Detail: fmt.Sprintf("%d external effects are journaled", i.PendingEffects)})
	}
	if i.ActiveSessionInvocations != 0 {
		findings = append(findings, UpgradeFinding{Code: "session_invocation.active", Detail: fmt.Sprintf("%d session-role invocations are active", i.ActiveSessionInvocations)})
	}
	if i.UnreleasedSessionInvocations != 0 {
		findings = append(findings, UpgradeFinding{Code: "session_invocation.unreleased", Detail: fmt.Sprintf("%d ended session-role invocations have no proved resource release", i.UnreleasedSessionInvocations)})
	}
	if i.QueuedSessionCommands != 0 {
		findings = append(findings, UpgradeFinding{Code: "session_command.queued", Detail: fmt.Sprintf("%d session commands are not completed", i.QueuedSessionCommands)})
	}
	return findings
}

// Migration describes what one explicit migration did.
type Migration struct {
	// FromVersion and ToVersion are the schema versions before and after.
	FromVersion int `json:"from_version"`
	ToVersion   int `json:"to_version"`
	// Backup is the pre-migration backup path, or empty when nothing migrated.
	Backup string `json:"backup,omitempty"`
	// Provenance is the provenance file beside Backup, or empty when nothing migrated.
	Provenance string `json:"provenance,omitempty"`
}

// ReleaseIdentity names the Factory build that migrates a store. The
// pre-migration backup records it in its provenance file.
type ReleaseIdentity struct {
	// Version is the release version, or "development" for a development build.
	Version string `json:"version"`
	// Revision is the source commit, or "unknown" when the build did not record it.
	Revision string `json:"revision"`
}

// InspectForUpgrade reads the schema version and the active work of an
// existing store through a narrow read-only path that reads the version of
// any schema. It never creates, migrates, backs up, or changes the store. A missing store is reported, not an error.
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
	if err := readActiveWork(ctx, database, &inspection); err != nil {
		return inspection, err
	}
	inspection.WorkerProcesses, err = ReadWorkerProcesses(ctx, database)
	return inspection, err
}

// readActiveWork reads the facts that decide whether the store passes the
// migration gate. It reads through any Querier, so the same facts serve the
// read-only upgrade inspection and the full-quiescence check under the
// snapshot write lock.
func readActiveWork(ctx context.Context, database Querier, inspection *UpgradeInspection) error {
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
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM ideation_invocations WHERE status = ?", SessionInvocationActive).Scan(&inspection.ActiveSessionInvocations); err != nil {
		return fmt.Errorf("read active session invocations: %w", err)
	}
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM ideation_invocations WHERE status <> ? AND resources_released_at IS NULL", SessionInvocationActive).Scan(&inspection.UnreleasedSessionInvocations); err != nil {
		return fmt.Errorf("read unreleased session invocations: %w", err)
	}
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM ideation_commands WHERE state <> ?", IdeationCommandCompleted).Scan(&inspection.QueuedSessionCommands); err != nil {
		return fmt.Errorf("read queued session commands: %w", err)
	}
	return nil
}

// migrationBackups lists the pre-migration backups whose name proves they
// belong to the store at path. A provenance file is not a backup, so it is not
// listed.
func migrationBackups(path string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("list operational store backups: %w", err)
	}
	prefix := filepath.Base(path) + ".bak-"
	var backups []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) && !strings.HasSuffix(entry.Name(), backupProvenanceSuffix) && entry.Type().IsRegular() {
			backups = append(backups, filepath.Join(filepath.Dir(path), entry.Name()))
		}
	}
	sort.Strings(backups)
	return backups, nil
}

// Migrate is the explicit store migration entry point. It opens the store
// read-write, which rolls back an interrupted transaction, then backs up and
// migrates an older supported schema in one transaction. release names this
// build in the provenance file of the backup. A current store is left
// unchanged. The caller must hold the coordinator lock.
func Migrate(ctx context.Context, path string, release ReleaseIdentity) (Migration, error) {
	opened, migration, err := openMigrating(ctx, path, true, release)
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
