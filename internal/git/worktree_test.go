package git_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if err := os.WriteFile(filepath.Join(repository, "AGENTS.md"), []byte("base agent guidance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "CONTEXT.md"), []byte("base context\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repository, "docs", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "docs", "agents", "conventions.md"), []byte("base conventions\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repository, "docs", "factory", "craft"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "docs", "factory", "craft", "implementation.md"), []byte("base implementation craft\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "README.md", "AGENTS.md", "CONTEXT.md", "docs/agents/conventions.md", "docs/factory/craft/implementation.md")
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
	if workspace.RunID != "run-fixed" {
		t.Fatalf("RunID = %q, want run-fixed", workspace.RunID)
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
	if err := os.WriteFile(filepath.Join(workspace.Worktree, "AGENTS.md"), []byte("mutable worker edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Worktree, "docs", "factory", "craft", "implementation.md"), []byte("mutable worker craft\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	craft, err := manager.ReadRepositoryCraft(context.Background(), workspace.Worktree, workspace.BaseSHA, "docs/factory/craft/implementation.md")
	if err != nil {
		t.Fatalf("ReadRepositoryCraft() error = %v", err)
	}
	if craft.Path != "docs/factory/craft/implementation.md" || craft.Content != "base implementation craft\n" {
		t.Fatalf("craft document = %#v, want immutable base content", craft)
	}
	documents, err := manager.ReadRepositoryGuidance(context.Background(), workspace.Worktree, workspace.BaseSHA)
	if err != nil {
		t.Fatalf("ReadRepositoryGuidance() error = %v", err)
	}
	if len(documents) != 3 {
		t.Fatalf("guidance documents = %#v, want three tracked guidance files", documents)
	}
	for index, want := range []struct{ path, content string }{
		{path: "AGENTS.md", content: "base agent guidance\n"},
		{path: "CONTEXT.md", content: "base context\n"},
		{path: "docs/agents/conventions.md", content: "base conventions\n"},
	} {
		if documents[index].Path != want.path || documents[index].Content != want.content {
			t.Errorf("guidance[%d] = %#v, want %q with %q", index, documents[index], want.path, want.content)
		}
	}
	gitMetadataProjection := filepath.Join(root, "worktrees", ".factory-git", "run-fixed")
	if err := os.MkdirAll(gitMetadataProjection, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := manager.Remove(context.Background(), repository, workspace); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := os.Stat(workspace.Worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree path still exists after Remove(): err = %v", err)
	}
	if _, err := os.Stat(gitMetadataProjection); !os.IsNotExist(err) {
		t.Fatalf("Git metadata projection still exists after Remove(): err = %v", err)
	}
	if got := strings.TrimSpace(runGit(t, repository, "branch", "--list", workspace.Branch)); got != "" {
		t.Fatalf("run branch = %q, want branch removed", got)
	}
}

// TestLocalGitWorkspaceMergesAnAdvancedTargetIntoTheRunBranch verifies the
// readiness synchronization handles divergent commits with a regular merge.
func TestLocalGitWorkspaceMergesAnAdvancedTargetIntoTheRunBranch(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	repository := filepath.Join(root, "project")
	remote := filepath.Join(root, "project-origin.git")
	initializeGitRepository(t, repository, remote)
	manager := &gitadapter.LocalWorktreeManager{WorktreeDir: filepath.Join(root, "worktrees")}
	workspace, err := manager.Create(context.Background(), repository, "main", "run-sync")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Worktree, "implementation.txt"), []byte("implementation\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workspace.Worktree, "add", "implementation.txt")
	runGit(t, workspace.Worktree, "commit", "-m", "implementation")

	if err := os.WriteFile(filepath.Join(repository, "target.txt"), []byte("target update\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "target.txt")
	runGit(t, repository, "commit", "-m", "advance target")
	runGit(t, repository, "push", "origin", "main")

	if err := manager.SynchronizeBase(context.Background(), gitadapter.BaseSyncRequest{
		WorktreePath: workspace.Worktree,
		TargetBranch: "main",
	}); err != nil {
		t.Fatalf("SynchronizeBase() error = %v", err)
	}
	parents := strings.Fields(strings.TrimSpace(runGit(t, workspace.Worktree, "rev-list", "--parents", "-n", "1", "HEAD")))
	if len(parents) != 3 {
		t.Fatalf("merged HEAD parents = %v, want merge commit with two parents", parents)
	}
	if got := strings.TrimSpace(runGit(t, workspace.Worktree, "branch", "--show-current")); got != workspace.Branch {
		t.Fatalf("run branch = %q, want unchanged %q", got, workspace.Branch)
	}
	for name, want := range map[string]string{"implementation.txt": "implementation\n", "target.txt": "target update\n"} {
		contents, readErr := os.ReadFile(filepath.Join(workspace.Worktree, name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(contents) != want {
			t.Errorf("%s = %q, want %q", name, contents, want)
		}
	}
	if err := manager.Remove(context.Background(), repository, workspace); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
}

// TestLocalWorktreeManagerCreatesAScopedTestCheckpoint verifies the test
// checkpoint cannot accidentally commit an implementation path and carries a
// distinct marker from the later implementation checkpoint.
func TestLocalWorktreeManagerCreatesAScopedTestCheckpoint(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	repository := filepath.Join(root, "project")
	remote := filepath.Join(root, "project-origin.git")
	initializeGitRepository(t, repository, remote)
	manager := &gitadapter.LocalWorktreeManager{WorktreeDir: filepath.Join(root, "worktrees")}
	workspace, err := manager.Create(context.Background(), repository, "main", "run-test-checkpoint")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspace.Worktree, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Worktree, "tests", "behavior_test.go"), []byte("package tests\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Worktree, "implementation.txt"), []byte("must remain uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := manager.CreateCheckpoint(context.Background(), gitadapter.CheckpointRequest{
		RunID:        "run-test-checkpoint",
		WorktreePath: workspace.Worktree,
		ParentSHA:    workspace.BaseSHA,
		Kind:         gitadapter.CheckpointKindTest,
		Paths:        []string{"tests/behavior_test.go"},
		Message:      "verified the behavior is red",
	})
	if err != nil {
		t.Fatalf("CreateCheckpoint() error = %v", err)
	}
	if !checkpoint.Created || !validCommitSHA(checkpoint.SHA) {
		t.Fatalf("checkpoint = %#v, want new test checkpoint", checkpoint)
	}
	if got := strings.TrimSpace(runGit(t, workspace.Worktree, "log", "-1", "--format=%B")); got != "factory: test checkpoint run-test-checkpoint\n\nverified the behavior is red" {
		t.Fatalf("checkpoint message = %q, want test marker", got)
	}
	if got := strings.TrimSpace(runGit(t, workspace.Worktree, "status", "--porcelain")); got != "?? implementation.txt" {
		t.Fatalf("checkpoint status = %q, want implementation path left uncommitted", got)
	}
	if err := manager.Remove(context.Background(), repository, workspace); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
}

// TestLocalGitWorkspaceCreatesOneImplementationCheckpoint verifies the host
// Git seam commits the worktree's changes and returns the resulting full SHA.
func TestLocalGitWorkspaceCreatesOneImplementationCheckpoint(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	repository := filepath.Join(root, "project")
	remote := filepath.Join(root, "project-origin.git")
	initializeGitRepository(t, repository, remote)
	manager := &gitadapter.LocalWorktreeManager{WorktreeDir: filepath.Join(root, "worktrees")}
	workspace, err := manager.Create(context.Background(), repository, "main", "run-checkpoint")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Worktree, "implementation.txt"), []byte("implemented\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	checkpoint, err := manager.CreateCheckpoint(context.Background(), gitadapter.CheckpointRequest{
		RunID:        "run-checkpoint",
		WorktreePath: workspace.Worktree,
		ParentSHA:    workspace.BaseSHA,
		Message:      "implementation checkpoint",
	})
	if err != nil {
		t.Fatalf("CreateCheckpoint() error = %v", err)
	}
	if !validCommitSHA(checkpoint.SHA) || !checkpoint.Created {
		t.Fatalf("checkpoint = %#v, want a new full-SHA checkpoint", checkpoint)
	}
	if got := strings.TrimSpace(runGit(t, workspace.Worktree, "log", "-1", "--format=%B")); got != "factory: implementation checkpoint run-checkpoint\n\nimplementation checkpoint" {
		t.Fatalf("checkpoint message = %q, want factory marker and supplied message", got)
	}

	repeated, err := manager.CreateCheckpoint(context.Background(), gitadapter.CheckpointRequest{
		RunID:        "run-checkpoint",
		WorktreePath: workspace.Worktree,
		ParentSHA:    workspace.BaseSHA,
		Message:      "implementation checkpoint",
	})
	if err != nil {
		t.Fatalf("repeated CreateCheckpoint() error = %v", err)
	}
	if repeated.SHA != checkpoint.SHA || repeated.Created {
		t.Fatalf("repeated checkpoint = %#v, want the original SHA without a second commit", repeated)
	}
	if got := strings.TrimSpace(runGit(t, workspace.Worktree, "rev-list", "--count", "HEAD")); got != "2" {
		t.Fatalf("commit count = %q, want one implementation commit", got)
	}

	if err := manager.Remove(context.Background(), repository, workspace); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
}

// TestLocalGitWorkspacePushesOnlyTheNamedRunBranch verifies the push command
// is host-side, non-forcing, and targets the run branch explicitly.
func TestLocalGitWorkspacePushesOnlyTheNamedRunBranch(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	repository := filepath.Join(root, "project")
	remote := filepath.Join(root, "project-origin.git")
	initializeGitRepository(t, repository, remote)
	manager := &gitadapter.LocalWorktreeManager{WorktreeDir: filepath.Join(root, "worktrees")}
	workspace, err := manager.Create(context.Background(), repository, "main", "run-push")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Worktree, "implementation.txt"), []byte("implemented\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CreateCheckpoint(context.Background(), gitadapter.CheckpointRequest{
		RunID: "run-push", WorktreePath: workspace.Worktree, ParentSHA: workspace.BaseSHA,
	}); err != nil {
		t.Fatalf("CreateCheckpoint() error = %v", err)
	}
	if err := manager.Push(context.Background(), gitadapter.PushRequest{WorktreePath: workspace.Worktree, Branch: workspace.Branch}); err != nil {
		t.Fatalf("Push() error = %v", err)
	}
	if got := strings.TrimSpace(runGit(t, root, "--git-dir", remote, "rev-parse", "refs/heads/"+workspace.Branch)); got != strings.TrimSpace(runGit(t, workspace.Worktree, "rev-parse", "HEAD")) {
		t.Fatalf("remote run branch = %q, want worktree HEAD", got)
	}

	if err := manager.Remove(context.Background(), repository, workspace); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
}

// initializeGitRepository creates a disposable repository with an origin and
// a configured commit identity for adapter integration tests.
func initializeGitRepository(t *testing.T, repository, remote string) {
	t.Helper()
	runGit(t, filepath.Dir(repository), "init", "--bare", remote)
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
}

// validCommitSHA reports whether a test value is a full Git object identity.
func validCommitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		if (character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') {
			continue
		}
		return false
	}
	return true
}

// runGit executes one Git command in a temporary test repository.
func runGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}
