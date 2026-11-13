package git_test

import (
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExactWorkspaceAdoptsAnAlreadyCreatedClaim checks response-loss recovery
// without a remote fetch or a second branch/worktree.
func TestExactWorkspaceAdoptsAnAlreadyCreatedClaim(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "init", "-b", "main")
	runGit(t, repo, "config", "user.email", "test@example.test")
	runGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("accepted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "base")
	base := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	manager := &gitadapter.LocalWorktreeManager{WorktreeDir: filepath.Join(root, "worktrees")}
	first, err := manager.CreateAtRevision(t.Context(), repo, base, "local-claim")
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.CreateAtRevision(t.Context(), repo, base, "local-claim")
	if err != nil || first != second {
		t.Fatalf("adoption=%#v,%v; original=%#v", second, err, first)
	}
	if err := os.WriteFile(filepath.Join(first.Worktree, "file.txt"), []byte("unexpected"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CreateAtRevision(t.Context(), repo, base, "local-claim"); err == nil {
		t.Fatal("adopted modified unclaimed workspace")
	}
}
