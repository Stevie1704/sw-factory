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
	"github.com/Stevie1704/sw-factory/internal/worker"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

const (
	// CleanupRetention is the minimum age of a terminal run before ordinary
	// local run artifacts may be removed.
	CleanupRetention = 7 * 24 * time.Hour
)

// cleanupWorkerRoles covers every factory-declared role that can create a
// run-scoped worker home, including deterministic gate execution.
var cleanupWorkerRoles = factoryWorkerRoles()

// factoryWorkerRoles derives cleanup identities from the factory registry so a
// newly declared visible role cannot leave its worker home behind.
func factoryWorkerRoles() []string {
	roles := []string{"gate"}
	for _, definition := range workflow.DefaultRegistry().Roles() {
		roles = append(roles, definition.Name)
	}
	return roles
}

// CleanupStore is the operational-store seam needed to list and delete local
// run artifacts while keeping evaluation-summary deletion separate.
type CleanupStore interface {
	OperationalStore
	ListCleanupCandidates(context.Context, time.Time, string) ([]store.CleanupCandidate, error)
	BeginCleanup(context.Context, string, time.Time) (store.CleanupTransaction, error)
}

// CleanupRequest selects a run and whether the destructive operation is
// explicitly confirmed. An empty RunID considers all eligible runs.
type CleanupRequest struct {
	// RunID narrows cleanup to one run when set.
	RunID string
	// Confirm authorizes removal after the caller has displayed the plan.
	Confirm bool
	// Before reuses a previously displayed cutoff during confirmation. Zero
	// selects the current clock minus CleanupRetention.
	Before time.Time
	// ExpectedPlan makes confirmation fail closed if the plan changed between
	// preview and mutation.
	ExpectedPlan *CleanupPlan
}

// CleanupRun describes every local target selected for one cleanup operation.
type CleanupRun struct {
	// RunID identifies the run and its private worker resources.
	RunID string
	// Status is the terminal status observed when the plan was built.
	Status store.Status
	// EligibleAt is the terminal time used for the seven-day cutoff.
	EligibleAt time.Time
	// RepositoryPath is the registered repository owning the local branch.
	RepositoryPath string
	// Branch is the local factory branch to remove. Remote branches are never
	// addressed by this target.
	Branch string
	// Worktree is the exact local worktree path to remove.
	Worktree string
	// WorkerRunID is the logical worker identity passed to the runtime adapter;
	// Docker names remain private to that adapter.
	WorkerRunID string
	// WorkerIDs contains the exact worker identities for this run. Review
	// invocations use separate identities while ordinary roles use RunID.
	WorkerIDs []string
	// StoredOutputs contains the exact generated output directories to remove.
	StoredOutputs []string
	// Roles selects run-scoped role-home volumes for the worker adapter.
	Roles []string
}

// CleanupSkippedRun explains why an otherwise old terminal run was withheld
// from the destructive plan.
type CleanupSkippedRun struct {
	// RunID identifies the withheld run.
	RunID string
	// Reason is a bounded operator-facing safety explanation.
	Reason string
}

// CleanupPlan is the complete operator-visible cleanup preview.
type CleanupPlan struct {
	// Before is the inclusive upper bound for terminal eligibility.
	Before time.Time
	// Runs contains exact resources that would be removed.
	Runs []CleanupRun
	// Skipped contains terminal runs withheld because their identity or state
	// could not be validated safely.
	Skipped []CleanupSkippedRun
}

// CleanupResult contains the preview and any operational rows removed after
// confirmation.
type CleanupResult struct {
	// Plan is returned for both preview and confirmed cleanup.
	Plan CleanupPlan
	// Deleted contains one row-count projection for each removed run.
	Deleted []store.CleanupDeletionResult
}

// CleanupConfirmationRequiredError tells a CLI or embedder to display the
// returned plan and repeat with explicit confirmation.
type CleanupConfirmationRequiredError struct{}

// Error describes the required cleanup confirmation.
func (*CleanupConfirmationRequiredError) Error() string {
	return "cleanup requires explicit confirmation"
}

// CleanupBlockedError reports that at least one candidate was withheld and no
// destructive cleanup was attempted for the plan.
type CleanupBlockedError struct {
	// Runs contains every skipped candidate that blocked the operation.
	Runs []CleanupSkippedRun
}

