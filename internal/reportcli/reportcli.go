// Package reportcli implements the factory-report command available inside a
// worker image.
package reportcli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/report"
)

// Request contains the worker command inputs and its explicit environment.
type Request struct {
	// Args are command-line arguments after the report command name.
	Args []string
	// Environment contains coordinator-provided identity and result variables.
	Environment map[string]string
	// Now supplies the report timestamp.
	Now func() time.Time
	// Output receives the successful report path.
	Output io.Writer
	// ErrorsOutput receives usage and operational errors.
	ErrorsOutput io.Writer
}

// Run validates command arguments, builds exactly one report outcome, and
// atomically writes it into the coordinator-provided invocation result path.
func Run(request Request) int {
	if request.Output == nil {
		request.Output = io.Discard
	}
	if request.ErrorsOutput == nil {
		request.ErrorsOutput = io.Discard
	}
	if request.Now == nil {
		request.Now = func() time.Time { return time.Now().UTC() }
	}
	environment := request.Environment
	lookup := func(name string) string {
		if environment != nil {
			return environment[name]
		}
		return os.Getenv(name)
	}
	flags := flag.NewFlagSet("factory-report", flag.ContinueOnError)
	flags.SetOutput(request.ErrorsOutput)
	outcome := flags.String("outcome", "", "completed, needs_clarification, or cannot_proceed")
	summary := flags.String("summary", "", "short observable summary")
	changeSummary := flags.String("change-summary", "", "completed handoff change summary")
	acceptance := pairList{}
	flags.Var(&acceptance, "acceptance", "criterion=evidence; may be repeated")
	productionFiles := stringList{}
	flags.Var(&productionFiles, "production-file", "repository-relative production file; may be repeated")
	focusedCommands := stringList{}
	flags.Var(&focusedCommands, "focused-command", "focused command run; may be repeated")
	limitations := stringList{}
	flags.Var(&limitations, "known-limitation", "known limitation; may be repeated")
	testObjections := testObjectionList{}
	flags.Var(&testObjections, "test-objection", "test|claim|evidence; may be repeated")
	testFiles := stringList{}
	flags.Var(&testFiles, "test-file", "repository-relative test file; may be repeated")
	infrastructureFiles := stringList{}
	flags.Var(&infrastructureFiles, "infrastructure-file", "repository-relative test-infrastructure file; may be repeated")
	focusedTestCommand := flags.String("focused-test-command", "", "focused test command the coordinator must rerun")
	expectedFailureReason := flags.String("expected-failure-reason", "", "failure identifier expected in the coordinator rerun output")
	observedFailures := pairList{}
	flags.Var(&observedFailures, "observed-failure", "red-test evidence kind=detail; may be repeated")
	uncoveredCriteria := stringList{}
	flags.Var(&uncoveredCriteria, "uncovered-criterion", "acceptance criterion not covered by this test handoff; may be repeated")
	objectionDecision := flags.String("objection-decision", "", "accepted or rejected response to the current test objection")
	objectionReason := flags.String("objection-reason", "", "observable reason for the test objection response")
	findings := reviewFindingList{}
	flags.Var(&findings, "finding", "review finding location|claim|evidence|severity|category|resolution|owner; may be repeated")
	questions := pairList{}
	flags.Var(&questions, "question", "question-id=question text; may be repeated")
	evidence := pairList{}
	flags.Var(&evidence, "evidence", "kind=detail; may be repeated")
	exemptions := stringList{}
	flags.Var(&exemptions, "exemption", "human, technical, or baseline; may be repeated")
	escalations := stringList{}
	flags.Var(&escalations, "escalation", "test_dispute or review_finding; may be repeated")
	blockers := stringList{}
	flags.Var(&blockers, "blocker", "setup, gate, test, review, harness, budget, or unknown; may be repeated")
	budgetExhausted := flags.Bool("budget-exhausted", false, "explicit harness signal that the configured budget was exhausted")
	inputTokens := flags.Int64("input-tokens", 0, "reliable harness-reported input tokens")
	outputTokens := flags.Int64("output-tokens", 0, "reliable harness-reported output tokens")
	totalTokens := flags.Int64("total-tokens", 0, "reliable harness-reported total tokens")
	costMicros := flags.Int64("cost-micros", 0, "reliable harness-reported cost in millionths of currency")
	costCurrency := flags.String("cost-currency", "", "three-letter uppercase currency for --cost-micros")
	nativeSessionID := flags.String("native-session-id", "", "harness-native session identifier")
	if err := flags.Parse(request.Args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(request.ErrorsOutput, errors.New("factory-report does not accept positional arguments"))
		return 2
	}
	identity := map[string]string{
		"invocation_id": lookup("FACTORY_INVOCATION_ID"),
		"run_id":        lookup("FACTORY_RUN_ID"),
		"harness":       lookup("FACTORY_HARNESS"),
		"role":          lookup("FACTORY_ROLE"),
		"stage":         lookup("FACTORY_STAGE"),
	}
	for field, value := range identity {
		if strings.TrimSpace(value) == "" {
			writeError(request.ErrorsOutput, fmt.Errorf("%s environment value is required", field))
			return 1
		}
	}
	resultDirectory := lookup("FACTORY_RESULT_DIR")
	if strings.TrimSpace(resultDirectory) == "" {
		writeError(request.ErrorsOutput, errors.New("FACTORY_RESULT_DIR environment value is required"))
		return 1
	}
	value := report.Report{
		SchemaVersion:   report.SchemaVersion,
		InvocationID:    identity["invocation_id"],
		RunID:           identity["run_id"],
		Harness:         identity["harness"],
		Role:            identity["role"],
		Stage:           identity["stage"],
		Outcome:         report.Outcome(*outcome),
		Summary:         *summary,
		BudgetExhausted: *budgetExhausted,
		NativeSessionID: *nativeSessionID,
		ReportedAt:      request.Now().UTC(),
	}
	if isReviewIdentity(identity["role"], identity["stage"]) && value.Outcome == report.OutcomeCompleted {
		value.ReviewHandoff = &report.ReviewHandoff{
			ReviewedSHA: lookup("FACTORY_CHECKPOINT_SHA"),
			Findings:    append([]report.ReviewFinding(nil), findings...),
		}
	}
	for _, item := range exemptions {
		value.Exemptions = append(value.Exemptions, report.Exemption(item))
	}
	for _, item := range escalations {
		value.Escalations = append(value.Escalations, report.EscalationCategory(item))
	}
	for _, item := range blockers {
		value.Blockers = append(value.Blockers, report.BlockerCategory(item))
	}
	if flagWasSet(flags, "input-tokens", "output-tokens", "total-tokens", "cost-micros", "cost-currency") {
		value.Usage = &report.Usage{
			Available:    true,
			InputTokens:  *inputTokens,
			OutputTokens: *outputTokens,
			TotalTokens:  *totalTokens,
			CostReported: flagWasSet(flags, "cost-micros", "cost-currency"),
			CostMicros:   *costMicros,
			Currency:     *costCurrency,
		}
	}
	if identity["role"] == "test" && identity["stage"] == "test" {
		for _, pair := range acceptance {
			value.TestHandoff = ensureTestHandoff(value.TestHandoff)
			value.TestHandoff.AcceptanceCoverage = append(value.TestHandoff.AcceptanceCoverage, report.AcceptanceMapping{Criterion: pair.Key, Evidence: pair.Value})
		}
		if len(testFiles) > 0 || len(infrastructureFiles) > 0 || *focusedTestCommand != "" || *expectedFailureReason != "" || len(observedFailures) > 0 || len(uncoveredCriteria) > 0 {
			value.TestHandoff = ensureTestHandoff(value.TestHandoff)
			value.TestHandoff.ChangedFiles = append(value.TestHandoff.ChangedFiles, testFiles...)
			for _, path := range infrastructureFiles {
				if !containsString(value.TestHandoff.ChangedFiles, path) {
					value.TestHandoff.ChangedFiles = append(value.TestHandoff.ChangedFiles, path)
				}
			}
			value.TestHandoff.InfrastructureChanges = append(value.TestHandoff.InfrastructureChanges, infrastructureFiles...)
			value.TestHandoff.FocusedTestCommand = *focusedTestCommand
			value.TestHandoff.ExpectedFailureReason = *expectedFailureReason
			value.TestHandoff.UncoveredCriteria = append(value.TestHandoff.UncoveredCriteria, uncoveredCriteria...)
			for _, pair := range observedFailures {
				value.TestHandoff.ObservedFailureEvidence = append(value.TestHandoff.ObservedFailureEvidence, report.Evidence{Kind: pair.Key, Detail: pair.Value})
			}
		}
	} else if value.ReviewHandoff != nil {
		// Review findings are assembled above from the dedicated --finding
		// flags; ordinary implementation handoff flags are intentionally not
		// accepted as a review payload.
	} else {
		for _, pair := range acceptance {
			value.Handoff = ensureHandoff(value.Handoff)
			value.Handoff.AcceptanceMapping = append(value.Handoff.AcceptanceMapping, report.AcceptanceMapping{Criterion: pair.Key, Evidence: pair.Value})
		}
		if *changeSummary != "" || len(productionFiles) > 0 || len(focusedCommands) > 0 || len(limitations) > 0 || len(testObjections) > 0 {
			value.Handoff = ensureHandoff(value.Handoff)
			value.Handoff.ChangeSummary = *changeSummary
			value.Handoff.ProductionFilesChanged = append([]string(nil), productionFiles...)
			value.Handoff.FocusedCommands = append([]string(nil), focusedCommands...)
			value.Handoff.KnownLimitations = append([]string(nil), limitations...)
			value.Handoff.TestObjections = append([]report.TestObjection(nil), testObjections...)
		}
	}
	if identity["role"] == "test" && identity["stage"] == "test" && (*objectionDecision != "" || *objectionReason != "") {
		value.TestObjectionResponse = &report.TestObjectionResponse{
			Decision: report.TestObjectionDecision(*objectionDecision),
			Reason:   *objectionReason,
		}
	}
	for _, pair := range questions {
		value.Questions = append(value.Questions, report.Question{ID: pair.Key, Prompt: pair.Value})
	}
	for _, pair := range evidence {
		value.Evidence = append(value.Evidence, report.Evidence{Kind: pair.Key, Detail: pair.Value})
	}
	path, err := report.WriteAtomicForInvocation(resultDirectory, value.InvocationID, value)
	if err != nil {
		writeError(request.ErrorsOutput, err)
		return 1
	}
	if _, err := fmt.Fprintf(request.Output, "report written: %s\n", path); err != nil {
		writeError(request.ErrorsOutput, err)
		return 1
	}
	return 0
}

