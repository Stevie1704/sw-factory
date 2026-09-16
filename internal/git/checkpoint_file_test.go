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

// checkpointRepository builds a repository whose HEAD has moved past an
// earlier commit, so a read can be proven to come from the requested
// checkpoint rather than from the working tree.
func checkpointRepository(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = root
		command.Env = append(os.Environ(), "GIT_AUTHOR_NAME=factory", "GIT_AUTHOR_EMAIL=factory@example.com", "GIT_COMMITTER_NAME=factory", "GIT_COMMITTER_EMAIL=factory@example.com")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
		}
		return strings.TrimSpace(string(output))
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "--initial-branch=main", ".")
	write("python/pyproject.toml", "[project]\nname = \"sil\"\n")
	run("add", "-A")
	run("commit", "-qm", "base")
	base := run("rev-parse", "HEAD")
	write("python/pyproject.toml", "[project]\nname = \"sil\"\n\n[project.scripts]\nsil-acc = \"sil.examples.acc:main\"\n")
	if err := os.Symlink("python/pyproject.toml", filepath.Join(root, "linked.toml")); err != nil {
		t.Fatal(err)
	}
	write("ignored.toml", "[project]\nname = \"ignored\"\n")
	write(".gitignore", "ignored.toml\n")
	run("add", "-A")
	run("commit", "-qm", "implementation checkpoint")
	return root, base
}

// TestReadFileAtCheckpointReadsTheCommittedContentNotTheWorkingTree verifies
// the production adapter serves the bytes committed at the requested
// checkpoint after later commits changed the same file.
func TestReadFileAtCheckpointReadsTheCommittedContentNotTheWorkingTree(t *testing.T) {
	root, base := checkpointRepository(t)

	content, err := gitadapter.NewGitWorkspace().ReadFileAtCheckpoint(context.Background(), root, base, "python/pyproject.toml")
	if err != nil {
		t.Fatalf("ReadFileAtCheckpoint() error = %v", err)
	}
	if got := string(content); got != "[project]\nname = \"sil\"\n" {
		t.Fatalf("ReadFileAtCheckpoint() = %q, want the content committed at the base checkpoint", got)
	}
}

// TestReadFileAtCheckpointRefusesASymbolicLink verifies a link is refused
// rather than served as its target path. Git stores a link as a blob, so the
// tree entry mode is the only thing that tells the two apart.
func TestReadFileAtCheckpointRefusesASymbolicLink(t *testing.T) {
	root, _ := checkpointRepository(t)
	head := "HEAD"

	_, err := gitadapter.NewGitWorkspace().ReadFileAtCheckpoint(context.Background(), root, head, "linked.toml")
	if err == nil || !strings.Contains(err.Error(), "is not a regular file tracked at checkpoint") {
		t.Fatalf("ReadFileAtCheckpoint() error = %v, want a refusal for a symbolic link", err)
	}
}

// TestReadFileAtCheckpointRefusesAnIgnoredFile verifies a path the checkpoint
// does not track fails here instead of falling back to the working tree.
func TestReadFileAtCheckpointRefusesAnIgnoredFile(t *testing.T) {
	root, _ := checkpointRepository(t)

	_, err := gitadapter.NewGitWorkspace().ReadFileAtCheckpoint(context.Background(), root, "HEAD", "ignored.toml")
	if err == nil || !strings.Contains(err.Error(), "is not a regular file tracked at checkpoint") {
		t.Fatalf("ReadFileAtCheckpoint() error = %v, want a refusal for an untracked file", err)
	}
}
