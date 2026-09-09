package factory

import (
	"context"
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
	if len(run.ActiveInvocationIDs) > 0 {
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
	if len(ids) == 1 {
		return fmt.Sprintf("- activity: `%s`\n- active invocation: `%s`\n", activity, safeStatusCommentValue(ids[0]))
	}
	safeIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		safeIDs = append(safeIDs, "`"+safeStatusCommentValue(id)+"`")
	}
	return fmt.Sprintf("- activity: `%s`\n- active invocations: %s\n", activity, strings.Join(safeIDs, ", "))
}

// ActiveInvocationStore is the optional restart-safe lookup used to prevent
// duplicate visible sessions for one active run.
type ActiveInvocationStore interface {
	ActiveInvocation(context.Context, string) (*store.Invocation, error)
}

// ActiveInvocationsStore is the optional restart-safe projection used by the
// concurrent review round. Implementations return every active invocation,
// while ActiveInvocationStore remains the compatibility fallback.
type ActiveInvocationsStore interface {
	ActiveInvocations(context.Context, string) ([]store.Invocation, error)
}

// activeInvocationsForRun loads all active invocations without requiring older
// embedding stores to implement the concurrent projection.
func activeInvocationsForRun(ctx context.Context, value interface{}, runID string) ([]store.Invocation, bool, error) {
	if activeStore, ok := value.(ActiveInvocationsStore); ok {
		active, err := activeStore.ActiveInvocations(ctx, runID)
		return active, true, err
	}
	if activeStore, ok := value.(ActiveInvocationStore); ok {
		active, err := activeStore.ActiveInvocation(ctx, runID)
		if err != nil || active == nil {
			return nil, true, err
		}
		return []store.Invocation{*active}, true, nil
	}
	return nil, false, nil
}

// addActiveInvocation adds one invocation to the durable activity projection.
func addActiveInvocation(run *store.Run, invocationID string) {
	if run == nil || strings.TrimSpace(invocationID) == "" {
		return
	}
	for _, activeID := range run.ActiveInvocationIDs {
		if activeID == invocationID {
			return
		}
	}
	run.ActiveInvocationIDs = append(run.ActiveInvocationIDs, invocationID)
}

// releaseActiveInvocation removes one invocation from the run's delegation
// projection without hiding concurrent reviewers that are still active.
func releaseActiveInvocation(run *store.Run, invocationID string) {
	if run == nil || strings.TrimSpace(invocationID) == "" {
		return
	}
	remaining := make([]string, 0, len(run.ActiveInvocationIDs))
	for _, activeID := range run.ActiveInvocationIDs {
		if activeID != invocationID {
			remaining = append(remaining, activeID)
		}
	}
	run.ActiveInvocationIDs = remaining
}

// clearActiveInvocations clears the current activity projection when a
// transition ends every visible invocation for the run.
func clearActiveInvocations(run *store.Run) {
	if run == nil {
		return
	}
	run.ActiveInvocationIDs = nil
}

// containsString reports whether a string occurs in a small coordinator-owned
// projection list.
func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
