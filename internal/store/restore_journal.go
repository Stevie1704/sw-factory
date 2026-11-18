package store

import (
	"errors"
	"fmt"
	"os"
)

// restoreJournalSuffix names the restore journal beside a store (ADR 0032).
const restoreJournalSuffix = ".restore.json"

// RestoreJournalPath returns the path of the restore journal of the store at
// storePath. While this file exists, the files of the store can be split
// between the live path and the restore work directory.
func RestoreJournalPath(storePath string) string {
	return storePath + restoreJournalSuffix
}

// RestoreInProgressError refuses to open a store while a restore journal
// exists. Only factory checkpoint restore continues that restore.
type RestoreInProgressError struct {
	// Journal is the path of the restore journal.
	Journal string
}

// Error names the journal and the command that continues the restore.
func (e *RestoreInProgressError) Error() string {
	return fmt.Sprintf("an interrupted restore journal exists at %s; continue it with factory checkpoint restore --config <path> --path <checkpoint> --json before the store is used", e.Journal)
}

// refuseDuringRestore returns a *RestoreInProgressError when a restore
// journal exists beside the store at absolutePath. A journal that cannot be
// inspected is an error, never a pass.
func refuseDuringRestore(absolutePath string) error {
	journal := RestoreJournalPath(absolutePath)
	_, err := os.Lstat(journal)
	switch {
	case err == nil:
		return &RestoreInProgressError{Journal: journal}
	case errors.Is(err, os.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("inspect the restore journal %s: %w", journal, err)
	}
}
