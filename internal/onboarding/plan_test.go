package onboarding

import (
	"strings"
	"testing"
)

// readyObservation returns an observation of a repository whose cheap,
// locally observed steps are all complete.
func readyObservation() Observation {
	return Observation{
		Inputs: map[Input]string{
			InputRelease:          "sha256:release",
			InputPolicy:           "sha256:policy",
			InputWorkerImage:      "example/worker@sha256:image",
			InputWorkerDefinition: "sha256:definition",
			InputGateInputs:       "sha256:gates",
			InputRegistration:     "sha256:registration",
			InputSkillEvidence:    "sha256:smoke",
		},
		Resources:     Check{Done: true},
		Configuration: Check{Done: true},
		Image:         Check{Done: true},
		Registration:  Check{Done: true},
		Smoke: []HarnessSmoke{
			{Harness: "claude", HarnessVersion: "2.1.232", Required: []string{"implement"}, Verified: []string{"implement"}, Missing: []string{}},
		},
	}
}

// recordedPlan returns a plan whose image, gate proof, and doctor evidence
// match readyObservation.
func recordedPlan() Plan {
	observation := readyObservation()
	plan := NewPlan("/repo", "/host/config.yaml", "/resources")
	plan.Record(StepImage, Evidence{Outcome: OutcomePassed, Inputs: observation.Subset(StepImage)})
	plan.Record(StepGateProof, Evidence{Outcome: OutcomePassed, Inputs: observation.Subset(StepGateProof), EvidenceID: "sha256:proof"})
	plan.Record(StepDoctor, Evidence{Outcome: OutcomePassed, Inputs: observation.Subset(StepDoctor)})
	return plan
}

// stepOf returns one evaluated step.
func stepOf(t *testing.T, status Status, name Step) StepStatus {
	t.Helper()
	for _, step := range status.Steps {
		if step.Name == name {
			return step
		}
	}
	t.Fatalf("status has no step %s", name)
	return StepStatus{}
}

// TestEvaluateFreshPlanStartsWithPrepare verifies that a new plan lists every
// onboarding step in procedure order, and that nothing is verified before
// evidence exists.
func TestEvaluateFreshPlanStartsWithPrepare(t *testing.T) {
	plan := NewPlan("/repo", "/host/config.yaml", "")
	status := Evaluate(plan, Observation{Inputs: map[Input]string{}})

	var names []string
	for _, step := range status.Steps {
		names = append(names, string(step.Name))
	}
	if got := strings.Join(names, ","); got != "prepare,configuration,image,gate_proof,registration,skill_smoke,doctor,live_proof" {
		t.Fatalf("steps = %s", got)
	}
	if step := stepOf(t, status, StepPrepare); step.State != StatePending || step.Action == "" {
		t.Fatalf("prepare = %+v, want pending with an action", step)
	}
	if step := stepOf(t, status, StepGateProof); step.State != StateBlocked || !strings.Contains(step.Reason, "image") {
		t.Fatalf("gate proof = %+v, want blocked by the image", step)
	}
	if status.Result != ResultIncomplete {
		t.Fatalf("result = %s, want incomplete", status.Result)
	}
}

// TestEvaluateRecordedEvidenceIsDoctorReady verifies that matching evidence
// makes every step up to the doctor verified, and that the live proof stays
// pending until the operator decides it.
func TestEvaluateRecordedEvidenceIsDoctorReady(t *testing.T) {
	status := Evaluate(recordedPlan(), readyObservation())

	for _, name := range []Step{StepPrepare, StepConfiguration, StepImage, StepGateProof, StepRegistration, StepSkillSmoke, StepDoctor} {
		if step := stepOf(t, status, name); step.State != StateVerified {
			t.Fatalf("%s = %+v, want verified", name, step)
		}
	}
	if step := stepOf(t, status, StepLiveProof); step.State != StatePending || step.OperatorChoice == "" {
		t.Fatalf("live proof = %+v, want pending with an operator choice", step)
	}
	if status.Result != ResultDoctorReady {
		t.Fatalf("result = %s, want doctor_ready", status.Result)
	}
}

// TestEvaluateChangedPolicyInvalidatesOnlyDependentEvidence verifies that a
// changed factory.yaml makes the gate proof and the doctor stale with a
// visible reason, while the image and the smoke evidence stay valid.
func TestEvaluateChangedPolicyInvalidatesOnlyDependentEvidence(t *testing.T) {
	observation := readyObservation()
	observation.Inputs[InputPolicy] = "sha256:changed"
	observation.Inputs[InputGateInputs] = "sha256:changed-gates"
	status := Evaluate(recordedPlan(), observation)

	for _, name := range []Step{StepGateProof, StepDoctor} {
		step := stepOf(t, status, name)
		if step.State != StateStale || !strings.Contains(step.Reason, "policy changed") {
			t.Fatalf("%s = %+v, want stale because the policy changed", name, step)
		}
	}
	for _, name := range []Step{StepImage, StepRegistration, StepSkillSmoke} {
		if step := stepOf(t, status, name); step.State != StateVerified {
			t.Fatalf("%s = %+v, want verified", name, step)
		}
	}
	if status.Result != ResultIncomplete {
		t.Fatalf("result = %s, want incomplete", status.Result)
	}
}

