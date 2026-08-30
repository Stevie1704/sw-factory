package factory

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPrepareGitMetadataProjectionRefreshesCheckpointObjects verifies a
// checkpoint committed after projection creation is available to the worker's
// exact review diff command.
func TestPrepareGitMetadataProjectionRefreshesCheckpointObjects(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	worktreePath := filepath.Join(root, "worktrees", "run-refresh")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}

	runProjectionTestGit(t, repositoryPath, "init", "-b", "main")
	runProjectionTestGit(t, repositoryPath, "config", "user.email", "factory@example.test")
	runProjectionTestGit(t, repositoryPath, "config", "user.name", "Factory Test")
	if err := os.WriteFile(filepath.Join(repositoryPath, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runProjectionTestGit(t, repositoryPath, "add", "README.md")
	runProjectionTestGit(t, repositoryPath, "commit", "-m", "base")
	baseSHA := runProjectionTestGit(t, repositoryPath, "rev-parse", "HEAD")
	runProjectionTestGit(t, repositoryPath, "worktree", "add", "-b", "factory/run-refresh", worktreePath)

	projection, err := prepareGitMetadataProjection("run-refresh", repositoryPath, worktreePath)
	if err != nil {
		t.Fatalf("prepare initial Git metadata projection: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repositoryPath, ".git", "remotes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryPath, ".git", "remotes", "origin"), []byte("URL https://user:secret@example.invalid/project.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(worktreePath, "checkpoint.txt"), []byte("checkpoint\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runProjectionTestGit(t, worktreePath, "add", "checkpoint.txt")
	runProjectionTestGit(t, worktreePath, "commit", "-m", "checkpoint")
	checkpointSHA := runProjectionTestGit(t, worktreePath, "rev-parse", "HEAD")

	refreshedProjection, err := prepareGitMetadataProjection("run-refresh", repositoryPath, worktreePath)
	if err != nil {
		t.Fatalf("refresh Git metadata projection: %v", err)
	}
	if refreshedProjection != projection {
		t.Fatalf("refreshed projection = %q, want stable path %q", refreshedProjection, projection)
	}
	for _, forbidden := range []string{"config", "remotes"} {
		if _, err := os.Stat(filepath.Join(refreshedProjection, forbidden)); !os.IsNotExist(err) {
			t.Fatalf("refreshed projection contains forbidden %q: %v", forbidden, err)
		}
	}
	repeatedProjection, err := prepareGitMetadataProjection("run-refresh", repositoryPath, worktreePath)
	if err != nil {
		t.Fatalf("repeat Git metadata projection refresh: %v", err)
	}
	if repeatedProjection != projection {
		t.Fatalf("repeated projection = %q, want stable path %q", repeatedProjection, projection)
	}

	command := exec.Command("git", "diff", "--no-ext-diff", "--binary", "--unified=80", baseSHA, checkpointSHA, "--", ".")
	command.Dir = worktreePath
	command.Env = projectionTestGitEnvironment(
		"GIT_DIR="+refreshedProjection,
		"GIT_WORK_TREE="+worktreePath,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("worker-style review diff: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "diff --git a/checkpoint.txt b/checkpoint.txt") {
		t.Fatalf("worker-style review diff = %q, want checkpoint change", output)
	}
}

// runProjectionTestGit runs one isolated Git command with system and global
// configuration disabled so the regression exercises only the projected data.
func runProjectionTestGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	command.Env = projectionTestGitEnvironment()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

// projectionTestGitEnvironment returns the isolated Git environment used by
// both repository setup commands and the worker-style review command.
func projectionTestGitEnvironment(extra ...string) []string {
	return append(append([]string(nil), os.Environ()...), append([]string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	}, extra...)...)
}
