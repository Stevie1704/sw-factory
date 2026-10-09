package webui

import (
	"slices"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// states returns the station states of a track in line order.
func states(track []station) []stationState {
	result := make([]stationState, len(track))
	for index, item := range track {
		result[index] = item.State
	}
	return result
}

// TestStageTrackPlacesTheRunOnTheLine verifies the stages before the run's
// stage are done, its stage is current, the later stages are ahead, and the
// stages the run skips are bypassed.
func TestStageTrackPlacesTheRunOnTheLine(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		run          store.Run
		route        workflow.Route
		testBypassed bool
		want         []stationState
	}{
		"default route at check": {
			run:  store.Run{Stage: store.StageCheck, Status: store.StatusWaitingForHuman},
			want: []stationState{"done", "done", "bypassed", "done", "done", "current", "ahead", "ahead", "ahead"},
		},
		"design route at architecture": {
			run:   store.Run{Stage: store.StageArchitecture, Status: store.StatusActive},
			route: workflow.RouteDesignAcceptance,
			want:  []stationState{"done", "done", "current", "ahead", "ahead", "ahead", "ahead", "ahead", "ahead"},
		},
		"standards review shares the review station": {
			run:  store.Run{Stage: workflow.StageStandardsReview, Status: store.StatusActive},
			want: []stationState{"done", "done", "bypassed", "done", "done", "done", "done", "current", "ahead"},
		},
		"complete run": {
			run:  store.Run{Stage: store.StageReady, Status: store.StatusComplete},
			want: []stationState{"done", "done", "bypassed", "done", "done", "done", "done", "done", "done"},
		},
		"test stage skipped": {
			run:          store.Run{Stage: store.StageImplementation, Status: store.StatusActive},
			testBypassed: true,
			want:         []stationState{"done", "done", "bypassed", "bypassed", "current", "ahead", "ahead", "ahead", "ahead"},
		},
		"stage off the line": {
			run:  store.Run{Stage: "unknown", Status: store.StatusActive},
			want: []stationState{"ahead", "ahead", "bypassed", "ahead", "ahead", "ahead", "ahead", "ahead", "ahead"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := states(stageTrack(test.run, test.route, test.testBypassed)); !slices.Equal(got, test.want) {
				t.Errorf("stageTrack states = %v, want %v", got, test.want)
			}
		})
	}
}

// TestStageTrackLabelsThePullRequestStage verifies the draft_pr stage reads
// as words.
func TestStageTrackLabelsThePullRequestStage(t *testing.T) {
	t.Parallel()

	track := stageTrack(store.Run{Stage: store.StageClaim}, workflow.RouteDefault, false)
	if got := track[stationIndex(store.StageDraftPR)].Label; got != "draft PR" {
		t.Errorf("draft_pr label = %q, want %q", got, "draft PR")
	}
}

// TestStationStatesAreSpoken verifies every station state has words for
// assistive technology, which cannot see the station colors.
func TestStationStatesAreSpoken(t *testing.T) {
	t.Parallel()

	for state, want := range map[stationState]string{
		stationDone:     "done",
		stationCurrent:  "current stage",
		stationAhead:    "not reached",
		stationBypassed: "skipped by this run",
	} {
		if got := state.Spoken(); got != want {
			t.Errorf("%s spoken = %q, want %q", state, got, want)
		}
	}
}

// TestBudgetGaugeFillsTheUsedBudget verifies one segment per budget unit,
// filled for each used unit and never more than the budget.
func TestBudgetGaugeFillsTheUsedBudget(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		used, budget int
		want         []bool
	}{
		{used: 2, budget: 3, want: []bool{true, true, false}},
		{used: 5, budget: 2, want: []bool{true, true}},
		{used: 0, budget: 0, want: []bool{}},
		{used: 1, budget: -1, want: []bool{}},
	} {
		if got := newBudgetGauge(test.used, test.budget).Segments(); !slices.Equal(got, test.want) {
			t.Errorf("budget %d / %d segments = %v, want %v", test.used, test.budget, got, test.want)
		}
	}
}
