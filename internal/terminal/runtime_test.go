package terminal_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/terminal"
)

// Adapter identities used by the fixtures below. The adapter is asked for
// UUID identifiers, so the fixtures use the identifier form the terminal
// actually returns.
const (
	controlWorkspaceID    = "03316ABB-2A2C-40F3-880C-857E74E1B3FC"
	runWorkspaceID        = "B57BB9B3-BD4D-439B-AD68-068AA1126809"
	implementationSurface = "10BB52EB-A479-4E0B-A198-8412FECE0F52"
	extraSurfaceID        = "85B1AEA6-DBAF-49EE-A88F-F806B8D172D9"
)

// treeResponse builds a topology fixture in the terminal's own response shape:
// windows own workspaces, workspaces own panes, and panes own titled surfaces.
func treeResponse(workspaceID string, titledSurfaces map[string]string) string {
	panes := make([]string, 0, len(titledSurfaces))
	for title, identifier := range titledSurfaces {
		panes = append(panes, `{"surfaces":[{"id":"`+identifier+`","title":"`+title+`","type":"terminal"}]}`)
	}
	return `{"windows":[{"workspaces":[{"id":"` + workspaceID + `","panes":[` + strings.Join(panes, ",") + `]}]}]}`
}

// TestCmuxRuntimeReadsSurfaceScrollback verifies the adapter asks the terminal
// for a surface's scrollback bounded to the requested line count. A failed
// launch is diagnosable only from this output, so the request must include the
// scrollback rather than just the visible screen.
func TestCmuxRuntimeReadsSurfaceScrollback(t *testing.T) {
	runner := &fakeRunner{tree: treeResponse(runWorkspaceID, map[string]string{"implementation": implementationSurface})}
	runtime := terminal.NewCmuxRuntime(runner)

	output, err := runtime.ReadSurface(context.Background(), implementationSurface, 200)
	if err != nil {
		t.Fatalf("ReadSurface() error = %v", err)
	}
	if strings.TrimSpace(output) != "OK" {
		t.Fatalf("ReadSurface() = %q, want the terminal output", output)
	}
	joined := strings.Join(runner.calls, "\n")
	for _, wanted := range []string{
		"read-screen --surface " + string(implementationSurface),
		"--workspace " + runWorkspaceID,
		"--scrollback",
		"--lines 200",
	} {
		if !strings.Contains(joined, wanted) {
			t.Fatalf("adapter calls = %q, want %q", joined, wanted)
		}
	}
}

// TestCmuxRuntimeRejectsAnUnboundedSurfaceRead verifies the adapter refuses a
// read without a line bound, so a diagnostic capture can never pull an
// arbitrarily large transcript.
func TestCmuxRuntimeRejectsAnUnboundedSurfaceRead(t *testing.T) {
	runtime := terminal.NewCmuxRuntime(&fakeRunner{})
	if _, err := runtime.ReadSurface(context.Background(), implementationSurface, 0); err == nil {
		t.Fatal("ReadSurface() error = nil, want a rejected unbounded read")
	}
}