// isReviewIdentity identifies every factory-owned review role that uses the
// shared structured finding contract.
func isReviewIdentity(role, stage string) bool {
	return (role == "spec_review" && stage == "review") ||
		(role == "standards_review" && stage == "standards_review")
}

// ensureHandoff returns a mutable handoff value for repeated completion flags.
func ensureHandoff(value *report.Handoff) *report.Handoff {
	if value != nil {
		return value
	}
	return &report.Handoff{}
}

// ensureTestHandoff returns a mutable test handoff value for repeated test
// stage flags.
func ensureTestHandoff(value *report.TestHandoff) *report.TestHandoff {
	if value != nil {
		return value
	}
	return &report.TestHandoff{}
}

// containsString reports whether a repeatable path flag was already included.
func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// flagWasSet reports whether a caller explicitly supplied one of the optional
// usage flags, preserving the distinction between zero and unavailable data.
func flagWasSet(flags *flag.FlagSet, names ...string) bool {
	set := make(map[string]struct{}, len(names))
	flags.Visit(func(value *flag.Flag) { set[value.Name] = struct{}{} })
	for _, name := range names {
		if _, ok := set[name]; ok {
			return true
		}
	}
	return false
}

// writeError writes one CLI error without exposing credentials or transcripts.
func writeError(output io.Writer, err error) {
	if err != nil {
		_, _ = fmt.Fprintf(output, "error: %v\n", err)
	}
}

