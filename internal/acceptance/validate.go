package acceptance

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// InterventionCauses are the causes by which the report groups human steps.
var InterventionCauses = []string{"authorization", "human-disposition", "credential", "environment", "factory-defect", "guidance-gap", "other"}

// WorkaroundKinds are the recorded kinds of action outside the supported
// interfaces. Each one disqualifies a pass.
var WorkaroundKinds = []string{"operational-store-edit", "run-branch-edit", "hidden-checkout-knowledge", "weakened-gate", "other"}

// maxTextRunes bounds every free-text field, so a report carries a summary
// and never a transcript.
const maxTextRunes = 300

// workerDigest matches a pinned worker image digest.
var workerDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// hostPath matches an absolute home-directory path, which names a user and
// must stay in local evidence.
var hostPath = regexp.MustCompile(`/(?:Users|home)/[^/\s]`)

// validate refuses results that would overstate or leak evidence.
func (r Results) validate() error {
	if r.WorkerDigest != "" && !workerDigest.MatchString(r.WorkerDigest) {
		return fmt.Errorf("worker_digest %q is not sha256:<64 hex digits>", r.WorkerDigest)
	}
	harnesses := r.WorkerHarnesses
	if r.HostHarness != nil {
		harnesses = append([]Harness{*r.HostHarness}, harnesses...)
	}
	for _, harness := range harnesses {
		if err := checkText("harness name", harness.Name, true); err != nil {
			return err
		}
		if err := checkText("harness version", harness.Version, false); err != nil {
			return err
		}
	}
	for _, result := range r.Results {
		if result.Evidence == EvidenceLive && r.HostHarness == nil {
			return errors.New("a live result needs host_harness")
		}
		if err := result.validate(); err != nil {
			return fmt.Errorf("scenario %q (%s): %w", result.Scenario, result.Evidence, err)
		}
	}
	return nil
}

// validate refuses one result whose outcome, reason, measurements, or text
// do not support its claim.
func (r Result) validate() error {
	if r.Evidence != EvidenceLive && r.Evidence != EvidenceDeterministic {
		return fmt.Errorf("evidence must be %s or %s", EvidenceLive, EvidenceDeterministic)
	}
	if !slices.Contains(outcomes, r.Outcome.Outcome) {
		return fmt.Errorf("outcome must be one of %s", strings.Join(outcomes, ", "))
	}
	if err := checkText("reason", r.Reason, r.Outcome.Outcome != OutcomePass); err != nil {
		return err
	}
	if r.Outcome.Outcome == OutcomePass && len(r.Workarounds) > 0 {
		return errors.New("a pass cannot use an unsupported workaround; record the outcome as fail")
	}
	if r.Evidence == EvidenceLive && r.Outcome.Outcome != OutcomeNotRun {
		if err := r.validateMeasurements(); err != nil {
			return err
		}
	}
	return r.validateDetails()
}

// validateMeasurements requires the duration and the usage of a live result
// that ran, so an unmeasured cost is recorded as such.
func (r Result) validateMeasurements() error {
	if r.DurationSeconds == nil || *r.DurationSeconds < 0 {
		return errors.New("a live result needs duration_seconds")
	}
	if r.Usage == nil {
		return errors.New("a live result needs usage: the measured values, or measured false with a reason")
	}
	if !r.Usage.Measured {
		return checkText("usage reason", r.Usage.Reason, true)
	}
	if r.Usage.InputTokens == nil && r.Usage.OutputTokens == nil && r.Usage.CostUSD == nil {
		return errors.New("measured usage needs input_tokens, output_tokens, or cost_usd")
	}
	for _, value := range []*int{r.Usage.InputTokens, r.Usage.OutputTokens} {
		if value != nil && *value < 0 {
			return errors.New("usage token counts cannot be negative")
		}
	}
	if r.Usage.CostUSD != nil && *r.Usage.CostUSD < 0 {
		return errors.New("usage cost cannot be negative")
	}
	return nil
}

// validateDetails checks the interventions and workarounds of one result.
func (r Result) validateDetails() error {
	for _, intervention := range r.Interventions {
		if !slices.Contains(InterventionCauses, intervention.Cause) {
			return fmt.Errorf("intervention cause must be one of %s", strings.Join(InterventionCauses, ", "))
		}
		if err := checkText("intervention summary", intervention.Summary, true); err != nil {
			return err
		}
	}
	for _, workaround := range r.Workarounds {
		if !slices.Contains(WorkaroundKinds, workaround.Kind) {
			return fmt.Errorf("workaround kind must be one of %s", strings.Join(WorkaroundKinds, ", "))
		}
		if err := checkText("workaround summary", workaround.Summary, true); err != nil {
			return err
		}
	}
	return nil
}

// checkText refuses a missing required value, and any value that is not one
// bounded line or that names a home-directory path.
func checkText(field, value string, required bool) error {
	switch {
	case value == "" && required:
		return fmt.Errorf("%s is required", field)
	case strings.ContainsAny(value, "\r\n"):
		return fmt.Errorf("%s must be one line", field)
	case utf8.RuneCountInString(value) > maxTextRunes:
		return fmt.Errorf("%s exceeds %d characters", field, maxTextRunes)
	case hostPath.MatchString(value):
		return fmt.Errorf("%s names a home-directory path; keep it in local evidence", field)
	}
	return nil
}
