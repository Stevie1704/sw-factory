package factory

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
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

// TestReviewRepairFindingIdentitySurvivesFreshReviewerWording verifies a
// relocated and paraphrased blocker still escalates instead of consuming a
// second automatic repair attempt.
func TestReviewRepairFindingIdentitySurvivesFreshReviewerWording(t *testing.T) {
	t.Parallel()

	first := store.ReviewRepairFinding{Finding: store.ReviewFinding{
		Location: "internal/factory/readiness.go:42",
		Claim:    "readiness skips the final gate",
		Category: "correctness",
	}}
	second := store.ReviewRepairFinding{Finding: store.ReviewFinding{
		Location: "internal/factory/readiness.go:118",
		Claim:    "the ready transition does not verify the last gate",
		Category: "correctness",
	}}
	history := []store.ReviewRepairAttempt{{Attempt: 1, Outcome: store.ReviewRepairStarted, BlockerKeys: []string{reviewRepairFindingKey(first)}}}
	if !reviewRepairHasRepeatedBlocker(history, reviewRepairBlockerKeys([]store.ReviewRepairFinding{second})) {
		t.Fatal("review repair treated a relocated, paraphrased blocker as new")
	}
	distinct := store.ReviewRepairFinding{Finding: store.ReviewFinding{
		Location: "internal/factory/readiness.go:130",
		Claim:    "the pull request body omits the target branch",
		Category: "correctness",
	}}
	if reviewRepairHasRepeatedBlocker(history, reviewRepairBlockerKeys([]store.ReviewRepairFinding{distinct})) {
		t.Fatal("review repair conflated a distinct same-file correctness blocker")
	}
	opposite := store.ReviewRepairFinding{Finding: store.ReviewFinding{
		Location: "internal/factory/readiness.go:142",
		Claim:    "the ready transition verifies the final gate",
		Category: "correctness",
	}}
	if reviewRepairHasRepeatedBlocker(history, reviewRepairBlockerKeys([]store.ReviewRepairFinding{opposite})) {
		t.Fatal("review repair conflated an opposite same-file correctness blocker")
	}
}

// TestReviewRepairTestOwnerRequiresStructuredObjection verifies a repair
// cannot silently turn a reviewer-assigned test blocker into an implementation
// handoff.
func TestReviewRepairTestOwnerRequiresStructuredObjection(t *testing.T) {
	t.Parallel()

	packet := SpecificationPacket{RepositoryConfig: configForRequiredTestReviewRepair()}
	run := store.Run{ReviewRepairPacket: &store.ReviewRepairPacket{Findings: []store.ReviewRepairFinding{{
		ReviewerRole: workflow.RoleStandardsReview,
		Finding:      store.ReviewFinding{Location: "tests/ready_test.go:12", Claim: "the ready behavior is untested", Category: "correctness", SuggestedOwner: workflow.RoleTest},
	}}}}
	value := report.Report{Outcome: report.OutcomeCompleted, Handoff: &report.Handoff{}}
	err := validateReviewRepairTestOwnerRouting(value, store.Invocation{Role: workflow.RoleImplementation, Stage: store.StageImplementation}, run, packet)
	if err == nil || !strings.Contains(err.Error(), "structured test objection") {
		t.Fatalf("validateReviewRepairTestOwnerRouting() error = %v, want structured objection refusal", err)
	}
}

// TestReviewRepairTestOwnerRoutesOneStructuredObjectionPerCycle verifies that
// one implementation report follows the existing single-objection protocol;
// remaining findings stay queued for later objection cycles.
func TestReviewRepairTestOwnerRoutesOneStructuredObjectionPerCycle(t *testing.T) {
	t.Parallel()

	findings := []store.ReviewRepairFinding{
		{Finding: store.ReviewFinding{Location: "tests/ready_test.go:12", Claim: "the ready behavior is untested", SuggestedOwner: workflow.RoleTest}},
		{Finding: store.ReviewFinding{Location: "tests/merge_test.go:24", Claim: "the merge behavior is untested", SuggestedOwner: workflow.RoleTest}},
	}
	packet := SpecificationPacket{RepositoryConfig: configForRequiredTestReviewRepair()}
	run := store.Run{ReviewRepairPacket: &store.ReviewRepairPacket{Findings: findings}}
	value := report.Report{Outcome: report.OutcomeCompleted, Handoff: &report.Handoff{
		TestObjections: []report.TestObjection{{Test: findings[0].Finding.Location, Claim: findings[0].Finding.Claim}},
	}}
	err := validateReviewRepairTestOwnerRouting(value, store.Invocation{Role: workflow.RoleImplementation, Stage: store.StageImplementation}, run, packet)
	if err != nil {
		t.Fatalf("validateReviewRepairTestOwnerRouting() error = %v, want one finding routed per cycle", err)
	}
	value.Handoff.TestObjections = append(value.Handoff.TestObjections, report.TestObjection{Test: findings[1].Finding.Location, Claim: findings[1].Finding.Claim})
	err = validateReviewRepairTestOwnerRouting(value, store.Invocation{Role: workflow.RoleImplementation, Stage: store.StageImplementation}, run, packet)
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("validateReviewRepairTestOwnerRouting() error = %v, want single-objection protocol refusal", err)
	}
}

