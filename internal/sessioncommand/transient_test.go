package sessioncommand_test

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// busyErrorForTest returns a real SQLite busy error: a second connection
// without a busy timeout writes while the first holds an exclusive lock.
func busyErrorForTest(t *testing.T) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "busy.db")
	open := func(query string) *sql.DB {
		database, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: query}).String())
		if err != nil {
			t.Fatal(err)
		}
		database.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = database.Close() })
		return database
	}
	holder := open("")
	if _, err := holder.ExecContext(t.Context(), "CREATE TABLE lock (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	tx, err := holder.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.ExecContext(t.Context(), "INSERT INTO lock VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	_, err = open("_pragma=busy_timeout(0)").ExecContext(t.Context(), "INSERT INTO lock VALUES (2)")
	if err == nil || !store.IsTransient(err) {
		t.Fatalf("second writer error = %v, want a transient busy error", err)
	}
	return err
}

// busyOnceStore fails the first session read with a transient store error.
type busyOnceStore struct {
	*store.Store
	err    error
	failed bool
}

// IdeationSession returns the transient error once.
func (b *busyOnceStore) IdeationSession(ctx context.Context, id string) (store.IdeationSession, error) {
	if !b.failed {
		b.failed = true
		return store.IdeationSession{}, b.err
	}
	return b.Store.IdeationSession(ctx, id)
}

// TestTransientStoreFailureLeavesTheCommandForTheNextPass verifies that a
// busy or failing store does not settle a command as refused: the command
// stays pending, and the next pass applies it.
func TestTransientStoreFailureLeavesTheCommandForTheNextPass(t *testing.T) {
	t.Parallel()

	c := newCoordinatorForTest(t)
	revision := c.createSession("session-1")
	c.submit("defer-1", "session-1", sessioncommand.KindDefer, revision, `{"reason":"Later."}`)
	busy := sessioncommand.NewDispatcher(sessioncommand.Env{
		Store: &busyOnceStore{Store: c.store, err: busyErrorForTest(t)}, Lifecycle: workflow.DefaultSessionLifecycle(),
		Now: func() time.Time { return dispatchNow },
	})
	if err := busy.Drain(t.Context()); err == nil {
		t.Fatal("Drain() hid the transient store failure")
	}
	if command := c.command("defer-1"); command.State != store.IdeationCommandApplying || command.Outcome != "" {
		t.Fatalf("command after a busy store = %#v, want it pending without an outcome", command)
	}

	c.drain()
	if applied := c.requireOutcome("defer-1", store.IdeationCommandApplied); applied.Attempts != 2 {
		t.Fatalf("attempts = %d, want the second pass to apply it", applied.Attempts)
	}
}
