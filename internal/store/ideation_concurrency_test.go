package store_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/store"
)

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
