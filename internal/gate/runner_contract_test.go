package gate_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/gate"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

const gateCheckpoint = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestRunnerExecutesSetupAndOneDeclaredGatePublishesTheExactCheckpoint
// verifies the deterministic coordinator path through the WorkerRuntime seam.
func TestRunnerExecutesSetupAndOneDeclaredGatePublishesTheExactCheckpoint(t *testing.T) {
	t.Parallel()

	runtime := &fakeRuntime{results: []worker.CommandResult{
		{ExitCode: 0, Stdout: "setup-ok"},
		{ExitCode: 0, Stdout: "gate-ok"},
	}}
	statuses := &fakeStatusPublisher{}
	runner := gate.Runner{Runtime: runtime, Statuses: statuses}

	result, err := runner.Run(context.Background(), gate.Request{
		RunID:                  "run-gate-1",
		CheckpointSHA:          gateCheckpoint,
		Setup:                  "go mod download",
		SetupEnvironmentPolicy: config.EnvironmentPolicyRole,
		SetupTimeout:           "1m",
		Gate: config.GateConfig{
			Name:              "test",
			Command:           "go test ./...",
			Timeout:           "2m",
			EnvironmentPolicy: config.EnvironmentPolicyClean,
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status.State != github.CommitStatusSuccess || result.Status.SHA != gateCheckpoint {
		t.Fatalf("Run() status = %#v, want success for exact checkpoint", result.Status)
	}
	if result.Status.Context != "factory/gate/test" {
		t.Fatalf("status context = %q, want stable gate context", result.Status.Context)
	}
	if len(runtime.commands) != 2 {
		t.Fatalf("commands = %#v, want setup followed by one gate", runtime.commands)
	}
	if runtime.commands[0].Command != "go mod download" || runtime.commands[1].Command != "go test ./..." {
		t.Fatalf("commands = %#v, want declared setup and gate", runtime.commands)
	}
	if runtime.commands[0].EnvironmentPolicy != worker.EnvironmentPolicyRole || runtime.commands[1].EnvironmentPolicy != worker.EnvironmentPolicyClean {
		t.Fatalf("command policies = %#v, want configured setup role and clean gate environments", runtime.commands)
	}
	if len(statuses.statuses) != 1 || statuses.statuses[0] != result.Status {
		t.Fatalf("published statuses = %#v, want one final status", statuses.statuses)
	}
}

// TestRunnerClassifiesSetupFailureAndDoesNotRunTheGate verifies setup is a
// deterministic prerequisite with a distinct failure category.
func TestRunnerClassifiesSetupFailureAndDoesNotRunTheGate(t *testing.T) {
	t.Parallel()

	runtime := &fakeRuntime{results: []worker.CommandResult{{ExitCode: 4, Stderr: "setup failed"}}}
	statuses := &fakeStatusPublisher{}
	runner := gate.Runner{Runtime: runtime, Statuses: statuses}

	result, err := runner.Run(context.Background(), gate.Request{
		RunID:         "run-gate-setup-failure",
		CheckpointSHA: gateCheckpoint,
		Setup:         "go mod download",
		SetupTimeout:  "1m",
		Gate: config.GateConfig{
			Name:              "test",
			Command:           "go test ./...",
			Timeout:           "2m",
			EnvironmentPolicy: config.EnvironmentPolicyClean,
		},
	})
	var setupFailure *gate.SetupFailure
	if !errors.As(err, &setupFailure) {
		t.Fatalf("Run() error = %v, want SetupFailure", err)
	}
	if result.Status.State != github.CommitStatusError || len(runtime.commands) != 1 {
		t.Fatalf("result = %#v commands = %#v, want setup error and no gate", result, runtime.commands)
	}
	if len(statuses.statuses) != 1 || statuses.statuses[0].State != github.CommitStatusError {
		t.Fatalf("published statuses = %#v, want setup error status", statuses.statuses)
	}
}

// TestRunnerClassifiesGateFailure verifies a non-zero declared gate remains a
// deterministic gate failure and is published against the same checkpoint.
func TestRunnerClassifiesGateFailure(t *testing.T) {
	t.Parallel()

	runtime := &fakeRuntime{results: []worker.CommandResult{
		{ExitCode: 0},
		{ExitCode: 9, Stderr: "test failed"},
	}}
	statuses := &fakeStatusPublisher{}
	runner := gate.Runner{Runtime: runtime, Statuses: statuses}

	result, err := runner.Run(context.Background(), gate.Request{
		RunID:         "run-gate-failure",
		CheckpointSHA: gateCheckpoint,
		Setup:         "setup",
		SetupTimeout:  "1m",
		Gate: config.GateConfig{
			Name:              "test",
			Command:           "test",
			Timeout:           "2m",
			Blocking:          true,
			EnvironmentPolicy: config.EnvironmentPolicyRole,
		},
	})
	var gateFailure *gate.GateFailure
	if !errors.As(err, &gateFailure) {
		t.Fatalf("Run() error = %v, want GateFailure", err)
	}
	if result.Status.State != github.CommitStatusFailure || result.Status.SHA != gateCheckpoint {
		t.Fatalf("result status = %#v, want failure at exact checkpoint", result.Status)
	}
	if runtime.commands[1].EnvironmentPolicy != worker.EnvironmentPolicyRole {
		t.Fatalf("gate policy = %q, want role policy", runtime.commands[1].EnvironmentPolicy)
	}
}

// TestRunnerRunsIndependentGatesAndSkipsFailedDependencies verifies one setup
// command feeds the ordered gate graph without allowing one failure to hide
// an independent result.
func TestRunnerRunsIndependentGatesAndSkipsFailedDependencies(t *testing.T) {
	t.Parallel()

	runtime := &fakeRuntime{results: []worker.CommandResult{
		{ExitCode: 0}, // setup
		{ExitCode: 7}, // format
		{ExitCode: 0}, // lint
	}}
	statuses := &fakeStatusPublisher{}
	runner := gate.Runner{Runtime: runtime, Statuses: statuses}

	result, err := runner.RunSuite(context.Background(), gate.SuiteRequest{
		RunID:            "run-suite-1",
		CheckpointSHA:    gateCheckpoint,
		Setup:            "setup",
		SetupTimeout:     "1m",
		SetupFingerprint: "setup-fingerprint-1",
		Phase:            gate.PhaseCheckpoint,
		Gates: []config.GateConfig{
			{Name: "format", Command: "format", Timeout: "1m", Blocking: true, EnvironmentPolicy: config.EnvironmentPolicyClean},
			{Name: "lint", Command: "lint", Timeout: "1m", Blocking: false, EnvironmentPolicy: config.EnvironmentPolicyRole},
			{Name: "test", Command: "test", Timeout: "1m", Blocking: true, DependsOn: []string{"format"}, EnvironmentPolicy: config.EnvironmentPolicyClean},
		},
	})
	if err == nil {
		t.Fatal("RunSuite() succeeded, want the blocking format failure")
	}
	var formatFailure *gate.GateFailure
	if !errors.As(err, &formatFailure) {
		t.Fatalf("RunSuite() error = %v, want GateFailure", err)
	}
	if len(result.Gates) != 3 || result.Gates[0].Status.State != github.CommitStatusFailure {
		t.Fatalf("suite gates = %#v, want all declared gate results", result.Gates)
	}
	if result.Gates[1].Status.State != github.CommitStatusSuccess || result.Gates[1].SetupFingerprint != "setup-fingerprint-1" {
		t.Fatalf("independent lint result = %#v, want success with setup fingerprint", result.Gates[1])
	}
	if !result.Gates[2].Skipped || !strings.Contains(result.Gates[2].SkipReason, "format") {
		t.Fatalf("dependent test result = %#v, want dependency skip reason", result.Gates[2])
	}
	if len(runtime.commands) != 3 || runtime.commands[0].Command != "setup" || runtime.commands[1].Command != "format" || runtime.commands[2].Command != "lint" {
		t.Fatalf("commands = %#v, want one setup plus independent gates", runtime.commands)
	}
	if len(statuses.statuses) != 3 {
		t.Fatalf("published statuses = %#v, want one result per declared gate", statuses.statuses)
	}
}

// TestRunnerDoesNotBlockOnANonBlockingIndependentFailure verifies the declared
// blocking policy controls suite disposition without hiding later results.
func TestRunnerDoesNotBlockOnANonBlockingIndependentFailure(t *testing.T) {
	t.Parallel()

	runtime := &fakeRuntime{results: []worker.CommandResult{
		{ExitCode: 0}, // setup
		{ExitCode: 3}, // advisory
		{ExitCode: 0}, // independent required gate
	}}
	statuses := &fakeStatusPublisher{}
	runner := gate.Runner{Runtime: runtime, Statuses: statuses}

	result, err := runner.RunSuite(context.Background(), gate.SuiteRequest{
		RunID:         "run-suite-advisory",
		CheckpointSHA: gateCheckpoint,
		Setup:         "setup",
		SetupTimeout:  "1m",
		Gates: []config.GateConfig{
			{Name: "advisory", Command: "advisory", Timeout: "1m", Blocking: false, EnvironmentPolicy: config.EnvironmentPolicyClean},
			{Name: "required", Command: "required", Timeout: "1m", Blocking: true, EnvironmentPolicy: config.EnvironmentPolicyClean},
		},
	})
	if err != nil {
		t.Fatalf("RunSuite() error = %v, want nonblocking failure to remain advisory", err)
	}
	if len(result.Gates) != 2 || result.Gates[0].Outcome != gate.OutcomeFailed || result.Gates[1].Outcome != gate.OutcomePassed {
		t.Fatalf("suite gates = %#v, want advisory failure and required success", result.Gates)
	}
	if len(runtime.commands) != 3 || len(statuses.statuses) != 2 {
		t.Fatalf("commands = %#v statuses = %#v, want setup plus both gates", runtime.commands, statuses.statuses)
	}
}

// TestRunnerFailsWhenABlockingGateDependsOnANonBlockingFailure verifies an
// advisory failure still blocks a required dependent gate from continuation.
func TestRunnerFailsWhenABlockingGateDependsOnANonBlockingFailure(t *testing.T) {
	t.Parallel()

	runtime := &fakeRuntime{results: []worker.CommandResult{
		{ExitCode: 0}, // setup
		{ExitCode: 3}, // advisory prerequisite
	}}
	statuses := &fakeStatusPublisher{}
	runner := gate.Runner{Runtime: runtime, Statuses: statuses}

	result, err := runner.RunSuite(context.Background(), gate.SuiteRequest{
		RunID:         "run-suite-dependent-advisory",
		CheckpointSHA: gateCheckpoint,
		Setup:         "setup",
		SetupTimeout:  "1m",
		Gates: []config.GateConfig{
			{Name: "advisory", Command: "advisory", Timeout: "1m", Blocking: false, EnvironmentPolicy: config.EnvironmentPolicyClean},
			{Name: "required", Command: "required", Timeout: "1m", Blocking: true, DependsOn: []string{"advisory"}, EnvironmentPolicy: config.EnvironmentPolicyClean},
		},
	})
	var suiteFailure *gate.SuiteFailure
	if !errors.As(err, &suiteFailure) {
		t.Fatalf("RunSuite() error = %v, want blocking dependency failure", err)
	}
	var dependencyFailure *gate.DependencyFailure
	if !errors.As(err, &dependencyFailure) {
		t.Fatalf("RunSuite() error = %v, want DependencyFailure", err)
	}
	if len(result.Gates) != 2 || !result.Gates[1].Skipped || result.Gates[1].Outcome != gate.OutcomeSkipped {
		t.Fatalf("suite gates = %#v, want required gate skipped", result.Gates)
	}
	if len(runtime.commands) != 2 {
		t.Fatalf("commands = %#v, want setup and advisory only", runtime.commands)
	}
}

// TestRunnerAggregatesIndependentBlockingFailures verifies one suite error
// carries every independent blocking failure for one repair decision.
func TestRunnerAggregatesIndependentBlockingFailures(t *testing.T) {
	t.Parallel()

	runtime := &fakeRuntime{results: []worker.CommandResult{{ExitCode: 0}, {ExitCode: 4}, {ExitCode: 5}}}
	statuses := &fakeStatusPublisher{}
	runner := gate.Runner{Runtime: runtime, Statuses: statuses}
	result, err := runner.RunSuite(context.Background(), gate.SuiteRequest{
		RunID:         "run-suite-failures",
		CheckpointSHA: gateCheckpoint,
		Setup:         "setup",
		SetupTimeout:  "1m",
		Gates: []config.GateConfig{
			{Name: "format", Command: "format", Timeout: "1m", Blocking: true, EnvironmentPolicy: config.EnvironmentPolicyClean},
			{Name: "lint", Command: "lint", Timeout: "1m", Blocking: true, EnvironmentPolicy: config.EnvironmentPolicyClean},
		},
	})
	var suiteFailure *gate.SuiteFailure
	if !errors.As(err, &suiteFailure) || len(suiteFailure.Failures) != 2 {
		t.Fatalf("RunSuite() error = %v, want two independent failures", err)
	}
	if len(result.Gates) != 2 || result.Gates[0].Outcome != gate.OutcomeFailed || result.Gates[1].Outcome != gate.OutcomeFailed {
		t.Fatalf("suite gates = %#v, want both independent failures retained", result.Gates)
	}
}

// TestRunnerClassifiesACommandTimeoutAsATypedGateFailure verifies a timeout is
// a deterministic gate result rather than worker infrastructure failure.
func TestRunnerClassifiesACommandTimeoutAsATypedGateFailure(t *testing.T) {
	t.Parallel()

	runtime := &fakeRuntime{
		results: []worker.CommandResult{{ExitCode: 0}},
		errors:  []error{nil, context.DeadlineExceeded},
	}
	statuses := &fakeStatusPublisher{}
	runner := gate.Runner{Runtime: runtime, Statuses: statuses}

	result, err := runner.RunSuite(context.Background(), gate.SuiteRequest{
		RunID:         "run-timeout",
		CheckpointSHA: gateCheckpoint,
		Setup:         "setup",
		SetupTimeout:  "1m",
		Phase:         gate.PhaseCheckpoint,
		Gates: []config.GateConfig{{
			Name: "test", Command: "test", Timeout: "1ms", Blocking: true,
			EnvironmentPolicy: config.EnvironmentPolicyClean,
		}},
	})
	var gateFailure *gate.GateFailure
	if !errors.As(err, &gateFailure) {
		t.Fatalf("RunSuite() error = %v, want GateFailure", err)
	}
	if !gateFailure.TimedOut || result.Gates[0].Status.State != github.CommitStatusFailure {
		t.Fatalf("timeout failure = %#v, result = %#v, want typed failure status", gateFailure, result.Gates[0])
	}
	if result.Gates[0].Status.Description != "factory gate timed out" {
		t.Fatalf("timeout description = %q, want typed gate timeout", result.Gates[0].Status.Description)
	}
}

// fakeRuntime is a WorkerRuntime adapter used at the gate module's public seam.
type fakeRuntime struct {
	commands []worker.CommandRequest
	results  []worker.CommandResult
	errors   []error
}

// Start implements WorkerRuntime for the gate contract tests.
func (f *fakeRuntime) Start(context.Context, worker.StartRequest) error { return nil }

// Resume implements WorkerRuntime for the gate contract tests.
func (f *fakeRuntime) Resume(context.Context, worker.ResumeRequest) error { return nil }

// RunCommand records the coordinator command and returns the next fixture.
func (f *fakeRuntime) RunCommand(_ context.Context, request worker.CommandRequest) (worker.CommandResult, error) {
	f.commands = append(f.commands, request)
	var result worker.CommandResult
	if len(f.results) != 0 {
		result = f.results[0]
		f.results = f.results[1:]
	}
	var err error
	if len(f.errors) != 0 {
		err = f.errors[0]
		f.errors = f.errors[1:]
	}
	return result, err
}

// Stop implements WorkerRuntime for the gate contract tests.
func (f *fakeRuntime) Stop(context.Context, string) error { return nil }

// Inspect implements WorkerRuntime for the gate contract tests.
func (f *fakeRuntime) Inspect(context.Context, string) (worker.Inspection, error) {
	return worker.Inspection{Exists: true, Running: true}, nil
}

// fakeStatusPublisher records the one status emitted by a gate run.
type fakeStatusPublisher struct {
	statuses []github.CommitStatus
}

// CreateCommitStatus implements the GitHub status seam for contract tests.
func (f *fakeStatusPublisher) CreateCommitStatus(_ context.Context, _ github.Repository, status github.CommitStatus) error {
	f.statuses = append(f.statuses, status)
	return nil
}

var _ worker.WorkerRuntime = (*fakeRuntime)(nil)
var _ github.CommitStatusPublisher = (*fakeStatusPublisher)(nil)