// Error describes why confirmed cleanup was refused before any mutation.
func (e *CleanupBlockedError) Error() string {
	return fmt.Sprintf("cleanup blocked for %d run(s)", len(e.Runs))
}

// CleanupPlanChangedError tells the caller to display a fresh plan because
// persisted state changed after the previous preview.
type CleanupPlanChangedError struct{}

// Error describes the stale-plan refusal.
func (*CleanupPlanChangedError) Error() string { return "cleanup plan changed; preview it again" }

// Cleanup previews eligible local artifacts and removes them only when the
// caller explicitly confirms the complete plan. It reads the lifecycle of a
// tracked pull request but never mutates GitHub, deletes remote branches, or
// removes local evaluation summaries.
func (s *Service) Cleanup(ctx context.Context, request CleanupRequest) (CleanupResult, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()

	registration, cleanupStore, closeStore, err := s.openCleanupStore(ctx)
	if err != nil {
		return CleanupResult{}, err
	}
	defer func() { _ = closeStore() }()

	minimumBefore := s.deps.Now().UTC().Add(-CleanupRetention)
	before := request.Before.UTC()
	if request.Before.IsZero() {
		before = minimumBefore
	}
	if before.After(minimumBefore) {
		return CleanupResult{}, fmt.Errorf("cleanup cutoff %s is newer than the required seven-day cutoff %s", before.Format(time.RFC3339Nano), minimumBefore.Format(time.RFC3339Nano))
	}
	before = before.UTC()
	candidates, err := cleanupStore.ListCleanupCandidates(ctx, before, request.RunID)
	if err != nil {
		return CleanupResult{}, err
	}
	plan := s.buildCleanupPlan(ctx, registration, candidates, before)
	result := CleanupResult{Plan: plan}
	if request.Confirm && request.ExpectedPlan != nil && !cleanupPlansEqual(plan, *request.ExpectedPlan) {
		return result, &CleanupPlanChangedError{}
	}
	if !request.Confirm {
		return result, &CleanupConfirmationRequiredError{}
	}
	if len(plan.Skipped) > 0 {
		return result, &CleanupBlockedError{Runs: append([]CleanupSkippedRun(nil), plan.Skipped...)}
	}
	if len(plan.Runs) == 0 {
		return result, nil
	}

	runtime, ok := s.deps.Worker.(worker.CleanupRuntime)
	if !ok {
		return result, errors.New("worker runtime does not support cleanup")
	}
	workspace := s.gitWorkspace()
	if workspace == nil {
		return result, errors.New("GitWorkspace is required for cleanup")
	}
	for _, target := range plan.Runs {
		cleanupTransaction, err := cleanupStore.BeginCleanup(ctx, target.RunID, plan.Before)
		if err != nil {
			return result, fmt.Errorf("reserve cleanup for run %q: %w", target.RunID, err)
		}
		rollback := func() { _ = cleanupTransaction.Rollback() }
		if err := runtime.Cleanup(ctx, worker.CleanupRequest{RunID: target.RunID, WorkerIDs: target.WorkerIDs, Roles: target.Roles, StoredOutputs: target.StoredOutputs}); err != nil {
			rollback()
			return result, fmt.Errorf("clean worker for run %q: %w", target.RunID, err)
		}
		if err := workspace.Remove(ctx, registration.Path, gitadapter.Workspace{RunID: target.RunID, Branch: target.Branch, Worktree: target.Worktree}); err != nil {
			rollback()
			return result, fmt.Errorf("remove Git workspace for run %q: %w", target.RunID, err)
		}
		deleted, err := cleanupTransaction.Delete(ctx)
		if err != nil {
			rollback()
			return result, fmt.Errorf("delete operational state for run %q: %w", target.RunID, err)
		}
		if err := cleanupTransaction.Commit(); err != nil {
			rollback()
			return result, fmt.Errorf("commit cleanup for run %q: %w", target.RunID, err)
		}
		if deleted.Runs > 0 {
			result.Deleted = append(result.Deleted, deleted)
		}
	}
	return result, nil
}

