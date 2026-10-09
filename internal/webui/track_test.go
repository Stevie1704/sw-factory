package webui

import (
	"slices"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// states returns the station states of a track in line order.
func states(track []station) []string {
	result := make([]string, len(track))
	for index, item := range track {
		result[index] = item.State
	}
	return result
}

// TestStageTrackPlacesTheRunOnTheLine verifies the stages before the run's
// stage are done, its stage is current, and the later stages are ahead.
func TestStageTrackPlacesTheRunOnTheLine(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		run   store.Run
		route workflow.Route
		want  []string
	}{
		"default route at check": {
			run:  store.Run{Stage: store.StageCheck, Status: store.StatusWaitingForHuman},
			want: []string{"done", "done", "bypassed", "done", "done", "current", "ahead", "ahead", "ahead"},
		},
		"design route at architecture": {
			run:   store.Run{Stage: store.StageArchitecture, Status: store.StatusActive},
			route: workflow.RouteDesignAcceptance,
			want:  []string{"done", "done", "current", "ahead", "ahead", "ahead", "ahead", "ahead", "ahead"},
		},
		"standards review shares the review station": {
			run:  store.Run{Stage: workflow.StageStandardsReview, Status: store.StatusActive},
			want: []string{"done", "done", "bypassed", "done", "done", "done", "done", "current", "ahead"},
		},
		"complete run": {
			run:  store.Run{Stage: store.StageReady, Status: store.StatusComplete},
			want: []string{"done", "done", "bypassed", "done", "done", "done", "done", "done", "done"},
		},
		"stage off the line": {
			run:  store.Run{Stage: "unknown", Status: store.StatusActive},
			want: []string{"ahead", "ahead", "bypassed", "ahead", "ahead", "ahead", "ahead", "ahead", "ahead"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := states(stageTrack(test.run, test.route)); !slices.Equal(got, test.want) {
				t.Errorf("stageTrack states = %v, want %v", got, test.want)
			}
		})
	}
}

// TestStageTrackLabelsThePullRequestStage verifies the draft_pr stage reads
// as words.
func TestStageTrackLabelsThePullRequestStage(t *testing.T) {
	t.Parallel()

	track := stageTrack(store.Run{Stage: store.StageClaim}, workflow.RouteDefault)
	if got := track[stationIndex(store.StageDraftPR)].Label; got != "draft PR" {
		t.Errorf("draft_pr label = %q, want %q", got, "draft PR")
	}
}

// TestGaugeSegmentsFillTheUsedBudget verifies one segment per budget unit,
// filled for each used unit and never more than the budget.
func TestGaugeSegmentsFillTheUsedBudget(t *testing.T) {
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
		if got := gaugeSegments(test.used, test.budget); !slices.Equal(got, test.want) {
			t.Errorf("gaugeSegments(%d, %d) = %v, want %v", test.used, test.budget, got, test.want)
		}
	}
}
