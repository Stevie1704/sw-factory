package store

import "time"

// SetInboxBusyTimeoutForTest sets the busy timeout that OpenCommandInbox applies
// to later opens and returns the previous value, so a test can restore it.
func SetInboxBusyTimeoutForTest(timeout time.Duration) time.Duration {
	previous := inboxBusyTimeout
	inboxBusyTimeout = timeout
	return previous
}
