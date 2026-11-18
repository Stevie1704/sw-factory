package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// RestoreFacts are the facts of a live store that restore preconditions 5
// and 6 of ADR 0032 read.
type RestoreFacts struct {
	// Exists reports that a file exists at the store path.
	Exists bool
	// Readable reports that the store opens read-only with the current
	// schema. The other fields are set only for a readable store.
	Readable bool
	// Damaged reports an existing store file that is empty, corrupt, or not
	// a database. Only a missing or damaged store skips precondition 6.
	Damaged bool
	// Unreadable says why an existing store is not readable.
	Unreadable string
	// Findings are the unmet full-quiescence conditions. A store that exists
	// but is neither readable nor damaged gives the store.unreadable finding:
	// its state is unknown evidence, never success.
	Findings []UpgradeFinding
	// Watermarks are the highest identities that the live store knows.
	Watermarks Watermarks
	// WorkerProcesses are the harness processes that the live store knows.
	WorkerProcesses []WorkerProcess
}

// storeUnreadable is the finding of an existing live store that does not
// open and is not damaged, for example because it is busy, not private, or
// of another schema.
const storeUnreadable = "store.unreadable"

// InspectForRestore reads the restore facts of the live store at path
// through the read-only opener. A missing or damaged store is a fact, not an
// error: restore replaces a damaged store. Any other store that does not
// open gives the store.unreadable finding, so restore refuses.
func InspectForRestore(ctx context.Context, path string) (RestoreFacts, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return RestoreFacts{}, nil
	}
	facts := RestoreFacts{Exists: true}
	opened, err := OpenReadOnly(ctx, path)
	var restore *RestoreInProgressError
	if errors.As(err, &restore) {
		return RestoreFacts{}, err
	}
	if err != nil {
		facts.Unreadable = err.Error()
		facts.Damaged = (info != nil && info.Mode().IsRegular() && info.Size() == 0) || isDamagedDatabase(err)
		if !facts.Damaged {
			facts.Findings = []UpgradeFinding{{Code: storeUnreadable, Detail: "the live store does not open, so restore cannot prove that it has nothing newer than the snapshot: " + err.Error()}}
		}
		return facts, nil
	}
	defer func() { _ = opened.Close() }()
	facts.Readable = true
	if facts.Findings, err = FullQuiescenceFindings(ctx, opened.db); err != nil {
		return RestoreFacts{}, err
	}
	if facts.Watermarks, err = ReadWatermarks(ctx, opened.db); err != nil {
		return RestoreFacts{}, err
	}
	if facts.WorkerProcesses, err = ReadWorkerProcesses(ctx, opened.db); err != nil {
		return RestoreFacts{}, err
	}
	return facts, nil
}

// isDamagedDatabase reports whether err says that SQLite found a corrupt
// file or a file that is not a database.
func isDamagedDatabase(err error) bool {
	var failure *sqlite.Error
	if !errors.As(err, &failure) {
		return false
	}
	switch failure.Code() & 0xff {
	case sqlite3.SQLITE_CORRUPT, sqlite3.SQLITE_NOTADB:
		return true
	}
	return false
}

// ReadSnapshotWorkerProcesses reads the harness processes of a store
// snapshot that no process writes. It opens the file immutable and
// read-only, so it creates no sidecar and changes nothing.
func ReadSnapshotWorkerProcesses(ctx context.Context, path string) ([]WorkerProcess, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve snapshot path: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: absolutePath, RawQuery: "mode=ro&immutable=1"}).String()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open snapshot: %w", err)
	}
	defer func() { _ = database.Close() }()
	return ReadWorkerProcesses(ctx, database)
}

// NewerThan names each kind of record of which w has more records or a
// later one than snapshot, so a restore to snapshot would lose them.
func (w Watermarks) NewerThan(snapshot Watermarks) []string {
	kinds := []struct {
		name           string
		live, recorded Watermark
	}{
		{"runs", w.Runs, snapshot.Runs},
		{"invocations", w.Invocations, snapshot.Invocations},
		{"session invocations", w.SessionInvocations, snapshot.SessionInvocations},
		{"publication ledger entries", w.PublicationLedger, snapshot.PublicationLedger},
		{"local issue identities", w.LocalIssues, snapshot.LocalIssues},
	}
	var newer []string
	for _, kind := range kinds {
		if kind.live.Count > kind.recorded.Count || laterWatermark(kind.live.Latest, kind.recorded.Latest) {
			newer = append(newer, fmt.Sprintf("%s (%d, latest %q; the snapshot has %d, latest %q)", kind.name, kind.live.Count, kind.live.Latest, kind.recorded.Count, kind.recorded.Latest))
		}
	}
	return newer
}

// laterWatermark reports whether live is later than recorded. Numbers
// compare as numbers; stored times compare as text.
func laterWatermark(live, recorded string) bool {
	liveNumber, liveErr := strconv.Atoi(live)
	recordedNumber, recordedErr := strconv.Atoi(recorded)
	if liveErr == nil && recordedErr == nil {
		return liveNumber > recordedNumber
	}
	return live > recorded
}
