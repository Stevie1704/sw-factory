package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/Stevie1704/sw-factory/internal/onboarding"
)

// runRecordLiveProof records one change of the disposable-issue proof: its
// start before the issue exists, the observed issue and run, the operator's
// refusal, or the abandonment of a started proof. The agent creates the
// issue; this command never does.
func runRecordLiveProof(ctx context.Context, planPath string, args []string, output, errorsOutput io.Writer) int {
	flags := newFlagSet("onboard record live-proof", errorsOutput)
	start := flags.Bool("start", false, "record the start of an approved proof and print the marker the issue body must carry")
	issue := flags.Int("issue", 0, "observe the issue that carries the marker, and its run")
	decline := flags.Bool("decline", false, "record that the operator declined the proof")
	abandon := flags.Bool("abandon", false, "remove a started proof after the operator confirmed that no issue carries its marker")
	if !parseRecordJSON(flags, args, errorsOutput) {
		return 2
	}
	actions := 0
	for _, selected := range []bool{*start, *issue != 0, *decline, *abandon} {
		if selected {
			actions++
		}
	}
	if actions != 1 || *issue < 0 {
		writeError(errorsOutput, errors.New("onboard record live-proof needs exactly one of --start, --issue <number>, --decline, and --abandon"))
		return 2
	}
	path, plan, observation, err := observePlan(ctx, planPath)
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	document := onboardingRecordDocument{Step: onboarding.StepLiveProof}
	var change func(*onboarding.Plan) error
	switch {
	case *start:
		if document.Marker, err = newProofMarker(); err != nil {
			writeError(errorsOutput, err)
			return 1
		}
		change = func(plan *onboarding.Plan) error { return plan.StartLiveProof(observation, document.Marker) }
	case *issue != 0:
		observed, err := observeProofIssue(ctx, plan, *issue)
		if err != nil {
			writeError(errorsOutput, err)
			return 1
		}
		change = func(plan *onboarding.Plan) error { return plan.RecordProofIssue(observed, observation) }
	case *decline:
		change = (*onboarding.Plan).DeclineLiveProof
	default:
		change = (*onboarding.Plan).AbandonLiveProof
	}
	plan, err = onboarding.Update(path, func(plan *onboarding.Plan, exists bool) error {
		if !exists {
			return fmt.Errorf("the plan %s no longer exists", path)
		}
		return change(plan)
	})
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	status := writeRecord(output, errorsOutput, document, path, plan, observation)
	if status == 0 && *issue != 0 && plan.LiveProof.State != onboarding.LiveProofPassed {
		return 1
	}
	return status
}

// observeProofIssue observes one issue for the started proof of the plan.
func observeProofIssue(ctx context.Context, plan onboarding.Plan, number int) (onboarding.ProofIssue, error) {
	if !plan.Started() {
		return onboarding.ProofIssue{}, errors.New("no disposable-issue proof is started: start one with --start")
	}
	marker := plan.LiveProof.Marker
	observed, err := onboardingProofForCLI(ctx, plan.HostConfig, number, marker)
	if err != nil {
		return onboarding.ProofIssue{}, fmt.Errorf("observe the proof issue #%d; nothing was recorded: %w", number, err)
	}
	issue := onboarding.ProofIssue{Number: number, Marker: marker, CarriesMarker: observed.CarriesMarker, Passed: observed.ReachedDraftPullRequest()}
	if observed.Run != nil {
		issue.RunStage, issue.RunStatus = string(observed.Run.Stage), string(observed.Run.Status)
	}
	return issue, nil
}

// newProofMarker returns a new unique proof marker.
func newProofMarker() (string, error) {
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("create the proof marker: %w", err)
	}
	return onboardingProofMarkerPrefix + hex.EncodeToString(random), nil
}
