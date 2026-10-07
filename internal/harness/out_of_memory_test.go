package harness_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestHeadlessFailureNamesAnOutOfMemoryKill verifies that a detached harness
// the worker memory limit killed is a typed out-of-memory failure. It keeps
// the unexpected-exit recovery category, names the cause, and survives the
// coordinator's error classification.
func TestHeadlessFailureNamesAnOutOfMemoryKill(t *testing.T) {
	t.Parallel()

	for name, runtime := range map[string]harness.HeadlessFailureInspector{
		"codex":  harness.NewCodexHeadless(&headlessTestWorker{inspection: worker.HeadlessInspection{Status: worker.HeadlessStatusExited, ExitCode: 137, OutOfMemory: true}}),
		"claude": harness.NewClaudeHeadless(&headlessTestWorker{inspection: worker.HeadlessInspection{Status: worker.HeadlessStatusExited, ExitCode: 137, OutOfMemory: true}}),
	} {
		err := runtime.HeadlessFailureFor(context.Background(), harness.HeadlessInspectionRequest{InvocationID: "inv-oom", RunID: "run-oom", WorkerID: "worker-oom"})
		classified := harness.ClassifyError(err, name)
		if !harness.IsOutOfMemory(classified) || !harness.IsUnexpectedExit(classified) {
			t.Fatalf("%s: classified failure = %v, want an out-of-memory unexpected exit", name, classified)
		}
		if message := classified.Error(); !strings.Contains(message, "out of memory") || !strings.Contains(message, name) {
			t.Fatalf("%s: failure message = %q, want the harness and the out-of-memory cause", name, message)
		}
	}
}

// TestHeadlessFailureKeepsAnOrdinaryExitApartFromOutOfMemory verifies a
// failed exit without a memory-limit kill stays an ordinary unexpected exit.
func TestHeadlessFailureKeepsAnOrdinaryExitApartFromOutOfMemory(t *testing.T) {
	t.Parallel()

	runtime := harness.NewCodexHeadless(&headlessTestWorker{inspection: worker.HeadlessInspection{Status: worker.HeadlessStatusExited, ExitCode: 137}})
	err := runtime.HeadlessFailureFor(context.Background(), harness.HeadlessInspectionRequest{InvocationID: "inv-exit", RunID: "run-exit", WorkerID: "worker-exit"})
	if classified := harness.ClassifyError(err, "codex"); harness.IsOutOfMemory(classified) || !harness.IsUnexpectedExit(classified) {
		t.Fatalf("classified failure = %v, want an ordinary unexpected exit", classified)
	}
}
