package webui

import (
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// stageLine is the main stage sequence the UI draws for every run, in the
// order a run passes through it, with the label each station shows. The
// standards review shares the review station.
var stageLine = []struct {
	Stage store.Stage
	Label string
}{
	{store.StageClaim, "claim"},
	{store.StagePreflight, "preflight"},
	{store.StageArchitecture, "architecture"},
	{store.StageTest, "test"},
	{store.StageImplementation, "implementation"},
	{store.StageCheck, "check"},
	{store.StageDraftPR, "draft PR"},
	{store.StageReview, "review"},
	{store.StageReady, "ready"},
}

// stationState is where a run stands relative to one station.
type stationState string

const (
	stationDone     stationState = "done"
	stationCurrent  stationState = "current"
	stationAhead    stationState = "ahead"
	stationBypassed stationState = "bypassed"
)

// Spoken describes the state for assistive technology, which cannot see the
// station colors.
func (s stationState) Spoken() string {
	switch s {
	case stationDone:
		return "done"
	case stationCurrent:
		return "current stage"
	case stationBypassed:
		return "skipped by this run"
	default:
		return "not reached"
	}
}

// station is one stage of a run's track.
type station struct {
	Label string
	State stationState
}

// Current reports whether the run is at this station.
func (s station) Current() bool {
	return s.State == stationCurrent
}

// stageTrack places a run on the stage line. The architecture station is
// bypassed unless the route selects it, and the test station when the run
// skips the independent test stage. A complete run has every other station
// done; a stage outside the line marks no station as current.
func stageTrack(run store.Run, route workflow.Route, testStageBypassed bool) []station {
	position := stationIndex(run.Stage)
	if run.Status == store.StatusComplete {
		position = len(stageLine)
	}
	track := make([]station, len(stageLine))
	for index, entry := range stageLine {
		bypassed := (entry.Stage == store.StageArchitecture && route != workflow.RouteDesignAcceptance) ||
			(entry.Stage == store.StageTest && testStageBypassed)
		track[index] = station{Label: entry.Label, State: stationStateAt(index, position, bypassed)}
	}
	return track
}

// stationStateAt returns the state of the station at index for a run at
// position, where a negative position means the run is off the line.
func stationStateAt(index, position int, bypassed bool) stationState {
	switch {
	case bypassed:
		return stationBypassed
	case position < 0 || index > position:
		return stationAhead
	case index < position:
		return stationDone
	default:
		return stationCurrent
	}
}

// stationIndex returns the line position of a stage, or -1 when the stage is
// not on the line.
func stationIndex(stage store.Stage) int {
	if stage == workflow.StageStandardsReview {
		stage = store.StageReview
	}
	for index, entry := range stageLine {
		if entry.Stage == stage {
			return index
		}
	}
	return -1
}

// budgetGauge is one repair budget of a run: the attempts used and the frozen
// maximum.
type budgetGauge struct {
	Used   int
	Budget int
}

// newBudgetGauge builds the gauge of one budget for a template.
func newBudgetGauge(used, budget int) budgetGauge {
	return budgetGauge{Used: used, Budget: budget}
}

// Segments returns one entry per budget unit, true for each used unit.
// Attempts above the budget fill every segment.
func (g budgetGauge) Segments() []bool {
	segments := make([]bool, max(g.Budget, 0))
	for index := range segments {
		segments[index] = index < g.Used
	}
	return segments
}
