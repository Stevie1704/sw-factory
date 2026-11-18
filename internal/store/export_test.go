package store

import (
	"context"
	"database/sql"
	"time"
)

// SnapshotWithVacuumForTest runs Snapshot with an injected VACUUM INTO
// step, so a test can make the snapshot fail as a full disk does.
func SnapshotWithVacuumForTest(ctx context.Context, source, destination string, check SnapshotCheck, vacuum func(context.Context, *sql.Conn, string) error) (SnapshotInspection, error) {
	return snapshot(ctx, source, destination, check, vacuum)
}

// SetInboxBusyTimeoutForTest sets the busy timeout that OpenCommandInbox applies
// to later opens and returns the previous value, so a test can restore it.
func SetInboxBusyTimeoutForTest(timeout time.Duration) time.Duration {
	previous := inboxBusyTimeout
	inboxBusyTimeout = timeout
	return previous
}
