package factory

import (
	"context"
	"errors"

	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
)

// DriveRunForTest performs one unattended progression pass for the
// registered repository and returns its outcome. It lets external tests drive
// the same seam the persistent coordinator uses.
func (s *Service) DriveRunForTest(ctx context.Context, sinks ...EventSink) (string, error) {
	registration, err := s.registration()
	if err != nil {
		return "", err
	}
	result, err := s.driveRun(ctx, registration, sinks...)
	return string(result.Outcome), err
}

// DrainSessionCommandsForTest applies every pending session command once,
// waits for the tracker commands it started, and closes its store. It is one
// pass of the coordinator's inbox loop, so external tests drive the same
// seam that factory start runs every second.
func (s *Service) DrainSessionCommandsForTest(ctx context.Context) error {
	registration, err := s.registration()
	if err != nil {
		return err
	}
	opened, err := s.deps.OpenStore(ctx, registration.OperationalDataPath)
	if err != nil {
		return err
	}
	defer func() { _ = opened.Close() }()
	commandStore, ok := opened.(sessionCommandStore)
	if !ok {
		return errors.New("operational store does not support session commands")
	}
	dispatcher := sessioncommand.NewDispatcher(s.sessionCommandEnv(commandStore, registration))
	defer dispatcher.Wait()
	return dispatcher.Drain(ctx)
}
