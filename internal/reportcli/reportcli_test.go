package reportcli_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/reportcli"
)

// TestRunWritesTheStructuredReportUsingInvocationEnvironment verifies that the
// worker command binds identity to coordinator-provided environment values.
func TestRunWritesTheStructuredReportUsingInvocationEnvironment(t *testing.T) {
	resultDirectory := t.TempDir()
	var output, errorsOutput bytes.Buffer
	status := reportcli.Run(reportcli.Request{
		Args: []string{
			"--outcome", "completed",
			"--summary", "implementation complete",
			"--change-summary", "added the behavior",
			"--acceptance", "criterion=focused test",
			"--production-file", "internal/factory/agent.go",
			"--focused-command", "go test ./internal/factory",
		},
		Environment: map[string]string{
			"FACTORY_INVOCATION_ID": "inv-1",
			"FACTORY_RUN_ID":        "run-1",
			"FACTORY_HARNESS":       "codex",
			"FACTORY_ROLE":          "implementation",
			"FACTORY_STAGE":         "implementation",
			"FACTORY_RESULT_DIR":    resultDirectory,
		},
		Now:          func() time.Time { return time.Date(2026, 8, 21, 8, 0, 0, 0, time.UTC) },
		Output:       &output,
		ErrorsOutput: &errorsOutput,
	})
	if status != 0 {
		t.Fatalf("Run() status = %d, stderr = %s", status, errorsOutput.String())
	}
	value, err := report.Read(resultDirectory + "/report.json")
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if value.InvocationID != "inv-1" || value.Handoff == nil || value.Handoff.ProductionFilesChanged[0] != "internal/factory/agent.go" {
		t.Fatalf("report = %#v, want coordinator-bound completed handoff", value)
	}
	if !strings.Contains(output.String(), "report written") {
		t.Fatalf("stdout = %q, want report path confirmation", output.String())
	}
}

// TestRunRejectsAClarificationWithoutQuestions verifies the CLI cannot emit an
// outcome that the coordinator would have to interpret.
func TestRunRejectsAClarificationWithoutQuestions(t *testing.T) {
	var errorsOutput bytes.Buffer
	status := reportcli.Run(reportcli.Request{
		Args: []string{"--outcome", "needs_clarification", "--summary", "need input"},
		Environment: map[string]string{
			"FACTORY_INVOCATION_ID": "inv-1",
			"FACTORY_RUN_ID":        "run-1",
			"FACTORY_HARNESS":       "codex",
			"FACTORY_ROLE":          "implementation",
			"FACTORY_STAGE":         "implementation",
			"FACTORY_RESULT_DIR":    t.TempDir(),
		},
		Output:       &bytes.Buffer{},
		ErrorsOutput: &errorsOutput,
	})
	if status != 1 || !strings.Contains(errorsOutput.String(), "question") {
		t.Fatalf("Run() status = %d, stderr = %q, want question validation error", status, errorsOutput.String())
	}
}

// TestRunAcceptsExplicitHarnessUsage verifies the worker-facing command keeps
// usage optional and writes only values supplied through numeric flags.
func TestRunAcceptsExplicitHarnessUsage(t *testing.T) {
	resultDirectory := t.TempDir()
	var errorsOutput bytes.Buffer
	status := reportcli.Run(reportcli.Request{
		Args: []string{"--outcome", "completed", "--summary", "done", "--change-summary", "changed", "--acceptance", "criterion=evidence", "--production-file", "internal/factory/agent.go", "--focused-command", "go test ./...", "--input-tokens", "12", "--output-tokens", "8", "--total-tokens", "20", "--cost-micros", "15", "--cost-currency", "USD", "--budget-exhausted", "--escalation", "review_finding", "--exemption", "technical", "--blocker", "review"},
		Environment: map[string]string{
			"FACTORY_INVOCATION_ID": "inv-1",
			"FACTORY_RUN_ID":        "run-1",
			"FACTORY_HARNESS":       "codex",
			"FACTORY_ROLE":          "implementation",
			"FACTORY_STAGE":         "implementation",
			"FACTORY_RESULT_DIR":    resultDirectory,
		},
		Output:       &bytes.Buffer{},
		ErrorsOutput: &errorsOutput,
	})
	if status != 0 {
		t.Fatalf("Run() status = %d, stderr = %s", status, errorsOutput.String())
	}
	value, err := report.Read(resultDirectory + "/report.json")
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if value.Usage == nil || !value.Usage.Available || value.Usage.TotalTokens != 20 || value.Usage.CostMicros != 15 {
		t.Fatalf("usage = %#v, want explicit CLI measurements", value.Usage)
	}
	if !value.BudgetExhausted || len(value.Escalations) != 1 || len(value.Exemptions) != 1 || len(value.Blockers) != 1 {
		t.Fatalf("evaluation signals = %#v, want explicit CLI signals", value)
	}
}

