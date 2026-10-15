package factory_test

import (
	"context"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// listedRuns is a read-only operational store that lists fixed runs.
type listedRuns struct {
	runs []store.Run
}

// CurrentRun reports no active run.
func (l listedRuns) CurrentRun(context.Context) (*store.Run, error) { return nil, nil }

// Close releases nothing.
func (l listedRuns) Close() error { return nil }

// ListRuns returns the fixed runs, newest first.
func (l listedRuns) ListRuns(context.Context) ([]store.Run, error) { return l.runs, nil }

// onboardingProofService returns a service whose tracker answers with issue
// and whose read-only store lists runs.
func onboardingProofService(issue tracker.Issue, runs ...store.Run) *factory.Service {
	host := config.HostConfig{SchemaVersion: config.CurrentHostSchemaVersion, Repositories: []config.RepositoryRegistration{{
		Path: "/repo", GitHub: config.GitHubConfig{Owner: "example", Repository: "project"},
		OperationalDataPath: "/state/factory.db", RepositoryConfigPath: "/repo/factory.yaml",
	}}}
	return factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:            &fakeConfig{value: host},
		Tracker:           &fakeGitHub{issueValue: issue},
		OpenStoreReadOnly: func(context.Context, string) (factory.OperationalStore, error) { return listedRuns{runs: runs}, nil },
	})
}

// TestObserveOnboardingProofReadsTheIssueAndItsLatestRun verifies that the
// observation reports whether the issue carries the marker and the newest
// run of that issue, and ignores runs of other issues.
func TestObserveOnboardingProofReadsTheIssueAndItsLatestRun(t *testing.T) {
	service := onboardingProofService(
		tracker.Issue{Number: 7, Body: "Add a test.\n\nfactory-onboarding-proof:abc"},
		store.Run{ID: "other", IssueNumber: 8, Stage: store.StageReady, Status: store.StatusComplete},
		store.Run{ID: "newest", IssueNumber: 7, Stage: store.StageDraftPR, Status: store.StatusActive},
		store.Run{ID: "older", IssueNumber: 7, Stage: store.StageImplementation, Status: store.StatusFailed},
	)
	observation, err := service.ObserveOnboardingProof(context.Background(), 7, "factory-onboarding-proof:abc")
	if err != nil {
		t.Fatal(err)
	}
	if !observation.CarriesMarker || observation.Run == nil || observation.Run.ID != "newest" || !observation.ReachedDraftPullRequest() {
		t.Fatalf("observation = %+v, want the newest run at draft_pr", observation)
	}

	observation, err = service.ObserveOnboardingProof(context.Background(), 7, "factory-onboarding-proof:other")
	if err != nil || observation.CarriesMarker {
		t.Fatalf("observation = %+v, %v; want an issue without the marker", observation, err)
	}
}

// TestObserveOnboardingProofWithoutARun verifies that an issue the
// coordinator has not claimed yet has no run and has not passed.
func TestObserveOnboardingProofWithoutARun(t *testing.T) {
	service := onboardingProofService(tracker.Issue{Number: 7, Body: "factory-onboarding-proof:abc"})
	observation, err := service.ObserveOnboardingProof(context.Background(), 7, "factory-onboarding-proof:abc")
	if err != nil || !observation.CarriesMarker || observation.Run != nil || observation.ReachedDraftPullRequest() {
		t.Fatalf("observation = %+v, %v; want the marker and no run", observation, err)
	}
}
