package store

import (
	"errors"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// IsTransient reports whether err is a store failure that a later attempt
// can succeed after: a busy or locked database, an I/O error, or a full
// disk. A caller retries such a failure instead of reporting it as a final
// refusal.
func IsTransient(err error) bool {
	var failure *sqlite.Error
	if !errors.As(err, &failure) {
		return false
	}
	switch failure.Code() & 0xff {
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED, sqlite3.SQLITE_IOERR, sqlite3.SQLITE_FULL:
		return true
	}
	return false
}
