package harness_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

const headlessTestSession = "7c9e6679-7425-40de-944b-e07fc1f90ae7"

// TestCodexHeadlessUsesOnlyTheDetachedWorkerSeam verifies command translation,
// JSON event identity, exact resume identity, and idempotent cleanup without a
// terminal adapter or coordinator-side process.
func TestCodexHeadlessUsesOnlyTheDetachedWorkerSeam(t *testing.T) {
	workerRuntime := &headlessTestWorker{inspection: worker.HeadlessInspection{
		Status: worker.HeadlessStatusRunning,
		Stdout: "Codex starting\n{\"type\":\"thread.started\",\"thread_id\":\"" + headlessTestSession + "\"}\n",
	}}
	runtime := harness.NewCodexHeadless(workerRuntime)
	request := harness.HeadlessStartRequest{
		InvocationID: "inv-headless", RunID: "run-headless", WorkerID: "worker-headless",
		Role: "implementation", Stage: "implementation", Prompt: "Implement the frozen issue.",
		Model: "gpt-5", ReasoningEffort: "high",
	}
	session, err := runtime.StartHeadless(context.Background(), request)
	if err != nil {
		t.Fatalf("StartHeadless() error = %v", err)
	}
	if session.NativeSessionID != headlessTestSession {
		t.Fatalf("native session = %q, want JSON thread identity", session.NativeSessionID)
	}
	if len(workerRuntime.starts) != 1 {
		t.Fatalf("headless starts = %#v, want one", workerRuntime.starts)
	}
	start := workerRuntime.starts[0]
	joined := strings.Join(start.Command, " ")
	for _, value := range []string{"codex", "exec", "--json", "-s", "danger-full-access", "-m", "gpt-5", "model_reasoning_effort=high"} {
		if !containsHeadlessArgument(start.Command, value) {
			t.Fatalf("headless command = %#v, want %q", start.Command, value)
		}
	}
	if !strings.Contains(joined, request.Prompt) || start.Mode != worker.HeadlessLaunchFresh {
		t.Fatalf("headless start request = %#v, want prompt and fresh mode", start)
	}
	if strings.Contains(joined, "-it") || strings.Contains(joined, "--tty") {
		t.Fatalf("headless command allocated a terminal: %q", joined)
	}

	resume := request
	resume.InvocationID = "inv-headless-resume"
	resume.ResumeSessionID = session.NativeSessionID
	resumed, err := runtime.ResumeHeadless(context.Background(), resume)
	if err != nil {
		t.Fatalf("ResumeHeadless() error = %v", err)
	}
	if resumed.NativeSessionID != headlessTestSession || workerRuntime.starts[1].Mode != worker.HeadlessLaunchResume {
		t.Fatalf("resumed session/request = %#v / %#v, want exact native identity and resume mode", resumed, workerRuntime.starts[1])
	}
	if err := runtime.CancelHeadless(context.Background(), session); err != nil {
		t.Fatalf("CancelHeadless() error = %v", err)
	}
	if err := runtime.FinishHeadless(context.Background(), resumed); err != nil {
		t.Fatalf("FinishHeadless() error = %v", err)
	}
	if workerRuntime.cancelCalls != 1 || workerRuntime.finishCalls != 1 {
		t.Fatalf("cleanup calls = cancel %d, finish %d; want one each", workerRuntime.cancelCalls, workerRuntime.finishCalls)
	}
}

// TestCodexHeadlessRecoversTheThreadIdentityFromInvocationState verifies that
// restart recovery uses the invocation's detached process output rather than
// adopting an unrelated older session from the shared role home.
func TestCodexHeadlessRecoversTheThreadIdentityFromInvocationState(t *testing.T) {
	workerRuntime := &headlessTestWorker{inspection: worker.HeadlessInspection{
		Status: worker.HeadlessStatusRunning,
		Stdout: "older role-home session\n{\"type\":\"thread.started\",\"thread_id\":\"" + headlessTestSession + "\"}\n",
	}}
	runtime := harness.NewCodexHeadless(workerRuntime)
	got, err := runtime.NativeSessionID(context.Background(), harness.NativeSessionRequest{
		InvocationID: "inv-headless-recovered", RunID: "run-headless", WorkerID: "worker-headless", Harness: harness.NameCodex,
	})
	if err != nil {
		t.Fatalf("NativeSessionID() error = %v", err)
	}
	if got != headlessTestSession {
		t.Fatalf("NativeSessionID() = %q, want thread.started identity %q", got, headlessTestSession)
	}
}

// TestCodexHeadlessWaitsForTheDetachedHelperState verifies the adapter does
// not turn the normal docker exec -d state-creation race into a launch failure.
func TestCodexHeadlessWaitsForTheDetachedHelperState(t *testing.T) {
	workerRuntime := &headlessTestWorker{inspections: []worker.HeadlessInspection{
		{Status: worker.HeadlessStatusMissing},
		{Status: worker.HeadlessStatusRunning, Stdout: "{\"type\":\"thread.started\",\"thread_id\":\"" + headlessTestSession + "\"}\n"},
	}}
	if _, err := harness.NewCodexHeadless(workerRuntime).StartHeadless(context.Background(), harness.HeadlessStartRequest{
		InvocationID: "inv-headless-race", RunID: "run-headless", Role: "implementation",
		Stage: "implementation", Prompt: "Start.",
	}); err != nil {
		t.Fatalf("StartHeadless() error = %v, want state polling", err)
	}
	if workerRuntime.inspectCalls != 2 {
		t.Fatalf("headless state inspections = %d, want initial missing state followed by running state", workerRuntime.inspectCalls)
	}
}

