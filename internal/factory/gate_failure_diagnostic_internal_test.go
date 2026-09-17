package factory

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/gate"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// diagnosticCheckpointSHA is the exact checkpoint the fixture suites evaluate.
const diagnosticCheckpointSHA = "abcdefabcdefabcdefabcdefabcdefabcdefabcd"

// diagnosticObservedAt is the fixed coordinator observation time.
var diagnosticObservedAt = time.Date(2026, 9, 17, 9, 30, 0, 0, time.UTC)

// TestGateFailureCauseNamesSetupCommandOutput verifies the cause reports what
// the setup command printed instead of the category of the failure. The
// reported run parked on "check repair waiting for infrastructure" while the
// actual blocker was a repository-owned virtual environment defect.
func TestGateFailureCauseNamesSetupCommandOutput(t *testing.T) {
	t.Parallel()

	suiteErr := &gate.SuiteFailure{Failures: []error{&gate.SetupFailure{Result: worker.CommandResult{
		ExitCode: 1,
		Stderr:   "error: Failed to create virtual environment\n  Caused by: A virtual environment already exists at '.venv'. Use '--clear' to replace it\n",
	}}}}

	cause := gateFailureCause(suiteErr)

	if !strings.HasPrefix(cause, "setup command failed with exit code 1: ") {
		t.Fatalf("cause = %q, want the typed setup failure first", cause)
	}
	if !strings.Contains(cause, "Failed to create virtual environment") || !strings.Contains(cause, "--clear") {
		t.Fatalf("cause = %q, want the leading setup output lines", cause)
	}
}

// TestGateFailureCauseNamesFailedGate verifies a declared gate failure reports
// the gate and its output rather than only an exit code.
func TestGateFailureCauseNamesFailedGate(t *testing.T) {
	t.Parallel()

	suiteErr := &gate.SuiteFailure{Failures: []error{&gate.GateFailure{
		Name:   "build",
		Result: worker.CommandResult{ExitCode: 2, Stderr: "undefined: Publish"},
	}}}

	if got, want := gateFailureCause(suiteErr), `gate "build" failed with exit code 2: undefined: Publish`; got != want {
		t.Fatalf("cause = %q, want %q", got, want)
	}
}

// TestGateFailureCauseFallsBackToStdoutThenTypedError verifies a command that
// printed nothing on standard error still names a blocker.
func TestGateFailureCauseFallsBackToStdoutThenTypedError(t *testing.T) {
	t.Parallel()

	stdoutOnly := &gate.SuiteFailure{Failures: []error{&gate.SetupFailure{Result: worker.CommandResult{ExitCode: 1, Stdout: "lockfile is out of date"}}}}
	if got, want := gateFailureCause(stdoutOnly), "setup command failed with exit code 1: lockfile is out of date"; got != want {
		t.Fatalf("stdout cause = %q, want %q", got, want)
	}

	silent := &gate.SuiteFailure{Failures: []error{&gate.SetupFailure{Cause: errors.New("worker unavailable")}}}
	if got, want := gateFailureCause(silent), "setup execution failed: worker unavailable"; got != want {
		t.Fatalf("silent cause = %q, want %q", got, want)
	}
}

// TestGateFailureCauseNamesATimeoutThatPrintedOutput verifies the failure mode
// survives the command output. A timed-out gate prints an ordinary partial
// transcript, so output alone cannot distinguish it from a non-zero exit and
// the operator would read a truncated test log as the blocker.
func TestGateFailureCauseNamesATimeoutThatPrintedOutput(t *testing.T) {
	t.Parallel()

	suiteErr := &gate.SuiteFailure{Failures: []error{&gate.GateFailure{
		Name:     "test",
		TimedOut: true,
		Result:   worker.CommandResult{Stdout: "ok internal/gate 0.4s"},
	}}}

	if got, want := gateFailureCause(suiteErr), `gate "test" timed out: ok internal/gate 0.4s`; got != want {
		t.Fatalf("cause = %q, want %q", got, want)
	}
}

