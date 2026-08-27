package harness_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestNativeSessionLivenessIsPartOfTheHarnessContract verifies both adapters
// use a worker-side process observation and map the command's presence result
// to the coordinator-neutral liveness capability.
func TestNativeSessionLivenessIsPartOfTheHarnessContract(t *testing.T) {
	for _, test := range []struct {
		name    string
		harness string
		marker  string
	}{
		{name: "codex", harness: harness.NameCodex, marker: "[c]odex"},
		{name: "claude", harness: harness.NameClaude, marker: "[c]laude"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, err := harness.New(test.harness, &livenessWorker{result: worker.CommandResult{ExitCode: 0}}, nil)
			if err != nil {
				t.Fatalf("harness.New() error = %v", err)
			}
			inspector, ok := runtime.(harness.NativeSessionLivenessInspector)
			if !ok {
				t.Fatalf("runtime %T does not implement NativeSessionLivenessInspector", runtime)
			}
			running, err := inspector.NativeSessionRunning(context.Background(), harness.NativeSessionRequest{RunID: "run-liveness", Harness: test.harness})
			if err != nil {
				t.Fatalf("NativeSessionRunning() error = %v", err)
			}
			if !running {
				t.Fatal("NativeSessionRunning() = false, want a running process")
			}

			workerRuntime := &livenessWorker{result: worker.CommandResult{ExitCode: 1}}
			deadRuntime, err := harness.New(test.harness, workerRuntime, nil)
			if err != nil {
				t.Fatalf("harness.New() for stopped process error = %v", err)
			}
			deadInspector := deadRuntime.(harness.NativeSessionLivenessInspector)
			dead, err := deadInspector.NativeSessionRunning(context.Background(), harness.NativeSessionRequest{RunID: "run-liveness", Harness: test.harness})
			if err != nil {
				t.Fatalf("NativeSessionRunning() for stopped process error = %v", err)
			}
			if dead {
				t.Fatal("NativeSessionRunning() = true, want an exited process")
			}
			if !strings.Contains(workerRuntime.lastRequest.Command, test.marker) || workerRuntime.lastRequest.EnvironmentPolicy != worker.EnvironmentPolicyClean || workerRuntime.lastRequest.Role != "coordinator" {
				t.Fatalf("liveness command request = %#v, want clean coordinator command for %s", workerRuntime.lastRequest, test.harness)
			}
		})
	}
}

// livenessWorker records the command used by a harness liveness inspection.
type livenessWorker struct {
	result      worker.CommandResult
	lastRequest worker.CommandRequest
}

// Start satisfies the worker runtime for harness liveness tests.
func (*livenessWorker) Start(context.Context, worker.StartRequest) error { return nil }

// Resume satisfies the worker runtime for harness liveness tests.
func (*livenessWorker) Resume(context.Context, worker.ResumeRequest) error { return nil }

// RunCommand returns the configured process-table result.
func (w *livenessWorker) RunCommand(_ context.Context, request worker.CommandRequest) (worker.CommandResult, error) {
	w.lastRequest = request
	return w.result, nil
}

// Stop satisfies the worker runtime for harness liveness tests.
func (*livenessWorker) Stop(context.Context, string) error { return nil }

// Inspect satisfies the worker runtime for harness liveness tests.
func (*livenessWorker) Inspect(context.Context, string) (worker.Inspection, error) {
	return worker.Inspection{Exists: true, Running: true}, nil
}

var _ worker.WorkerRuntime = (*livenessWorker)(nil)
