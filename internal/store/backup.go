package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
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

// backupDatabase takes the pre-migration backup of the store at path and
// returns the backup path. The backup is an online snapshot with no quiescence
// check, so it uses the same sequence as Snapshot and never copies the live
// file. It then writes the provenance file beside it. On any failure, neither
// file remains.
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
		inspection, err := snapshot(ctx, path, backupPath, nil, true, vacuumOnConnection)
		if err != nil {
			return "", fmt.Errorf("back up the operational store before migration: %w", err)
		}
		provenance := backupProvenance{
			SourceSchemaVersion: inspection.File.SchemaVersion,
			TargetSchemaVersion: CurrentSchemaVersion,
			MigratingRelease:    release,
			SHA256:              inspection.File.SHA256,
			Size:                inspection.File.Size,
		}
		if err := writeBackupProvenance(backupPath+backupProvenanceSuffix, provenance); err != nil {
			_ = os.Remove(backupPath)
			return "", err
		}
		return backupPath, nil
	}
	return "", errors.New("could not choose a unique store backup path")
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

// backupPathFree reports whether neither the backup nor its provenance file
// at backupPath exists yet.
func backupPathFree(backupPath string) (bool, error) {
	for _, name := range []string{backupPath, backupPath + backupProvenanceSuffix} {
		_, err := os.Stat(name)
		if err == nil {
			return false, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("inspect store backup path: %w", err)
		}
	}
	return true, nil
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
