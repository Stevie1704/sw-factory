package factory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// resetWorkspaceCloseTimeout bounds one terminal workspace close so an
// unresponsive terminal cannot stall the rest of a reset.
const resetWorkspaceCloseTimeout = 10 * time.Second

// resetDatabaseSidecarSuffixes are the SQLite files that belong to one exact
// database path. They are named rather than discovered so reset never widens
// into an unrelated file that merely shares a directory.
var resetDatabaseSidecarSuffixes = []string{"-wal", "-shm", "-journal"}

// resetRetainedResources names everything reset deliberately keeps. It is part
// of the operator-visible plan so a preview distinguishes removal from
// retention without the operator consulting documentation.
var resetRetainedResources = []string{
	"the registered source checkout, its tracked files, factory.yaml, and ordinary local branches",
	"installed factory, factory-report, and factory-worker-attach binaries",
	"Docker worker images",
	"repository-declared cache directories",
	"host Codex and Claude credential sources and host harness state",
	"GitHub label definitions, issues, pull requests, reviews, comments, commit statuses, and merged history",
	"remote factory/* branches",
}

// ResetStore is the operational-store seam reset needs to read every persisted
// run, regardless of status or retention age.
type ResetStore interface {
	OperationalStore
	ListResetCandidates(context.Context) ([]store.CleanupCandidate, error)
}

// ResetRequest selects whether the destructive operation is explicitly
// confirmed. Without confirmation the operation is a read-only preview.
type ResetRequest struct {
	// Confirm authorizes removal after the caller has displayed the plan.
	Confirm bool
}

// ResetRun describes every local target one persisted run contributes to a
// reset plan.
type ResetRun struct {
	// RunID identifies the run and its private worker resources.
	RunID string
	// Status is the run status observed when the final plan was built.
	Status store.Status
	// Branch is the local factory branch to remove.
	Branch string
	// Worktree is the exact local worktree path to remove.
	Worktree string
	// GitProjection is the exact private Git metadata directory the Git
	// workspace adapter removes with the worktree.
	GitProjection string
	// WorkspaceIDs contains the terminal workspace handles to close.
	WorkspaceIDs []string
	// WorkerIDs contains the exact worker container identities to remove.
	WorkerIDs []string
	// Roles selects the run-scoped role-home volumes to remove.
	Roles []string
	// StoredOutputs contains the exact generated invocation and result
	// directories to remove.
	StoredOutputs []string
}

// ResetCredentialStore is one factory-managed credential volume identified by a
// persisted credential-store identity. Ordinary cleanup retains these; only a
// complete reset removes them.
type ResetCredentialStore struct {
	// RunID is the run whose worker mounted the store, used as the adapter's
	// legacy fallback identity.
	RunID string
	// CredentialStoreID is the persisted credential-store identity.
	CredentialStoreID string
}

// ResetLifecycleTransition reports one non-terminal run that GitHub observation
// carried to a terminal outcome through the ordinary lifecycle projection
// before its local state was planned for removal.
type ResetLifecycleTransition struct {
	// RunID identifies the observed run.
	RunID string
	// Outcome is the coordinator-owned lifecycle decision.
	Outcome LifecycleOutcome
	// Reason is the operator-facing explanation recorded with the transition.
	Reason string
}

// ResetBlocker explains why reset refused before performing any deletion.
type ResetBlocker struct {
	// RunID identifies the affected run, or is empty for an installation-wide
	// refusal such as a held coordinator lock.
	RunID string
	// Reason is the bounded operator-facing explanation.
	Reason string
	// Action is the corrective step through the existing supervised workflow.
	Action string
}

