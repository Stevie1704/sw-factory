package factory

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// supervisorHeartbeatLoop renews one local coordinator heartbeat until the
// polling supervisor stops. Its store stays open for the loop lifetime so a
// long progression pass does not need to interrupt its own work to publish
// liveness.
type supervisorHeartbeatLoop struct {
	writer      SupervisorHeartbeatWriter
	operational OperationalStore
	coordinator string
	startedAt   time.Time
	ttl         time.Duration
	cadence     time.Duration
	now         Clock
	cancel      context.CancelFunc
	done        chan struct{}
	errors      chan error
	once        sync.Once
}

// startSupervisorHeartbeat opens the operational store, records an initial
// heartbeat, and starts renewal when the store supports the heartbeat
// projection. Older test-only store adapters remain usable without the
// optional projection seam.
func (s *Service) startSupervisorHeartbeat(ctx context.Context, registration config.RepositoryRegistration, interval, backoff time.Duration) (*supervisorHeartbeatLoop, error) {
	operational, err := s.deps.OpenStore(ctx, registration.OperationalDataPath)
	if err != nil {
		return nil, fmt.Errorf("open operational store for supervisor heartbeat: %w", err)
	}
	writer, ok := operational.(SupervisorHeartbeatWriter)
	if !ok {
		if closeErr := operational.Close(); closeErr != nil {
			return nil, fmt.Errorf("close operational store without supervisor heartbeat support: %w", closeErr)
		}
		return nil, nil
	}
	ttl := interval + backoff
	startedAt := s.deps.Now().UTC()
	heartbeat := store.SupervisorHeartbeat{
		Coordinator: s.deps.Coordinator,
		PID:         os.Getpid(),
		StartedAt:   startedAt,
		RenewedAt:   startedAt,
		ExpiresAt:   startedAt.Add(ttl),
	}
	if err := writer.SaveSupervisorHeartbeat(ctx, heartbeat); err != nil {
		_ = operational.Close()
		return nil, fmt.Errorf("write initial supervisor heartbeat: %w", err)
	}
	heartbeatContext, cancel := context.WithCancel(ctx)
	loop := &supervisorHeartbeatLoop{
		writer:      writer,
		operational: operational,
		coordinator: s.deps.Coordinator,
		startedAt:   startedAt,
		ttl:         ttl,
		cadence:     supervisorHeartbeatCadence(interval, ttl),
		now:         s.deps.Now,
		cancel:      cancel,
		done:        make(chan struct{}),
		errors:      make(chan error, 1),
	}
	go loop.run(heartbeatContext)
	return loop, nil
}

// run renews the heartbeat on a bounded cadence and stops when the polling
// context ends. A write failure is retained for the coordinator loop to report.
func (loop *supervisorHeartbeatLoop) run(ctx context.Context) {
	defer close(loop.done)
	ticker := time.NewTicker(loop.cadence)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			renewedAt := loop.now().UTC()
			err := loop.writer.SaveSupervisorHeartbeat(ctx, store.SupervisorHeartbeat{
				Coordinator: loop.coordinator,
				PID:         os.Getpid(),
				StartedAt:   loop.startedAt,
				RenewedAt:   renewedAt,
				ExpiresAt:   renewedAt.Add(loop.ttl),
			})
			if err == nil {
				continue
			}
			select {
			case loop.errors <- err:
			default:
			}
			return
		}
	}
}

// stop cancels renewal, waits for its final store operation, and closes the
// heartbeat store without erasing the last observable heartbeat.
func (loop *supervisorHeartbeatLoop) stop() {
	if loop == nil {
		return
	}
	loop.once.Do(func() {
		loop.cancel()
		<-loop.done
		_ = loop.operational.Close()
	})
}

// heartbeatError returns a renewal failure without blocking the polling loop.
func (loop *supervisorHeartbeatLoop) heartbeatError() error {
	if loop == nil {
		return nil
	}
	select {
	case err := <-loop.errors:
		return err
	default:
		return nil
	}
}

// supervisorHeartbeatCadence chooses a renewal interval that is no longer
// than half the expiry window while respecting the configured poll interval.
func supervisorHeartbeatCadence(interval, ttl time.Duration) time.Duration {
	cadence := interval
	if half := ttl / 2; half > 0 && cadence > half {
		cadence = half
	}
	if cadence <= 0 {
		return time.Millisecond
	}
	return cadence
}
