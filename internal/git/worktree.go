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

	"github.com/Stevie1704/sw-factory/internal/ref"
)

// DefaultRemoteName is the repository remote used by factory Git operations.
const DefaultRemoteName = "origin"

// Workspace identifies the fetched base commit and isolated checkout created
// for one run.
type Workspace struct {
	// RunID identifies the run-owned metadata projection adjacent to Worktree.
	// It is optional for compatibility with callers that only manage Git state.
	RunID    string
	BaseSHA  string
	Branch   string
	Worktree string
}

// CheckpointKind identifies the workflow checkpoint represented by a commit.
type CheckpointKind string

const (
	// CheckpointKindImplementation is the production implementation checkpoint.
	CheckpointKindImplementation CheckpointKind = "implementation"
	// CheckpointKindTest is the protected test-stage checkpoint.
	CheckpointKindTest CheckpointKind = "test"
)

// CheckpointRequest selects one host-side checkpoint.
type CheckpointRequest struct {
	// RunID identifies the factory run owning the checkpoint.
	RunID string
	// WorktreePath is the absolute run worktree to commit.
	WorktreePath string
	// ParentSHA is the last checkpoint known by the coordinator.
	ParentSHA string
	// Kind selects the marker and ownership of the checkpoint. Empty means an
	// implementation checkpoint for compatibility with earlier callers.
	Kind CheckpointKind
	// Paths are optional repository-relative paths to stage. Empty stages all
	// changes, which is used only by the implementation checkpoint.
	Paths []string
	// Message is the human-readable portion of the checkpoint commit message.
	Message string
}

// CheckpointResult identifies the immutable commit created or recovered by a
// checkpoint operation.
type CheckpointResult struct {
	// SHA is the full commit identity.
	SHA string
	// Created reports whether this call created a new commit.
	Created bool
}

// PushRequest selects one run branch for a host-side push.
type PushRequest struct {
	// WorktreePath is the run worktree whose current HEAD is pushed.
	WorktreePath string
	// Branch is the exact remote branch name to update.
	Branch string
}

// BaseSyncRequest selects a fast-forward-only synchronization with a remote
// target branch.
type BaseSyncRequest struct {
	// WorktreePath is the run worktree to update.
	WorktreePath string
	// TargetBranch is the remote branch to fetch and merge.
	TargetBranch string
}

// WorktreeState is the coordinator-observed state used to validate a harness
// handoff against the actual checkout.
type WorktreeState struct {
	// RepositoryPath is the main checkout root owning the worktree's Git
	// metadata, resolved from git-common-dir.
	RepositoryPath string
	// Branch is the current local branch name, or empty when HEAD is detached.
	Branch string
	// HeadSHA is the current full commit identity.
	HeadSHA string
	// ChangedPaths contains repository-relative paths reported by Git.
	ChangedPaths []string
}

// WorktreeManager creates an isolated branch and worktree from a target branch.
type WorktreeManager interface {
	Create(context.Context, string, string, string) (Workspace, error)
	Remove(context.Context, string, Workspace) error
}

// WorktreeInspector is the optional read-only worktree observation seam.
type WorktreeInspector interface {
	// Inspect reads the current commit and changed paths without mutating Git.
	Inspect(context.Context, string) (WorktreeState, error)
}

// GitWorkspace is the task-oriented host Git seam used by the coordinator.
// It keeps branch/worktree creation, validation, checkpoints, base
// synchronization, pushes, and cleanup out of workflow code and workers.
type GitWorkspace interface {
	WorktreeManager
	WorktreeInspector
	CreateCheckpoint(context.Context, CheckpointRequest) (CheckpointResult, error)
	Push(context.Context, PushRequest) error
	SynchronizeBase(context.Context, BaseSyncRequest) error
}

// RemoteBranchInspector is the optional read-only projection used to
// recognize a completed push after a coordinator restart.
type RemoteBranchInspector interface {
	RemoteBranchHead(context.Context, PushRequest) (string, error)
}