// ResetPlan is the complete operator-visible reset preview.
type ResetPlan struct {
	// ConfigPath is the selected host configuration, removed last.
	ConfigPath string
	// RepositoryPath is the registered repository whose installation is reset.
	RepositoryPath string
	// OperationalDataPath is the operational SQLite database to remove.
	OperationalDataPath string
	// DatabaseSidecars contains the exact SQLite sidecar files to remove.
	DatabaseSidecars []string
	// MigrationBackups contains only backups proven to belong to that exact
	// database by its own file name prefix.
	MigrationBackups []string
	// CoordinatorLock is the exact lock file for the registered checkout.
	CoordinatorLock string
	// ControlWorkspace is the registered control workspace name to close.
	ControlWorkspace string
	// Runs contains every run's exact local targets.
	Runs []ResetRun
	// CredentialStores contains every distinct factory-managed credential
	// volume identity to remove.
	CredentialStores []ResetCredentialStore
	// EvaluationSummaries counts the evaluation projections that disappear with
	// the operational store.
	EvaluationSummaries int
	// Retained names everything reset deliberately keeps.
	Retained []string
}

// ResetFailure reports one planned target that a confirmed reset could not
// remove. It never carries credentials, credential paths, database content,
// issue text, prompts, diffs, or command output.
type ResetFailure struct {
	// Target is the operator-facing identity of the resource.
	Target string
	// Reason is the bounded, sanitized failure explanation.
	Reason string
}

// ResetResult contains the plan and, after confirmation, what reset removed and
// what remains.
type ResetResult struct {
	// Plan is returned for both preview and confirmed reset.
	Plan ResetPlan
	// Lifecycle records every terminal transition reset published to GitHub
	// before local deletion.
	Lifecycle []ResetLifecycleTransition
	// Removed names every planned target reset completed, in deletion order.
	Removed []string
	// Remaining names every planned target that still exists.
	Remaining []ResetFailure
}

// ResetConfirmationRequiredError tells a CLI or embedder to display the
// returned plan and repeat with explicit confirmation.
type ResetConfirmationRequiredError struct{}

// Error describes the required reset confirmation.
func (*ResetConfirmationRequiredError) Error() string {
	return "reset requires explicit confirmation"
}

// ResetBlockedError reports that reset refused before any deletion. It carries
// every blocker so an operator resolves them in one pass.
type ResetBlockedError struct {
	// Blockers contains every refusal that prevented the operation.
	Blockers []ResetBlocker
}

// Error describes why reset was refused before any mutation.
func (e *ResetBlockedError) Error() string {
	return fmt.Sprintf("reset blocked by %d unresolved condition(s)", len(e.Blockers))
}

// ResetIncompleteError reports that a confirmed reset removed some targets but
// could not remove all of them. The operational store and host configuration
// are retained so the operation can be repeated.
type ResetIncompleteError struct {
	// Remaining contains every planned target that still exists.
	Remaining []ResetFailure
}

// Error describes an incomplete reset without naming sensitive payloads.
func (e *ResetIncompleteError) Error() string {
	return fmt.Sprintf("reset left %d target(s) in place; rerun reset after resolving them", len(e.Remaining))
}

