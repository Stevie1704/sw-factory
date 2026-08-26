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

// remoteFailureKind identifies the bounded failure categories rendered by the
// Git remote diagnosis without retaining command output.
type remoteFailureKind string

const (
	remoteFailureConfiguration  remoteFailureKind = "configuration"
	remoteFailureCheckout       remoteFailureKind = "checkout"
	remoteFailureRoot           remoteFailureKind = "root"
	remoteFailureFetchRead      remoteFailureKind = "fetch-read"
	remoteFailureFetchIdentity  remoteFailureKind = "fetch-identity"
	remoteFailurePushRead       remoteFailureKind = "push-read"
	remoteFailurePushIdentity   remoteFailureKind = "push-identity"
	remoteFailureBranchRead     remoteFailureKind = "branch-read"
	remoteFailureBranchNoCommit remoteFailureKind = "branch-no-commit"
)

// remoteCheckError carries only a safe category from one Git remote probe.
type remoteCheckError struct {
	kind remoteFailureKind
}

// Error returns a bounded error that never includes Git command output.
func (e *remoteCheckError) Error() string {
	return "Git remote diagnosis failed"
}

// remoteFailure creates one categorized Git remote diagnosis error.
func remoteFailure(kind remoteFailureKind) error {
	return &remoteCheckError{kind: kind}
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
				problem, action := remoteDiagnosis(err)
				return doctor.Failure("git remote", problem, action)
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

// CheckRemote verifies the ordinary checkout, its configured remote identity,
// and the target branch without changing any Git reference.
func (m *LocalWorktreeManager) CheckRemote(ctx context.Context, request DoctorRequest) error {
	if err := validateDoctorRepository(request.RepositoryPath); err != nil {
		return remoteFailure(remoteFailureCheckout)
	}
	if strings.TrimSpace(request.RemoteName) == "" {
		return remoteFailure(remoteFailureConfiguration)
	}
	if err := validateRefPart(request.RemoteName); err != nil {
		return remoteFailure(remoteFailureConfiguration)
	}
	if strings.TrimSpace(request.ExpectedOwner) == "" || strings.TrimSpace(request.ExpectedRepository) == "" {
		return remoteFailure(remoteFailureConfiguration)
	}
	if strings.TrimSpace(request.TargetBranch) == "" {
		return remoteFailure(remoteFailureConfiguration)
	}
	if err := validateRefPart(request.TargetBranch); err != nil {
		return remoteFailure(remoteFailureConfiguration)
	}
	rootOutput, err := m.runner().Run(ctx, request.RepositoryPath, []string{"rev-parse", "--show-toplevel"})
	if err != nil {
		return remoteFailure(remoteFailureCheckout)
	}
	root, err := filepath.Abs(strings.TrimSpace(string(rootOutput)))
	if err != nil {
		return remoteFailure(remoteFailureRoot)
	}
	// Resolve symlinks for both paths before comparing to handle cases like
	// macOS /tmp vs /private/tmp where symlink-equivalent paths should match.
	resolvedRoot := root
	if evalRoot, err := filepath.EvalSymlinks(root); err == nil {
		resolvedRoot = evalRoot
	}
	resolvedRepoPath := request.RepositoryPath
	if evalRepoPath, err := filepath.EvalSymlinks(request.RepositoryPath); err == nil {
		resolvedRepoPath = evalRepoPath
	}
	if filepath.Clean(resolvedRoot) != filepath.Clean(resolvedRepoPath) {
		return remoteFailure(remoteFailureRoot)
	}
	remoteOutput, err := m.runner().Run(ctx, request.RepositoryPath, []string{"remote", "get-url", request.RemoteName})
	if err != nil {
		return remoteFailure(remoteFailureFetchRead)
	}
	if !remoteMatches(string(remoteOutput), request.ExpectedOwner, request.ExpectedRepository) {
		return remoteFailure(remoteFailureFetchIdentity)
	}
	pushRemoteOutput, err := m.runner().Run(ctx, request.RepositoryPath, []string{"remote", "get-url", "--push", request.RemoteName})
	if err != nil {
		return remoteFailure(remoteFailurePushRead)
	}
	if !remoteMatches(string(pushRemoteOutput), request.ExpectedOwner, request.ExpectedRepository) {
		return remoteFailure(remoteFailurePushIdentity)
	}
	branchOutput, err := m.runner().Run(ctx, request.RepositoryPath, []string{"ls-remote", "--exit-code", request.RemoteName, "refs/heads/" + request.TargetBranch})
	if err != nil {
		return remoteFailure(remoteFailureBranchRead)
	}
	if strings.TrimSpace(string(branchOutput)) == "" {
		return remoteFailure(remoteFailureBranchNoCommit)
	}
	return nil
}

// remoteDiagnosis translates one safe Git remote category into the specific
// problem and corrective action shown by the startup report.
func remoteDiagnosis(err error) (string, string) {
	var failure *remoteCheckError
	if !errors.As(err, &failure) {
		return "the Git remote diagnosis could not be classified", "repair the registered checkout and configured Git remote, then retry the diagnosis"
	}
	switch failure.kind {
	case remoteFailureConfiguration:
		return "the Git remote diagnosis request is incomplete or unsafe", "repair the registered repository identity, remote name, and target branch"
	case remoteFailureCheckout:
		return "the registered checkout cannot be inspected as a Git repository", "repair the registered checkout path and initialize or restore its Git metadata"
	case remoteFailureRoot:
		return "Git resolved a checkout root different from the registered repository", "register the checkout's actual Git root or correct the repository path"
	case remoteFailureFetchRead:
		return "the configured Git fetch remote is missing or unreadable", "configure the factory remote and verify the checkout can read it"
	case remoteFailureFetchIdentity:
		return "the Git fetch remote does not identify the registered GitHub repository", "set the configured remote's fetch URL to the registered GitHub repository"
	case remoteFailurePushRead:
		return "the configured Git push remote is missing or unreadable", "configure a push URL for the factory remote and verify the checkout can read it"
	case remoteFailurePushIdentity:
		return "the Git push remote does not identify the registered GitHub repository", "set the configured remote's push URL to the registered GitHub repository"
	case remoteFailureBranchRead:
		return "the configured Git target branch cannot be read from the remote", "fetch the target branch and verify network and remote permissions"
	case remoteFailureBranchNoCommit:
		return "the configured Git target branch has no commit on the remote", "choose an existing target branch or publish the branch before starting the factory"
	default:
		return "the Git remote diagnosis returned an unknown failure", "repair the registered checkout and configured Git remote, then retry the diagnosis"
	}
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
