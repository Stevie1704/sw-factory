package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ExactWorktreeManager creates or adopts one durable claim's exact workspace.
// It never fetches or chooses a new base on replay.
type ExactWorktreeManager interface {
	CreateAtRevision(context.Context, string, string, string) (Workspace, error)
	DiscardRevision(context.Context, string, string) error
}

// DiscardRevision removes the workspace and branch that CreateAtRevision may
// have made for runID. An absent workspace is a successful result.
func (m *LocalWorktreeManager) DiscardRevision(ctx context.Context, repositoryPath, runID string) error {
	if err := validateRefPart(runID); err != nil {
		return err
	}
	return m.Remove(ctx, repositoryPath, Workspace{RunID: runID, Branch: "factory/" + runID, Worktree: m.worktreePath(repositoryPath, runID)})
}

// CreateAtRevision creates a factory branch at revision, or adopts an exact
// clean worktree left by a lost response. Conflicts require reconciliation.
func (m *LocalWorktreeManager) CreateAtRevision(ctx context.Context, repositoryPath, revision, runID string) (Workspace, error) {
	if !filepath.IsAbs(repositoryPath) || !validCommitSHA(revision) || runID == "" {
		return Workspace{}, errors.New("exact claim requires repository, full revision and run identity")
	}
	if err := validateRefPart(runID); err != nil {
		return Workspace{}, err
	}
	branch := "factory/" + runID
	worktree := m.worktreePath(repositoryPath, runID)
	workspace := Workspace{RunID: runID, BaseSHA: revision, Branch: branch, Worktree: worktree}
	objectType, err := m.runner().Run(ctx, repositoryPath, []string{"cat-file", "-t", revision})
	if err != nil || strings.TrimSpace(string(objectType)) != "commit" {
		return Workspace{}, errors.New("exact claim base is unavailable or not a commit")
	}
	registered, err := m.worktreeRegistered(ctx, repositoryPath, worktree)
	if err != nil {
		return Workspace{}, err
	}
	if registered {
		state, err := m.Inspect(ctx, worktree)
		if err != nil {
			return Workspace{}, err
		}
		if state.Branch != branch || state.HeadSHA != revision || len(state.ChangedPaths) != 0 {
			return Workspace{}, errors.New("reserved claim workspace conflicts with its frozen branch/base; inspect before retry")
		}
		return workspace, nil
	}
	if _, err := os.Lstat(worktree); err == nil {
		return Workspace{}, errors.New("reserved claim path exists without Git worktree registration; inspect before retry")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Workspace{}, err
	}
	output, refErr := m.runner().Run(ctx, repositoryPath, []string{"show-ref", "--verify", "--hash", "refs/heads/" + branch})
	if refErr == nil && strings.TrimSpace(string(output)) != revision {
		return Workspace{}, errors.New("reserved claim branch moved; inspect before retry")
	}
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		return Workspace{}, err
	}
	args := []string{"worktree", "add", worktree, branch}
	if refErr != nil {
		args = []string{"worktree", "add", "-b", branch, worktree, revision}
	}
	if _, err := m.runner().Run(ctx, repositoryPath, args); err != nil {
		return Workspace{}, fmt.Errorf("create reserved claim workspace (retry adopts a completed effect): %w", err)
	}
	state, err := m.Inspect(ctx, worktree)
	if err != nil {
		return Workspace{}, err
	}
	if state.Branch != branch || state.HeadSHA != revision || len(state.ChangedPaths) != 0 {
		return Workspace{}, errors.New("created claim workspace does not match frozen revision")
	}
	return workspace, nil
}