// Reset previews and, when explicitly confirmed, returns one registered factory
// installation to its pre-init local state. Without confirmation it performs no
// filesystem, Git, Docker, terminal, store, configuration, or GitHub mutation.
//
// Confirmation re-observes current state before deleting anything: it proves
// that no coordinator holds the repository lock, carries every non-terminal run
// to a terminal outcome through the ordinary GitHub lifecycle projection, and
// rebuilds the deletion plan from the resulting persisted state. The operational
// store is the manifest that identifies most targets, so it and the host
// configuration are removed last and are retained whenever a planned target
// still needs them.
func (s *Service) Reset(ctx context.Context, request ResetRequest) (ResetResult, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()

	registration, err := s.registration()
	if err != nil {
		return ResetResult{}, err
	}
	lockPath := coordinatorLockPath(registration)
	held, err := coordinatorLockHeld(lockPath)
	if err != nil {
		return ResetResult{}, err
	}
	if held {
		return ResetResult{}, &ResetBlockedError{Blockers: []ResetBlocker{{
			Reason: "the coordinator for this repository is running and owns its lock",
			Action: "run factory stop, then repeat factory reset",
		}}}
	}

	opened, err := s.deps.OpenStore(ctx, registration.OperationalDataPath)
	if err != nil {
		return ResetResult{}, err
	}
	resetStore, ok := opened.(ResetStore)
	if !ok {
		_ = opened.Close()
		return ResetResult{}, errors.New("operational store does not support reset")
	}
	storeClosed := false
	defer func() {
		if !storeClosed {
			_ = resetStore.Close()
		}
	}()

	plan, blockers, err := s.buildResetPlan(ctx, registration, resetStore, lockPath)
	if err != nil {
		return ResetResult{}, err
	}
	if len(blockers) > 0 {
		return ResetResult{Plan: plan}, &ResetBlockedError{Blockers: blockers}
	}
	if err := s.checkResetAdapters(ctx, registration, plan); err != nil {
		return ResetResult{Plan: plan}, err
	}
	if !request.Confirm {
		return ResetResult{Plan: plan}, &ResetConfirmationRequiredError{}
	}

	// Confirmation never treats the preview as authority. Lifecycle
	// reconciliation is committed to the store first, and the final deletion
	// plan is rebuilt from the resulting terminal state.
	transitions, blockers, err := s.reconcileResetLifecycle(ctx, registration, resetStore)
	if err != nil {
		return ResetResult{Plan: plan}, err
	}
	if len(blockers) > 0 {
		return ResetResult{Plan: plan}, &ResetBlockedError{Blockers: blockers}
	}
	plan, blockers, err = s.buildResetPlan(ctx, registration, resetStore, lockPath)
	if err != nil {
		return ResetResult{Plan: plan, Lifecycle: transitions}, err
	}
	if len(blockers) > 0 {
		return ResetResult{Plan: plan, Lifecycle: transitions}, &ResetBlockedError{Blockers: blockers}
	}

	result := ResetResult{Plan: plan, Lifecycle: transitions}
	s.executeReset(ctx, registration, plan, &result)
	if len(result.Remaining) > 0 {
		return result, &ResetIncompleteError{Remaining: result.Remaining}
	}
	if err := resetStore.Close(); err != nil {
		result.Remaining = append(result.Remaining, ResetFailure{Target: "operational store", Reason: safeStatusCommentValue(err.Error())})
		return result, &ResetIncompleteError{Remaining: result.Remaining}
	}
	storeClosed = true
	s.removeResetStore(plan, &result)
	if len(result.Remaining) > 0 {
		return result, &ResetIncompleteError{Remaining: result.Remaining}
	}
	if err := os.Remove(plan.ConfigPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		result.Remaining = append(result.Remaining, ResetFailure{Target: "host configuration " + plan.ConfigPath, Reason: safeStatusCommentValue(err.Error())})
		return result, &ResetIncompleteError{Remaining: result.Remaining}
	}
	result.Removed = append(result.Removed, "host configuration "+plan.ConfigPath)
	return result, nil
}

// buildResetPlan derives every removable target from validated persisted
// identities and registered absolute paths. It performs no mutation, so both
// the preview and the post-lifecycle confirmation use it unchanged.
func (s *Service) buildResetPlan(ctx context.Context, registration config.RepositoryRegistration, resetStore ResetStore, lockPath string) (ResetPlan, []ResetBlocker, error) {
	candidates, err := resetStore.ListResetCandidates(ctx)
	if err != nil {
		return ResetPlan{}, nil, err
	}
	databasePath := filepath.Clean(registration.OperationalDataPath)
	plan := ResetPlan{
		ConfigPath:          s.configPath,
		RepositoryPath:      registration.Path,
		OperationalDataPath: databasePath,
		CoordinatorLock:     lockPath,
		ControlWorkspace:    defaultString(registration.Cmux.ControlWorkspace, "factory-control"),
		Retained:            append([]string(nil), resetRetainedResources...),
	}
	plan.DatabaseSidecars = existingDatabaseSidecars(databasePath)
	backups, err := databaseMigrationBackups(databasePath)
	if err != nil {
		return ResetPlan{}, nil, err
	}
	plan.MigrationBackups = backups
	// The evaluation projection lives in the same database, so reset reports
	// how many summaries disappear with it. Retention outside reset is
	// unchanged and stays owned by the explicit evaluation-delete command.
	if reader, ok := resetStore.(EvaluationReadStore); ok {
		summaries, summaryErr := reader.ListEvaluationSummaries(ctx, "")
		if summaryErr != nil {
			return ResetPlan{}, nil, summaryErr
		}
		plan.EvaluationSummaries = len(summaries)
	}

	var blockers []ResetBlocker
	seenCredentialStores := make(map[string]struct{})
	for _, candidate := range candidates {
		resources, reason := validateRunLocalResources(registration, candidate)
		if reason != "" {
			blockers = append(blockers, ResetBlocker{
				RunID:  candidate.Run.ID,
				Reason: reason,
				Action: "resolve the run's persisted state, then repeat factory reset",
			})
			continue
		}
		if blocker := s.resetLifecycleBlocker(ctx, registration, candidate.Run); blocker != nil {
			blockers = append(blockers, *blocker)
			continue
		}
		plan.Runs = append(plan.Runs, ResetRun{
			RunID:         candidate.Run.ID,
			Status:        candidate.Run.Status,
			Branch:        resources.Branch,
			Worktree:      resources.Worktree,
			GitProjection: runGitProjectionPath(resources.Worktree, candidate.Run.ID),
			WorkspaceIDs:  resources.WorkspaceIDs,
			WorkerIDs:     resources.WorkerIDs,
			Roles:         resources.Roles,
			StoredOutputs: resources.StoredOutputs,
		})
		for _, storeID := range resources.CredentialStoreIDs {
			if _, exists := seenCredentialStores[storeID]; exists {
				continue
			}
			seenCredentialStores[storeID] = struct{}{}
			plan.CredentialStores = append(plan.CredentialStores, ResetCredentialStore{RunID: candidate.Run.ID, CredentialStoreID: storeID})
		}
	}
	return plan, blockers, nil
}

