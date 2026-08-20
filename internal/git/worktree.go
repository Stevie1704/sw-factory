// Package git contains host-side Git and worktree adapters.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Workspace identifies the fetched base commit and isolated checkout created
// for one run.
type Workspace struct {
	BaseSHA  string
	Branch   string
	Worktree string
}

// WorktreeManager creates an isolated branch and worktree from a target branch.
type WorktreeManager interface {
	Create(context.Context, string, string, string) (Workspace, error)
	Remove(context.Context, string, Workspace) error
}

// CommandRunner is the executable seam for host-side Git commands.
type CommandRunner interface {
	Run(context.Context, string, []string) ([]byte, error)
}

// commandRunner executes the host git binary.
type commandRunner struct{}

// Run executes git in the requested repository directory.
func (commandRunner) Run(ctx context.Context, directory string, args []string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = directory
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			return nil, err
		}
		return nil, fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), message, err)
	}
	return stdout.Bytes(), nil
}

// LocalWorktreeManager fetches the configured target branch and creates a new
// branch/worktree from the fetched commit. It never checks out or changes the
// ordinary repository checkout.
// LocalWorktreeManager implements WorktreeManager with the host git command.
type LocalWorktreeManager struct {
	Runner      CommandRunner
	WorktreeDir string
}

// NewWorktreeManager returns a Git-backed worktree manager.
func NewWorktreeManager() *LocalWorktreeManager { return &LocalWorktreeManager{} }

// runner returns the injected command runner or the production Git runner.
func (m *LocalWorktreeManager) runner() CommandRunner {
	if m.Runner == nil {
		return commandRunner{}
	}
	return m.Runner
}

// Create fetches targetBranch from origin and creates factory/runID without
// checking out the ordinary repository.
func (m *LocalWorktreeManager) Create(ctx context.Context, repositoryPath, targetBranch, runID string) (Workspace, error) {
	if strings.TrimSpace(repositoryPath) == "" {
		return Workspace{}, errors.New("repository path is required")
	}
	if strings.TrimSpace(targetBranch) == "" {
		return Workspace{}, errors.New("target branch is required")
	}
	if strings.TrimSpace(runID) == "" {
		return Workspace{}, errors.New("run id is required")
	}
	if _, err := os.Stat(repositoryPath); err != nil {
		return Workspace{}, fmt.Errorf("inspect repository for worktree: %w", err)
	}
	if err := validateRefPart(targetBranch); err != nil {
		return Workspace{}, fmt.Errorf("target branch: %w", err)
	}
	if err := validateRefPart(runID); err != nil {
		return Workspace{}, fmt.Errorf("run id: %w", err)
	}

	if _, err := m.runner().Run(ctx, repositoryPath, []string{"fetch", "--no-tags", "origin", "refs/heads/" + targetBranch}); err != nil {
		return Workspace{}, fmt.Errorf("fetch target branch %q: %w", targetBranch, err)
	}
	baseOutput, err := m.runner().Run(ctx, repositoryPath, []string{"rev-parse", "FETCH_HEAD"})
	if err != nil {
		return Workspace{}, fmt.Errorf("resolve fetched target branch %q: %w", targetBranch, err)
	}
	baseSHA := strings.TrimSpace(string(baseOutput))
	if baseSHA == "" {
		return Workspace{}, errors.New("fetched target branch did not produce a commit SHA")
	}

	branch := "factory/" + runID
	worktree := m.worktreePath(repositoryPath, runID)
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		return Workspace{}, fmt.Errorf("create worktree parent: %w", err)
	}
	if _, err := m.runner().Run(ctx, repositoryPath, []string{"worktree", "add", "-b", branch, worktree, baseSHA}); err != nil {
		return Workspace{}, fmt.Errorf("create worktree %q: %w", worktree, err)
	}
	return Workspace{BaseSHA: baseSHA, Branch: branch, Worktree: worktree}, nil
}

// Remove deletes a run worktree and its factory branch after a claim failure.
// It attempts both operations so a partial cleanup still reports every error.
func (m *LocalWorktreeManager) Remove(ctx context.Context, repositoryPath string, workspace Workspace) error {
	if strings.TrimSpace(repositoryPath) == "" {
		return errors.New("repository path is required")
	}
	if strings.TrimSpace(workspace.Branch) == "" {
		return errors.New("workspace branch is required")
	}
	if strings.TrimSpace(workspace.Worktree) == "" {
		return errors.New("workspace path is required")
	}
	if _, err := os.Stat(repositoryPath); err != nil {
		return fmt.Errorf("inspect repository for worktree removal: %w", err)
	}
	if err := validateRefPart(workspace.Branch); err != nil {
		return fmt.Errorf("workspace branch: %w", err)
	}

	var cleanupErrors []error
	if _, err := m.runner().Run(ctx, repositoryPath, []string{"worktree", "remove", "--force", workspace.Worktree}); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("remove worktree %q: %w", workspace.Worktree, err))
	}
	if _, err := m.runner().Run(ctx, repositoryPath, []string{"branch", "-D", workspace.Branch}); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("remove branch %q: %w", workspace.Branch, err))
	}
	return errors.Join(cleanupErrors...)
}

// worktreePath returns the configured or default sibling worktree location.
func (m *LocalWorktreeManager) worktreePath(repositoryPath, runID string) string {
	if strings.TrimSpace(m.WorktreeDir) != "" {
		return filepath.Join(m.WorktreeDir, runID)
	}
	return filepath.Join(filepath.Dir(repositoryPath), ".factory-worktrees", filepath.Base(repositoryPath), runID)
}

// validateRefPart prevents user-controlled ref fragments from becoming
// ambiguous Git command arguments.
func validateRefPart(value string) error {
	if strings.ContainsAny(value, "\r\n") || strings.HasPrefix(value, "-") || strings.Contains(value, "..") || strings.ContainsAny(value, " ~^:?*[\\") {
		return fmt.Errorf("contains characters that cannot be used safely: %q", value)
	}
	return nil
}