// stringList collects repeatable nonempty string flags.
type stringList []string

// String returns the comma-separated flag value.
func (value *stringList) String() string { return strings.Join(*value, ",") }

// Set appends one nonempty string flag value.
func (value *stringList) Set(input string) error {
	if strings.TrimSpace(input) == "" {
		return errors.New("value must not be empty")
	}
	*value = append(*value, input)
	return nil
}

// testObjectionList collects the three bounded fields required to challenge a
// protected test from the implementation handoff.
type testObjectionList []report.TestObjection

// String renders the compact test|claim|evidence flag form.
func (value *testObjectionList) String() string {
	parts := make([]string, 0, len(*value))
	for _, objection := range *value {
		parts = append(parts, strings.Join([]string{objection.Test, objection.Claim, objection.Evidence}, "|"))
	}
	return strings.Join(parts, ",")
}

// Set parses one test|claim|evidence objection without shell interpretation.
func (value *testObjectionList) Set(input string) error {
	parts := strings.SplitN(input, "|", 3)
	if len(parts) != 3 {
		return errors.New("value must use test|claim|evidence form")
	}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			return errors.New("test objection fields must not be empty")
		}
	}
	*value = append(*value, report.TestObjection{Test: parts[0], Claim: parts[1], Evidence: parts[2]})
	return nil
}