// TestCmuxRuntimeKeepsTerminalHandlesBehindThePortableSeam verifies that the
// adapter creates the control/run surfaces and routes input and notifications
// without exposing adapter-specific command details to callers.
func TestCmuxRuntimeKeepsTerminalHandlesBehindThePortableSeam(t *testing.T) {
	runner := &fakeRunner{
		workspaceIDs: []string{controlWorkspaceID, runWorkspaceID},
		tree: treeResponse(runWorkspaceID, map[string]string{
			"implementation": implementationSurface,
			// Present because CreateSurface adds it to this workspace below.
			"extra": extraSurfaceID,
		}),
		newSurface: `{"pane_id":"C53FF016-4543-4BF4-8BD3-B104BCCA2EF8","surface_id":"` + extraSurfaceID + `","type":"terminal","workspace_id":"` + runWorkspaceID + `"}`,
	}
	runtime := terminal.NewCmuxRuntime(runner)
	control, err := runtime.EnsureControlWorkspace(context.Background(), terminal.WorkspaceRequest{
		Name:             "factory-control",
		Description:      "factory coordinator",
		WorkingDirectory: "/tmp/factory",
	})
	if err != nil {
		t.Fatalf("EnsureControlWorkspace() error = %v", err)
	}
	if control.ID != controlWorkspaceID {
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
	if run.ID != runWorkspaceID || run.Implementation.ID != implementationSurface || run.Status.ID != "" || run.Checks.ID != "" {
		t.Fatalf("run workspace = %#v, want only the implementation surface enabled", run)
	}
	extra, err := runtime.CreateSurface(context.Background(), terminal.SurfaceRequest{
		WorkspaceID: run.ID,
		Name:        "extra",
		Command:     terminal.Command{Executable: "factory-worker-attach", Args: []string{"--run-id", "run-1"}},
	})
	if err != nil {
		t.Fatalf("CreateSurface() error = %v", err)
	}
	if extra.ID != extraSurfaceID {
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
	for _, wanted := range []string{"workspace create", "--id-format uuids", "send --surface", "send-key --surface", "notify", "close-surface"} {
		if !strings.Contains(joined, wanted) {
			t.Fatalf("adapter calls = %q, want %q", joined, wanted)
		}
	}
	// The legacy workspace alias ignores the structured output flags, so the
	// adapter must never fall back to it.
	if strings.Contains(joined, "new-workspace") || strings.Contains(joined, "send-input") {
		t.Fatalf("adapter used a command the terminal does not support: %q", joined)
	}
	// A trailing newline must be pressed as Enter. Typing it together with the
	// text reads as a paste and leaves the input uncommitted.
	for _, call := range runner.calls {
		if strings.HasPrefix(call, "send --surface") && strings.HasSuffix(call, "\n") {
			t.Fatalf("adapter typed a trailing newline instead of pressing Enter: %q", call)
		}
	}
	if strings.Contains(joined, "docker.sock") {
		t.Fatalf("terminal adapter referenced the Docker socket: %q", joined)
	}
}

// TestCmuxRuntimeRejectsMalformedRunWorkspace verifies that a workspace
// without the required supervision surfaces cannot be accepted.
func TestCmuxRuntimeRejectsMalformedRunWorkspace(t *testing.T) {
	runner := &fakeRunner{
		workspaceIDs: []string{runWorkspaceID},
		tree:         treeResponse(runWorkspaceID, map[string]string{"unrelated": extraSurfaceID}),
	}
	_, err := terminal.NewCmuxRuntime(runner).EnsureRunWorkspace(context.Background(), terminal.RunWorkspaceRequest{RunID: "run-1", Name: "factory-run-1", WorkingDirectory: "/tmp/factory"})
	if err == nil || !strings.Contains(err.Error(), "implementation") {
		t.Fatalf("EnsureRunWorkspace() error = %v, want required surface error", err)
	}
}

// TestCmuxRuntimeReportsDeletedWorkspaceAsMissing verifies that cmux's
// specific missing-workspace response becomes a successful negative
// inspection so recovery can recreate the workspace.
func TestCmuxRuntimeReportsDeletedWorkspaceAsMissing(t *testing.T) {
	workspaceID := terminal.WorkspaceID(runWorkspaceID)
	runner := &fakeRunner{treeErr: errors.New("not_found: Workspace not found")}

	inspection, err := terminal.NewCmuxRuntime(runner).InspectWorkspace(context.Background(), workspaceID)
	if err != nil {
		t.Fatalf("InspectWorkspace() error = %v, want missing workspace inspection", err)
	}
	if inspection.Exists {
		t.Fatalf("InspectWorkspace() = %#v, want missing workspace", inspection)
	}
	if inspection.WorkspaceID != workspaceID {
		t.Fatalf("InspectWorkspace() workspace ID = %q, want %q", inspection.WorkspaceID, workspaceID)
	}
}

// TestCmuxRuntimePreservesOtherInspectionErrors verifies that only the
// specific missing-workspace response is converted into a negative inspection.
func TestCmuxRuntimePreservesOtherInspectionErrors(t *testing.T) {
	tests := []struct {
		name   string
		runner *fakeRunner
		want   string
	}{
		{
			name:   "cmux unreachable",
			runner: &fakeRunner{treeErr: errors.New("cmux socket unreachable")},
			want:   "cmux socket unreachable",
		},
		{
			name:   "malformed response",
			runner: &fakeRunner{tree: "{malformed"},
			want:   "decode terminal topology",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := terminal.NewCmuxRuntime(test.runner).InspectWorkspace(context.Background(), terminal.WorkspaceID(runWorkspaceID))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("InspectWorkspace() error = %v, want %q", err, test.want)
			}
		})
	}
}

// fakeRunner answers adapter commands the way the terminal does: structured
// JSON for creation and topology queries, and a plain acknowledgement line for
// every lifecycle command.
type fakeRunner struct {
	workspaceIDs []string
	tree         string
	// treeErr is returned when the fixture handles a topology query.
	treeErr    error
	newSurface string
	calls      []string
}

// Run records one host terminal command and returns its fixture.
func (r *fakeRunner) Run(_ context.Context, args []string, _ []byte) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(args, " "))
	switch {
	case len(args) > 1 && args[0] == "workspace" && args[1] == "create":
		identifier := runWorkspaceID
		if len(r.workspaceIDs) > 0 {
			identifier = r.workspaceIDs[0]
			r.workspaceIDs = r.workspaceIDs[1:]
		}
		return []byte(`{"group_id":null,"surface_id":"` + implementationSurface + `","window_id":"BE1B197B-CC74-4061-BD7D-E24C82705E08","workspace_id":"` + identifier + `"}`), nil
	case args[0] == "tree":
		if r.treeErr != nil {
			return nil, r.treeErr
		}
		return []byte(r.tree), nil
	case args[0] == "new-surface":
		return []byte(r.newSurface), nil
	default:
		return []byte("OK\n"), nil
	}
}

var _ terminal.CommandRunner = (*fakeRunner)(nil)
var _ terminal.TerminalRuntime = (*terminal.CmuxRuntime)(nil)
var _ terminal.SurfaceReader = (*terminal.CmuxRuntime)(nil)
