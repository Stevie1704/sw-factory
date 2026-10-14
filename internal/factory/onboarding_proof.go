package factory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// OnboardingProofObservation is the read-only observation of the
// disposable issue that proves an onboarding.
type OnboardingProofObservation struct {
	// CarriesMarker reports that the issue body contains the proof marker,
	// so the issue is the one the onboarding plan started.
	CarriesMarker bool
	// Run is the newest run of the issue, or nil before a claim.
	Run *store.Run
}

// ReachedDraftPullRequest reports whether the run of the proof issue opened
// its draft pull request, which is the success of the proof.
func (o OnboardingProofObservation) ReachedDraftPullRequest() bool {
	return o.Run != nil && slices.Contains([]store.Stage{store.StageDraftPR, store.StageReview, store.StageReady}, o.Run.Stage) && o.Run.Status != store.StatusFailed && o.Run.Status != store.StatusCancelled
}

// runLister lists every persisted run, newest first.
type runLister interface {
	ListRuns(context.Context) ([]store.Run, error)
}

// ObserveOnboardingProof reads one issue from the tracker and its newest run
// from the operational store, without changing either. The marker ties the
// issue to the proof that the onboarding plan started.
func (s *Service) ObserveOnboardingProof(ctx context.Context, issueNumber int, marker string) (OnboardingProofObservation, error) {
	if issueNumber <= 0 || strings.TrimSpace(marker) == "" {
		return OnboardingProofObservation{}, errors.New("a positive issue number and a proof marker are required")
	}
	registration, err := s.registration()
	if err != nil {
		return OnboardingProofObservation{}, err
	}
	issue, err := s.deps.Tracker.Issue(ctx, registeredRepository(registration), issueNumber)
	if err != nil {
		return OnboardingProofObservation{}, fmt.Errorf("read the proof issue #%d: %w", issueNumber, err)
	}
	observation := OnboardingProofObservation{CarriesMarker: strings.Contains(issue.Body, marker)}
	if err := validateOperationalPath(registration.Path, registration.OperationalDataPath); err != nil {
		return OnboardingProofObservation{}, err
	}
	opened, err := s.deps.OpenStoreReadOnly(ctx, registration.OperationalDataPath)
	if err != nil {
		return OnboardingProofObservation{}, fmt.Errorf("open operational store read-only: %w", err)
	}
	defer func() { _ = opened.Close() }()
	lister, ok := opened.(runLister)
	if !ok {
		return OnboardingProofObservation{}, errors.New("operational store does not list runs")
	}
	runs, err := lister.ListRuns(ctx)
	if err != nil {
		return OnboardingProofObservation{}, err
	}
	for _, run := range runs {
		if run.IssueNumber == issueNumber {
			observation.Run = &run
			break
		}
	}
	return observation, nil
}