// TestReviewRepairTestOwnerAllowsAcceptedObjectionResolution verifies that a
// revised test-owned finding no longer forces the same objection on the next
// implementation handoff.
func TestReviewRepairTestOwnerAllowsAcceptedObjectionResolution(t *testing.T) {
	t.Parallel()

	finding := store.ReviewRepairFinding{
		ReviewerRole: workflow.RoleStandardsReview,
		Finding: store.ReviewFinding{
			Location:       "tests/ready_test.go:12",
			Claim:          "the ready behavior is untested",
			Category:       "correctness",
			SuggestedOwner: workflow.RoleTest,
		},
	}
	packet := SpecificationPacket{RepositoryConfig: configForRequiredTestReviewRepair()}
	run := store.Run{ReviewRepairPacket: &store.ReviewRepairPacket{
		Findings:                     []store.ReviewRepairFinding{finding},
		ResolvedTestOwnerFindingKeys: []string{reviewRepairTestOwnerFindingKey(finding)},
	}}
	value := report.Report{Outcome: report.OutcomeCompleted, Handoff: &report.Handoff{}}
	if err := validateReviewRepairTestOwnerRouting(value, store.Invocation{Role: workflow.RoleImplementation, Stage: store.StageImplementation}, run, packet); err != nil {
		t.Fatalf("validateReviewRepairTestOwnerRouting() error = %v, want resolved finding to pass", err)
	}
}

// TestIndependentImplementationRejectsNewTestPaths verifies that a review
// repair cannot bypass test ownership by adding an unprotected test file.
func TestIndependentImplementationRejectsNewTestPaths(t *testing.T) {
	t.Parallel()

	policy := configForRequiredTestReviewRepair().TestPolicy
	if err := validateImplementationTestPaths([]string{"tests/new_ready_test.go"}, policy); err == nil || !strings.Contains(err.Error(), "test-owned path") {
		t.Fatalf("validateImplementationTestPaths() error = %v, want test ownership refusal", err)
	}
}

// TestRecordReviewRepairTestOwnerResolutionTracksTheAcceptedFinding verifies
// that the packet records an opaque resolution identity for one objection.
func TestRecordReviewRepairTestOwnerResolutionTracksTheAcceptedFinding(t *testing.T) {
	t.Parallel()

	finding := store.ReviewRepairFinding{Finding: store.ReviewFinding{
		Location:       "tests/ready_test.go:12",
		Claim:          "the ready behavior is untested",
		SuggestedOwner: workflow.RoleTest,
	}}
	run := store.Run{
		ReviewRepairPacket: &store.ReviewRepairPacket{Findings: []store.ReviewRepairFinding{finding}},
		TestObjection:      &store.TestObjection{Test: "tests/ready_test.go:12", Claim: "the ready behavior is untested"},
	}
	if err := recordReviewRepairTestOwnerResolution(&run); err != nil {
		t.Fatalf("recordReviewRepairTestOwnerResolution() error = %v", err)
	}
	if len(run.ReviewRepairPacket.ResolvedTestOwnerFindingKeys) != 1 || run.ReviewRepairPacket.ResolvedTestOwnerFindingKeys[0] != reviewRepairTestOwnerFindingKey(finding) {
		t.Fatalf("resolved test-owner keys = %#v, want one matching key", run.ReviewRepairPacket.ResolvedTestOwnerFindingKeys)
	}
}

// TestRecordReviewRepairTestOwnerResolutionIgnoresObjectionWithoutFindings
// verifies an accepted objection is harmless after all test-owned findings are
// absent or resolved.
func TestRecordReviewRepairTestOwnerResolutionIgnoresObjectionWithoutFindings(t *testing.T) {
	t.Parallel()

	run := store.Run{
		ReviewRepairPacket: &store.ReviewRepairPacket{},
		TestObjection:      &store.TestObjection{Test: "tests/ready_test.go:12", Claim: "the ready behavior is untested"},
	}
	if err := recordReviewRepairTestOwnerResolution(&run); err != nil {
		t.Fatalf("recordReviewRepairTestOwnerResolution() error = %v, want no-op without test-owner findings", err)
	}
}