// AncestryInspector is the optional read-only projection that reports whether
// one commit is reachable from another. Reconciliation uses it to tell an
// unpushed local checkpoint apart from a genuinely diverged remote branch.
type AncestryInspector interface {
	IsAncestor(ctx context.Context, worktreePath, ancestor, descendant string) (bool, error)
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

// NewGitWorkspace returns the host-side Git workspace adapter.
func NewGitWorkspace() *LocalWorktreeManager { return NewWorktreeManager() }

// LocalGitWorkspace is the host-side Git workspace implementation. The alias
// preserves the existing LocalWorktreeManager name for callers of earlier
// foundation stages.
type LocalGitWorkspace = LocalWorktreeManager

// Inspect reads the current HEAD and porcelain status of one run worktree.
func (m *LocalWorktreeManager) Inspect(ctx context.Context, worktreePath string) (WorktreeState, error) {
	if strings.TrimSpace(worktreePath) == "" {
		return WorktreeState{}, errors.New("worktree path is required")
	}
	commonDirOutput, err := m.runner().Run(ctx, worktreePath, []string{"rev-parse", "--git-common-dir"})
	if err != nil {
		return WorktreeState{}, fmt.Errorf("inspect worktree repository: %w", err)
	}
	branchOutput, err := m.runner().Run(ctx, worktreePath, []string{"branch", "--show-current"})
	if err != nil {
		return WorktreeState{}, fmt.Errorf("inspect worktree branch: %w", err)
	}
	headOutput, err := m.runner().Run(ctx, worktreePath, []string{"rev-parse", "HEAD"})
	if err != nil {
		return WorktreeState{}, fmt.Errorf("inspect worktree HEAD: %w", err)
	}
	statusOutput, err := m.runner().Run(ctx, worktreePath, []string{"status", "--porcelain=v1", "-z", "--untracked-files=all"})
	if err != nil {
		return WorktreeState{}, fmt.Errorf("inspect worktree changes: %w", err)
	}
	return WorktreeState{
		RepositoryPath: repositoryPathFromCommonDir(worktreePath, strings.TrimSpace(string(commonDirOutput))),
		Branch:         strings.TrimSpace(string(branchOutput)),
		HeadSHA:        strings.TrimSpace(string(headOutput)),
		ChangedPaths:   porcelainPaths(string(statusOutput)),
	}, nil
}

// repositoryPathFromCommonDir converts Git's common metadata path into the
// owning checkout root while preserving an empty observation for malformed
// command output.
func repositoryPathFromCommonDir(worktreePath, commonDir string) string {
	if strings.TrimSpace(commonDir) == "" {
		return ""
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(worktreePath, commonDir)
	}
	return filepath.Dir(filepath.Clean(commonDir))
}

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

	if _, err := m.runner().Run(ctx, repositoryPath, []string{"fetch", "--no-tags", DefaultRemoteName, "refs/heads/" + targetBranch}); err != nil {
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
	return Workspace{RunID: runID, BaseSHA: baseSHA, Branch: branch, Worktree: worktree}, nil
}

// CreateCheckpoint validates the run worktree and creates one host-side
// implementation commit. A checkpoint marker makes retries recover the
// original commit rather than creating a duplicate.
func (m *LocalWorktreeManager) CreateCheckpoint(ctx context.Context, request CheckpointRequest) (CheckpointResult, error) {
	if strings.TrimSpace(request.RunID) == "" {
		return CheckpointResult{}, errors.New("checkpoint run id is required")
	}
	if err := validateRefPart(request.RunID); err != nil {
		return CheckpointResult{}, fmt.Errorf("checkpoint run id: %w", err)
	}
	if strings.TrimSpace(request.WorktreePath) == "" {
		return CheckpointResult{}, errors.New("checkpoint worktree path is required")
	}
	if !filepath.IsAbs(request.WorktreePath) {
		return CheckpointResult{}, errors.New("checkpoint worktree path must be absolute")
	}
	if !validCommitSHA(request.ParentSHA) {
		return CheckpointResult{}, errors.New("checkpoint parent SHA must be a full commit identity")
	}
	kind := request.Kind
	if kind == "" {
		kind = CheckpointKindImplementation
	}
	if kind != CheckpointKindImplementation && kind != CheckpointKindTest {
		return CheckpointResult{}, fmt.Errorf("unsupported checkpoint kind %q", kind)
	}
	for index, path := range request.Paths {
		if err := validateCheckpointPath(path); err != nil {
			return CheckpointResult{}, fmt.Errorf("checkpoint path %d: %w", index, err)
		}
	}
	message := strings.TrimSpace(request.Message)
	if strings.ContainsRune(message, '\x00') {
		return CheckpointResult{}, errors.New("checkpoint message contains a NUL byte")
	}
	marker := checkpointMarker(kind, request.RunID)
	state, err := m.Inspect(ctx, request.WorktreePath)
	if err != nil {
		return CheckpointResult{}, fmt.Errorf("inspect worktree before checkpoint: %w", err)
	}
	if state.HeadSHA != request.ParentSHA {
		if m.hasCheckpointMarker(ctx, request.WorktreePath, marker) {
			if len(state.ChangedPaths) != 0 {
				return CheckpointResult{}, fmt.Errorf("worktree has changes after the existing %s checkpoint", kind)
			}
			return CheckpointResult{SHA: state.HeadSHA}, nil
		}
		return CheckpointResult{}, fmt.Errorf("worktree HEAD %q does not match checkpoint parent %q", state.HeadSHA, request.ParentSHA)
	}
	if len(state.ChangedPaths) == 0 {
		if m.hasCheckpointMarker(ctx, request.WorktreePath, marker) {
			return CheckpointResult{SHA: state.HeadSHA}, nil
		}
		return CheckpointResult{}, fmt.Errorf("worktree has no %s changes to checkpoint", kind)
	}
	if _, err := m.runner().Run(ctx, request.WorktreePath, []string{"diff", "--check"}); err != nil {
		return CheckpointResult{}, fmt.Errorf("validate worktree before checkpoint: %w", err)
	}
	commitMessage := marker
	if message != "" {
		commitMessage += "\n\n" + message
	}
	stageArgs := []string{"add"}
	if len(request.Paths) == 0 {
		stageArgs = append(stageArgs, "--all", "--")
	} else {
		stageArgs = append(stageArgs, "--")
		stageArgs = append(stageArgs, request.Paths...)
	}
	if _, err := m.runner().Run(ctx, request.WorktreePath, stageArgs); err != nil {
		return CheckpointResult{}, fmt.Errorf("stage %s checkpoint: %w", kind, err)
	}
	if _, err := m.runner().Run(ctx, request.WorktreePath, []string{"commit", "--message", commitMessage}); err != nil {
		return CheckpointResult{}, fmt.Errorf("create %s checkpoint: %w", kind, err)
	}
	headOutput, err := m.runner().Run(ctx, request.WorktreePath, []string{"rev-parse", "HEAD"})
	if err != nil {
		return CheckpointResult{}, fmt.Errorf("resolve %s checkpoint: %w", kind, err)
	}
	head := strings.TrimSpace(string(headOutput))
	if !validCommitSHA(head) {
		return CheckpointResult{}, fmt.Errorf("%s checkpoint did not produce a full commit identity", kind)
	}
	return CheckpointResult{SHA: head, Created: true}, nil
}

// checkpointMarker returns the first-line idempotency marker for one checkpoint.
func checkpointMarker(kind CheckpointKind, runID string) string {
	return "factory: " + string(kind) + " checkpoint " + runID
}

// validateCheckpointPath rejects paths that could escape the run checkout or
// address Git metadata during scoped staging.
func validateCheckpointPath(path string) error {
	if strings.TrimSpace(path) == "" || strings.ContainsAny(path, "\x00\r\n\\") || filepath.IsAbs(path) {
		return errors.New("must be a safe repository-relative path")
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean == ".git" || strings.HasPrefix(clean, ".git/") {
		return errors.New("must remain inside the repository checkout")
	}
	return nil
}

// Push publishes the current run worktree HEAD to its named remote branch
// without force-pushing or changing the ordinary checkout.
func (m *LocalWorktreeManager) Push(ctx context.Context, request PushRequest) error {
	if strings.TrimSpace(request.WorktreePath) == "" {
		return errors.New("push worktree path is required")
	}
	if !filepath.IsAbs(request.WorktreePath) {
		return errors.New("push worktree path must be absolute")
	}
	if strings.TrimSpace(request.Branch) == "" {
		return errors.New("push branch is required")
	}
	if err := validateRefPart(request.Branch); err != nil {
		return fmt.Errorf("push branch: %w", err)
	}
	if _, err := m.runner().Run(ctx, request.WorktreePath, []string{"push", "--set-upstream", DefaultRemoteName, "HEAD:refs/heads/" + request.Branch}); err != nil {
		return fmt.Errorf("push run branch %q: %w", request.Branch, err)
	}
	return nil
}

// RemoteBranchHead reads the exact origin branch head without changing the
// local checkout. An empty result means the remote branch does not exist.
func (m *LocalWorktreeManager) RemoteBranchHead(ctx context.Context, request PushRequest) (string, error) {
	if strings.TrimSpace(request.WorktreePath) == "" {
		return "", errors.New("remote branch worktree path is required")
	}
	if !filepath.IsAbs(request.WorktreePath) {
		return "", errors.New("remote branch worktree path must be absolute")
	}
	if strings.TrimSpace(request.Branch) == "" {
		return "", errors.New("remote branch is required")
	}
	if err := validateRefPart(request.Branch); err != nil {
		return "", fmt.Errorf("remote branch: %w", err)
	}
	output, err := m.runner().Run(ctx, request.WorktreePath, []string{"ls-remote", "--heads", DefaultRemoteName, "refs/heads/" + request.Branch})
	if err != nil {
		return "", fmt.Errorf("read remote branch %q: %w", request.Branch, err)
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 {
		return "", nil
	}
	if !validCommitSHA(fields[0]) {
		return "", errors.New("remote branch response did not contain a full commit identity")
	}
	return fields[0], nil
}

// IsAncestor reports whether ancestor is reachable from descendant without
// changing the local checkout. An unknown commit is reported as not reachable
// rather than as an error, so a pruned remote object cannot claim agreement.
func (m *LocalWorktreeManager) IsAncestor(ctx context.Context, worktreePath, ancestor, descendant string) (bool, error) {
	if strings.TrimSpace(worktreePath) == "" {
		return false, errors.New("ancestry worktree path is required")
	}
	if !filepath.IsAbs(worktreePath) {
		return false, errors.New("ancestry worktree path must be absolute")
	}
	if !validCommitSHA(ancestor) || !validCommitSHA(descendant) {
		return false, errors.New("ancestry requires two full commit identities")
	}
	if ancestor == descendant {
		return true, nil
	}
	if _, err := m.runner().Run(ctx, worktreePath, []string{"merge-base", "--is-ancestor", ancestor, descendant}); err != nil {
		// git exits non-zero both for "not an ancestor" and for an unknown
		// commit. Neither proves agreement, so both report false.
		return false, nil
	}
	return true, nil
}

// SynchronizeBase fetches the named target branch and fast-forwards the run
// worktree to it. It never rebases or force-updates a run branch.
func (m *LocalWorktreeManager) SynchronizeBase(ctx context.Context, request BaseSyncRequest) error {
	if strings.TrimSpace(request.WorktreePath) == "" {
		return errors.New("base synchronization worktree path is required")
	}
	if !filepath.IsAbs(request.WorktreePath) {
		return errors.New("base synchronization worktree path must be absolute")
	}
	if strings.TrimSpace(request.TargetBranch) == "" {
		return errors.New("base synchronization target branch is required")
	}
	if err := validateRefPart(request.TargetBranch); err != nil {
		return fmt.Errorf("base synchronization target branch: %w", err)
	}
	if _, err := m.runner().Run(ctx, request.WorktreePath, []string{"fetch", "--no-tags", DefaultRemoteName, "refs/heads/" + request.TargetBranch}); err != nil {
		return fmt.Errorf("fetch base branch %q: %w", request.TargetBranch, err)
	}
	if _, err := m.runner().Run(ctx, request.WorktreePath, []string{"merge", "--ff-only", "FETCH_HEAD"}); err != nil {
		return fmt.Errorf("fast-forward base branch %q: %w", request.TargetBranch, err)
	}
	return nil
}

// hasCheckpointMarker reports whether the current commit carries the marker
// used to make a repeated checkpoint request idempotent.
func (m *LocalWorktreeManager) hasCheckpointMarker(ctx context.Context, worktreePath, marker string) bool {
	output, err := m.runner().Run(ctx, worktreePath, []string{"log", "-1", "--format=%B"})
	if err != nil {
		return false
	}
	firstLine := strings.SplitN(strings.TrimSpace(string(output)), "\n", 2)[0]
	return firstLine == marker
}

// Remove deletes a run worktree, its factory branch, and its exact run-owned
// Git metadata projection. It attempts every operation so a partial cleanup
// still reports every error. It never addresses a remote branch.
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
	if workspace.RunID != "" {
		if err := validateRefPart(workspace.RunID); err != nil {
			return fmt.Errorf("workspace run id: %w", err)
		}
		if filepath.Base(filepath.Clean(workspace.Worktree)) != workspace.RunID {
			return errors.New("workspace path does not end in its run id")
		}
	}

	var cleanupErrors []error
	if _, err := m.runner().Run(ctx, repositoryPath, []string{"worktree", "remove", "--force", workspace.Worktree}); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("remove worktree %q: %w", workspace.Worktree, err))
	}
	if _, err := m.runner().Run(ctx, repositoryPath, []string{"branch", "-D", workspace.Branch}); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("remove branch %q: %w", workspace.Branch, err))
	}
	if workspace.RunID != "" {
		if err := removeRunGitMetadata(workspace); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	return errors.Join(cleanupErrors...)
}

