package factory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// assigningTracker is a tracker whose work items must name their target
// repository, like the Azure DevOps extension route (ADR 0030).
type assigningTracker struct {
	*fakeGitHub
	assignment *tracker.RepositoryAssignment
	err        error
	calls      int
	// labels records the issue labels the claim check received.
	labels []string
}

// ClaimRepositoryAssignment returns the scripted validation result.
func (a *assigningTracker) ClaimRepositoryAssignment(_ context.Context, _ tracker.Repository, issue tracker.Issue) (*tracker.RepositoryAssignment, error) {
	a.calls++
	a.labels = append([]string(nil), issue.Labels...)
	return a.assignment, a.err
}

// assignedIssue is an authorized work item with an assignment tag and a plan
// correlation tag.
func assignedIssue() tracker.Issue {
	return tracker.Issue{Number: 42, Title: "Assigned", State: "open", Labels: []string{"factory-plan:s/1/a", "factory-repo:3b1f6a2e-8c4d-4e5f-9a6b-7c8d9e0f1a2b", tracker.LabelAgentReady}}
}

// newClaimServiceWithTracker is newClaimService with an assigning tracker.
func newClaimServiceWithTracker(adapter *assigningTracker, worktree *fakeWorktree, runStore *fakeRunStore) *factory.Service {
	host := config.HostConfig{SchemaVersion: config.CurrentHostSchemaVersion, Repositories: []config.RepositoryRegistration{{
		Path:                 "/repo",
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		AuthorizedUsers:      []string{"alice"},
		OperationalDataPath:  "/outside/factory.db",
		RepositoryConfigPath: "/repo/factory.yaml",
	}}}
	return factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:         &fakeConfig{value: host},
		OpenStore:      func(context.Context, string) (factory.OperationalStore, error) { return runStore, nil },
		LoadRepository: func(string) (config.RepositoryConfig, error) { return validRepositoryConfig(), nil },
		Tracker:        adapter,
		Worktree:       worktree,
		Now:            func() time.Time { return time.Date(2026, 8, 20, 10, 11, 12, 0, time.UTC) },
		NewRunID:       func() (string, error) { return "run-fixed", nil },
		Coordinator:    "coordinator-test",
	})
}

// claimWorktree returns a deterministic claim workspace.
func claimWorktree() *fakeWorktree {
	return &fakeWorktree{workspace: gitadapter.Workspace{BaseSHA: "base", Branch: "factory/run-fixed", Worktree: "/worktree/run-fixed"}}
}

// TestClaimFreezesTheValidatedRepositoryAssignmentWithTheRun verifies that
// the claim validates the assignment and stores the validated identity on
// every saved run record, and that the state-tag update keeps the
// assignment and correlation tags.
func TestClaimFreezesTheValidatedRepositoryAssignmentWithTheRun(t *testing.T) {
	t.Parallel()

	assignment := &tracker.RepositoryAssignment{Organization: "contoso", ProjectID: "project-guid", Project: "Factory Pilot", RepositoryID: "3b1f6a2e-8c4d-4e5f-9a6b-7c8d9e0f1a2b", Repository: "service"}
	adapter := &assigningTracker{fakeGitHub: &fakeGitHub{issueValue: assignedIssue()}, assignment: assignment}
	runStore := &fakeRunStore{}
	service := newClaimServiceWithTracker(adapter, claimWorktree(), runStore)

	result, err := service.ClaimIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("ClaimIssue() error = %v", err)
	}
	want := store.TrackerRepository{Organization: "contoso", ProjectID: "project-guid", Project: "Factory Pilot", RepositoryID: assignment.RepositoryID, Repository: "service"}
	if result.Run.TrackerRepository == nil || *result.Run.TrackerRepository != want {
		t.Fatalf("run tracker repository = %+v, want %+v", result.Run.TrackerRepository, want)
	}
	for index, saved := range runStore.saved {
		if saved.TrackerRepository == nil || *saved.TrackerRepository != want {
			t.Fatalf("saved run %d tracker repository = %+v, want it frozen from the first save", index, saved.TrackerRepository)
		}
	}
	if adapter.calls != 1 || len(adapter.labels) != 3 {
		t.Fatalf("assignment checks = %d with labels %v, want one check of the claimed issue", adapter.calls, adapter.labels)
	}
	if got, want := adapter.replacedLabels, []string{"factory-plan:s/1/a", "factory-repo:3b1f6a2e-8c4d-4e5f-9a6b-7c8d9e0f1a2b", tracker.LabelAgentRunning}; !equalStrings(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
}

// TestClaimRefusesAnInvalidRepositoryAssignmentBeforeAnyEffect verifies that
// a refused assignment stops the claim before a worktree, a run record, or a
// tracker write exists, and returns the adapter's repair diagnostic.
func TestClaimRefusesAnInvalidRepositoryAssignmentBeforeAnyEffect(t *testing.T) {
	t.Parallel()

	refusal := errors.New("the work item has no repository assignment; add the tag factory-repo:3b1f6a2e-8c4d-4e5f-9a6b-7c8d9e0f1a2b")
	adapter := &assigningTracker{fakeGitHub: &fakeGitHub{issueValue: assignedIssue()}, err: refusal}
	worktree := claimWorktree()
	runStore := &fakeRunStore{}

	_, err := newClaimServiceWithTracker(adapter, worktree, runStore).ClaimIssue(context.Background(), 42)
	if !errors.Is(err, refusal) {
		t.Fatalf("ClaimIssue() error = %v, want the assignment refusal", err)
	}
	if worktree.called || len(runStore.saved) != 0 || len(adapter.replacedHistory) != 0 || len(adapter.createdComments) != 0 {
		t.Fatalf("claim effects after refusal: worktree=%t runs=%d labels=%d comments=%d", worktree.called, len(runStore.saved), len(adapter.replacedHistory), len(adapter.createdComments))
	}
}

// TestClaimOnARouteWithoutAssignmentKeepsTheRunUnassigned verifies that a
// route that needs no assignment leaves the run's tracker repository nil.
func TestClaimOnARouteWithoutAssignmentKeepsTheRunUnassigned(t *testing.T) {
	t.Parallel()

	adapter := &assigningTracker{fakeGitHub: &fakeGitHub{issueValue: assignedIssue()}}
	result, err := newClaimServiceWithTracker(adapter, claimWorktree(), &fakeRunStore{}).ClaimIssue(context.Background(), 42)
	if err != nil || result.Run.TrackerRepository != nil {
		t.Fatalf("ClaimIssue() = %+v, %v, want an unassigned run", result.Run.TrackerRepository, err)
	}
}