// TestRecordReviewRepairTestOwnerResolutionRejectsUnmatchedObjection verifies
// an objection cannot resolve a different outstanding test-owned finding.
func TestRecordReviewRepairTestOwnerResolutionRejectsUnmatchedObjection(t *testing.T) {
	t.Parallel()

	run := store.Run{
		ReviewRepairPacket: &store.ReviewRepairPacket{Findings: []store.ReviewRepairFinding{{
			Finding: store.ReviewFinding{Location: "tests/ready_test.go:12", Claim: "the ready behavior is untested", SuggestedOwner: workflow.RoleTest},
		}}},
		TestObjection: &store.TestObjection{Test: "tests/merge_test.go:24", Claim: "the merge behavior is untested"},
	}
	if err := recordReviewRepairTestOwnerResolution(&run); err == nil || !strings.Contains(err.Error(), "does not identify") {
		t.Fatalf("recordReviewRepairTestOwnerResolution() error = %v, want unmatched objection refusal", err)
	}
}

// configForRequiredTestReviewRepair supplies only the policy fields needed to
// exercise the coordinator's independent-test ownership rule.
func configForRequiredTestReviewRepair() config.RepositoryConfig {
	return config.RepositoryConfig{TestPolicy: config.TestPolicy{Mode: config.TestModeRequired}}
}

// TestResetRevisionProjectionClearsSupersededEvidence verifies a specification
// amendment receives fresh check, review, and test-stage projections.
func TestResetRevisionProjectionClearsSupersededEvidence(t *testing.T) {
	t.Parallel()

	packet := SpecificationPacket{RepositoryConfig: config.RepositoryConfig{RetryLimits: config.RetryLimits{CheckRepair: 2, ReviewRepair: 2}}}
	run := store.Run{
		CheckRepairAttempts:       2,
		CheckRepairBudget:         2,
		CheckRepairPendingAttempt: 0,
		ReviewRepairAttempts:      2,
		TestHandoff:               &store.TestHandoff{FocusedTestCommand: "go test ./..."},
		TestInvocationID:          "inv-test",
		TestCheckpointSHA:         strings.Repeat("b", 64),
		TestExemption:             &store.TestExemption{Kind: "human", Justification: "documented"},
		ProtectedTestPaths:        []store.ProtectedTestPath{{Path: "tests/ready_test.go", SHA256: strings.Repeat("c", 64)}},
		TestStageSkipped:          true,
	}
	resetRevisionProjection(&run, packet)
	if run.CheckRepairAttempts != 0 || run.CheckRepairBudget != 2 || run.CheckRepairPendingAttempt != 0 {
		t.Fatalf("check repair state after revision = %#v, want a fresh budget", run)
	}
	if run.TestHandoff != nil || run.TestInvocationID != "" || run.TestCheckpointSHA != "" || run.TestExemption != nil || len(run.ProtectedTestPaths) != 0 || run.TestStageSkipped {
		t.Fatalf("test evidence after revision = %#v, want cleared", run)
	}
}

// TestBlockingReviewFindingsIgnoreUnconfiguredReviewerResults verifies stale
// results from a role absent from the frozen packet cannot block or enter a
// repair packet.
func TestBlockingReviewFindingsIgnoreUnconfiguredReviewerResults(t *testing.T) {
	t.Parallel()

	packetData, err := json.Marshal(SpecificationPacket{RepositoryConfig: config.RepositoryConfig{
		RoleHarnessDefaults: map[string]config.Harness{workflow.RoleSpecificationReview: config.HarnessCodex},
		ModelOptions:        map[string][]string{workflow.RoleSpecificationReview: {"review-model"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := strings.Repeat("a", 64)
	run := store.Run{
		CheckpointSHA:       checkpoint,
		SpecificationPacket: string(packetData),
		SpecificationReview: &store.ReviewResult{CheckpointSHA: checkpoint},
		StandardsReview: &store.ReviewResult{CheckpointSHA: checkpoint, Findings: []store.ReviewFinding{{
			Claim: "unconfigured standards blocker", Severity: "blocker", Category: "standards",
		}}},
	}
	if reviewHasBlockingResult(run) {
		t.Fatal("unconfigured standards result blocked readiness")
	}
	if findings := blockingReviewFindingsForRun(run); len(findings) != 0 {
		t.Fatalf("blocking findings = %#v, want none from an unconfigured reviewer", findings)
	}
}
