package terminal_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/terminal"
)

// TestCmuxRuntimeKeepsTerminalHandlesBehindThePortableSeam verifies that the
// adapter creates the control/run surfaces and routes input and notifications
// without exposing adapter-specific command details to callers.
func TestCmuxRuntimeKeepsTerminalHandlesBehindThePortableSeam(t *testing.T) {
	runner := &fakeRunner{responses: [][]byte{
		[]byte(`{"id":"workspace-control"}`),
		[]byte(`{"id":"workspace-run","surfaces":[{"id":"surface-status","name":"status"},{"id":"surface-implementation","name":"implementation"},{"id":"surface-checks","name":"checks"}]}`),
		[]byte(`{"id":"surface-extra","workspace_id":"workspace-run"}`),
	}}
	runtime := terminal.NewCmuxRuntime(runner)
	control, err := runtime.EnsureControlWorkspace(context.Background(), terminal.WorkspaceRequest{
		Name:             "factory-control",
		Description:      "factory coordinator",
		WorkingDirectory: "/tmp/factory",
	})
	if err != nil {
		t.Fatalf("EnsureControlWorkspace() error = %v", err)
	}
	if control.ID != "workspace-control" {
		t.Fatalf("control workspace = %#v, want opaque workspace handle", control)
	}
	run, err := runtime.EnsureRunWorkspace(context.Background(), terminal.RunWorkspaceRequest{
		RunID:            "run-1",
		Name:             "factory-run-1",
		Description:      "issue #6",
		WorkingDirectory: "/tmp/factory",
	})
	if err != nil {
		t.Fatalf("EnsureRunWorkspace() error = %v", err)
	}
	if run.ID != "workspace-run" || run.Status.ID != "surface-status" || run.Implementation.ID != "surface-implementation" || run.Checks.ID != "surface-checks" {
		t.Fatalf("run workspace = %#v, want status/implementation/checks surfaces", run)
	}
	extra, err := runtime.CreateSurface(context.Background(), terminal.SurfaceRequest{
		WorkspaceID: run.ID,
		Name:        "extra",
		Command:     terminal.Command{Executable: "factory-worker-attach", Args: []string{"--run-id", "run-1"}},
	})
	if err != nil {
		t.Fatalf("CreateSurface() error = %v", err)
	}
	if extra.ID != "surface-extra" {
		t.Fatalf("extra surface = %#v, want opaque surface handle", extra)
	}
	if err := runtime.SendInput(context.Background(), extra.ID, []byte("continue\n")); err != nil {
		t.Fatalf("SendInput() error = %v", err)
	}
	if err := runtime.LaunchSurface(context.Background(), extra.ID, terminal.Command{Executable: "factory-worker-attach", Args: []string{"--run-id", "run-1"}}); err != nil {
		t.Fatalf("LaunchSurface() error = %v", err)
	}
	if err := runtime.Notify(context.Background(), terminal.Notification{WorkspaceID: run.ID, Title: "stage", Body: "implementation started"}); err != nil {
		t.Fatalf("Notify() error = %v", err)
	}
	if err := runtime.CloseSurface(context.Background(), extra.ID); err != nil {
		t.Fatalf("CloseSurface() error = %v", err)
	}

	joined := strings.Join(runner.calls, "\n")
	if !strings.Contains(joined, "new-workspace") || !strings.Contains(joined, "send-input") || !strings.Contains(joined, "notify") {
		t.Fatalf("adapter calls = %q, want workspace/input/notification operations", joined)
	}
	if strings.Contains(joined, "docker.sock") {
		t.Fatalf("terminal adapter referenced the Docker socket: %q", joined)
	}
}

// TestCmuxRuntimeRejectsMalformedRunWorkspace verifies that a workspace
// without the required supervision surfaces cannot be accepted.
func TestCmuxRuntimeRejectsMalformedRunWorkspace(t *testing.T) {
	runner := &fakeRunner{responses: [][]byte{[]byte(`{"id":"workspace-run","surfaces":[{"id":"surface-status","name":"status"}]}`)}}
	_, err := terminal.NewCmuxRuntime(runner).EnsureRunWorkspace(context.Background(), terminal.RunWorkspaceRequest{RunID: "run-1", Name: "factory-run-1", WorkingDirectory: "/tmp/factory"})
	if err == nil || !strings.Contains(err.Error(), "implementation") {
		t.Fatalf("EnsureRunWorkspace() error = %v, want required surface error", err)
	}
}

// fakeRunner returns deterministic adapter responses and records commands.
type fakeRunner struct {
	responses [][]byte
	calls     []string
}

// Run records one host terminal command and returns its next fixture.
func (r *fakeRunner) Run(_ context.Context, args []string, _ []byte) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(args, " "))
	if len(r.responses) == 0 {
		return []byte(`{"id":"ok"}`), nil
	}
	response := r.responses[0]
	r.responses = r.responses[1:]
	return response, nil
}

var _ terminal.CommandRunner = (*fakeRunner)(nil)
var _ terminal.TerminalRuntime = (*terminal.CmuxRuntime)(nil)
