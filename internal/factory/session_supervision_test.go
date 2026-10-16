package factory_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/publication"
	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// errSessionStoreBusy is the session-store failure of the supervision tests.
var errSessionStoreBusy = errors.New("session store unavailable")

// sessionCommandStoreForTest is the session-command store seam of the
// coordinator, as one interface to embed.
type sessionCommandStoreForTest interface {
	sessioncommand.Store
	publication.Store
}

// failingSessionStore is a run store whose session-role and session-command
// reads fail. Only the failing methods are implemented; the embedded
// interfaces are nil.
type failingSessionStore struct {
	*fakeRunStore
	factory.SessionRoleStore
	sessionCommandStoreForTest
}

// ActiveSessionInvocations fails as a broken store does.
func (failingSessionStore) ActiveSessionInvocations(context.Context) ([]store.SessionInvocation, error) {
	return nil, errSessionStoreBusy
}

// PendingIdeationCommands fails as a broken store does.
func (failingSessionStore) PendingIdeationCommands(context.Context) ([]store.IdeationCommand, error) {
	return nil, errSessionStoreBusy
}

// IdeationSession resolves the ambiguity of the embedded seams; the tests
// never reach it.
func (failingSessionStore) IdeationSession(context.Context, string) (store.IdeationSession, error) {
	return store.IdeationSession{}, errSessionStoreBusy
}

// IdeationHistory resolves the ambiguity of the embedded seams; the tests
// never reach it.
func (failingSessionStore) IdeationHistory(context.Context, string) (store.IdeationHistory, error) {
	return store.IdeationHistory{}, errSessionStoreBusy
}

// TransitionIdeationSession resolves the ambiguity of the embedded seams;
// the tests never reach it.
func (failingSessionStore) TransitionIdeationSession(context.Context, store.IdeationWrite) (store.IdeationSession, error) {
	return store.IdeationSession{}, errSessionStoreBusy
}

// CloseSessionInvocation resolves the ambiguity of the embedded seams; the
// tests never reach it.
func (failingSessionStore) CloseSessionInvocation(context.Context, store.SessionInvocationEnd) (store.SessionInvocation, error) {
	return store.SessionInvocation{}, errSessionStoreBusy
}

// TestSessionSupervisionFailuresAreRetryEventsWithAReasonAndAnAttempt verifies
// that a failing session-role reconciliation and a failing session-command
// pass never stop the coordinator, and that each is reported as a retry
// event with a reason and a consecutive attempt count.
func TestSessionSupervisionFailuresAreRetryEventsWithAReasonAndAnAttempt(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	var cancel context.CancelFunc
	lease := &pollingLease{}
	lease.onRenew = func(tracker.Lease) {
		if len(lease.calls) == 3 && cancel != nil {
			cancel()
		}
	}
	var events pollingEventSink
	service := newPollingService(root, &fakeGitHub{}, &pollingIssueReader{}, lease, failingSessionStore{fakeRunStore: &fakeRunStore{}}, &fakeWorktree{}, nil)
	ctx, stop := context.WithCancel(context.Background())
	cancel = stop

	if err := service.Start(ctx, &events); err != nil {
		t.Fatalf("Start() error = %v, want session failures to be non-fatal", err)
	}
	attempts := map[string][]int{}
	for _, event := range events.events {
		if event.Kind != factory.EventRetry || (event.Operation != "session role reconciliation" && event.Operation != "session command dispatch") {
			continue
		}
		if event.Reason == "" {
			t.Errorf("retry event %#v has no reason", event)
		}
		attempts[event.Operation] = append(attempts[event.Operation], event.Attempt)
	}
	if got := attempts["session role reconciliation"]; len(got) < 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("session role reconciliation attempts = %v, want 1, 2, ...", got)
	}
	if got := attempts["session command dispatch"]; len(got) < 1 || got[0] != 1 {
		t.Errorf("session command dispatch attempts = %v, want 1, ...", got)
	}
}
