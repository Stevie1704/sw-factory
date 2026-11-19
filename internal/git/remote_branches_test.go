package git_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
)

// lsRemoteRunner answers git ls-remote with fixed output and records the
// arguments.
type lsRemoteRunner struct {
	output string
	err    error
	args   [][]string
}

// Run returns the configured ls-remote output.
func (r *lsRemoteRunner) Run(_ context.Context, _ string, args []string) ([]byte, error) {
	r.args = append(r.args, args)
	return []byte(r.output), r.err
}

// TestRemoteBranchesListsTheBranchesBelowAPrefix verifies that the remote
// branch listing reads ls-remote of the prefix only and returns the branch
// names.
func TestRemoteBranchesListsTheBranchesBelowAPrefix(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("a", 40)
	runner := &lsRemoteRunner{output: head + "\trefs/heads/factory/run-2\n" + head + "\trefs/heads/factory/run-1\n"}
	manager := &gitadapter.LocalWorktreeManager{Runner: runner}
	branches, err := manager.RemoteBranches(context.Background(), "/work/service", "factory/")
	if err != nil || !slices.Equal(branches, []string{"factory/run-1", "factory/run-2"}) {
		t.Fatalf("RemoteBranches() = %v, %v; want both factory branches in order", branches, err)
	}
	if len(runner.args) != 1 || !slices.Contains(runner.args[0], "ls-remote") || !slices.Contains(runner.args[0], "refs/heads/factory/*") {
		t.Fatalf("git calls = %v, want one ls-remote of refs/heads/factory/*", runner.args)
	}
}

// TestRemoteBranchesReportsAnUnreadableRemote verifies that a failed
// listing is an error, never an empty success.
func TestRemoteBranchesReportsAnUnreadableRemote(t *testing.T) {
	t.Parallel()
	manager := &gitadapter.LocalWorktreeManager{Runner: &lsRemoteRunner{err: errors.New("remote unreachable")}}
	if _, err := manager.RemoteBranches(context.Background(), "/work/service", "factory/"); err == nil {
		t.Fatal("RemoteBranches() of an unreachable remote succeeded")
	}
}
