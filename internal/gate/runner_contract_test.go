package gate_test

import (
	"context"
	"errors"
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
		RunID:         "run-gate-1",
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
	if runtime.commands[0].EnvironmentPolicy != worker.EnvironmentPolicyClean || runtime.commands[1].EnvironmentPolicy != worker.EnvironmentPolicyClean {
		t.Fatalf("command policies = %#v, want clean coordinator environments", runtime.commands)
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

// fakeRuntime is a WorkerRuntime adapter used at the gate module's public seam.
type fakeRuntime struct {
	commands []worker.CommandRequest
	results  []worker.CommandResult
}

// Start implements WorkerRuntime for the gate contract tests.
func (f *fakeRuntime) Start(context.Context, worker.StartRequest) error { return nil }

// Resume implements WorkerRuntime for the gate contract tests.
func (f *fakeRuntime) Resume(context.Context, worker.ResumeRequest) error { return nil }

// RunCommand records the coordinator command and returns the next fixture.
func (f *fakeRuntime) RunCommand(_ context.Context, request worker.CommandRequest) (worker.CommandResult, error) {
	f.commands = append(f.commands, request)
	result := f.results[0]
	f.results = f.results[1:]
	return result, nil
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
