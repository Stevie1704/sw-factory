package reportcli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/reportcli"
)

// summaryEnvironment returns the coordinator environment of one PR writer.
func summaryEnvironment(role, stage, resultDirectory string) map[string]string {
	return map[string]string{
		"FACTORY_INVOCATION_ID": "inv-pr",
		"FACTORY_RUN_ID":        "run-pr",
		"FACTORY_HARNESS":       "claude",
		"FACTORY_ROLE":          role,
		"FACTORY_STAGE":         stage,
		"FACTORY_RESULT_DIR":    resultDirectory,
	}
}

// TestRunWritesThePullRequestSummaryFromAFile verifies the PR writer passes a
// multi-line markdown body through a file instead of a single-line flag.
func TestRunWritesThePullRequestSummaryFromAFile(t *testing.T) {
	resultDirectory := t.TempDir()
	body := "## Summary\n\nAdds a role.\n"
	summaryPath := filepath.Join(t.TempDir(), "summary.md")
	if err := os.WriteFile(summaryPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var errorsOutput bytes.Buffer
	status := reportcli.Run(reportcli.Request{
		Args:         []string{"--outcome", "completed", "--summary", "summary written", "--summary-file", summaryPath},
		Environment:  summaryEnvironment("pr_writer", "pr_summary", resultDirectory),
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
	if value.PullRequestSummary == nil || value.PullRequestSummary.Body != body {
		t.Fatalf("pull-request summary = %#v, want the file body", value.PullRequestSummary)
	}
}

// TestRunRejectsASummaryFileOutsideThePullRequestWriter verifies only the PR
// writer may carry a summary payload, and that it carries nothing else.
func TestRunRejectsASummaryFileOutsideThePullRequestWriter(t *testing.T) {
	summaryPath := filepath.Join(t.TempDir(), "summary.md")
	if err := os.WriteFile(summaryPath, []byte("text"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		role, stage string
		args        []string
	}{
		"implementation role": {"implementation", "implementation", []string{"--outcome", "completed", "--summary", "s", "--summary-file", summaryPath}},
		"writer with handoff": {"pr_writer", "pr_summary", []string{"--outcome", "completed", "--summary", "s", "--summary-file", summaryPath, "--change-summary", "c"}},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			status := reportcli.Run(reportcli.Request{
				Args:         test.args,
				Environment:  summaryEnvironment(test.role, test.stage, t.TempDir()),
				Output:       &bytes.Buffer{},
				ErrorsOutput: &bytes.Buffer{},
			})
			if status == 0 {
				t.Fatal("Run() status = 0, want a refused summary payload")
			}
		})
	}
}
