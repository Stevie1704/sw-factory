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