// resetLifecycleBlocker reads the GitHub lifecycle of one non-terminal run and
// refuses reset while its issue or pull request is genuinely live. It uses the
// coordinator's single lifecycle rule set rather than a reset-specific
// interpretation, and it performs no mutation.
func (s *Service) resetLifecycleBlocker(ctx context.Context, registration config.RepositoryRegistration, run store.Run) *ResetBlocker {
	if store.IsTerminalStatus(run.Status) {
		return nil
	}
	repository := commandRepository(registration)
	issue, err := s.deps.GitHub.Issue(ctx, repository, run.IssueNumber)
	if err != nil {
		return &ResetBlocker{
			RunID:  run.ID,
			Reason: fmt.Sprintf("GitHub lifecycle for issue #%d could not be read", run.IssueNumber),
			Action: "restore GitHub access, then repeat factory reset",
		}
	}
	pullRequest, hasPullRequest, err := s.trackedPullRequest(ctx, registration, run)
	if err != nil {
		return &ResetBlocker{
			RunID:  run.ID,
			Reason: fmt.Sprintf("GitHub lifecycle for the tracked pull request of issue #%d could not be read", run.IssueNumber),
			Action: "restore GitHub access, then repeat factory reset",
		}
	}
	decision, err := classifyLifecycle(run, issue, pullRequest, hasPullRequest)
	if err != nil {
		return &ResetBlocker{
			RunID:  run.ID,
			Reason: safeStatusCommentValue(err.Error()),
			Action: "resolve the pull request on GitHub, then repeat factory reset",
		}
	}
	if decision.Outcome != LifecycleUnchanged {
		return nil
	}
	return &ResetBlocker{
		RunID:  run.ID,
		Reason: fmt.Sprintf("run %s is not terminal and its GitHub issue #%d or pull request is still live", run.ID, run.IssueNumber),
		Action: fmt.Sprintf("close or merge issue #%d and its pull request through the supervised workflow, or comment /factory cancel on the issue, then repeat factory reset", run.IssueNumber),
	}
}