// removeRunGitMetadata removes one exact run directory under the sibling
// .factory-git projection. Symlinks and non-directories are rejected so a
// persisted path cannot widen Git cleanup beyond the run's own metadata.
func removeRunGitMetadata(workspace Workspace) error {
	metadataPath := filepath.Join(filepath.Dir(filepath.Clean(workspace.Worktree)), ".factory-git", workspace.RunID)
	info, err := os.Lstat(metadataPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Git metadata %q: %w", metadataPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("Git metadata %q must not be a symbolic link", metadataPath)
	}
	if !info.IsDir() {
		return fmt.Errorf("Git metadata %q is not a directory", metadataPath)
	}
	if err := os.RemoveAll(metadataPath); err != nil {
		return fmt.Errorf("remove Git metadata %q: %w", metadataPath, err)
	}
	return nil
}

// worktreePath returns the configured or default sibling worktree location.
func (m *LocalWorktreeManager) worktreePath(repositoryPath, runID string) string {
	if strings.TrimSpace(m.WorktreeDir) != "" {
		return filepath.Join(m.WorktreeDir, runID)
	}
	return filepath.Join(filepath.Dir(repositoryPath), ".factory-worktrees", filepath.Base(repositoryPath), runID)
}

// porcelainPaths extracts repository paths from NUL-delimited Git
// porcelain-v1 output. Rename and copy records contain destination first and
// source second; only the destination is relevant to observed changes.
func porcelainPaths(output string) []string {
	paths := []string{}
	fields := strings.Split(output, "\x00")
	for index := 0; index < len(fields); index++ {
		record := fields[index]
		if len(record) < 4 {
			continue
		}
		status := record[:2]
		path := record[3:]
		if path != "" {
			paths = append(paths, filepath.ToSlash(path))
		}
		if strings.ContainsAny(status, "RC") && index+1 < len(fields) {
			index++
		}
	}
	return paths
}

// validateRefPart prevents user-controlled ref fragments from becoming
// ambiguous Git command arguments.
func validateRefPart(value string) error {
	return ref.ValidatePart(value)
}

// validCommitSHA reports whether value is a full SHA-1 or SHA-256 identity.
func validCommitSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
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
