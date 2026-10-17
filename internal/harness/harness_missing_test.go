package harness_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestHeadlessLaunchNamesAHarnessMissingFromTheWorkerImage verifies that a
// launch on a worker image without the selected harness executable is a typed
// harness-missing failure, during identity discovery and after settlement.
// The worker helper records that evidence when the command cannot start.
func TestHeadlessLaunchNamesAHarnessMissingFromTheWorkerImage(t *testing.T) {
	t.Parallel()

	missing := worker.HeadlessInspection{Status: worker.HeadlessStatusExited, ExitCode: 127, Stderr: worker.HeadlessStartFailedMarker + "\n", StartFailed: true}
	_, launchErr := harness.NewCopilotHeadless(&headlessTestWorker{inspection: missing}).StartHeadless(context.Background(), copilotRequest())
	settledErr := harness.NewCopilotHeadless(&headlessTestWorker{inspection: missing}).HeadlessFailureFor(context.Background(), harness.HeadlessInspectionRequest{
		InvocationID: "inv-copilot", RunID: "run-copilot", WorkerID: "worker-copilot",
	})
	for phase, err := range map[string]error{"discovery": launchErr, "settlement": settledErr} {
		classified := harness.ClassifyError(err, harness.NameCopilot)
		if !harness.IsHarnessMissing(classified) || !harness.IsUnexpectedExit(classified) {
			t.Fatalf("%s: classified failure = %v, want a harness-missing unexpected exit", phase, classified)
		}
		if message := classified.Error(); !strings.Contains(message, "worker image") || !strings.Contains(message, harness.NameCopilot) {
			t.Fatalf("%s: failure message = %q, want the harness and the worker image cause", phase, message)
		}
	}
}

// TestHeadlessFailureKeepsACommandExitApartFromAMissingHarness verifies that
// exit status 127 without the worker helper's start-failure evidence, such as
// a command not found inside the running harness, stays an ordinary exit.
func TestHeadlessFailureKeepsACommandExitApartFromAMissingHarness(t *testing.T) {
	t.Parallel()

	runtime := harness.NewCopilotHeadless(&headlessTestWorker{inspection: worker.HeadlessInspection{Status: worker.HeadlessStatusExited, ExitCode: 127, Stderr: "sh: 1: jq: not found\n"}})
	err := runtime.HeadlessFailureFor(context.Background(), harness.HeadlessInspectionRequest{InvocationID: "inv-exit", RunID: "run-exit", WorkerID: "worker-exit"})
	if classified := harness.ClassifyError(err, harness.NameCopilot); harness.IsHarnessMissing(classified) || !harness.IsUnexpectedExit(classified) {
		t.Fatalf("classified failure = %v, want an ordinary unexpected exit", classified)
	}
}

// TestHeadlessFailureIgnoresAStartFailureLineAChildPrinted verifies a started
// command that prints the helper's start-failure line and exits 127 is not a
// missing harness: only the helper's own state names that cause.
func TestHeadlessFailureIgnoresAStartFailureLineAChildPrinted(t *testing.T) {
	t.Parallel()

	forged := worker.HeadlessInspection{Status: worker.HeadlessStatusExited, ExitCode: 127, Stderr: worker.HeadlessStartFailedMarker + "\n"}
	err := harness.NewCopilotHeadless(&headlessTestWorker{inspection: forged}).HeadlessFailureFor(context.Background(), harness.HeadlessInspectionRequest{InvocationID: "inv-forged", RunID: "run-forged", WorkerID: "worker-forged"})
	if classified := harness.ClassifyError(err, harness.NameCopilot); harness.IsHarnessMissing(classified) || !harness.IsUnexpectedExit(classified) {
		t.Fatalf("classified failure = %v, want an ordinary unexpected exit", classified)
	}
}