// reconcileResetLifecycle carries every non-terminal run to its terminal
// outcome through the ordinary lifecycle projection and commits the result to
// the operational store before the final deletion plan is built. Deleting the
// store first would strand a merged or closed run with a running label on
// GitHub, because the status-comment and run identities live only here.
func (s *Service) reconcileResetLifecycle(ctx context.Context, registration config.RepositoryRegistration, resetStore ResetStore) ([]ResetLifecycleTransition, []ResetBlocker, error) {
	runStore, ok := resetStore.(RunStore)
	if !ok {
		return nil, nil, errors.New("operational store does not support run coordination")
	}
	candidates, err := resetStore.ListResetCandidates(ctx)
	if err != nil {
		return nil, nil, err
	}
	var transitions []ResetLifecycleTransition
	var blockers []ResetBlocker
	for _, candidate := range candidates {
		run := candidate.Run
		if store.IsTerminalStatus(run.Status) {
			continue
		}
		result, err := s.observeLifecycle(ctx, registration, runStore, &run)
		if err != nil {
			blockers = append(blockers, ResetBlocker{
				RunID:  run.ID,
				Reason: safeStatusCommentValue(fmt.Sprintf("lifecycle observation failed: %v", err)),
				Action: "restore GitHub access, then repeat factory reset",
			})
			continue
		}
		if result.Outcome == LifecycleUnchanged {
			blockers = append(blockers, ResetBlocker{
				RunID:  run.ID,
				Reason: fmt.Sprintf("run %s is not terminal and its GitHub issue #%d or pull request is still live", run.ID, run.IssueNumber),
				Action: fmt.Sprintf("close or merge issue #%d and its pull request through the supervised workflow, or comment /factory cancel on the issue, then repeat factory reset", run.IssueNumber),
			})
			continue
		}
		transitions = append(transitions, ResetLifecycleTransition{RunID: run.ID, Outcome: result.Outcome, Reason: result.Reason})
	}
	return transitions, blockers, nil
}

// checkResetAdapters verifies that every adapter the plan needs is available
// before deletion begins, so known unavailability cannot leave a half-reset
// installation behind.
func (s *Service) checkResetAdapters(ctx context.Context, registration config.RepositoryRegistration, plan ResetPlan) error {
	var blockers []ResetBlocker
	needsWorker := false
	needsTerminal := plan.ControlWorkspace != ""
	needsGit := false
	for _, run := range plan.Runs {
		needsGit = true
		if len(run.WorkerIDs) > 0 || len(run.Roles) > 0 || len(run.StoredOutputs) > 0 {
			needsWorker = true
		}
		if len(run.WorkspaceIDs) > 0 {
			needsTerminal = true
		}
	}
	if len(plan.CredentialStores) > 0 {
		needsWorker = true
	}
	if needsGit && s.gitWorkspace() == nil {
		blockers = append(blockers, ResetBlocker{Reason: "the Git workspace adapter is unavailable", Action: "restore the Git adapter, then repeat factory reset"})
	}
	if needsWorker {
		if _, ok := s.deps.Worker.(worker.CleanupRuntime); !ok {
			blockers = append(blockers, ResetBlocker{Reason: "the worker runtime cannot remove worker resources", Action: "restore the worker runtime, then repeat factory reset"})
		} else if checker, ok := s.deps.Worker.(interface{ CheckDocker(context.Context) error }); ok {
			if err := checker.CheckDocker(ctx); err != nil {
				blockers = append(blockers, ResetBlocker{Reason: safeStatusCommentValue(fmt.Sprintf("the worker runtime is unavailable: %v", err)), Action: "start the worker runtime, then repeat factory reset"})
			}
		}
	}
	if needsTerminal {
		terminalRuntime, err := s.resetTerminalRuntime(registration)
		if err != nil {
			blockers = append(blockers, ResetBlocker{Reason: safeStatusCommentValue(fmt.Sprintf("the terminal adapter is unavailable: %v", err)), Action: "start the terminal adapter, then repeat factory reset"})
		} else if checker, ok := terminalRuntime.(interface {
			CheckExecutable(context.Context) error
		}); ok {
			if err := checker.CheckExecutable(ctx); err != nil {
				blockers = append(blockers, ResetBlocker{Reason: safeStatusCommentValue(fmt.Sprintf("the terminal adapter is unavailable: %v", err)), Action: "start the terminal adapter, then repeat factory reset"})
			}
		}
	}
	if len(blockers) > 0 {
		return &ResetBlockedError{Blockers: blockers}
	}
	return nil
}

