package checkpoint

import "context"

// ApplyRestoreWithCrashForTest runs ApplyRestore and calls crash at each
// named crash point. An error from crash stops the restore at once, as a
// process crash does, without any cleanup.
func ApplyRestoreWithCrashForTest(ctx context.Context, plan RestorePlan, crash func(point string) error) error {
	return (&restorer{journal: RestoreJournal{SchemaVersion: RestoreJournalSchemaVersion, RestorePlan: plan}, crash: crash}).apply(ctx)
}
