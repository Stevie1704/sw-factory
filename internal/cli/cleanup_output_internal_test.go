package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/factory"
)

// TestWriteRetainedWorkspacesNamesEveryWorkspace verifies the operator-facing
// line for a workspace cleanup could not close. Cleanup deletes the only
// durable record of the handle, so this line is the operator's sole notice
// that manual closure is required.
func TestWriteRetainedWorkspacesNamesEveryWorkspace(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	if !writeRetainedWorkspaces(&output, &output, []factory.CleanupRetainedWorkspace{
		{RunID: "run-a", WorkspaceID: "workspace-a", Reason: "terminal server is not running"},
		{RunID: "run-b", WorkspaceID: "workspace-b", Reason: "connection refused"},
	}) {
		t.Fatal("writeRetainedWorkspaces() = false, want a successful write")
	}
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("retained workspace output = %q, want one line per workspace", output.String())
	}
	for index, wanted := range []string{
		"cleanup retained terminal workspace: workspace-a run=run-a reason=terminal server is not running (close it manually)",
		"cleanup retained terminal workspace: workspace-b run=run-b reason=connection refused (close it manually)",
	} {
		if lines[index] != wanted {
			t.Fatalf("retained workspace line %d = %q, want %q", index, lines[index], wanted)
		}
	}
}

// TestWriteRetainedWorkspacesReportsAWriteFailure verifies the helper reports a
// failed write instead of losing the notice silently.
func TestWriteRetainedWorkspacesReportsAWriteFailure(t *testing.T) {
	t.Parallel()

	if writeRetainedWorkspaces(failingWriter{}, &bytes.Buffer{}, []factory.CleanupRetainedWorkspace{
		{RunID: "run-a", WorkspaceID: "workspace-a", Reason: "connection refused"},
	}) {
		t.Fatal("writeRetainedWorkspaces() = true, want a failed write to be reported")
	}
}

// failingWriter rejects every write so the helper's error path is observable.
type failingWriter struct{}

// Write always fails.
func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed pipe") }
