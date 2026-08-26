package git_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/doctor"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
)

// TestLocalWorktreeManagerChecksTheConfiguredRemoteAndTargetBranch verifies
// diagnosis checks the checkout identity before probing the remote branch.
func TestLocalWorktreeManagerChecksTheConfiguredRemoteAndTargetBranch(t *testing.T) {
	repositoryPath := t.TempDir()
	runner := &gitDoctorRunner{repositoryPath: repositoryPath}
	manager := &gitadapter.LocalWorktreeManager{Runner: runner}
	err := manager.CheckRemote(context.Background(), gitadapter.DoctorRequest{
		RepositoryPath:     repositoryPath,
		RemoteName:         "origin",
		ExpectedOwner:      "example",
		ExpectedRepository: "project",
		TargetBranch:       "main",
	})
	if err != nil {
		t.Fatalf("CheckRemote() error = %v", err)
	}
	joined := strings.Join(runner.commands, "\n")
	for _, want := range []string{"rev-parse --show-toplevel", "remote get-url origin", "remote get-url --push origin", "ls-remote --exit-code origin refs/heads/main"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Git commands = %q, want %q", joined, want)
		}
	}
}

// TestLocalWorktreeManagerChecksHooksAndWorktreeSupport verifies the two
// repository-local capability checks use read-only Git observations.
func TestLocalWorktreeManagerChecksHooksAndWorktreeSupport(t *testing.T) {
	repositoryPath := t.TempDir()
	hooksPath := filepath.Join(repositoryPath, ".git", "hooks")
	if err := os.MkdirAll(hooksPath, 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &gitDoctorRunner{repositoryPath: repositoryPath, hooksPath: hooksPath}
	manager := &gitadapter.LocalWorktreeManager{Runner: runner}
	request := gitadapter.DoctorRequest{RepositoryPath: repositoryPath}
	if err := manager.CheckHooks(context.Background(), request); err != nil {
		t.Fatalf("CheckHooks() error = %v", err)
	}
	if err := manager.CheckWorktree(context.Background(), request); err != nil {
		t.Fatalf("CheckWorktree() error = %v", err)
	}
	joined := strings.Join(runner.commands, "\n")
	if !strings.Contains(joined, "rev-parse --git-path hooks") || !strings.Contains(joined, "worktree list --porcelain") {
		t.Fatalf("Git commands = %q, want hooks and worktree checks", joined)
	}
}

// TestStartupChecksKeepGitFailuresIndependent verifies one failed Git check
// does not suppress the other Git-owned diagnostics.
func TestStartupChecksKeepGitFailuresIndependent(t *testing.T) {
	checker := &fakeGitDoctorChecker{remoteErr: errors.New("remote unavailable")}
	report := doctor.Run(context.Background(), gitadapter.StartupChecks(checker, gitadapter.DoctorRequest{})...)
	if report.Ready() || len(report.Failures()) != 1 {
		t.Fatalf("Git report = %#v, want one failure", report)
	}
	if len(report.Results) != 3 || checker.calls != 3 {
		t.Fatalf("Git results/calls = %d/%d, want three", len(report.Results), checker.calls)
	}
}

// gitDoctorRunner returns deterministic observations for Git diagnosis.
type gitDoctorRunner struct {
	repositoryPath string
	hooksPath      string
	commands       []string
}

// Run records one host-side Git command and returns its safe fixture output.
func (r *gitDoctorRunner) Run(_ context.Context, _ string, args []string) ([]byte, error) {
	r.commands = append(r.commands, strings.Join(args, " "))
	switch {
	case len(args) == 2 && args[0] == "rev-parse" && args[1] == "--show-toplevel":
		return []byte(r.repositoryPath + "\n"), nil
	case len(args) == 2 && args[0] == "rev-parse" && args[1] == "--is-inside-work-tree":
		return []byte("true\n"), nil
	case len(args) == 3 && args[0] == "remote":
		return []byte("git@github.com:example/project.git\n"), nil
	case len(args) == 4 && args[0] == "remote" && args[2] == "--push":
		return []byte("git@github.com:example/project.git\n"), nil
	case len(args) > 0 && args[0] == "ls-remote":
		return []byte("0123456789abcdef0123456789abcdef01234567\trefs/heads/main\n"), nil
	case len(args) == 3 && args[0] == "rev-parse" && args[1] == "--git-path":
		return []byte(r.hooksPath + "\n"), nil
	default:
		return []byte("worktree\n"), nil
	}
}

// fakeGitDoctorChecker records the three Git-owned diagnosis calls.
type fakeGitDoctorChecker struct {
	remoteErr error
	calls     int
}

// CheckRemote implements the Git diagnosis seam for composition tests.
func (f *fakeGitDoctorChecker) CheckRemote(context.Context, gitadapter.DoctorRequest) error {
	f.calls++
	return f.remoteErr
}

// CheckHooks implements the Git diagnosis seam for composition tests.
func (f *fakeGitDoctorChecker) CheckHooks(context.Context, gitadapter.DoctorRequest) error {
	f.calls++
	return nil
}

// CheckWorktree implements the Git diagnosis seam for composition tests.
func (f *fakeGitDoctorChecker) CheckWorktree(context.Context, gitadapter.DoctorRequest) error {
	f.calls++
	return nil
}

var _ gitadapter.CommandRunner = (*gitDoctorRunner)(nil)
var _ gitadapter.DoctorChecker = (*fakeGitDoctorChecker)(nil)