// TestGateFailureCauseIsSingleLineSanitizedAndBounded verifies the cause stays
// safe to embed in the one-line status comment field that carries it.
func TestGateFailureCauseIsSingleLineSanitizedAndBounded(t *testing.T) {
	t.Parallel()

	suiteErr := &gate.SuiteFailure{Failures: []error{&gate.GateFailure{
		Name:   "test",
		Result: worker.CommandResult{ExitCode: 1, Stderr: "panic: `boom`\x07\n" + strings.Repeat("y", 4*maxGateFailureCauseBytes)},
	}}}

	cause := gateFailureCause(suiteErr)

	if strings.ContainsAny(cause, "\n\r`\x07") {
		t.Fatalf("cause = %q, want one sanitized line", cause)
	}
	if len(cause) > maxGateFailureCauseBytes {
		t.Fatalf("cause = %d bytes, want at most %d", len(cause), maxGateFailureCauseBytes)
	}
}

// TestGateFailureCauseIgnoresNonDeterministicFailures verifies an error that
// carries no command observation adds nothing to a lifecycle reason.
func TestGateFailureCauseIgnoresNonDeterministicFailures(t *testing.T) {
	t.Parallel()

	if cause := gateFailureCause(errors.New("harness rate limit")); cause != "" {
		t.Fatalf("cause = %q, want no cause without a typed command failure", cause)
	}
	if cause := gateFailureCause(nil); cause != "" {
		t.Fatalf("nil cause = %q, want an empty cause", cause)
	}
}

// TestWriteGateFailureDiagnosticRecordsSetupAndGateOutput verifies the parked
// run leaves the command output on disk beside its other run artifacts.
func TestWriteGateFailureDiagnosticRecordsSetupAndGateOutput(t *testing.T) {
	t.Parallel()

	run := diagnosticRun(t)
	results := []gate.Result{
		{CheckpointSHA: diagnosticCheckpointSHA, GateName: "build", Phase: gate.PhaseCheckpoint, Blocking: true, Skipped: true, SkipReason: "setup failed", Outcome: gate.OutcomeSetupFailed, SetupRan: true, Setup: worker.CommandResult{ExitCode: 1, Stderr: "error: Failed to create virtual environment"}, Status: github.CommitStatus{State: github.CommitStatusError}},
		{CheckpointSHA: diagnosticCheckpointSHA, GateName: "lint", Phase: gate.PhaseCheckpoint, Blocking: true, Skipped: true, SkipReason: "setup failed", Outcome: gate.OutcomeSetupFailed, SetupRan: true, Setup: worker.CommandResult{ExitCode: 1, Stderr: "error: Failed to create virtual environment"}, Status: github.CommitStatus{State: github.CommitStatusError}},
	}
	suiteErr := &gate.SuiteFailure{Failures: []error{&gate.SetupFailure{Result: results[0].Setup}}}

	writeGateFailureDiagnostic(run, gate.PhaseCheckpoint, results, suiteErr, diagnosticObservedAt)

	body := readDiagnostic(t, diagnosticPath(run, "checkpoint.log"))
	for _, fragment := range []string{
		diagnosticObservedAt.Format(time.RFC3339),
		diagnosticCheckpointSHA,
		string(gate.PhaseCheckpoint),
		"error: Failed to create virtual environment",
		`gate "build"`,
		`gate "lint"`,
		"setup failed",
	} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("diagnostic body = %q, want it to contain %q", body, fragment)
		}
	}
}

