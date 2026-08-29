package factory

import (
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// RunActivity is the operator-facing distinction between a run that exists, a
// run whose harness invocation is executing, and a run that is parked in a
// waiting state. The single `agent-running` GitHub label cannot express it,
// so unattended progression publishes this value beside the label.
type RunActivity string

const (
	// ActivityInvocationActive means one harness invocation is executing for
	// the run and the coordinator is waiting for its structured result.
	ActivityInvocationActive RunActivity = "invocation-active"
	// ActivityRunActive means the run is active and the coordinator owns the
	// next transition; no harness is executing.
	ActivityRunActive RunActivity = "run-active"
	// ActivityWaitingForHuman means unattended progression stopped until a
	// person answers, disposes, or reconciles the run.
	ActivityWaitingForHuman RunActivity = "waiting-for-human"
	// ActivityWaitingForHarness means unattended progression stopped until
	// harness or worker infrastructure recovers.
	ActivityWaitingForHarness RunActivity = "waiting-for-harness"
	// ActivityTerminal means the run has no further coordinator progression.
	ActivityTerminal RunActivity = "terminal"
)

// RunActivityFor derives the operator-facing activity of one persisted run.
// It reads only the run projection, so the status comment stays a pure
// function of the record that restart reconciliation compares.
func RunActivityFor(run store.Run) RunActivity {
	if store.IsTerminalStatus(run.Status) {
		return ActivityTerminal
	}
	switch run.Status {
	case store.StatusWaitingForHuman:
		return ActivityWaitingForHuman
	case store.StatusWaitingForHarness:
		return ActivityWaitingForHarness
	}
	if run.ActiveInvocationID != "" || len(run.ActiveInvocationIDs) > 0 {
		return ActivityInvocationActive
	}
	return ActivityRunActive
}

// activityStatusComment renders the activity distinction for the single
// editable supervision comment. The invocation identity appears only while
// that invocation is the one the run currently delegates to.
func activityStatusComment(run store.Run) string {
	activity := RunActivityFor(run)
	if activity != ActivityInvocationActive {
		return fmt.Sprintf("- activity: `%s`\n", activity)
	}
	ids := append([]string(nil), run.ActiveInvocationIDs...)
	if len(ids) == 0 && run.ActiveInvocationID != "" {
		ids = []string{run.ActiveInvocationID}
	}
	if len(ids) == 1 {
		return fmt.Sprintf("- activity: `%s`\n- active invocation: `%s`\n", activity, safeStatusCommentValue(ids[0]))
	}
	safeIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		safeIDs = append(safeIDs, "`"+safeStatusCommentValue(id)+"`")
	}
	return fmt.Sprintf("- activity: `%s`\n- active invocations: %s\n", activity, strings.Join(safeIDs, ", "))
}
