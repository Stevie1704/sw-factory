package factory

import "context"

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