// TestRunWritesTheTestStageHandoff verifies the worker-facing command can
// publish acceptance coverage, changed test files, and observed red evidence.
func TestRunWritesTheTestStageHandoff(t *testing.T) {
	resultDirectory := t.TempDir()
	var errorsOutput bytes.Buffer
	status := reportcli.Run(reportcli.Request{
		Args: []string{
			"--outcome", "completed",
			"--summary", "red test suite published",
			"--acceptance", "criterion=focused red test",
			"--test-file", "internal/factory/agent_test.go",
			"--infrastructure-file", "test-support/fixtures.go",
			"--focused-test-command", "go test ./internal/factory -run TestNewBehavior",
			"--expected-failure-reason", "expected behavior assertion",
			"--observed-failure", "exit_code=1",
		},
		Environment: map[string]string{
			"FACTORY_INVOCATION_ID": "inv-test",
			"FACTORY_RUN_ID":        "run-test",
			"FACTORY_HARNESS":       "codex",
			"FACTORY_ROLE":          "test",
			"FACTORY_STAGE":         "test",
			"FACTORY_RESULT_DIR":    resultDirectory,
		},
		Output:       &bytes.Buffer{},
		ErrorsOutput: &errorsOutput,
	})
	if status != 0 {
		t.Fatalf("Run() status = %d, stderr = %s", status, errorsOutput.String())
	}
	value, err := report.Read(resultDirectory + "/report.json")
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if value.TestHandoff == nil || value.TestHandoff.FocusedTestCommand == "" || value.TestHandoff.ExpectedFailureReason != "expected behavior assertion" || len(value.TestHandoff.ObservedFailureEvidence) != 1 {
		t.Fatalf("test handoff = %#v, want focused command and red evidence", value.TestHandoff)
	}
}

// TestRunWritesTestObjectionsAndResponses verifies the worker-facing report
// command exposes the structured implementation dispute protocol.
func TestRunWritesTestObjectionsAndResponses(t *testing.T) {
	implementationDirectory := t.TempDir()
	var implementationErrors bytes.Buffer
	status := reportcli.Run(reportcli.Request{
		Args: []string{
			"--outcome", "completed", "--summary", "implementation dispute",
			"--change-summary", "implemented public behavior", "--acceptance", "behavior=focused test",
			"--production-file", "internal/factory/agent.go", "--focused-command", "go test ./internal/factory",
			"--test-objection", "internal/factory/agent_test.go:TestBehavior|the assertion is too narrow|public behavior passes while the assertion fails",
		},
		Environment: map[string]string{
			"FACTORY_INVOCATION_ID": "inv-implementation", "FACTORY_RUN_ID": "run-1", "FACTORY_HARNESS": "codex",
			"FACTORY_ROLE": "implementation", "FACTORY_STAGE": "implementation", "FACTORY_RESULT_DIR": implementationDirectory,
		},
		Output: &bytes.Buffer{}, ErrorsOutput: &implementationErrors,
	})
	if status != 0 {
		t.Fatalf("implementation Run() status = %d, stderr = %s", status, implementationErrors.String())
	}
	implementation, err := report.Read(implementationDirectory + "/report.json")
	if err != nil {
		t.Fatal(err)
	}
	if implementation.Handoff == nil || len(implementation.Handoff.TestObjections) != 1 || implementation.Handoff.TestObjections[0].Claim == "" {
		t.Fatalf("implementation handoff = %#v, want one structured objection", implementation.Handoff)
	}

	testDirectory := t.TempDir()
	var testErrors bytes.Buffer
	status = reportcli.Run(reportcli.Request{
		Args: []string{"--outcome", "completed", "--summary", "objection accepted", "--objection-decision", "accepted", "--objection-reason", "the public contract is correct", "--acceptance", "behavior=revised focused test", "--test-file", "internal/factory/agent_test.go", "--focused-test-command", "go test ./internal/factory -run TestBehavior", "--expected-failure-reason", "expected behavior assertion", "--observed-failure", "exit_code=1"},
		Environment: map[string]string{
			"FACTORY_INVOCATION_ID": "inv-test", "FACTORY_RUN_ID": "run-1", "FACTORY_HARNESS": "codex",
			"FACTORY_ROLE": "test", "FACTORY_STAGE": "test", "FACTORY_RESULT_DIR": testDirectory,
		},
		Output: &bytes.Buffer{}, ErrorsOutput: &testErrors,
	})
	if status != 0 {
		t.Fatalf("test Run() status = %d, stderr = %s", status, testErrors.String())
	}
	testReport, err := report.Read(testDirectory + "/report.json")
	if err != nil {
		t.Fatal(err)
	}
	if testReport.TestObjectionResponse == nil || testReport.TestObjectionResponse.Decision != report.TestObjectionAccepted || testReport.TestHandoff == nil {
		t.Fatalf("test report = %#v, want accepted objection response and revised handoff", testReport)
	}
}

