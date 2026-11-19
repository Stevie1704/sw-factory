package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// backupProvenanceSuffix is appended to a pre-migration backup path to name
// its provenance file.
const backupProvenanceSuffix = ".json"

// backupProvenance is the content of <store>.bak-<timestamp>.json. It ties a
// pre-migration backup to the schema it was taken from, the schema the
// migration targets, the build that migrated it, and the exact bytes of the
// backup.
type backupProvenance struct {
	SourceSchemaVersion int             `json:"source_schema_version"`
	TargetSchemaVersion int             `json:"target_schema_version"`
	MigratingRelease    ReleaseIdentity `json:"migrating_release"`
	SHA256              string          `json:"sha256"`
	Size                int64           `json:"size"`
}

// MigrationGateError reports that the store did not pass the migration gate
// when the pre-migration backup checked it again under the write lock. Work
// entered the store after the earlier gate check, so nothing was backed up or
// migrated.
type MigrationGateError struct {
	// Findings are the migration gate findings under the write lock.
	Findings []UpgradeFinding
}

// Error lists the codes of the findings.
func (e *MigrationGateError) Error() string {
	codes := make([]string, 0, len(e.Findings))
	for _, finding := range e.Findings {
		codes = append(codes, finding.Code)
	}
	return "the operational store no longer passes the migration gate: " + strings.Join(codes, ", ")
}

// migrationGateCheck is the SnapshotCheck of the pre-migration backup. It
// reads the store facts of the migration gate on the connection that holds the
// write lock, so no writer can add work between this check and the snapshot.
func migrationGateCheck(ctx context.Context, view Querier) error {
	var inspection UpgradeInspection
	if err := readActiveWork(ctx, view, &inspection); err != nil {
		return err
	}
	if findings := inspection.ActivityFindings(); len(findings) != 0 {
		return &MigrationGateError{Findings: findings}
	}
	return nil
}

// backupStagingSuffix marks a backup file or provenance file that is still
// being written. The final name appears only by an atomic rename, so a
// stopped backup leaves at most staging files, which are never a backup.
const backupStagingSuffix = ".partial"

// backupDatabase takes the pre-migration backup of the store at path and
// returns the backup path. The backup is an online snapshot that checks the
// store facts of the migration gate again under the write lock, so it uses
// the same sequence as Snapshot and never copies the live file. A failed
// check returns *MigrationGateError. Both the backup and its provenance are
// written to staging names first and then renamed into place, the provenance
// last, so a backup with provenance is always complete. On any failure,
// neither file remains.
func backupDatabase(ctx context.Context, path string, release ReleaseIdentity) (string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		backupPath := path + ".bak-" + backupTimestamp(attempt)
		free, err := backupPathFree(backupPath)
		if err != nil {
			return "", err
		}
		if !free {
			continue
		}
		if err := writeBackup(ctx, path, backupPath, release); err != nil {
			removeBackupFiles(backupPath)
			return "", err
		}
		return backupPath, nil
	}
	return "", errors.New("could not choose a unique store backup path")
}

// writeBackup writes the snapshot and the provenance to their staging names,
// then renames the snapshot and, last, the provenance into place and syncs
// the directory.
func writeBackup(ctx context.Context, path, backupPath string, release ReleaseIdentity) error {
	provenancePath := backupPath + backupProvenanceSuffix
	inspection, err := snapshot(ctx, path, backupPath+backupStagingSuffix, migrationGateCheck, true, vacuumOnConnection)
	if err != nil {
		return fmt.Errorf("back up the operational store before migration: %w", err)
	}
	provenance := backupProvenance{
		SourceSchemaVersion: inspection.File.SchemaVersion,
		TargetSchemaVersion: CurrentSchemaVersion,
		MigratingRelease:    release,
		SHA256:              inspection.File.SHA256,
		Size:                inspection.File.Size,
	}
	if err := writeBackupProvenance(provenancePath+backupStagingSuffix, provenance); err != nil {
		return err
	}
	for _, name := range []string{backupPath, provenancePath} {
		if err := os.Rename(name+backupStagingSuffix, name); err != nil {
			return fmt.Errorf("move store backup into place: %w", err)
		}
	}
	return syncBackupDirectory(filepath.Dir(backupPath))
}

// removeBackupFiles removes the backup, its provenance, and their staging
// files after a failed backup.
func removeBackupFiles(backupPath string) {
	for _, name := range backupFileNames(backupPath) {
		_ = os.Remove(name)
	}
}

// backupFileNames lists every name that one backup can use: the backup, its
// provenance, and the staging name of each.
func backupFileNames(backupPath string) []string {
	provenancePath := backupPath + backupProvenanceSuffix
	return []string{backupPath, provenancePath, backupPath + backupStagingSuffix, provenancePath + backupStagingSuffix}
}

// syncBackupDirectory syncs the directory so the renames survive a crash.
func syncBackupDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open store backup directory: %w", err)
	}
	syncErr := handle.Sync()
	if closeErr := handle.Close(); syncErr == nil {
		syncErr = closeErr
	}
	if syncErr != nil {
		return fmt.Errorf("sync store backup directory: %w", syncErr)
	}
	return nil
}

// backupTimestamp names a backup by its UTC creation instant. A later attempt
// adds a counter, so two backups in the same instant never share a name.
func backupTimestamp(attempt int) string {
	suffix := time.Now().UTC().Format("20060102T150405.000000000Z07:00")
	if attempt > 0 {
		suffix += "-" + strconv.Itoa(attempt)
	}
	return suffix
}

// backupPathFree reports whether no file of the backup at backupPath exists
// yet, staging files included.
func backupPathFree(backupPath string) (bool, error) {
	for _, name := range backupFileNames(backupPath) {
		_, err := os.Lstat(name)
		if err == nil {
			return false, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("inspect store backup path: %w", err)
		}
	}
	return true, nil
}

// verifiedBackup reports whether the backup at backupPath has a readable
// provenance file whose SHA-256 and size match the backup bytes. Any read
// error counts as unverified, so an incomplete backup is never listed.
func verifiedBackup(backupPath string) bool {
	content, err := os.ReadFile(backupPath + backupProvenanceSuffix)
	if err != nil {
		return false
	}
	var provenance backupProvenance
	if err := json.Unmarshal(content, &provenance); err != nil || provenance.SHA256 == "" {
		return false
	}
	size, digest, err := FileDigest(backupPath)
	return err == nil && size == provenance.Size && digest == provenance.SHA256
}

// writeBackupProvenance creates the provenance file privately, syncs it, and
// removes it again when any write step fails.
func writeBackupProvenance(path string, provenance backupProvenance) error {
	content, err := json.MarshalIndent(provenance, "", "  ")
	if err != nil {
		return fmt.Errorf("encode store backup provenance: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create store backup provenance: %w", err)
	}
	_, writeErr := file.Write(append(content, '\n'))
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if closeErr := file.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		_ = os.Remove(path)
		return fmt.Errorf("write store backup provenance: %w", writeErr)
	}
	return nil
}
