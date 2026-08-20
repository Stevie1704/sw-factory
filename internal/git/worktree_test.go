package git_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
)

// TestLocalWorktreeManagerCreatesFromFetchedTargetWithoutChangingOrdinaryCheckout
// verifies the real Git adapter preserves the ordinary checkout.
func TestLocalWorktreeManagerCreatesFromFetchedTargetWithoutChangingOrdinaryCheckout(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	repository := filepath.Join(root, "project")
	remote := filepath.Join(root, "project-origin.git")
	runGit(t, root, "init", "--bare", remote)
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "init", "-b", "main")
	runGit(t, repository, "config", "user.email", "factory@example.test")
	runGit(t, repository, "config", "user.name", "Factory Test")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "README.md")
	runGit(t, repository, "commit", "-m", "initial")
	runGit(t, repository, "remote", "add", "origin", remote)
	runGit(t, repository, "push", "origin", "main")

	beforeBranch := strings.TrimSpace(runGit(t, repository, "branch", "--show-current"))
	beforeStatus := runGit(t, repository, "status", "--porcelain")
	manager := &gitadapter.LocalWorktreeManager{WorktreeDir: filepath.Join(root, "worktrees")}
	workspace, err := manager.Create(context.Background(), repository, "main", "run-fixed")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	wantBaseSHA := strings.TrimSpace(runGit(t, repository, "rev-parse", "HEAD"))
	if workspace.BaseSHA != wantBaseSHA {
		t.Fatalf("BaseSHA = %q, want fetched target SHA %q", workspace.BaseSHA, wantBaseSHA)
	}
	if workspace.Branch != "factory/run-fixed" {
		t.Fatalf("Branch = %q, want factory/run-fixed", workspace.Branch)
	}
	wantWorktree := filepath.Join(root, "worktrees", "run-fixed")
	if workspace.Worktree != wantWorktree {
		t.Fatalf("Worktree = %q, want %q", workspace.Worktree, wantWorktree)
	}
	if got := strings.TrimSpace(runGit(t, repository, "branch", "--show-current")); got != beforeBranch {
		t.Fatalf("ordinary checkout branch = %q, want unchanged %q", got, beforeBranch)
	}
	if got := runGit(t, repository, "status", "--porcelain"); got != beforeStatus {
		t.Fatalf("ordinary checkout status = %q, want unchanged %q", got, beforeStatus)
	}
	if got := strings.TrimSpace(runGit(t, workspace.Worktree, "branch", "--show-current")); got != workspace.Branch {
		t.Fatalf("worktree branch = %q, want %q", got, workspace.Branch)
	}
	contents, err := os.ReadFile(filepath.Join(workspace.Worktree, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "base\n" {
		t.Fatalf("worktree README = %q, want target branch contents", contents)
	}
}

// runGit executes one Git command in a temporary test repository.
func runGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}
