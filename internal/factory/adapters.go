package factory

import (
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
)

// gitWorkspace resolves the new task-oriented seam while retaining the
// foundation Worktree adapter as a compatibility fallback.
func (s *Service) gitWorkspace() gitadapter.GitWorkspace {
	if s.deps.GitWorkspace != nil {
		return s.deps.GitWorkspace
	}
	workspace, _ := s.deps.Worktree.(gitadapter.GitWorkspace)
	return workspace
}

// worktreeManager resolves the task-oriented Git workspace for the existing
// claim/cleanup operations while keeping foundation adapters compatible.
func (s *Service) worktreeManager() gitadapter.WorktreeManager {
	if s.deps.GitWorkspace != nil {
		return s.deps.GitWorkspace
	}
	return s.deps.Worktree
}

// worktreeInspector resolves the read-only validation seam used by report
// acceptance.
func (s *Service) worktreeInspector() gitadapter.WorktreeInspector {
	if s.deps.GitWorkspace != nil {
		return s.deps.GitWorkspace
	}
	inspector, _ := s.deps.Worktree.(gitadapter.WorktreeInspector)
	return inspector
}

// checkpointFileReader resolves the exact-checkpoint read seam used by the
// baseline verifier. It returns nil when no configured adapter can read a
// tracked file at a commit, and the verifier then refuses rather than falling
// back to the working tree.
func (s *Service) checkpointFileReader() gitadapter.CheckpointFileReader {
	if reader, ok := s.deps.GitWorkspace.(gitadapter.CheckpointFileReader); ok {
		return reader
	}
	reader, _ := s.deps.Worktree.(gitadapter.CheckpointFileReader)
	return reader
}

// pullRequestClient resolves the dedicated pull-request adapter or a GitHub
// client that implements it directly.
func (s *Service) pullRequestClient() github.PullRequestClient {
	if s.deps.PullRequests != nil {
		return s.deps.PullRequests
	}
	client, _ := s.deps.GitHub.(github.PullRequestClient)
	return client
}
