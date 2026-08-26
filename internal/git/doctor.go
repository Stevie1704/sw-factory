package git

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/doctor"
)

// DoctorRequest identifies the registered checkout and the GitHub branch it
// must be able to operate on during a factory run.
type DoctorRequest struct {
	// RepositoryPath is the ordinary host checkout.
	RepositoryPath string
	// RemoteName is the Git remote used by factory operations.
	RemoteName string
	// ExpectedOwner is the registered GitHub owner.
	ExpectedOwner string
	// ExpectedRepository is the registered GitHub repository name.
	ExpectedRepository string
	// TargetBranch is the branch fetched to begin a run.
	TargetBranch string
}

// DoctorChecker is the Git-owned startup diagnosis seam.
type DoctorChecker interface {
	CheckRemote(context.Context, DoctorRequest) error
	CheckHooks(context.Context, DoctorRequest) error
	CheckWorktree(context.Context, DoctorRequest) error
}

// StartupChecks returns independent Git remote, hooks, and worktree checks.
func StartupChecks(checker DoctorChecker, request DoctorRequest) []doctor.Check {
	return []doctor.Check{
		func(ctx context.Context) doctor.Result {
			if checker == nil {
				return doctor.Failure("git remote", "the Git diagnosis adapter is unavailable", "configure the host Git workspace adapter")
			}
			if err := checker.CheckRemote(ctx, request); err != nil {
				return doctor.Failure("git remote", "the registered checkout cannot reach the configured GitHub target", "repair the origin remote and ensure the configured target branch exists")
			}
			return doctor.Success("git remote")
		},
		func(ctx context.Context) doctor.Result {
			if checker == nil {
				return doctor.Failure("git hooks", "the Git diagnosis adapter is unavailable", "configure the host Git workspace adapter")
			}
			if err := checker.CheckHooks(ctx, request); err != nil {
				return doctor.Failure("git hooks", "the checkout's Git hooks directory cannot be inspected", "restore a readable Git hooks directory or correct core.hooksPath")
			}
			return doctor.Success("git hooks")
		},
		func(ctx context.Context) doctor.Result {
			if checker == nil {
				return doctor.Failure("git worktree", "the Git diagnosis adapter is unavailable", "configure the host Git workspace adapter")
			}
			if err := checker.CheckWorktree(ctx, request); err != nil {
				return doctor.Failure("git worktree", "the registered checkout cannot provide the worktree support required by a run", "repair the checkout and enable the Git worktree command")
			}
			return doctor.Success("git worktree")
		},
	}
}

// CheckRemote verifies the ordinary checkout, its origin identity, and the
// configured target branch without changing any Git reference.
func (m *LocalWorktreeManager) CheckRemote(ctx context.Context, request DoctorRequest) error {
	if err := validateDoctorRepository(request.RepositoryPath); err != nil {
		return err
	}
	if strings.TrimSpace(request.RemoteName) == "" {
		return errors.New("Git remote name is required")
	}
	if strings.TrimSpace(request.ExpectedOwner) == "" || strings.TrimSpace(request.ExpectedRepository) == "" {
		return errors.New("expected GitHub repository identity is required")
	}
	if strings.TrimSpace(request.TargetBranch) == "" {
		return errors.New("Git target branch is required")
	}
	if err := validateRefPart(request.TargetBranch); err != nil {
		return fmt.Errorf("Git target branch: %w", err)
	}
	rootOutput, err := m.runner().Run(ctx, request.RepositoryPath, []string{"rev-parse", "--show-toplevel"})
	if err != nil {
		return fmt.Errorf("inspect Git checkout: %w", err)
	}
	root, err := filepath.Abs(strings.TrimSpace(string(rootOutput)))
	if err != nil || filepath.Clean(root) != filepath.Clean(request.RepositoryPath) {
		return errors.New("Git checkout root does not match the registered repository")
	}
	remoteOutput, err := m.runner().Run(ctx, request.RepositoryPath, []string{"remote", "get-url", request.RemoteName})
	if err != nil {
		return fmt.Errorf("read Git remote: %w", err)
	}
	if !remoteMatches(string(remoteOutput), request.ExpectedOwner, request.ExpectedRepository) {
		return errors.New("Git remote does not identify the registered GitHub repository")
	}
	pushRemoteOutput, err := m.runner().Run(ctx, request.RepositoryPath, []string{"remote", "get-url", "--push", request.RemoteName})
	if err != nil {
		return fmt.Errorf("read Git push remote: %w", err)
	}
	if !remoteMatches(string(pushRemoteOutput), request.ExpectedOwner, request.ExpectedRepository) {
		return errors.New("Git push remote does not identify the registered GitHub repository")
	}
	branchOutput, err := m.runner().Run(ctx, request.RepositoryPath, []string{"ls-remote", "--exit-code", request.RemoteName, "refs/heads/" + request.TargetBranch})
	if err != nil {
		return fmt.Errorf("probe Git target branch: %w", err)
	}
	if strings.TrimSpace(string(branchOutput)) == "" {
		return errors.New("Git target branch returned no commit")
	}
	return nil
}