// TestWriteGateFailureDiagnosticBoundsCommandOutput verifies one policy bounds
// the retained output, so an unbounded gate cannot fill the run directory.
func TestWriteGateFailureDiagnosticBoundsCommandOutput(t *testing.T) {
	t.Parallel()

	run := diagnosticRun(t)
	results := []gate.Result{{
		CheckpointSHA: diagnosticCheckpointSHA,
		GateName:      "test",
		Phase:         gate.PhaseCheckpoint,
		Blocking:      true,
		Outcome:       gate.OutcomeFailed,
		Gate:          worker.CommandResult{ExitCode: 1, Stdout: strings.Repeat("x", maxRepairDiagnosticRunes+500)},
	}}
	suiteErr := &gate.SuiteFailure{Failures: []error{&gate.GateFailure{Name: "test", Result: results[0].Gate}}}

	writeGateFailureDiagnostic(run, gate.PhaseCheckpoint, results, suiteErr, diagnosticObservedAt)

	body := readDiagnostic(t, diagnosticPath(run, "checkpoint.log"))

	if !strings.Contains(body, "[truncated]") {
		t.Fatalf("diagnostic body = %q, want the bounded-output marker", body)
	}
	if len(body) > maxRepairDiagnosticRunes+1024 {
		t.Fatalf("diagnostic body = %d bytes, want the shared bounding policy to apply", len(body))
	}
}

// TestWriteGateFailureDiagnosticRecordsOnlyDeterministicFailures verifies a
// diagnostic that cannot or must not be written never masks the failure the
// caller already reports. A transport or persistence failure carries no
// command observation, and filing the passing suite that preceded it would
// name a healthy checkpoint as the cause of the pause.
func TestWriteGateFailureDiagnosticRecordsOnlyDeterministicFailures(t *testing.T) {
	t.Parallel()

	suiteErr := &gate.SuiteFailure{Failures: []error{&gate.SetupFailure{Result: worker.CommandResult{ExitCode: 1, Stderr: "boom"}}}}
	results := []gate.Result{{CheckpointSHA: diagnosticCheckpointSHA, GateName: "build", Phase: gate.PhaseCheckpoint}}
	tests := []struct {
		name     string
		run      store.Run
		phase    gate.Phase
		suiteErr error
	}{
		{name: "no worktree", run: store.Run{ID: "run-1"}, phase: gate.PhaseCheckpoint, suiteErr: suiteErr},
		{name: "unsupported phase", run: diagnosticRun(t), phase: gate.Phase("unsupported"), suiteErr: suiteErr},
		{name: "no failure", run: diagnosticRun(t), phase: gate.PhaseCheckpoint},
		{name: "persistence failure", run: diagnosticRun(t), phase: gate.PhaseCheckpoint, suiteErr: errors.New("SQLite busy")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writeGateFailureDiagnostic(test.run, test.phase, results, test.suiteErr, diagnosticObservedAt)

			if _, err := os.Stat(diagnosticPath(test.run, "checkpoint.log")); !os.IsNotExist(err) {
				t.Fatalf("stat diagnostic error = %v, want no written diagnostic", err)
			}
		})
	}
}

// TestWithGateFailureCauseKeepsTheCategoryPrefix verifies the enriched reason
// still starts with the category an operator and the tests match on.
func TestWithGateFailureCauseKeepsTheCategoryPrefix(t *testing.T) {
	t.Parallel()

	if got, want := withGateFailureCause("check repair waiting for infrastructure", "setup: boom"), "check repair waiting for infrastructure: setup: boom"; got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}
	if got, want := withGateFailureCause("check repair waiting for infrastructure", ""), "check repair waiting for infrastructure"; got != want {
		t.Fatalf("reason without a cause = %q, want %q", got, want)
	}
}

// diagnosticRun returns a run whose worktree sits in an isolated directory.
func diagnosticRun(t *testing.T) store.Run {
	t.Helper()
	return store.Run{ID: "run-diagnostic", CheckpointSHA: diagnosticCheckpointSHA, Worktree: filepath.Join(t.TempDir(), "worktrees", "run-diagnostic")}
}

// diagnosticPath returns the fixed location of one run's phase diagnostic.
func diagnosticPath(run store.Run, name string) string {
	return filepath.Join(filepath.Dir(run.Worktree), ".factory-agents", run.ID, gateFailureDiagnosticDirectoryName, name)
}

// readDiagnostic reads a written diagnostic and fails when it is absent.
func readDiagnostic(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read diagnostic: %v", err)
	}
	return string(body)
}
