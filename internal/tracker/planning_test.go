package tracker_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// TestWriteNotAppliedTrustsOnlyTypedNoChangeErrors verifies that only the
// typed permission, capability, and mapping errors prove that a create made
// no change; ambiguous, multiple-candidate, and untyped errors leave the
// outcome unknown.
func TestWriteNotAppliedTrustsOnlyTypedNoChangeErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "permission", err: tracker.NewPlanError(tracker.PlanErrorPermission, "denied"), want: true},
		{name: "unsupported", err: tracker.NewPlanError(tracker.PlanErrorUnsupportedCapability, "no sub-issues"), want: true},
		{name: "wrapped invalid mapping", err: fmt.Errorf("create: %w", tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "no type")), want: true},
		{name: "ambiguous", err: tracker.NewPlanError(tracker.PlanErrorAmbiguous, "timeout"), want: false},
		{name: "multiple candidates", err: tracker.NewPlanError(tracker.PlanErrorMultipleCandidates, "two"), want: false},
		{name: "untyped", err: errors.New("connection reset"), want: false},
		{name: "deadline", err: context.DeadlineExceeded, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := tracker.WriteNotApplied(test.err); got != test.want {
				t.Fatalf("WriteNotApplied(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}

// TestPlanCorrelationIsStableAndIndependentOfExternalIdentity verifies the
// documented correlation key shape.
func TestPlanCorrelationIsStableAndIndependentOfExternalIdentity(t *testing.T) {
	t.Parallel()

	if got := tracker.PlanCorrelation("session-1", 3, "node-a"); got != "session-1/3/node-a" {
		t.Fatalf("PlanCorrelation() = %q", got)
	}
}

// TestIsFactoryStateLabelIgnoresCase verifies that every factory state label
// is recognized in any case and that ordinary labels are not.
func TestIsFactoryStateLabelIgnoresCase(t *testing.T) {
	t.Parallel()

	if !tracker.IsFactoryStateLabel(" AGENT-READY ") || !tracker.IsFactoryStateLabel(tracker.LabelAgentRunning) {
		t.Fatal("IsFactoryStateLabel() missed a factory label")
	}
	if tracker.IsFactoryStateLabel("ready-for-agent") || tracker.IsFactoryStateLabel("enhancement") {
		t.Fatal("IsFactoryStateLabel() matched an ordinary label")
	}
}
