package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ContextSnapshot identifies one detached checkout of a fetched commit. A
// session role reads it through a read-only worker mount; it has no branch
// and never shares a working tree with the user.
type ContextSnapshot struct {
	// Revision is the full commit identity of the checkout.
	Revision string
	// Worktree is the absolute host path of the checkout.
	Worktree string
}

// ContextSnapshotter creates and removes reproducible read-only repository
// context for session roles.
type ContextSnapshotter interface {
	// CreateContextSnapshot fetches targetBranch from origin and checks out
	// the fetched commit detached in a directory named snapshotID.
	CreateContextSnapshot(ctx context.Context, repositoryPath, targetBranch, snapshotID string) (ContextSnapshot, error)
	// RemoveContextSnapshot removes the checkout. An absent checkout is a
	// successful result.
	RemoveContextSnapshot(ctx context.Context, repositoryPath string, snapshot ContextSnapshot) error
}

// CreateContextSnapshot fetches targetBranch from origin and adds a detached
// worktree at the fetched commit. It never checks out anything in the
// ordinary repository and creates no branch.
func (m *LocalWorktreeManager) CreateContextSnapshot(ctx context.Context, repositoryPath, targetBranch, snapshotID string) (ContextSnapshot, error) {
	if strings.TrimSpace(repositoryPath) == "" {
		return ContextSnapshot{}, errors.New("repository path is required")
	}
	if err := validateRefPart(targetBranch); err != nil {
		return ContextSnapshot{}, fmt.Errorf("target branch: %w", err)
	}
	if err := validateRefPart(snapshotID); err != nil {
		return ContextSnapshot{}, fmt.Errorf("snapshot id: %w", err)
	}
	if _, err := m.runner().Run(ctx, repositoryPath, []string{"fetch", "--no-tags", DefaultRemoteName, "refs/heads/" + targetBranch}); err != nil {
		return ContextSnapshot{}, fmt.Errorf("fetch target branch %q: %w", targetBranch, err)
	}
	output, err := m.runner().Run(ctx, repositoryPath, []string{"rev-parse", "FETCH_HEAD"})
	if err != nil {
		return ContextSnapshot{}, fmt.Errorf("resolve fetched target branch %q: %w", targetBranch, err)
	}
	revision := strings.TrimSpace(string(output))
	if !validCommitSHA(revision) {
		return ContextSnapshot{}, errors.New("fetched target branch did not produce a full commit identity")
	}
	worktree := m.worktreePath(repositoryPath, snapshotID)
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		return ContextSnapshot{}, fmt.Errorf("create snapshot parent: %w", err)
	}
	if _, err := m.runner().Run(ctx, repositoryPath, []string{"worktree", "add", "--detach", worktree, revision}); err != nil {
		return ContextSnapshot{}, fmt.Errorf("create context snapshot %q: %w", worktree, err)
	}
	return ContextSnapshot{Revision: revision, Worktree: worktree}, nil
}

// RemoveContextSnapshot removes a detached snapshot worktree when Git still
// reports it. Repeating the removal is safe.
func (m *LocalWorktreeManager) RemoveContextSnapshot(ctx context.Context, repositoryPath string, snapshot ContextSnapshot) error {
	if strings.TrimSpace(repositoryPath) == "" || strings.TrimSpace(snapshot.Worktree) == "" {
		return errors.New("repository and snapshot paths are required")
	}
	registered, err := m.worktreeRegistered(ctx, repositoryPath, snapshot.Worktree)
	if err != nil {
		return err
	}
	if registered {
		if _, err := m.runner().Run(ctx, repositoryPath, []string{"worktree", "remove", "--force", snapshot.Worktree}); err != nil {
			return fmt.Errorf("remove context snapshot %q: %w", snapshot.Worktree, err)
		}
		return nil
	}
	if _, err := os.Lstat(snapshot.Worktree); err == nil {
		return fmt.Errorf("context snapshot %q still exists but Git no longer reports it", snapshot.Worktree)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect context snapshot %q: %w", snapshot.Worktree, err)
	}
	return nil
}

var _ ContextSnapshotter = (*LocalWorktreeManager)(nil)
