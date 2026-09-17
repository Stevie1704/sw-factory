package factory

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestSupervisorHeartbeatLoopRetriesAfterWriteFailure verifies one transient
// renewal failure does not stop subsequent heartbeat writes.
func TestSupervisorHeartbeatLoopRetriesAfterWriteFailure(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	calls := 0
	firstCall := make(chan struct{}, 1)
	loop := &supervisorHeartbeatLoop{
		write: func(context.Context, store.SupervisorHeartbeat) error {
			mu.Lock()
			calls++
			call := calls
			mu.Unlock()
			if call == 1 {
				firstCall <- struct{}{}
				return errors.New("sqlite busy")
			}
			return nil
		},
		coordinator: "coordinator-test",
		startedAt:   time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC),
		ttl:         time.Minute,
		cadence:     time.Millisecond,
		now:         func() time.Time { return time.Date(2026, 9, 17, 10, 0, 1, 0, time.UTC) },
		done:        make(chan struct{}),
		errors:      make(chan error, 1),
	}
	ctx, cancel := context.WithCancel(context.Background())
	loop.cancel = cancel
	go loop.run(ctx)

	select {
	case <-firstCall:
	case <-time.After(time.Second):
		t.Fatal("heartbeat loop did not attempt its first renewal")
	}
	deadline := time.After(time.Second)
	for {
		mu.Lock()
		got := calls
		mu.Unlock()
		if got >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("heartbeat writes = %d, want at least 3 after one failure", got)
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-loop.done
}
