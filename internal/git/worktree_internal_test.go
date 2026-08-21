package git

import (
	"context"
	"reflect"
	"testing"
)

// TestInspectParsesNulDelimitedStatus verifies paths with spaces and arrow
// text remain intact while rename/copy source fields are ignored.
func TestInspectParsesNulDelimitedStatus(t *testing.T) {
	runner := &inspectRunner{status: " M internal/foo -> bar.go\x00?? internal/ümlaut.go\x00R  new name.go\x00old name.go\x00C  copy.go\x00source.go\x00"}
	state, err := (&LocalWorktreeManager{Runner: runner}).Inspect(context.Background(), "/worktree")
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	want := []string{"internal/foo -> bar.go", "internal/ümlaut.go", "new name.go", "copy.go"}
	if !reflect.DeepEqual(state.ChangedPaths, want) {
		t.Fatalf("ChangedPaths = %#v, want %#v", state.ChangedPaths, want)
	}
	if len(runner.calls) != 2 || runner.calls[1][0] != "status" || !containsArgument(runner.calls[1], "-z") {
		t.Fatalf("Git calls = %#v, want porcelain status with -z", runner.calls)
	}
}

// inspectRunner returns deterministic HEAD and status output for Inspect.
type inspectRunner struct {
	status string
	calls  [][]string
}

// Run records one Git command and returns its fixture output.
func (r *inspectRunner) Run(_ context.Context, _ string, args []string) ([]byte, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if args[0] == "rev-parse" {
		return []byte("0123456789abcdef\n"), nil
	}
	return []byte(r.status), nil
}

// containsArgument reports whether one Git argument is present.
func containsArgument(args []string, wanted string) bool {
	for _, argument := range args {
		if argument == wanted {
			return true
		}
	}
	return false
}