// executeReset removes every planned target except the operational store and
// host configuration, in the order that preserves retry information for as
// long as possible. Independent targets are attempted individually so one
// bounded failure cannot hide every later one.
func (s *Service) executeReset(ctx context.Context, registration config.RepositoryRegistration, plan ResetPlan, result *ResetResult) {
	terminalRuntime := s.resetTerminalRuntimeOrNil(registration)
	for _, run := range plan.Runs {
		for _, workspaceID := range run.WorkspaceIDs {
			s.closeResetWorkspace(ctx, terminalRuntime, workspaceID, result)
		}
	}
	if plan.ControlWorkspace != "" {
		s.closeResetControlWorkspace(ctx, terminalRuntime, plan.ControlWorkspace, result)
	}

	runtime, _ := s.deps.Worker.(worker.CleanupRuntime)
	for _, run := range plan.Runs {
		if runtime == nil {
			result.Remaining = append(result.Remaining, ResetFailure{Target: "worker resources for run " + run.RunID, Reason: "the worker runtime cannot remove worker resources"})
			continue
		}
		if err := runtime.Cleanup(ctx, worker.CleanupRequest{RunID: run.RunID, WorkerIDs: run.WorkerIDs, Roles: run.Roles, StoredOutputs: run.StoredOutputs}); err != nil {
			result.Remaining = append(result.Remaining, ResetFailure{Target: "worker resources for run " + run.RunID, Reason: safeStatusCommentValue(err.Error())})
			continue
		}
		result.Removed = append(result.Removed, "worker resources for run "+run.RunID)
	}
	remover, _ := s.deps.Worker.(worker.CredentialStoreRemover)
	for _, credentialStore := range plan.CredentialStores {
		target := "credential storage " + credentialStore.CredentialStoreID
		if remover == nil {
			result.Remaining = append(result.Remaining, ResetFailure{Target: target, Reason: "the worker runtime cannot remove factory-managed credential storage"})
			continue
		}
		if err := remover.RemoveCredentialStore(ctx, worker.RemoveCredentialStoreRequest{RunID: credentialStore.RunID, CredentialStoreID: credentialStore.CredentialStoreID}); err != nil {
			result.Remaining = append(result.Remaining, ResetFailure{Target: target, Reason: safeStatusCommentValue(err.Error())})
			continue
		}
		result.Removed = append(result.Removed, target)
	}

	workspace := s.gitWorkspace()
	for _, run := range plan.Runs {
		target := "Git workspace for run " + run.RunID
		if workspace == nil {
			result.Remaining = append(result.Remaining, ResetFailure{Target: target, Reason: "the Git workspace adapter is unavailable"})
			continue
		}
		if err := workspace.Remove(ctx, registration.Path, gitadapter.Workspace{RunID: run.RunID, Branch: run.Branch, Worktree: run.Worktree}); err != nil {
			result.Remaining = append(result.Remaining, ResetFailure{Target: target, Reason: safeStatusCommentValue(err.Error())})
			continue
		}
		result.Removed = append(result.Removed, target)
	}

	if err := removeCoordinatorLock(plan.CoordinatorLock); err != nil {
		result.Remaining = append(result.Remaining, ResetFailure{Target: "coordinator lock " + plan.CoordinatorLock, Reason: safeStatusCommentValue(err.Error())})
		return
	}
	result.Removed = append(result.Removed, "coordinator lock "+plan.CoordinatorLock)
}

// removeResetStore removes the operational database, its SQLite sidecars, and
// only the migration backups proven to belong to that exact database. It runs
// after every resource whose identity depended on the store.
func (s *Service) removeResetStore(plan ResetPlan, result *ResetResult) {
	targets := append([]string{plan.OperationalDataPath}, plan.DatabaseSidecars...)
	targets = append(targets, plan.MigrationBackups...)
	for _, path := range targets {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			result.Remaining = append(result.Remaining, ResetFailure{Target: "operational store file " + path, Reason: safeStatusCommentValue(err.Error())})
			continue
		}
		result.Removed = append(result.Removed, "operational store file "+path)
	}
}

