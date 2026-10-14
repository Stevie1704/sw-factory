package gateproof

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
)

// checkpointIdentity is the fixed author, committer, and date of a proof
// checkpoint, so the commit depends only on its tree and its parent.
var checkpointIdentity = []string{
	"GIT_AUTHOR_NAME=Software Factory", "GIT_AUTHOR_EMAIL=factory@localhost", "GIT_AUTHOR_DATE=1970-01-01T00:00:00Z",
	"GIT_COMMITTER_NAME=Software Factory", "GIT_COMMITTER_EMAIL=factory@localhost", "GIT_COMMITTER_DATE=1970-01-01T00:00:00Z",
}

// checkpointCandidate builds the Git metadata of a proof in gitDir and
// returns its HEAD commit. It copies the repository's metadata the way a run
// projects it, without configuration, hooks, or remotes, keeps the HEAD of a
// linked worktree, and commits the
// projected candidate files in work on top of HEAD. A worker thus sees the
// candidate as a clean checkpoint with the real history. Host Git
// configuration is not read, so no host-defined filter runs on candidate
// files, and the checkout is only read.
func checkpointCandidate(ctx context.Context, repository, work, gitDir string) (string, error) {
	common, err := hostValue(ctx, repository, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if err := gitadapter.CopyGitMetadata(common, gitDir); err != nil {
		return "", fmt.Errorf("copy the Git metadata: %w", err)
	}
	// A linked worktree keeps its own HEAD in its admin directory; the copy
	// of the common directory holds the HEAD of the primary checkout.
	private, err := hostValue(ctx, repository, nil, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return "", err
	}
	if private != common {
		if err := gitadapter.CopyGitMetadataFile(filepath.Join(private, "HEAD"), filepath.Join(gitDir, "HEAD"), 0o640); err != nil {
			return "", fmt.Errorf("copy the worktree HEAD: %w", err)
		}
	}
	// The copied index describes the checkout, not the candidate.
	if err := os.Remove(filepath.Join(gitDir, "index")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("remove the copied Git index: %w", err)
	}
	environment := append([]string{"GIT_DIR=" + gitDir, "GIT_WORK_TREE=" + work, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}, checkpointIdentity...)
	if _, err := hostGit(ctx, work, environment, "add", "--all", "--force"); err != nil {
		return "", err
	}
	tree, err := hostValue(ctx, work, environment, "write-tree")
	if err != nil {
		return "", err
	}
	commitArgs := []string{"commit-tree", tree, "-m", "factory onboarding proof"}
	if parent, err := hostValue(ctx, work, environment, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err == nil {
		commitArgs = append(commitArgs, "-p", parent)
	}
	commit, err := hostValue(ctx, work, environment, commitArgs...)
	if err != nil {
		return "", err
	}
	if _, err := hostGit(ctx, work, environment, "update-ref", "HEAD", commit); err != nil {
		return "", err
	}
	if err := gitadapter.MakeGitProjectionReadable(gitDir); err != nil {
		return "", fmt.Errorf("prepare the Git metadata permissions: %w", err)
	}
	return commit, nil
}

// hostGit runs one host Git command under the factory Git policy and returns
// its standard output.
func hostGit(ctx context.Context, directory string, environment []string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	command := exec.CommandContext(ctx, "git", gitadapter.HostCommandArgs(args...)...)
	command.Dir = directory
	if environment != nil {
		command.Env = append(os.Environ(), environment...)
	}
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// hostValue runs one host Git command that prints one value and returns the
// value without surrounding white space.
func hostValue(ctx context.Context, directory string, environment []string, args ...string) (string, error) {
	output, err := hostGit(ctx, directory, environment, args...)
	return strings.TrimSpace(output), err
}