// pair is one key=value flag parsed without shell interpretation.
type pair struct {
	// Key is the left side of the pair.
	Key string
	// Value is the right side of the pair.
	Value string
}

// pairList collects repeatable key=value flags.
type pairList []pair

// String returns the comma-separated pair value.
func (value *pairList) String() string {
	parts := make([]string, 0, len(*value))
	for _, item := range *value {
		parts = append(parts, item.Key+"="+item.Value)
	}
	return strings.Join(parts, ",")
}

// Set parses one nonempty key=value flag.
func (value *pairList) Set(input string) error {
	key, item, ok := strings.Cut(input, "=")
	if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(item) == "" {
		return errors.New("value must use nonempty key=value form")
	}
	*value = append(*value, pair{Key: key, Value: item})
	return nil
}

// reviewFindingList parses one complete finding from seven pipe-separated
// fields. The worker prompt supplies the order so each accepted finding is
// independently routable without a second mutable parser state.
type reviewFindingList []report.ReviewFinding

// String renders the compact finding form used by flag help and tests.
func (value *reviewFindingList) String() string {
	parts := make([]string, 0, len(*value))
	for _, finding := range *value {
		parts = append(parts, strings.Join([]string{
			finding.Location,
			finding.Claim,
			finding.Evidence,
			string(finding.Severity),
			string(finding.Category),
			finding.SuggestedResolution,
			finding.SuggestedOwner,
		}, "|"))
	}
	return strings.Join(parts, ",")
}

// Set appends one complete structured review finding.
func (value *reviewFindingList) Set(input string) error {
	parts := strings.Split(input, "|")
	if len(parts) != 7 {
		return errors.New("finding must use location|claim|evidence|severity|category|resolution|owner form")
	}
	for index, part := range parts {
		if strings.TrimSpace(part) == "" {
			return fmt.Errorf("finding field %d must not be empty", index+1)
		}
	}
	*value = append(*value, report.ReviewFinding{
		Location:            parts[0],
		Claim:               parts[1],
		Evidence:            parts[2],
		Severity:            report.ReviewSeverity(parts[3]),
		Category:            report.ReviewCategory(parts[4]),
		SuggestedResolution: parts[5],
		SuggestedOwner:      parts[6],
	})
	return nil
}