// closeResetWorkspace closes one run terminal workspace. An absent workspace is
// a successful result, so a repeated reset completes idempotently.
func (s *Service) closeResetWorkspace(ctx context.Context, terminalRuntime terminal.TerminalRuntime, workspaceID string, result *ResetResult) {
	target := "terminal workspace " + workspaceID
	if terminalRuntime == nil {
		result.Remaining = append(result.Remaining, ResetFailure{Target: target, Reason: "the terminal adapter is unavailable"})
		return
	}
	// An unreachable terminal can accept the connection and never answer, so
	// each close is bounded rather than allowed to stall the reset.
	closeCtx, cancel := context.WithTimeout(ctx, resetWorkspaceCloseTimeout)
	err := terminalRuntime.CloseWorkspace(closeCtx, terminal.WorkspaceID(workspaceID))
	cancel()
	if err != nil {
		result.Remaining = append(result.Remaining, ResetFailure{Target: target, Reason: safeStatusCommentValue(err.Error())})
		return
	}
	result.Removed = append(result.Removed, target)
}

// closeResetControlWorkspace closes the registered control workspace. Version
// one registers exactly one repository per host configuration, so removing that
// configuration leaves no registration able to reference the workspace.
func (s *Service) closeResetControlWorkspace(ctx context.Context, terminalRuntime terminal.TerminalRuntime, name string, result *ResetResult) {
	target := "control workspace " + name
	if terminalRuntime == nil {
		result.Remaining = append(result.Remaining, ResetFailure{Target: target, Reason: "the terminal adapter is unavailable"})
		return
	}
	finder, ok := terminalRuntime.(terminal.WorkspaceFinder)
	if !ok {
		result.Remaining = append(result.Remaining, ResetFailure{Target: target, Reason: "the terminal adapter cannot resolve a workspace by name"})
		return
	}
	found, exists, err := finder.FindWorkspace(ctx, name)
	if err != nil {
		result.Remaining = append(result.Remaining, ResetFailure{Target: target, Reason: safeStatusCommentValue(err.Error())})
		return
	}
	if !exists {
		result.Removed = append(result.Removed, target)
		return
	}
	s.closeResetWorkspace(ctx, terminalRuntime, string(found.ID), result)
}

// resetTerminalRuntime resolves the terminal adapter without creating a
// workspace, so a preview stays free of terminal mutation.
func (s *Service) resetTerminalRuntime(registration config.RepositoryRegistration) (terminal.TerminalRuntime, error) {
	if s.deps.Terminal != nil {
		return s.deps.Terminal, nil
	}
	return s.lifecycleModule().ensureTerminalRuntime(registration.Cmux.SocketPath)
}

// resetTerminalRuntimeOrNil resolves the terminal adapter and reports an
// unavailable adapter as a nil runtime, so each affected target records its own
// bounded failure instead of aborting the whole reset.
func (s *Service) resetTerminalRuntimeOrNil(registration config.RepositoryRegistration) terminal.TerminalRuntime {
	terminalRuntime, err := s.resetTerminalRuntime(registration)
	if err != nil {
		return nil
	}
	return terminalRuntime
}

// runGitProjectionPath names the exact private Git metadata directory that the
// Git workspace adapter removes together with one run's worktree.
func runGitProjectionPath(worktree, runID string) string {
	return filepath.Join(filepath.Dir(filepath.Clean(worktree)), ".factory-git", runID)
}

// existingDatabaseSidecars names the SQLite sidecars that currently exist for
// one exact database path. Absent sidecars are omitted so the plan shows only
// real targets.
func existingDatabaseSidecars(databasePath string) []string {
	var sidecars []string
	for _, suffix := range resetDatabaseSidecarSuffixes {
		path := databasePath + suffix
		if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
			sidecars = append(sidecars, path)
		}
	}
	return sidecars
}

// databaseMigrationBackups returns only the migration backups whose name proves
// they belong to that exact database. The store writes them beside the database
// as "<database>.bak-<timestamp>", so a name prefix on the exact path is proof
// of ownership without a broad directory scan.
func databaseMigrationBackups(databasePath string) ([]string, error) {
	directory := filepath.Dir(databasePath)
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read operational store directory: %w", err)
	}
	prefix := filepath.Base(databasePath) + ".bak-"
	var backups []string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("inspect operational store backup: %w", err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("operational store backup %q is not a regular file", path)
		}
		backups = append(backups, path)
	}
	sort.Strings(backups)
	return backups, nil
}
