package terminal

import (
	"context"
	"errors"
	"os/exec"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/doctor"
)

// DoctorChecker is the cmux-owned startup diagnosis seam.
type DoctorChecker interface {
	CheckExecutable(context.Context) error
	CheckSocket(context.Context) error
}

// StartupChecks returns independent cmux executable and socket-reachability
// checks so a broken terminal setup is fully reported in one run.
func StartupChecks(checker DoctorChecker) []doctor.Check {
	return []doctor.Check{
		func(ctx context.Context) doctor.Result {
			if checker == nil {
				return doctor.Failure("cmux executable", "the cmux diagnosis adapter is unavailable", "configure the macOS cmux terminal adapter")
			}
			if err := checker.CheckExecutable(ctx); err != nil {
				return doctor.Failure("cmux executable", "cmux is not available on the coordinator PATH", "install cmux and make its command available to the factory process")
			}
			return doctor.Success("cmux executable")
		},
		func(ctx context.Context) doctor.Result {
			if checker == nil {
				return doctor.Failure("cmux socket", "the cmux diagnosis adapter is unavailable", "configure the macOS cmux terminal adapter")
			}
			if err := checker.CheckSocket(ctx); err != nil {
				return doctor.Failure("cmux socket", "the coordinator cannot reach the configured cmux control socket", "start cmux from the coordinator's permitted environment and verify the configured socket path")
			}
			return doctor.Success("cmux socket")
		},
	}
}

// CheckExecutable verifies the cmux binary exists when the production command
// runner is in use. Injected runners represent an already controlled adapter.
func (r *CmuxRuntime) CheckExecutable(context.Context) error {
	if r.Runner != nil {
		return nil
	}
	binary := strings.TrimSpace(r.Binary)
	if binary == "" {
		binary = "cmux"
	}
	if _, err := exec.LookPath(binary); err != nil {
		return errors.New("cmux executable is unavailable")
	}
	return nil
}

// CheckSocket performs a read-only cmux topology query through the same
// configured socket and process boundary used by terminal operations.
func (r *CmuxRuntime) CheckSocket(ctx context.Context) error {
	if _, err := r.topology(ctx, []string{"tree", "--json", "--id-format", "uuids"}); err != nil {
		return errors.New("cmux topology query failed")
	}
	return nil
}

var _ DoctorChecker = (*CmuxRuntime)(nil)
