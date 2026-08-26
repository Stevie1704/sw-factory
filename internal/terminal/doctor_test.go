package terminal_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/terminal"
)

// TestStartupChecksVerifyCmuxAvailabilityAndReachability verifies diagnosis
// reports both terminal prerequisites independently.
func TestStartupChecksVerifyCmuxAvailabilityAndReachability(t *testing.T) {
	runner := &cmuxDoctorRunner{tree: `{"windows":[]}`}
	runtime := terminal.NewCmuxRuntime(runner)
	report := doctor.Run(context.Background(), terminal.StartupChecks(runtime)...)
	if !report.Ready() || len(report.Results) != 2 {
		t.Fatalf("cmux report = %#v, want two passing checks", report)
	}
	if runner.calls != 1 || !strings.Contains(runner.lastArgs, "tree --json --id-format uuids") {
		t.Fatalf("cmux calls = %d/%q, want one topology probe", runner.calls, runner.lastArgs)
	}
}

// TestStartupChecksReportAnUnavailableCmuxSocket verifies a socket failure is
// actionable and does not expose the command runner's error text.
func TestStartupChecksReportAnUnavailableCmuxSocket(t *testing.T) {
	secret := "socket-secret"
	runner := &cmuxDoctorRunner{err: errors.New(secret)}
	report := doctor.Run(context.Background(), terminal.StartupChecks(terminal.NewCmuxRuntime(runner))...)
	if report.Ready() {
		t.Fatal("cmux report is ready despite an unavailable socket")
	}
	for _, result := range report.Results {
		if strings.Contains(result.Problem+result.Action, secret) {
			t.Fatalf("cmux result exposed command error: %#v", result)
		}
	}
}

// cmuxDoctorRunner records one topology probe.
type cmuxDoctorRunner struct {
	tree     string
	err      error
	calls    int
	lastArgs string
}

// Run implements the terminal command runner for diagnosis tests.
func (r *cmuxDoctorRunner) Run(_ context.Context, args []string, _ []byte) ([]byte, error) {
	r.calls++
	r.lastArgs = strings.Join(args, " ")
	return []byte(r.tree), r.err
}

var _ terminal.CommandRunner = (*cmuxDoctorRunner)(nil)
