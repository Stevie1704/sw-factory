package git_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
)

// TestContextSnapshotIsADetachedCheckoutOfTheFetchedTarget verifies that a
// session context snapshot checks out the fetched target commit detached,
// creates no branch, leaves the user's checkout and its uncommitted work
// unchanged, and is removed idempotently.
func TestContextSnapshotIsADetachedCheckoutOfTheFetchedTarget(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	repository := filepath.Join(root, "project")
	initializeGitRepository(t, repository, filepath.Join(root, "project-origin.git"))
	target := strings.TrimSpace(runGit(t, repository, "rev-parse", "HEAD"))
	runGit(t, repository, "checkout", "-b", "user-work")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("uncommitted user edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeBranches := runGit(t, repository, "branch", "--list")
	beforeStatus := runGit(t, repository, "status", "--porcelain")

	manager := &gitadapter.LocalWorktreeManager{WorktreeDir: filepath.Join(root, "worktrees")}
	snapshot, err := manager.CreateContextSnapshot(context.Background(), repository, "main", "sinv-1")
	if err != nil {
		t.Fatalf("CreateContextSnapshot() error = %v", err)
	}
	if snapshot.Revision != target || filepath.Base(snapshot.Worktree) != "sinv-1" {
		t.Fatalf("snapshot = %#v, want target %s in a directory named for the invocation", snapshot, target)
	}
	if branch := strings.TrimSpace(runGit(t, snapshot.Worktree, "branch", "--show-current")); branch != "" {
		t.Fatalf("snapshot branch = %q, want a detached checkout", branch)
	}
	if content, err := os.ReadFile(filepath.Join(snapshot.Worktree, "README.md")); err != nil || string(content) != "base\n" {
		t.Fatalf("snapshot README = %q, %v; want the committed target content", content, err)
	}
	if runGit(t, repository, "branch", "--list") != beforeBranches || runGit(t, repository, "status", "--porcelain") != beforeStatus {
		t.Fatal("creating a snapshot changed the user's branches or working tree")
	}

	for range 2 {
		if err := manager.RemoveContextSnapshot(context.Background(), repository, snapshot); err != nil {
			t.Fatalf("RemoveContextSnapshot() error = %v", err)
		}
	}
	if _, err := os.Stat(snapshot.Worktree); !os.IsNotExist(err) {
		t.Fatalf("snapshot directory still exists: %v", err)
	}
	if runGit(t, repository, "status", "--porcelain") != beforeStatus {
		t.Fatal("removing a snapshot changed the user's working tree")
	}
}