// openCleanupStore loads the registered repository and opens its cleanup-capable
// local store without invoking terminal or harness adapters.
func (s *Service) openCleanupStore(ctx context.Context) (config.RepositoryRegistration, CleanupStore, func() error, error) {
	registration, err := s.registration()
	if err != nil {
		return config.RepositoryRegistration{}, nil, func() error { return nil }, err
	}
	opened, err := s.deps.OpenStore(ctx, registration.OperationalDataPath)
	if err != nil {
		return config.RepositoryRegistration{}, nil, func() error { return nil }, err
	}
	cleanupStore, ok := opened.(CleanupStore)
	if !ok {
		_ = opened.Close()
		return config.RepositoryRegistration{}, nil, func() error { return nil }, errors.New("operational store does not support cleanup")
	}
	return registration, cleanupStore, opened.Close, nil
}

// buildCleanupPlan validates persisted identity and derives only exact,
// run-scoped resources from each store candidate. It observes tracked pull
// requests so an open PR keeps its worktree and session state.
func (s *Service) buildCleanupPlan(ctx context.Context, registration config.RepositoryRegistration, candidates []store.CleanupCandidate, before time.Time) CleanupPlan {
	plan := CleanupPlan{Before: before.UTC()}
	for _, candidate := range candidates {
		target, reason := s.cleanupTarget(ctx, registration, candidate)
		if reason != "" {
			plan.Skipped = append(plan.Skipped, CleanupSkippedRun{RunID: candidate.Run.ID, Reason: reason})
			continue
		}
		plan.Runs = append(plan.Runs, target)
	}
	return plan
}

// cleanupTarget validates one candidate before it can enter a destructive
// plan. Every rejection is fail-closed so a malformed row cannot widen cleanup.
func (s *Service) cleanupTarget(ctx context.Context, registration config.RepositoryRegistration, candidate store.CleanupCandidate) (CleanupRun, string) {
	run := candidate.Run
	if !safeCleanupIdentifier(run.ID) {
		return CleanupRun{}, "run identifier is not a safe local identifier"
	}
	if candidate.PendingEffect != nil {
		return CleanupRun{}, fmt.Sprintf("pending external effect %q requires reconciliation", candidate.PendingEffect.ID)
	}
	if resolvePath(run.RepositoryPath) != resolvePath(registration.Path) {
		return CleanupRun{}, "run repository does not match the registered repository"
	}
	if !store.IsTerminalStatus(run.Status) {
		return CleanupRun{}, "run is no longer terminal"
	}
	if run.PullRequestNumber < 0 {
		return CleanupRun{}, "pull-request number is invalid"
	}
	if run.PullRequestNumber > 0 {
		pullRequest, found, err := s.trackedPullRequest(ctx, registration, run)
		if err != nil {
			return CleanupRun{}, fmt.Sprintf("cannot verify tracked pull request: %v", err)
		}
		if !found {
			return CleanupRun{}, "tracked pull request could not be observed"
		}
		if pullRequest.Merged {
			// A merged pull request is terminal and its local resources are
			// eligible once the seven-day retention cutoff is reached.
		} else if strings.EqualFold(strings.TrimSpace(pullRequest.State), "open") {
			return CleanupRun{}, "pull request is still open"
		} else if !strings.EqualFold(strings.TrimSpace(pullRequest.State), "closed") {
			return CleanupRun{}, "pull request lifecycle is not conclusively terminal"
		}
	}
	expectedBranch := "factory/" + run.ID
	if run.Branch != expectedBranch {
		return CleanupRun{}, fmt.Sprintf("branch %q is not the run-owned branch %q", run.Branch, expectedBranch)
	}
	if !filepath.IsAbs(run.Worktree) || filepath.Base(filepath.Clean(run.Worktree)) != run.ID {
		return CleanupRun{}, "worktree is not an absolute run-scoped path"
	}
	worktree := filepath.Clean(run.Worktree)
	if pathWithin(registration.Path, worktree) || pathWithin(worktree, registration.Path) {
		return CleanupRun{}, "worktree overlaps the registered repository"
	}
	if reason := cleanupDirectoryTarget(worktree, "worktree"); reason != "" {
		return CleanupRun{}, reason
	}

	storedOutputs := make([]string, 0, len(candidate.Invocations)*2)
	seenOutputs := make(map[string]struct{}, len(candidate.Invocations)*2)
	for _, invocation := range candidate.Invocations {
		for _, path := range []string{invocation.InvocationDirectory, invocation.ResultDirectory} {
			if path == "" {
				continue
			}
			clean := filepath.Clean(path)
			if pathWithin(registration.Path, clean) {
				return CleanupRun{}, "stored output overlaps the registered repository"
			}
			if _, exists := seenOutputs[clean]; !exists {
				seenOutputs[clean] = struct{}{}
				storedOutputs = append(storedOutputs, clean)
			}
		}
	}
	if err := worker.ValidateCleanupStoredOutputs(run.ID, storedOutputs); err != nil {
		return CleanupRun{}, err.Error()
	}
	sort.Strings(storedOutputs)

	roles := append([]string(nil), cleanupWorkerRoles...)
	roleSet := make(map[string]struct{}, len(roles))
	for _, role := range roles {
		roleSet[role] = struct{}{}
	}
	workerIDs := []string{run.ID}
	seenWorkerIDs := map[string]struct{}{run.ID: {}}
	for _, invocation := range candidate.Invocations {
		if !safeCleanupIdentifier(invocation.Role) {
			return CleanupRun{}, fmt.Sprintf("invocation %q has an unsafe worker role", invocation.ID)
		}
		workerID := workerIDForInvocation(invocation)
		if !safeCleanupIdentifier(workerID) {
			return CleanupRun{}, fmt.Sprintf("invocation %q has an unsafe worker identity", invocation.ID)
		}
		if _, exists := seenWorkerIDs[workerID]; !exists {
			seenWorkerIDs[workerID] = struct{}{}
			workerIDs = append(workerIDs, workerID)
		}
		if _, exists := roleSet[invocation.Role]; !exists {
			roleSet[invocation.Role] = struct{}{}
			roles = append(roles, invocation.Role)
		}
	}
	sort.Strings(workerIDs)
	sort.Strings(roles)
	eligibleAt := run.TerminalAt
	if eligibleAt.IsZero() {
		eligibleAt = run.UpdatedAt
	}
	if eligibleAt.IsZero() {
		return CleanupRun{}, "run has no terminal timestamp"
	}
	return CleanupRun{
		RunID:          run.ID,
		Status:         run.Status,
		EligibleAt:     eligibleAt.UTC(),
		RepositoryPath: registration.Path,
		Branch:         run.Branch,
		Worktree:       worktree,
		WorkerRunID:    run.ID,
		WorkerIDs:      workerIDs,
		StoredOutputs:  storedOutputs,
		Roles:          roles,
	}, ""
}