// TestCodexHeadlessClassifiesOnlyMachineReadableFailureCodes verifies raw
// native prose cannot become a coordinator outcome while explicit event codes
// do map to the existing typed capacity policy.
func TestCodexHeadlessClassifiesOnlyMachineReadableFailureCodes(t *testing.T) {
	workerRuntime := &headlessTestWorker{inspection: worker.HeadlessInspection{
		Status: worker.HeadlessStatusExited,
		Stdout: "human says rate limit but no event\n",
		Stderr: "token=must-not-reach-error",
	}}
	_, err := harness.NewCodexHeadless(workerRuntime).StartHeadless(context.Background(), harness.HeadlessStartRequest{
		InvocationID: "inv-headless-failure", RunID: "run-headless", Role: "implementation",
		Stage: "implementation", Prompt: "Start.",
	})
	if !errors.Is(err, harness.ErrUnexpectedExit) {
		t.Fatalf("prose failure = %v, want unexpected exit", err)
	}
	if strings.Contains(err.Error(), "token=") || strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("headless error leaked native diagnostics: %v", err)
	}

	workerRuntime = &headlessTestWorker{inspection: worker.HeadlessInspection{
		Status: worker.HeadlessStatusExited,
		Stdout: "{\"type\":\"error\",\"code\":\"rate_limit_exceeded\"}\n",
	}}
	_, err = harness.NewCodexHeadless(workerRuntime).StartHeadless(context.Background(), harness.HeadlessStartRequest{
		InvocationID: "inv-headless-rate", RunID: "run-headless", Role: "implementation",
		Stage: "implementation", Prompt: "Start.",
	})
	if !errors.Is(err, harness.ErrRateLimited) {
		t.Fatalf("machine failure = %v, want rate-limited outcome", err)
	}
	if diagnostics := harness.HeadlessDiagnostics(err); !strings.Contains(diagnostics, "rate_limit_exceeded") {
		t.Fatalf("headless diagnostics = %q, want bounded local event output", diagnostics)
	}
}

// TestCodexHeadlessMapsLaunchFailuresToTheExistingTypedOutcome verifies a
// worker launch failure cannot expose native or Docker prose as coordinator
// error text.
func TestCodexHeadlessMapsLaunchFailuresToTheExistingTypedOutcome(t *testing.T) {
	workerRuntime := &headlessTestWorker{startErr: errors.New("docker: secret launch detail")}
	_, err := harness.NewCodexHeadless(workerRuntime).StartHeadless(context.Background(), harness.HeadlessStartRequest{
		InvocationID: "inv-headless-launch-failure", RunID: "run-headless", Role: "implementation",
		Stage: "implementation", Prompt: "Start.",
	})
	if !errors.Is(err, harness.ErrUnexpectedExit) {
		t.Fatalf("launch failure = %v, want unexpected exit", err)
	}
	if strings.Contains(err.Error(), "secret launch detail") {
		t.Fatalf("launch failure leaked worker detail: %v", err)
	}
}

// containsHeadlessArgument reports whether a command contains one exact or
// embedded option value needed by the contract assertion.
func containsHeadlessArgument(command []string, wanted string) bool {
	for _, argument := range command {
		if argument == wanted || strings.Contains(argument, wanted) {
			return true
		}
	}
	return false
}

// headlessTestWorker records the adapter-facing detached requests and returns
// a controlled bounded inspection projection.
type headlessTestWorker struct {
	starts       []worker.HeadlessRequest
	inspection   worker.HeadlessInspection
	inspections  []worker.HeadlessInspection
	inspectCalls int
	startErr     error
	cancelCalls  int
	finishCalls  int
}

// Start implements the base worker seam.
func (*headlessTestWorker) Start(context.Context, worker.StartRequest) error { return nil }

// Resume implements the base worker seam.
func (*headlessTestWorker) Resume(context.Context, worker.ResumeRequest) error { return nil }

// RunCommand implements the base worker seam.
func (*headlessTestWorker) RunCommand(context.Context, worker.CommandRequest) (worker.CommandResult, error) {
	return worker.CommandResult{}, nil
}

// Stop implements the base worker seam.
func (*headlessTestWorker) Stop(context.Context, string) error { return nil }

// Inspect implements the base worker seam.
func (*headlessTestWorker) Inspect(context.Context, string) (worker.Inspection, error) {
	return worker.Inspection{Exists: true, Running: true}, nil
}

// StartHeadless records one detached launch request.
func (w *headlessTestWorker) StartHeadless(_ context.Context, request worker.HeadlessRequest) (worker.HeadlessExecution, error) {
	w.starts = append(w.starts, request)
	if w.startErr != nil {
		return worker.HeadlessExecution{}, w.startErr
	}
	return worker.HeadlessExecution{RunID: request.RunID, WorkerID: request.WorkerID, InvocationID: request.InvocationID}, nil
}

// InspectHeadless returns the controlled process state.
func (w *headlessTestWorker) InspectHeadless(context.Context, worker.HeadlessRequest) (worker.HeadlessInspection, error) {
	w.inspectCalls++
	if len(w.inspections) > 0 {
		inspection := w.inspections[0]
		w.inspections = w.inspections[1:]
		return inspection, nil
	}
	return w.inspection, nil
}

// CancelHeadless records one cancellation request.
func (w *headlessTestWorker) CancelHeadless(context.Context, worker.HeadlessRequest) error {
	w.cancelCalls++
	return nil
}

// FinishHeadless records one accepted-completion request.
func (w *headlessTestWorker) FinishHeadless(context.Context, worker.HeadlessRequest) error {
	w.finishCalls++
	return nil
}

var _ worker.HeadlessProcessRuntime = (*headlessTestWorker)(nil)
