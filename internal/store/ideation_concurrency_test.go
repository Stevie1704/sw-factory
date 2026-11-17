package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// inboxBusyTimeoutForTest is the busy timeout of OpenCommandInbox. SQLite's
// busy handler gives up after this long.
const inboxBusyTimeoutForTest = 5 * time.Second

// TestCommandInboxAppendOutlastsOneBusyTimeout verifies that an append still
// succeeds when another connection holds SQLite's write lock for longer than
// one busy timeout. The first attempt gives up at the timeout; the append
// then retries its transaction once the lock is free.
func TestCommandInboxAppendOutlastsOneBusyTimeout(t *testing.T) {
	t.Parallel()

	opened, path := openIdeationStoreForTest(t)
	approvedSessionForTest(t, opened, "session-1")
	inbox, err := store.OpenCommandInbox(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inbox.Close() })
	holdWriteLockForTest(t, path, inboxBusyTimeoutForTest+500*time.Millisecond)

	if _, _, err := inbox.AcceptIdeationCommand(t.Context(), commandForTest("request-busy", "session-1", 1)); err != nil {
		t.Fatalf("append while the store stays busy past one timeout: %v", err)
	}
}

// holdWriteLockForTest takes SQLite's write lock on path from a second
// connection and releases it after hold. The release runs in the background,
// so the caller can use the store while the lock is held.
func holdWriteLockForTest(t *testing.T, path string, hold time.Duration) {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	conn, err := database.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("take the write lock: %v", err)
	}
	released := make(chan error, 1)
	timer := time.AfterFunc(hold, func() {
		_, err := conn.ExecContext(context.Background(), "COMMIT")
		released <- err
	})
	t.Cleanup(func() {
		if timer.Stop() {
			return
		}
		if err := <-released; err != nil {
			t.Errorf("release the write lock: %v", err)
		}
	})
}

// TestCoordinatorWritesSucceedWhileTheUIAppendsCommands verifies that the
// coordinator's ledger and invocation writes reserve SQLite's write lock
// before they read. The UI appends inbox commands through a second
// connection at the same time; a writer that reads first cannot upgrade its
// lock past that connection and fails with SQLITE_BUSY at once instead of
// waiting.
func TestCoordinatorWritesSucceedWhileTheUIAppendsCommands(t *testing.T) {
	t.Parallel()

	const rounds = 150
	opened, path := openIdeationStoreForTest(t)
	approvedSessionForTest(t, opened, "session-1")
	planned, err := opened.StartPublicationLedger(t.Context(), "session-1", "approval-1", publicationOperationsForTest(), ideationNow)
	if err != nil {
		t.Fatal(err)
	}
	invocations := make([]store.SessionInvocation, 0, rounds)
	for index := range rounds {
		session := createIdeationSessionForTest(t, opened, fmt.Sprintf("session-role-%d", index))
		_, invocation := startInvocationForTest(t, opened, session, fmt.Sprintf("invocation-%d", index), "turn-1")
		invocations = append(invocations, invocation)
	}
	inbox, err := store.OpenCommandInbox(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inbox.Close() })

	var group sync.WaitGroup
	failures := make(chan error, 3*rounds)
	group.Go(func() {
		for index := range rounds {
			if _, _, err := inbox.AcceptIdeationCommand(t.Context(), commandForTest(fmt.Sprintf("request-%d", index), "session-1", 1)); err != nil {
				failures <- fmt.Errorf("inbox append %d: %w", index, err)
			}
		}
	})
	group.Go(func() {
		for index := range rounds {
			operation := planned[0]
			operation.State, operation.Attempts = store.PublicationOperationAttempted, index+1
			if _, err := opened.RecordPublicationOperation(t.Context(), operation); err != nil {
				failures <- fmt.Errorf("ledger write %d: %w", index, err)
			}
			end := store.SessionInvocationEnd{InvocationID: invocations[index].ID, Status: store.SessionInvocationCancelled, Reason: "the session left the pinned revision", At: ideationNow}
			if _, err := opened.CloseSessionInvocation(t.Context(), end); err != nil {
				failures <- fmt.Errorf("invocation close %d: %w", index, err)
			}
		}
	})
	group.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
}
