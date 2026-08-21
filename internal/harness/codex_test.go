package harness_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestCodexStartsAnInteractiveSessionThroughThePortableSeams verifies the
// Codex command, role environment, visible surface, and initial prompt input.
func TestCodexStartsAnInteractiveSessionThroughThePortableSeams(t *testing.T) {
	workerRuntime := &fakeWorker{}
	terminalRuntime := &fakeTerminal{surface: terminal.Surface{ID: "surface-implementation", WorkspaceID: "workspace-run", Name: "implementation"}}
	runtime := harness.NewCodex(workerRuntime, terminalRuntime)
	session, err := runtime.Start(context.Background(), harness.StartRequest{
		InvocationID:    "inv-1",
		RunID:           "run-1",
		Role:            "implementation",
		Stage:           "implementation",
		WorkspaceID:     "workspace-run",
		Surface:         terminalRuntime.surface,
		Prompt:          "Implement the frozen specification.",
		Model:           "gpt-5",
		ReasoningEffort: "high",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if session.Surface.ID != "surface-implementation" {
		t.Fatalf("session surface = %#v, want visible implementation surface", session.Surface)
	}
	if len(workerRuntime.requests) != 1 {
		t.Fatalf("interactive worker requests = %#v, want one", workerRuntime.requests)
	}
	request := workerRuntime.requests[0]
	if strings.Join(request.Command, " ") != "codex -a untrusted -s danger-full-access -m gpt-5 -c model_reasoning_effort=high" {
		t.Fatalf("Codex command = %#v, want sandbox flags before command", request.Command)
	}
	if request.Environment["FACTORY_INVOCATION_ID"] != "inv-1" || request.Environment["FACTORY_STAGE"] != "implementation" {
		t.Fatalf("Codex environment = %#v, want invocation and stage identity", request.Environment)
	}
	if string(terminalRuntime.inputs[0]) != "Implement the frozen specification.\n" {
		t.Fatalf("surface input = %q, want initial prompt", terminalRuntime.inputs[0])
	}
	if terminalRuntime.launched.Executable != "factory-worker-attach" {
		t.Fatalf("surface launch = %#v, want attach helper in the existing implementation surface", terminalRuntime.launched)
	}
}

// TestCodexResumesWithNativeSessionIdentifier verifies the resume command's
// global flags precede the resume subcommand as required by the spike.
func TestCodexResumesWithNativeSessionIdentifier(t *testing.T) {
	workerRuntime := &fakeWorker{}
	terminalRuntime := &fakeTerminal{surface: terminal.Surface{ID: "surface-implementation", WorkspaceID: "workspace-run", Name: "implementation"}}
	_, err := harness.NewCodex(workerRuntime, terminalRuntime).Resume(context.Background(), harness.StartRequest{
		InvocationID:    "inv-2",
		RunID:           "run-1",
		Role:            "implementation",
		Stage:           "implementation",
		WorkspaceID:     "workspace-run",
		Surface:         terminalRuntime.surface,
		ResumeSessionID: "session-1",
		Prompt:          "Continue the implementation.",
	})
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if got := strings.Join(workerRuntime.requests[0].Command, " "); got != "codex -a untrusted -s danger-full-access resume session-1" {
		t.Fatalf("resume command = %q, want native resume command", got)
	}
}

// TestCodexFreshStartIgnoresStaleSessions verifies fresh discovery returns a
// session created after the baseline snapshot rather than the newest old file.
func TestCodexFreshStartIgnoresStaleSessions(t *testing.T) {
	workerRuntime := &snapshotWorker{
		fakeWorker: &fakeWorker{},
		snapshots:  [][]string{{"session-old"}, {"session-old"}, {"session-old", "session-new"}},
	}
	terminalRuntime := &fakeTerminal{surface: terminal.Surface{ID: "surface-implementation", WorkspaceID: "workspace-run", Name: "implementation"}}
	session, err := harness.NewCodex(workerRuntime, terminalRuntime).Start(context.Background(), harness.StartRequest{
		InvocationID: "inv-3",
		RunID:        "run-1",
		Role:         "implementation",
		Stage:        "implementation",
		WorkspaceID:  "workspace-run",
		Surface:      terminalRuntime.surface,
		Prompt:       "Start the implementation.",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if session.NativeSessionID != "session-new" {
		t.Fatalf("NativeSessionID = %q, want newly persisted session", session.NativeSessionID)
	}
}

// TestCodexFinishLeavesTheSurfaceRecoverable verifies that accepted completion
// exits Codex without destroying its cmux surface or worker state.
func TestCodexFinishLeavesTheSurfaceRecoverable(t *testing.T) {
	terminalRuntime := &fakeTerminal{surface: terminal.Surface{ID: "surface-implementation", WorkspaceID: "workspace-run", Name: "implementation"}}
	runtime := harness.NewCodex(&fakeWorker{}, terminalRuntime)
	if err := runtime.Finish(context.Background(), harness.Session{Surface: terminalRuntime.surface}); err != nil {
		t.Fatalf("Finish() error = %v", err)
	}
	if terminalRuntime.closed != "" || string(terminalRuntime.inputs[0]) != "/exit\n" {
		t.Fatalf("finish operations = closed=%q inputs=%q, want graceful exit with recoverable surface", terminalRuntime.closed, terminalRuntime.inputs)
	}
}

// fakeWorker implements the worker seams used by Codex contract tests.
type fakeWorker struct {
	requests []worker.InteractiveRequest
}

// snapshotWorker adds deterministic native-session snapshots to the common
// worker fixture without making every harness test wait for discovery.
type snapshotWorker struct {
	*fakeWorker
	snapshots [][]string
	index     int
}

// NativeSessionIDs returns the next persisted-session snapshot.
func (w *snapshotWorker) NativeSessionIDs(context.Context, worker.NativeSessionRequest) ([]string, error) {
	if w.index >= len(w.snapshots) {
		return nil, nil
	}
	value := append([]string(nil), w.snapshots[w.index]...)
	w.index++
	return value, nil
}

// Start implements WorkerRuntime for the harness test.
func (*fakeWorker) Start(context.Context, worker.StartRequest) error { return nil }

// Resume implements WorkerRuntime for the harness test.
func (*fakeWorker) Resume(context.Context, worker.ResumeRequest) error { return nil }

// RunCommand implements WorkerRuntime for the harness test.
func (*fakeWorker) RunCommand(context.Context, worker.CommandRequest) (worker.CommandResult, error) {
	return worker.CommandResult{}, nil
}

// Stop implements WorkerRuntime for the harness test.
func (*fakeWorker) Stop(context.Context, string) error { return nil }

// Inspect implements WorkerRuntime for the harness test.
func (*fakeWorker) Inspect(context.Context, string) (worker.Inspection, error) {
	return worker.Inspection{Exists: true, Running: true}, nil
}

// InteractiveCommand records one interactive worker request.
func (w *fakeWorker) InteractiveCommand(_ context.Context, request worker.InteractiveRequest) (worker.InteractiveCommand, error) {
	w.requests = append(w.requests, request)
	return worker.InteractiveCommand{Executable: "factory-worker-attach", Args: []string{"--run-id", request.RunID}}, nil
}

// fakeTerminal records visible surface operations without reading a screen.
type fakeTerminal struct {
	surface  terminal.Surface
	inputs   [][]byte
	closed   string
	launched terminal.Command
}

// EnsureControlWorkspace implements TerminalRuntime for the harness test.
func (*fakeTerminal) EnsureControlWorkspace(context.Context, terminal.WorkspaceRequest) (terminal.Workspace, error) {
	return terminal.Workspace{ID: "workspace-control"}, nil
}

// EnsureRunWorkspace implements TerminalRuntime for the harness test.
func (*fakeTerminal) EnsureRunWorkspace(context.Context, terminal.RunWorkspaceRequest) (terminal.RunWorkspace, error) {
	return terminal.RunWorkspace{}, nil
}

// CreateSurface records the command-backed visible surface.
func (t *fakeTerminal) CreateSurface(_ context.Context, request terminal.SurfaceRequest) (terminal.Surface, error) {
	if request.WorkspaceID != t.surface.WorkspaceID {
		return terminal.Surface{}, nil
	}
	return t.surface, nil
}

// LaunchSurface records the command launched in a layout-created surface.
func (t *fakeTerminal) LaunchSurface(_ context.Context, _ terminal.SurfaceID, command terminal.Command) error {
	t.launched = command
	return nil
}

// SendInput records exact bytes sent to the surface.
func (t *fakeTerminal) SendInput(_ context.Context, _ terminal.SurfaceID, input []byte) error {
	t.inputs = append(t.inputs, input)
	return nil
}

// Notify implements TerminalRuntime for the harness test.
func (*fakeTerminal) Notify(context.Context, terminal.Notification) error { return nil }

// CloseSurface records the detached surface.
func (t *fakeTerminal) CloseSurface(_ context.Context, surfaceID terminal.SurfaceID) error {
	t.closed = string(surfaceID)
	return nil
}

// CloseWorkspace implements TerminalRuntime for the harness test.
func (*fakeTerminal) CloseWorkspace(context.Context, terminal.WorkspaceID) error { return nil }

var _ worker.WorkerRuntime = (*fakeWorker)(nil)
var _ worker.InteractiveRuntime = (*fakeWorker)(nil)
var _ worker.NativeSessionSnapshotProvider = (*snapshotWorker)(nil)
var _ terminal.TerminalRuntime = (*fakeTerminal)(nil)
