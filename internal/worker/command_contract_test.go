package worker_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestDockerRuntimeTerminatesACancelledCommandInsideTheWorker verifies that a
// deadline does not end at the host Docker CLI: the adapter asks the worker to
// terminate the same command identity before it reports the timeout.
func TestDockerRuntimeTerminatesACancelledCommandInsideTheWorker(t *testing.T) {
	for _, test := range []struct {
		name string
		want error
	}{
		{name: "deadline", want: context.DeadlineExceeded},
		{name: "caller cancellation", want: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, runID, logPath := startCommandWorker(t)
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			if test.want == context.Canceled {
				ctx, cancel = context.WithCancel(context.Background())
				defer cancel()
				time.AfterFunc(300*time.Millisecond, cancel)
			}

			_, err := runtime.RunCommand(ctx, slowCommand(runID))

			if !errors.Is(err, test.want) {
				t.Fatalf("RunCommand() error = %v, want %v after confirmed termination", err, test.want)
			}
			var terminationErr *worker.CommandTerminationError
			if errors.As(err, &terminationErr) {
				t.Fatalf("RunCommand() error = %v, want no termination discrepancy after confirmed termination", err)
			}
			lines := readStubLog(t, logPath)
			commandID := commandIdentity(t, findLogLine(t, lines, "slow-command"), "factory-command ")
			terminateID := commandIdentity(t, findLogLine(t, lines, "factory-command-terminate "), "factory-command-terminate ")
			if commandID != terminateID {
				t.Fatalf("terminated command %q, want the cancelled command %q", terminateID, commandID)
			}
		})
	}
}

// TestDockerRuntimeReportsAnUnconfirmedTermination verifies that a command
// whose termination the worker cannot confirm is a typed infrastructure
// discrepancy, never an ordinary timeout that a repair could consume.
func TestDockerRuntimeReportsAnUnconfirmedTermination(t *testing.T) {
	for _, test := range []struct {
		name   string
		status string
	}{
		{name: "process group survived forced termination", status: "3"},
		{name: "lost cancellation response", status: "255"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, runID, _ := startCommandWorker(t)
			t.Setenv("WORKER_DOCKER_TERMINATE_STATUS", test.status)
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()

			_, err := runtime.RunCommand(ctx, slowCommand(runID))

			var terminationErr *worker.CommandTerminationError
			if !errors.As(err, &terminationErr) {
				t.Fatalf("RunCommand() error = %v, want a typed termination discrepancy", err)
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("RunCommand() error = %v, want no timeout classification while the command may run", err)
			}
			message := err.Error()
			if !strings.Contains(message, "stop or recreate the worker") || strings.Contains(message, "docker") || strings.Contains(message, "factory-worker-") {
				t.Fatalf("RunCommand() error = %q, want actionable seam vocabulary", message)
			}
		})
	}
}

// TestDockerRuntimeTreatsAVanishedWorkerAsTerminated verifies that a worker
// which disappeared during cancellation has no surviving command processes.
func TestDockerRuntimeTreatsAVanishedWorkerAsTerminated(t *testing.T) {
	runtime, runID, _ := startCommandWorker(t)
	t.Setenv("WORKER_DOCKER_TERMINATE_STATUS", "1")
	t.Setenv("WORKER_DOCKER_TERMINATE_REMOVES", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_, err := runtime.RunCommand(ctx, slowCommand(runID))

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunCommand() error = %v, want the timeout once the worker is gone", err)
	}
}

// TestDockerRuntimeRefusesWorkAfterAnUnconfirmedTermination verifies that the
// worker-side overlap guard surfaces as the typed discrepancy rather than as a
// command exit result.
func TestDockerRuntimeRefusesWorkAfterAnUnconfirmedTermination(t *testing.T) {
	runtime, runID, _ := startCommandWorker(t)

	result, err := runtime.RunCommand(context.Background(), worker.CommandRequest{
		RunID:             runID,
		Command:           "refused-command",
		EnvironmentPolicy: worker.EnvironmentPolicyClean,
		Role:              "gate",
	})

	var terminationErr *worker.CommandTerminationError
	if !errors.As(err, &terminationErr) {
		t.Fatalf("RunCommand() = %#v, %v; want a typed termination discrepancy", result, err)
	}
}

// startCommandWorker starts one stub-backed worker for command supervision.
func startCommandWorker(t *testing.T) (*worker.DockerRuntime, string, string) {
	t.Helper()
	stub, logPath, _ := writeDockerStub(t)
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	request := worker.StartRequest{
		RunID:           "run-contract-supervision",
		WorktreePath:    makeDirectory(t, "worktree"),
		GitMetadataPath: makeDirectory(t, "git-metadata"),
		Image:           "ghcr.io/example/factory-worker",
		ImageDigest:     testWorkerDigest,
	}
	if err := runtime.Start(context.Background(), request); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	return runtime, request.RunID, logPath
}

// slowCommand selects the stub command that outlives a short deadline.
func slowCommand(runID string) worker.CommandRequest {
	return worker.CommandRequest{
		RunID:             runID,
		Command:           "slow-command",
		EnvironmentPolicy: worker.EnvironmentPolicyClean,
		Role:              "gate",
	}
}

// commandIdentity returns the command identity that follows the supervision
// marker and the records root in one logged Docker invocation.
func commandIdentity(t *testing.T, line, marker string) string {
	t.Helper()
	index := strings.LastIndex(line, marker)
	if index < 0 {
		t.Fatalf("log line %q has no %q marker", line, marker)
	}
	fields := strings.Fields(line[index+len(marker):])
	if len(fields) < 2 || fields[1] == "" {
		t.Fatalf("log line %q has no command identity", line)
	}
	return fields[1]
}
