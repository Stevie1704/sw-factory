package onboarding

import (
	"strings"
	"testing"
	"time"
)

// TestRecordObservedRefusesSupersededEvidence verifies that a writer whose
// observation started before the recorded evidence cannot replace it.
func TestRecordObservedRefusesSupersededEvidence(t *testing.T) {
	observation := readyObservation()
	plan := recordedPlan()
	started := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	newer := Evidence{Outcome: OutcomePassed, Inputs: observation.Subset(StepGateProof), RecordedAt: started.Add(time.Millisecond).Format(time.RFC3339Nano)}
	plan.Record(StepGateProof, newer)

	err := plan.RecordObserved(StepGateProof, Evidence{Outcome: OutcomeFailed, Inputs: observation.Subset(StepGateProof)}, started, observation)
	if err == nil || !strings.Contains(err.Error(), "newer evidence") {
		t.Fatalf("error = %v, want a refusal of the superseded evidence", err)
	}
	if plan.Evidence[StepGateProof].Outcome != OutcomePassed {
		t.Fatal("the newer evidence was replaced")
	}
}

// TestRecordObservedRefusesChangedOrUnobservedInputs verifies that evidence
// is recorded only for the inputs that are current when it is written.
func TestRecordObservedRefusesChangedOrUnobservedInputs(t *testing.T) {
	before := readyObservation()
	started := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	evidence := Evidence{Outcome: OutcomePassed, Inputs: before.Subset(StepGateProof)}

	changed := readyObservation()
	changed.Inputs[InputPolicy] = "sha256:changed"
	plan := NewPlan("/repo", "/host/config.yaml", "")
	if err := plan.RecordObserved(StepGateProof, evidence, started, changed); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("error = %v, want a refusal of changed inputs", err)
	}

	unobserved := readyObservation()
	unobserved.Inputs[InputGateInputs] = ""
	unobservedEvidence := Evidence{Outcome: OutcomePassed, Inputs: unobserved.Subset(StepGateProof)}
	if err := plan.RecordObserved(StepGateProof, unobservedEvidence, started, unobserved); err == nil || !strings.Contains(err.Error(), "could not be observed") {
		t.Fatalf("error = %v, want a refusal of an unobserved input", err)
	}
	if _, ok := plan.Evidence[StepGateProof]; ok {
		t.Fatal("refused evidence was recorded")
	}

	if err := plan.RecordObserved(StepGateProof, evidence, started, before); err != nil {
		t.Fatal(err)
	}
	if got := plan.Evidence[StepGateProof].RecordedAt; got != started.Format(time.RFC3339Nano) {
		t.Fatalf("recorded_at = %q, want the start of the observation", got)
	}
}

// TestLiveProofKeepsTheInputsItStartedWith verifies that a passed proof is
// valid only for the inputs at its start, not for the inputs at the time
// its issue is recorded.
func TestLiveProofKeepsTheInputsItStartedWith(t *testing.T) {
	atStart := readyObservation()
	plan := recordedPlan()
	if err := plan.StartLiveProof(atStart, "factory-onboarding-proof:abc"); err != nil {
		t.Fatal(err)
	}
	changed := readyObservation()
	changed.Inputs[InputPolicy] = "sha256:policy-b"
	issue := ProofIssue{Number: 7, Marker: "factory-onboarding-proof:abc", CarriesMarker: true, Passed: true}
	if err := plan.RecordProofIssue(issue); err != nil {
		t.Fatal(err)
	}
	if got := plan.LiveProof.Inputs[InputPolicy]; got != "sha256:policy" {
		t.Fatalf("passed proof inputs policy = %q, want the policy at the start", got)
	}
	if step := stepOf(t, Evaluate(plan, changed), StepLiveProof); step.State != StateStale || !strings.Contains(step.Reason, "policy changed") {
		t.Fatalf("live proof under another policy = %+v, want stale", step)
	}
	if step := stepOf(t, Evaluate(plan, atStart), StepLiveProof); step.State != StateVerified {
		t.Fatalf("live proof under its own policy = %+v, want verified", step)
	}
}