// TestEvaluateChangedWorkerDefinitionMakesTheImageStale verifies that an
// edited worker definition or skill set without a rebuild makes the image
// stale and blocks the steps that use it.
func TestEvaluateChangedWorkerDefinitionMakesTheImageStale(t *testing.T) {
	observation := readyObservation()
	observation.Inputs[InputWorkerDefinition] = "sha256:edited"
	status := Evaluate(recordedPlan(), observation)

	if step := stepOf(t, status, StepImage); step.State != StateStale || !strings.Contains(step.Reason, "worker_definition changed") {
		t.Fatalf("image = %+v, want stale because the definition changed", step)
	}
	if step := stepOf(t, status, StepSkillSmoke); step.State != StateBlocked {
		t.Fatalf("skill smoke = %+v, want blocked by the image", step)
	}
}

// TestEvaluatePartialSmokeNamesOnlyTheMissingCalls verifies that a partial
// smoke keeps the verified skills and that the action repeats only the
// missing paid calls.
func TestEvaluatePartialSmokeNamesOnlyTheMissingCalls(t *testing.T) {
	observation := readyObservation()
	observation.Smoke = []HarnessSmoke{
		{Harness: "claude", Required: []string{"implement", "pr"}, Verified: []string{"implement"}, Missing: []string{"pr"}},
		{Harness: "codex", Required: []string{"implement"}, Verified: []string{"implement"}, Missing: []string{}},
	}
	status := Evaluate(recordedPlan(), observation)

	step := stepOf(t, status, StepSkillSmoke)
	if step.State != StatePending {
		t.Fatalf("skill smoke = %+v, want pending", step)
	}
	if !strings.Contains(step.Action, "HARNESSES=claude") || !strings.Contains(step.Action, "MANDATORY_SKILLS=pr ") || strings.Contains(step.Action, "codex") {
		t.Fatalf("action = %q, want a retry of claude/pr only", step.Action)
	}
	if len(step.Harnesses) != 2 || step.Harnesses[1].Missing == nil {
		t.Fatalf("harnesses = %+v, want both harnesses reported", step.Harnesses)
	}
}

// TestEvaluateFailedEvidenceIsNotVerified verifies that a recorded failure
// with unchanged inputs leaves the step pending with its findings.
func TestEvaluateFailedEvidenceIsNotVerified(t *testing.T) {
	plan := recordedPlan()
	plan.Record(StepDoctor, Evidence{Outcome: OutcomeFailed, Inputs: readyObservation().Subset(StepDoctor), Findings: []string{"github.labels.missing"}})
	status := Evaluate(plan, readyObservation())

	if step := stepOf(t, status, StepDoctor); step.State != StatePending || !strings.Contains(step.Reason, "github.labels.missing") {
		t.Fatalf("doctor = %+v, want pending with the finding", step)
	}
}

// TestEvaluateLiveProofOutcomes verifies the three final results and that a
// started proof without an observed issue stays blocked.
func TestEvaluateLiveProofOutcomes(t *testing.T) {
	observation := readyObservation()
	cases := []struct {
		name   string
		proof  LiveProof
		state  State
		result Result
	}{
		{"declined", LiveProof{State: LiveProofDeclined}, StateSkipped, ResultLiveProofDeclined},
		{"started", LiveProof{State: LiveProofStarted, Marker: "factory-onboarding-proof:abc"}, StateBlocked, ResultDoctorReady},
		{"issue recorded", LiveProof{State: LiveProofIssueRecorded, Marker: "m", Issue: 7, RunStage: "implementation"}, StatePending, ResultDoctorReady},
		{"passed", LiveProof{State: LiveProofPassed, Marker: "m", Issue: 7, Inputs: observation.Subset(StepLiveProof)}, StateVerified, ResultLiveProofPassed},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			plan := recordedPlan()
			proof := test.proof
			plan.LiveProof = &proof
			status := Evaluate(plan, observation)
			step := stepOf(t, status, StepLiveProof)
			if step.State != test.state || status.Result != test.result {
				t.Fatalf("live proof = %+v, result %s; want %s and %s", step, status.Result, test.state, test.result)
			}
		})
	}
	plan := recordedPlan()
	plan.LiveProof = &LiveProof{State: LiveProofStarted, Marker: "factory-onboarding-proof:abc"}
	step := stepOf(t, Evaluate(plan, observation), StepLiveProof)
	if !strings.Contains(step.Reason, "factory-onboarding-proof:abc") || !strings.Contains(step.Reason, "unknown") {
		t.Fatalf("started proof reason = %q, want the marker and an unknown outcome", step.Reason)
	}
}
