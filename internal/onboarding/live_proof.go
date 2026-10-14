package onboarding

import (
	"errors"
	"fmt"
)

// Valid reports whether the recorded evidence of one step passed and every
// input it depends on is unchanged, so the step need not run again.
func (p Plan) Valid(step Step, observation Observation) bool {
	evidence, ok := p.Evidence[step]
	if !ok || evidence.Outcome != OutcomePassed {
		return false
	}
	return len(evaluator{plan: p, observation: observation}.changed(step, evidence.Inputs)) == 0
}

// Started reports whether a live proof was started and its outcome is not
// final, so another start would risk a second proof issue.
func (p Plan) Started() bool {
	return p.LiveProof != nil && (p.LiveProof.State == LiveProofStarted || p.LiveProof.State == LiveProofIssueRecorded)
}

// StartLiveProof records the intent of a disposable-issue proof with its
// marker before the issue exists. It refuses while the doctor is not
// verified, while an earlier proof has an unknown or open outcome, and after
// a proof that is still valid.
func (p *Plan) StartLiveProof(observation Observation, marker string) error {
	if p.Started() {
		return fmt.Errorf("a disposable-issue proof with the marker %s is already started: record its issue with --issue, or abandon it with --abandon after the operator confirms that no issue carries the marker", p.LiveProof.Marker)
	}
	states := map[Step]State{}
	for _, step := range Evaluate(*p, observation).Steps {
		states[step.Name] = step.State
	}
	if states[StepDoctor] != StateVerified {
		return errors.New("the doctor is not verified for the current inputs: record the doctor first")
	}
	if states[StepLiveProof] == StateVerified {
		return errors.New("the disposable-issue proof already passed for the current inputs")
	}
	p.LiveProof = &LiveProof{State: LiveProofStarted, Marker: marker, UpdatedAt: now()}
	return nil
}

// ProofIssue is the observation of the issue that claims to be the proof
// issue.
type ProofIssue struct {
	// Number is the issue number.
	Number int
	// Marker is the marker the issue body was checked for.
	Marker string
	// CarriesMarker reports that the body contains Marker.
	CarriesMarker bool
	// RunStage and RunStatus describe its newest run, or are empty.
	RunStage, RunStatus string
	// Passed reports that the run opened its draft pull request.
	Passed bool
}

// RecordProofIssue records the observed proof issue of the started proof.
// An issue without the marker is never recorded, so an unrelated issue
// cannot complete the proof.
func (p *Plan) RecordProofIssue(issue ProofIssue, observation Observation) error {
	if !p.Started() {
		return errors.New("no disposable-issue proof is started: start one with --start")
	}
	proof := *p.LiveProof
	if issue.Marker != proof.Marker {
		return errors.New("the started proof changed while the issue was observed: run the command again")
	}
	if proof.Issue != 0 && proof.Issue != issue.Number {
		return fmt.Errorf("the plan already records issue #%d for this proof", proof.Issue)
	}
	if !issue.CarriesMarker {
		return fmt.Errorf("issue #%d does not carry the marker %s, so it is not the proof issue", issue.Number, proof.Marker)
	}
	proof.Issue, proof.RunStage, proof.RunStatus, proof.UpdatedAt = issue.Number, issue.RunStage, issue.RunStatus, now()
	proof.State = LiveProofIssueRecorded
	if issue.Passed {
		proof.State, proof.Inputs = LiveProofPassed, observation.Subset(StepLiveProof)
	}
	p.LiveProof = &proof
	return nil
}

// DeclineLiveProof records that the operator declined the proof. It refuses
// while a started proof has an unknown or open outcome.
func (p *Plan) DeclineLiveProof() error {
	if p.Started() {
		return fmt.Errorf("the disposable-issue proof with the marker %s is started: record its issue or abandon it first", p.LiveProof.Marker)
	}
	p.LiveProof = &LiveProof{State: LiveProofDeclined, UpdatedAt: now()}
	return nil
}

// AbandonLiveProof removes a started proof after the operator confirmed
// that it created no issue, or that its issue is given up.
func (p *Plan) AbandonLiveProof() error {
	if !p.Started() {
		return errors.New("no disposable-issue proof is started")
	}
	p.LiveProof = nil
	return nil
}
