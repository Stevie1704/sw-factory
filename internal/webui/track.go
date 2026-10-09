package webui

import (
	"strings"

	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// line is the main stage sequence the UI draws for every run, in the order a
// run passes through it. The standards review shares the review station.
var line = []store.Stage{
	store.StageClaim,
	store.StagePreflight,
	store.StageArchitecture,
	store.StageTest,
	store.StageImplementation,
	store.StageCheck,
	store.StageDraftPR,
	store.StageReview,
	store.StageReady,
}

// Station states a stage track can show.
const (
	stationDone     = "done"
	stationCurrent  = "current"
	stationAhead    = "ahead"
	stationBypassed = "bypassed"
)

// station is one stage of a run's track.
type station struct {
	Label string
	// State is done, current, ahead, or bypassed.
	State string
}

// stageTrack places a run on the main stage line. Stages before the run's
// stage are done, a complete run has every stage done, and the architecture
// stage is bypassed unless the route selects it. A stage outside the line
// marks no station as current.
func stageTrack(run store.Run, route workflow.Route) []station {
	position := stationIndex(run.Stage)
	if run.Status == store.StatusComplete {
		position = len(line)
	}
	track := make([]station, len(line))
	for index, stage := range line {
		state := stationAhead
		switch {
		case stage == store.StageArchitecture && route != workflow.RouteDesignAcceptance:
			state = stationBypassed
		case position < 0:
		case index < position:
			state = stationDone
		case index == position:
			state = stationCurrent
		}
		track[index] = station{Label: strings.ReplaceAll(string(stage), "_pr", " PR"), State: state}
	}
	return track
}

// stationIndex returns the line position of a stage, or -1 when the stage is
// not on the line.
func stationIndex(stage store.Stage) int {
	if stage == workflow.StageStandardsReview {
		stage = store.StageReview
	}
	for index, candidate := range line {
		if candidate == stage {
			return index
		}
	}
	return -1
}

// gaugeSegments returns one entry per budget unit, true for each used unit.
// Attempts above the budget fill every segment.
func gaugeSegments(used, budget int) []bool {
	segments := make([]bool, max(budget, 0))
	for index := range segments {
		segments[index] = index < used
	}
	return segments
}