// TestRunWritesTheSpecificationReviewFindingContract verifies repeated review
// flags bind complete findings to the coordinator-provided checkpoint.
func TestRunWritesTheSpecificationReviewFindingContract(t *testing.T) {
	resultDirectory := t.TempDir()
	var errorsOutput bytes.Buffer
	status := reportcli.Run(reportcli.Request{
		Args: []string{
			"--outcome", "completed", "--summary", "review complete",
			"--finding", "internal/factory/review.go:1|behavior is incorrect|focused assertion|blocker|correctness|reject the behavior|implementation",
			"--finding", "internal/factory/review.go:2|name is subjective|style preference|advisory|taste|consider renaming|implementation",
		},
		Environment: map[string]string{
			"FACTORY_INVOCATION_ID":  "inv-review",
			"FACTORY_RUN_ID":         "run-review",
			"FACTORY_HARNESS":        "codex",
			"FACTORY_ROLE":           "spec_review",
			"FACTORY_STAGE":          "review",
			"FACTORY_CHECKPOINT_SHA": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			"FACTORY_RESULT_DIR":     resultDirectory,
		},
		Output:       &bytes.Buffer{},
		ErrorsOutput: &errorsOutput,
	})
	if status != 0 {
		t.Fatalf("Run() status = %d, stderr = %s", status, errorsOutput.String())
	}
	value, err := report.Read(resultDirectory + "/report.json")
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if value.ReviewHandoff == nil || value.ReviewHandoff.ReviewedSHA != "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" || len(value.ReviewHandoff.Findings) != 2 {
		t.Fatalf("review handoff = %#v, want exact SHA and two findings", value.ReviewHandoff)
	}
}

// TestRunCarriesFindingsForIncompleteReviewOutcomes verifies the CLI keeps
// structured findings when the reviewer also reports a bounded interruption.
func TestRunCarriesFindingsForIncompleteReviewOutcomes(t *testing.T) {
	for _, test := range []struct {
		name       string
		outcome    string
		contextArg string
	}{
		{name: "needs clarification", outcome: "needs_clarification", contextArg: "--question=artifact=which artifact should be reviewed"},
		{name: "cannot proceed", outcome: "cannot_proceed", contextArg: "--evidence=artifact=the required artifact is unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			resultDirectory := t.TempDir()
			var errorsOutput bytes.Buffer
			status := reportcli.Run(reportcli.Request{
				Args: []string{
					"--outcome", test.outcome, "--summary", "review stopped after establishing a finding",
					"--finding", "internal/factory/review.go:42|the checkpoint violates the requirement|the observed behavior is incorrect|blocker|correctness|repair the behavior|implementation",
					test.contextArg,
				},
				Environment: map[string]string{
					"FACTORY_INVOCATION_ID":  "inv-incomplete-review",
					"FACTORY_RUN_ID":         "run-incomplete-review",
					"FACTORY_HARNESS":        "codex",
					"FACTORY_ROLE":           "spec_review",
					"FACTORY_STAGE":          "review",
					"FACTORY_CHECKPOINT_SHA": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
					"FACTORY_RESULT_DIR":     resultDirectory,
				},
				Output: &bytes.Buffer{}, ErrorsOutput: &errorsOutput,
			})
			if status != 0 {
				t.Fatalf("Run() status = %d, stderr = %s", status, errorsOutput.String())
			}
			value, err := report.Read(resultDirectory + "/report.json")
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			if value.ReviewHandoff == nil || len(value.ReviewHandoff.Findings) != 1 {
				t.Fatalf("review handoff = %#v, want one finding for %s", value.ReviewHandoff, test.outcome)
			}
		})
	}
}

// TestRunRejectsReviewFindingsForNonReviewOutcomes verifies outcome-inapplicable
// finding flags fail explicitly instead of disappearing from the report.
func TestRunRejectsReviewFindingsForNonReviewOutcomes(t *testing.T) {
	var errorsOutput bytes.Buffer
	status := reportcli.Run(reportcli.Request{
		Args: []string{
			"--outcome", "cannot_proceed", "--summary", "implementation stopped",
			"--evidence", "command=the command cannot run",
			"--finding", "internal/factory/agent.go:42|the behavior is wrong|the test demonstrates it|blocker|correctness|repair it|implementation",
		},
		Environment: map[string]string{
			"FACTORY_INVOCATION_ID": "inv-implementation",
			"FACTORY_RUN_ID":        "run-implementation",
			"FACTORY_HARNESS":       "codex",
			"FACTORY_ROLE":          "implementation",
			"FACTORY_STAGE":         "implementation",
			"FACTORY_RESULT_DIR":    t.TempDir(),
		},
		Output: &bytes.Buffer{}, ErrorsOutput: &errorsOutput,
	})
	if status == 0 || !strings.Contains(errorsOutput.String(), "--finding") {
		t.Fatalf("Run() status = %d, stderr = %q, want explicit finding rejection", status, errorsOutput.String())
	}
}
