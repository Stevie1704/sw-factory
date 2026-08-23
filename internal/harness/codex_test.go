package harness_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestCodexStartsAnInteractiveSessionThroughThePortableSeams verifies the
// Codex command, role environment, visible surface, and argument-passed prompt.
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
	wantCommand := []string{
		"codex", "-a", "never", "-s", "danger-full-access",
		"-m", "gpt-5", "-c", "model_reasoning_effort=high",
		"Implement the frozen specification.",
	}
	if strings.Join(request.Command, "\x00") != strings.Join(wantCommand, "\x00") {
		t.Fatalf("Codex command = %#v, want sandbox flags followed by the initial prompt argument", request.Command)
	}
	if request.Environment["FACTORY_INVOCATION_ID"] != "inv-1" || request.Environment["FACTORY_STAGE"] != "implementation" {
		t.Fatalf("Codex environment = %#v, want invocation and stage identity", request.Environment)
	}
	if len(terminalRuntime.inputs) != 0 {
		t.Fatalf("surface inputs = %q, want startup to avoid racing terminal input", terminalRuntime.inputs)
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
	wantCommand := []string{"codex", "-a", "never", "-s", "danger-full-access", "resume", "session-1", "Continue the implementation."}
	if strings.Join(workerRuntime.requests[0].Command, "\x00") != strings.Join(wantCommand, "\x00") {
		t.Fatalf("resume command = %#v, want native resume command", workerRuntime.requests[0].Command)
	}
	if len(terminalRuntime.inputs) != 0 {
		t.Fatalf("surface inputs = %q, want resume prompt passed as an argument", terminalRuntime.inputs)
	}
}

// TestCodexFreshStartIgnoresStaleSessions verifies fresh discovery returns a
// session created after the baseline snapshot rather than the newest old file.
func TestCodexFreshStartIgnoresStaleSessions(t *testing.T) {
	workerRuntime := &snapshotWorker{
		fakeWorker: &fakeWorker{},
		snapshots:  [][]string{{"session-old"}, {"session-old", "session-new"}},
	}
	terminalRuntime := &fakeTerminal{
		surface:    terminal.Surface{ID: "surface-implementation", WorkspaceID: "workspace-run", Name: "implementation"},
		launchHook: workerRuntime.markSurfaceStarted,
	}
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

// TestCodexDiscoveryFailureClosesTheSurface verifies a failed fresh-session
// lookup does not leave the launched surface or Codex process running.
func TestCodexDiscoveryFailureClosesTheSurface(t *testing.T) {
	workerRuntime := &snapshotWorker{fakeWorker: &fakeWorker{}, snapshots: [][]string{{"session-old"}}}
	terminalRuntime := &fakeTerminal{
		surface:    terminal.Surface{ID: "surface-implementation", WorkspaceID: "workspace-run", Name: "implementation"},
		launchHook: workerRuntime.markSurfaceStarted,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := harness.NewCodex(workerRuntime, terminalRuntime).Start(ctx, harness.StartRequest{
		InvocationID: "inv-4",
		RunID:        "run-1",
		Role:         "implementation",
		Stage:        "implementation",
		WorkspaceID:  "workspace-run",
		Surface:      terminalRuntime.surface,
		Prompt:       "Start the implementation.",
	})
	if !errors.Is(err, harness.ErrNativeSessionUnavailable) {
		t.Fatalf("Start() error = %v, want native-session-unavailable sentinel", err)
	}
	if terminalRuntime.closed != string(terminalRuntime.surface.ID) {
		t.Fatalf("closed surface = %q, want %q", terminalRuntime.closed, terminalRuntime.surface.ID)
	}
}

// TestCodexLegacyDiscoveryFailureClosesTheSurface verifies cleanup also
// covers runtimes that expose only the single-session lookup seam.
func TestCodexLegacyDiscoveryFailureClosesTheSurface(t *testing.T) {
	workerRuntime := &failingNativeSessionWorker{fakeWorker: &fakeWorker{}}
	terminalRuntime := &fakeTerminal{surface: terminal.Surface{ID: "surface-implementation", WorkspaceID: "workspace-run", Name: "implementation"}}
	_, err := harness.NewCodex(workerRuntime, terminalRuntime).Start(context.Background(), harness.StartRequest{
		InvocationID: "inv-5",
		RunID:        "run-1",
		Role:         "implementation",
		Stage:        "implementation",
		WorkspaceID:  "workspace-run",
		Surface:      terminalRuntime.surface,
		Prompt:       "Start the implementation.",
	})
	if err == nil || !strings.Contains(err.Error(), "discover Codex native session: native session lookup failed") {
		t.Fatalf("Start() error = %v, want wrapped legacy discovery error", err)
	}
	if terminalRuntime.closed != string(terminalRuntime.surface.ID) {
		t.Fatalf("closed surface = %q, want %q", terminalRuntime.closed, terminalRuntime.surface.ID)
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
	snapshots      [][]string
	surfaceStarted bool
}

// failingNativeSessionWorker exposes the legacy single-session lookup with a
// deterministic failure for cleanup tests.
type failingNativeSessionWorker struct {
	*fakeWorker
}

// NativeSessionID returns the configured legacy lookup failure.
func (*failingNativeSessionWorker) NativeSessionID(context.Context, worker.NativeSessionRequest) (string, error) {
	return "", errors.New("native session lookup failed")
}

// markSurfaceStarted models Codex persisting its new session during surface
// launch, after the baseline snapshot has been captured.
func (w *snapshotWorker) markSurfaceStarted() {
	w.surfaceStarted = true
}

// NativeSessionIDs returns the persisted-session snapshot for the current
// surface-start phase.
func (w *snapshotWorker) NativeSessionIDs(context.Context, worker.NativeSessionRequest) ([]string, error) {
	index := 0
	if w.surfaceStarted {
		index = 1
	}
	if index >= len(w.snapshots) {
		return nil, nil
	}
	value := append([]string(nil), w.snapshots[index]...)
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
	surface    terminal.Surface
	inputs     [][]byte
	closed     string
	launched   terminal.Command
	launchHook func()
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
	if t.launchHook != nil {
		t.launchHook()
	}
	return t.surface, nil
}

// LaunchSurface records the command launched in a layout-created surface.
func (t *fakeTerminal) LaunchSurface(_ context.Context, _ terminal.SurfaceID, command terminal.Command) error {
	t.launched = command
	if t.launchHook != nil {
		t.launchHook()
	}
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
var _ worker.NativeSessionProvider = (*failingNativeSessionWorker)(nil)
var _ terminal.TerminalRuntime = (*fakeTerminal)(nil)
