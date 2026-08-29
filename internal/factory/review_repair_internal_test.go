package factory

import (
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestDecideReviewRepairCoversTheBoundedWorkflow verifies that review repair
// starts only when its budget and reservation state permit it, and that a
// materially repeated blocker escalates before another edit is attempted.
func TestDecideReviewRepairCoversTheBoundedWorkflow(t *testing.T) {
	t.Parallel()

	finding := store.ReviewRepairFinding{
		ReviewerRole: "spec_review",
		Finding: store.ReviewFinding{
			Location: "internal/factory/review.go:42",
			Claim:    "ready transition skips the required gate",
			Severity: "blocker",
			Category: "correctness",
		},
	}
	repeatedKey := reviewRepairFindingKey(finding)
	tests := []struct {
		name        string
		run         store.Run
		findings    []store.ReviewRepairFinding
		wantKind    reviewRepairDecisionKind
		wantAttempt int
		wantRemain  int
	}{
		{
			name:        "starts first repair",
			run:         store.Run{ReviewRepairBudget: 2},
			findings:    []store.ReviewRepairFinding{finding},
			wantKind:    reviewRepairDecisionStart,
			wantAttempt: 1,
			wantRemain:  1,
		},
		{
			name: "starts second distinct repair",
			run: store.Run{
				ReviewRepairAttempts: 1,
				ReviewRepairBudget:   2,
				ReviewRepairHistory: []store.ReviewRepairAttempt{{
					Attempt: 1, Outcome: store.ReviewRepairStarted,
					BlockerKeys: []string{strings.Repeat("a", 64)},
				}},
			},
			findings:    []store.ReviewRepairFinding{finding},
			wantKind:    reviewRepairDecisionStart,
			wantAttempt: 2,
			wantRemain:  0,
		},
		{
			name: "escalates repeated blocker",
			run: store.Run{
				ReviewRepairAttempts: 1,
				ReviewRepairBudget:   2,
				ReviewRepairHistory: []store.ReviewRepairAttempt{{
					Attempt: 1, Outcome: store.ReviewRepairStarted,
					BlockerKeys: []string{repeatedKey},
				}},
			},
			findings:    []store.ReviewRepairFinding{finding},
			wantKind:    reviewRepairDecisionEscalate,
			wantAttempt: 1,
			wantRemain:  1,
		},
		{
			name: "escalates exhausted budget",
			run: store.Run{
				ReviewRepairAttempts: 2,
				ReviewRepairBudget:   2,
				ReviewRepairHistory: []store.ReviewRepairAttempt{{
					Attempt: 1, Outcome: store.ReviewRepairStarted,
				}, {
					Attempt: 2, Outcome: store.ReviewRepairStarted,
				}},
			},
			findings:    []store.ReviewRepairFinding{finding},
			wantKind:    reviewRepairDecisionEscalate,
			wantAttempt: 2,
			wantRemain:  0,
		},
		{
			name:        "waits when budget is disabled",
			run:         store.Run{ReviewRepairBudget: 0},
			findings:    []store.ReviewRepairFinding{finding},
			wantKind:    reviewRepairDecisionWait,
			wantAttempt: 0,
			wantRemain:  0,
		},
		{
			name: "waits while reservation is pending",
			run: store.Run{
				ReviewRepairAttempts:       1,
				ReviewRepairBudget:         2,
				ReviewRepairPendingAttempt: 2,
			},
			findings:    []store.ReviewRepairFinding{finding},
			wantKind:    reviewRepairDecisionWait,
			wantAttempt: 0,
			wantRemain:  0,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			got := decideReviewRepair(test.run, test.findings)
			if got.Kind != test.wantKind || got.Attempt != test.wantAttempt || got.Remaining != test.wantRemain {
				t.Fatalf("decideReviewRepair() = %#v, want kind=%q attempt=%d remaining=%d", got, test.wantKind, test.wantAttempt, test.wantRemain)
			}
		})
	}
}
