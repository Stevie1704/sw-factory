package factory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// supervisorHeartbeatLoop renews one local coordinator heartbeat until the
// polling supervisor stops. Each write opens the store only for the duration
// of that write so a heartbeat connection does not remain in contention with
// the coordinator's operational-store connection.
type supervisorHeartbeatLoop struct {
	write       func(context.Context, store.SupervisorHeartbeat) error
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

// startSupervisorHeartbeat records an initial heartbeat and starts renewal
// when the store supports the heartbeat projection. Older test-only store
// adapters remain usable without the optional projection seam. Renewal
// failures are retained for the polling loop to report, but never prevent the
// coordinator from continuing its work.
func (s *Service) startSupervisorHeartbeat(ctx context.Context, registration config.RepositoryRegistration, interval, backoff time.Duration) (*supervisorHeartbeatLoop, error) {
	operational, err := s.deps.OpenStore(ctx, registration.OperationalDataPath)
	if err != nil {
		return nil, fmt.Errorf("open operational store for supervisor heartbeat: %w", err)
	}
	if operational == nil {
		return nil, errors.New("open operational store for supervisor heartbeat returned nil")
	}
	_, ok := operational.(SupervisorHeartbeatWriter)
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
	heartbeatContext, cancel := context.WithCancel(ctx)
	loop := &supervisorHeartbeatLoop{
		coordinator: s.deps.Coordinator,
		startedAt:   startedAt,
		ttl:         ttl,
		cadence:     supervisorHeartbeatCadence(interval, ttl),
		now:         s.deps.Now,
		cancel:      cancel,
		done:        make(chan struct{}),
		errors:      make(chan error, 1),
	}
	loop.write = func(writeContext context.Context, value store.SupervisorHeartbeat) error {
		opened, openErr := s.deps.OpenStore(writeContext, registration.OperationalDataPath)
		if openErr != nil {
			return fmt.Errorf("open operational store for supervisor heartbeat renewal: %w", openErr)
		}
		if opened == nil {
			return errors.New("open operational store for supervisor heartbeat renewal returned nil")
		}
		saveErr := saveSupervisorHeartbeat(writeContext, opened, value)
		closeErr := opened.Close()
		if saveErr != nil {
			return saveErr
		}
		if closeErr != nil {
			return fmt.Errorf("close operational store after supervisor heartbeat renewal: %w", closeErr)
		}
		return nil
	}
	if err := saveSupervisorHeartbeat(ctx, operational, heartbeat); err != nil {
		loop.reportError(fmt.Errorf("write initial supervisor heartbeat: %w", err))
	}
	if closeErr := operational.Close(); closeErr != nil {
		loop.reportError(fmt.Errorf("close operational store after initial supervisor heartbeat: %w", closeErr))
	}
	go loop.run(heartbeatContext)
	return loop, nil
}

// run renews the heartbeat on a bounded cadence and stops when the polling
// context ends. A write failure is retained for the coordinator loop to report
// while the next cadence retries the write.
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
			err := loop.write(ctx, store.SupervisorHeartbeat{
				Coordinator: loop.coordinator,
				PID:         os.Getpid(),
				StartedAt:   loop.startedAt,
				RenewedAt:   renewedAt,
				ExpiresAt:   renewedAt.Add(loop.ttl),
			})
			if err == nil {
				continue
			}
			loop.reportError(err)
		}
	}
}

// stop cancels renewal and waits for its final store operation without erasing
// the last observable heartbeat.
func (loop *supervisorHeartbeatLoop) stop() {
	if loop == nil {
		return
	}
	loop.once.Do(func() {
		loop.cancel()
		<-loop.done
	})
}

// reportError makes one heartbeat failure available to the polling loop
// without allowing a slow or absent consumer to block renewal retries.
func (loop *supervisorHeartbeatLoop) reportError(err error) {
	if loop == nil || err == nil || loop.errors == nil {
		return
	}
	select {
	case loop.errors <- err:
	default:
	}
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

// saveSupervisorHeartbeat writes one heartbeat through the optional store
// projection, keeping the type assertion in one place for initial and renewal
// writes.
func saveSupervisorHeartbeat(ctx context.Context, operational OperationalStore, heartbeat store.SupervisorHeartbeat) error {
	writer, ok := operational.(SupervisorHeartbeatWriter)
	if !ok {
		return errors.New("operational store does not support supervisor heartbeats")
	}
	if err := writer.SaveSupervisorHeartbeat(ctx, heartbeat); err != nil {
		return fmt.Errorf("save supervisor heartbeat: %w", err)
	}
	return nil
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
