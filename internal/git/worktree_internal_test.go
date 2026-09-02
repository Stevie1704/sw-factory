package git

import (
	"context"
	"reflect"
	"testing"
)

// TestValidateRepositoryFilePathRejectsNonCanonicalPaths verifies repository
// file identities cannot vary through dot segments or repeated separators.
func TestValidateRepositoryFilePathRejectsNonCanonicalPaths(t *testing.T) {
	for _, path := range []string{
		"./docs/factory/craft/implementation.md",
		"docs//factory/craft/implementation.md",
		"docs/./factory/craft/implementation.md",
	} {
		t.Run(path, func(t *testing.T) {
			if err := validateRepositoryFilePath(path); err == nil {
				t.Fatalf("validateRepositoryFilePath(%q) error = nil, want non-canonical path rejection", path)
			}
		})
	}
}

// TestInspectParsesNulDelimitedStatus verifies paths with spaces and arrow
// text remain intact while rename/copy source fields are ignored. The rename
// and copy records mirror verified `git status --porcelain=v1 -z` output:
// destination path first, followed by the source path.
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
	if state.Branch != "factory/run-fixed" {
		t.Fatalf("Branch = %q, want factory/run-fixed", state.Branch)
	}
	if state.RepositoryPath != "/repo" {
		t.Fatalf("RepositoryPath = %q, want /repo", state.RepositoryPath)
	}
	if len(runner.calls) != 4 || runner.calls[3][0] != "status" || !containsArgument(runner.calls[3], "-z") {
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
	if args[0] == "rev-parse" && containsArgument(args, "--git-common-dir") {
		return []byte("/repo/.git\n"), nil
	}
	if args[0] == "branch" {
		return []byte("factory/run-fixed\n"), nil
	}
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
