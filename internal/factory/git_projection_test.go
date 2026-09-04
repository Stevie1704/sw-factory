package factory

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/worker"
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

	command := exec.CommandContext(t.Context(), "git", "diff", "--no-ext-diff", "--no-textconv", "--unified=80", baseSHA, checkpointSHA, "--", ".")
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

// TestPrepareGitMetadataProjectionRemovesDeletedLooseRef verifies that a
// refreshed projection does not leave a deleted source ref visible to commands
// run through the Docker worker adapter.
func TestPrepareGitMetadataProjectionRemovesDeletedLooseRef(t *testing.T) {
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	worktreePath := filepath.Join(root, "worktrees", "run-deleted-ref")
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
	runProjectionTestGit(t, repositoryPath, "update-ref", "refs/heads/deleted", "HEAD")
	runProjectionTestGit(t, repositoryPath, "worktree", "add", "-b", "factory/run-deleted-ref", worktreePath)

	projection, err := prepareGitMetadataProjection("run-deleted-ref", repositoryPath, worktreePath)
	if err != nil {
		t.Fatalf("prepare initial Git metadata projection: %v", err)
	}
	projectedRef := filepath.Join(projection, "refs", "heads", "deleted")
	if _, err := os.Stat(projectedRef); err != nil {
		t.Fatalf("inspect initially projected loose ref: %v", err)
	}

	runProjectionTestGit(t, repositoryPath, "update-ref", "-d", "refs/heads/deleted")
	if _, err := prepareGitMetadataProjection("run-deleted-ref", repositoryPath, worktreePath); err != nil {
		t.Fatalf("refresh Git metadata projection after ref deletion: %v", err)
	}

	runtime := &worker.DockerRuntime{DockerBinary: writeProjectionDockerStub(t, projection, worktreePath)}
	result, err := runtime.RunCommand(t.Context(), worker.CommandRequest{
		RunID:             "run-deleted-ref",
		Command:           "git show-ref --verify refs/heads/deleted",
		EnvironmentPolicy: worker.EnvironmentPolicyClean,
		Role:              "gate",
	})
	if err != nil {
		t.Fatalf("DockerRuntime.RunCommand() error = %v", err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("git show-ref verified deleted loose ref from refreshed projection: %#v", result)
	}
}

// runProjectionTestGit runs one isolated Git command with system and global
// configuration disabled so the regression exercises only the projected data.
func runProjectionTestGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.CommandContext(t.Context(), "git", arguments...)
	command.Dir = directory
	command.Env = projectionTestGitEnvironment()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

// writeProjectionDockerStub creates the narrow Docker CLI surface needed to
// run a worker command against host-side projection fixtures without a daemon.
func writeProjectionDockerStub(t *testing.T, projection, worktree string) string {
	t.Helper()
	stub := filepath.Join(t.TempDir(), "docker-stub")
	script := `#!/bin/sh
set -eu
if [ "${1:-}" = "container" ] && [ "${2:-}" = "inspect" ]; then
  printf 'true\tprojection-test-image\n'
  exit 0
fi
if [ "${1:-}" = "exec" ]; then
  while [ "$#" -gt 0 ] && [ "$1" != "/bin/sh" ]; do
    shift
  done
  if [ "$#" -lt 3 ] || [ "$2" != "-c" ]; then
    echo "malformed worker command" >&2
    exit 125
  fi
  shift 2
  GIT_DIR="$PROJECTION_TEST_GIT_DIR" \
  GIT_WORK_TREE="$PROJECTION_TEST_GIT_WORK_TREE" \
  GIT_CONFIG_NOSYSTEM=1 \
  GIT_CONFIG_GLOBAL=/dev/null \
  GIT_CONFIG_SYSTEM=/dev/null \
  exec /bin/sh -c "$1"
fi
echo "unsupported Docker invocation" >&2
exit 125
`
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROJECTION_TEST_GIT_DIR", projection)
	t.Setenv("PROJECTION_TEST_GIT_WORK_TREE", worktree)
	return stub
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