// CheckHooks verifies the configured Git hooks path is present and readable.
// Hooks are never executed by diagnosis.
func (m *LocalWorktreeManager) CheckHooks(ctx context.Context, request DoctorRequest) error {
	if err := validateDoctorRepository(request.RepositoryPath); err != nil {
		return err
	}
	output, err := m.runner().Run(ctx, request.RepositoryPath, []string{"rev-parse", "--git-path", "hooks"})
	if err != nil {
		return fmt.Errorf("resolve Git hooks path: %w", err)
	}
	hooksPath := strings.TrimSpace(string(output))
	if hooksPath == "" {
		return errors.New("Git returned an empty hooks path")
	}
	if !filepath.IsAbs(hooksPath) {
		hooksPath = filepath.Join(request.RepositoryPath, hooksPath)
	}
	info, err := os.Stat(hooksPath)
	if err != nil {
		return fmt.Errorf("inspect Git hooks directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New("Git hooks path is not a directory")
	}
	return nil
}

// CheckWorktree verifies the checkout is a worktree and can enumerate its
// current worktree metadata without creating or removing a user worktree.
func (m *LocalWorktreeManager) CheckWorktree(ctx context.Context, request DoctorRequest) error {
	if err := validateDoctorRepository(request.RepositoryPath); err != nil {
		return err
	}
	insideOutput, err := m.runner().Run(ctx, request.RepositoryPath, []string{"rev-parse", "--is-inside-work-tree"})
	if err != nil {
		return fmt.Errorf("probe Git worktree support: %w", err)
	}
	if strings.TrimSpace(string(insideOutput)) != "true" {
		return errors.New("Git did not report a worktree checkout")
	}
	if _, err := m.runner().Run(ctx, request.RepositoryPath, []string{"worktree", "list", "--porcelain"}); err != nil {
		return fmt.Errorf("list Git worktrees: %w", err)
	}
	return nil
}

// validateDoctorRepository verifies a repository path before invoking Git.
func validateDoctorRepository(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("Git repository path is required")
	}
	if !filepath.IsAbs(path) {
		return errors.New("Git repository path must be absolute")
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect Git repository path: %w", err)
	}
	if !info.IsDir() {
		return errors.New("Git repository path is not a directory")
	}
	return nil
}

// remoteMatches compares a GitHub owner/repository identity while accepting
// the HTTPS and scp-like SSH URL forms emitted by Git.
func remoteMatches(value, owner, repository string) bool {
	remote := strings.TrimSpace(value)
	if remote == "" || strings.ContainsAny(remote, "\x00\r\n") {
		return false
	}
	if !strings.Contains(remote, "://") {
		at := strings.LastIndex(remote, "@")
		colon := strings.Index(remote, ":")
		if colon <= at {
			return false
		}
		remote = "ssh://" + remote[:colon] + "/" + remote[colon+1:]
	}
	parsed, err := url.Parse(remote)
	if err != nil || !strings.EqualFold(parsed.Hostname(), "github.com") {
		return false
	}
	path := strings.Trim(strings.TrimSuffix(parsed.Path, ".git"), "/")
	wanted := strings.Trim(strings.TrimSuffix(owner+"/"+repository, ".git"), "/")
	return strings.EqualFold(path, wanted)
}

var _ DoctorChecker = (*LocalWorktreeManager)(nil)