// cleanupDirectoryTarget verifies that an exact planned directory is safe to
// remove. Missing directories are valid because a prior partial cleanup may
// already have removed them.
func cleanupDirectoryTarget(path, label string) string {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) {
		return label + " is not a safe absolute directory"
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		return fmt.Sprintf("inspect %s: %v", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return label + " must not be a symbolic link"
	}
	if !info.IsDir() {
		return label + " is not a directory"
	}
	return ""
}

// safeCleanupIdentifier accepts only one path component suitable for both
// generated directory names and the worker runtime's run identity.
func safeCleanupIdentifier(value string) bool {
	if value == "" || value == "." || value == ".." || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

// cleanupPlansEqual compares the operator-visible target identity used to
// protect a two-step preview-and-confirm command from a stale plan.
func cleanupPlansEqual(left, right CleanupPlan) bool {
	if !left.Before.Equal(right.Before) || len(left.Runs) != len(right.Runs) || len(left.Skipped) != len(right.Skipped) {
		return false
	}
	for index := range left.Runs {
		leftRun, rightRun := left.Runs[index], right.Runs[index]
		if leftRun.RunID != rightRun.RunID || leftRun.Status != rightRun.Status || !leftRun.EligibleAt.Equal(rightRun.EligibleAt) || leftRun.RepositoryPath != rightRun.RepositoryPath || leftRun.Branch != rightRun.Branch || leftRun.Worktree != rightRun.Worktree || leftRun.WorkerRunID != rightRun.WorkerRunID || !stringSlicesEqual(leftRun.WorkerIDs, rightRun.WorkerIDs) || !stringSlicesEqual(leftRun.StoredOutputs, rightRun.StoredOutputs) || !stringSlicesEqual(leftRun.Roles, rightRun.Roles) {
			return false
		}
	}
	for index := range left.Skipped {
		if left.Skipped[index] != right.Skipped[index] {
			return false
		}
	}
	return true
}

// stringSlicesEqual compares ordered cleanup target fields without exposing a
// mutable alias through the plan comparison.
func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
