package reportcli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/reportcli"
)

// sessionEnvironment returns the coordinator environment of one session-role
// worker. FACTORY_RUN_ID names only the worker owner.
func sessionEnvironment(role, resultDirectory string) map[string]string {
	return map[string]string{
		"FACTORY_INVOCATION_ID":    "sinv-1",
		"FACTORY_RUN_ID":           "session-owner",
		"FACTORY_SESSION_ID":       "session-1",
		"FACTORY_SESSION_REVISION": "5",
		"FACTORY_HARNESS":          "codex",
		"FACTORY_ROLE":             role,
		"FACTORY_STAGE":            role,
		"FACTORY_RESULT_DIR":       resultDirectory,
	}
}

// writePayload writes one JSON payload file and returns its path.
func writePayload(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "payload.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunWritesSessionPayloadsFromFiles verifies that ideation and planning
// roles pass their structured payloads through JSON files, and that the report
// names the session and its pinned revision instead of a run.
func TestRunWritesSessionPayloadsFromFiles(t *testing.T) {
	tests := []struct {
		role    string
		flag    string
		payload string
		check   func(report.Report) bool
	}{
		{"ideation", "--ideation-file", `{"message":"Two questions follow.\nFirst: who?","questions":[{"id":"who","prompt":"Who uses it?"}],"readiness":{"proposal":"investigation_needed","rationale":"Unknown audience."}}`,
			func(value report.Report) bool {
				return value.Ideation != nil && strings.Contains(value.Ideation.Message, "\n") && len(value.Ideation.Questions) == 1
			}},
		{"planning", "--plan-file", `{"nodes":[{"id":"fix","kind":"task","title":"Fix the export","acceptance_criteria":["export succeeds"]}]}`,
			func(value report.Report) bool { return value.PlanDraft != nil && len(value.PlanDraft.Nodes) == 1 }},
	}
	for _, test := range tests {
		resultDirectory := t.TempDir()
		var errorsOutput bytes.Buffer
		status := reportcli.Run(reportcli.Request{
			Args:         []string{"--outcome", "completed", "--summary", "role turn done", test.flag, writePayload(t, test.payload)},
			Environment:  sessionEnvironment(test.role, resultDirectory),
			ErrorsOutput: &errorsOutput,
		})
		if status != 0 {
			t.Fatalf("Run(%s) status = %d, stderr = %s", test.role, status, errorsOutput.String())
		}
		value, err := report.Read(filepath.Join(resultDirectory, report.ReportFileName))
		if err != nil {
			t.Fatalf("Read(%s) error = %v", test.role, err)
		}
		if value.RunID != "" || value.SessionID != "session-1" || value.SessionRevision != 5 || !test.check(value) {
			t.Fatalf("Run(%s) report = %#v, want session-owned payload", test.role, value)
		}
	}
}

// TestRunRejectsMisplacedSessionPayloads verifies that a session payload file
// is refused for the wrong role or outside a session, and that an unknown JSON
// field in a payload file is refused.
func TestRunRejectsMisplacedSessionPayloads(t *testing.T) {
	ideation := `{"message":"hello"}`
	plan := `{"nodes":[{"id":"a","kind":"task","title":"A"}]}`
	tests := map[string]struct {
		environment map[string]string
		flag        string
		payload     string
	}{
		"plan for ideation":      {sessionEnvironment("ideation", ""), "--plan-file", plan},
		"ideation for planning":  {sessionEnvironment("planning", ""), "--ideation-file", ideation},
		"unknown field":          {sessionEnvironment("ideation", ""), "--ideation-file", `{"message":"hi","tracker_label":"agent-ready"}`},
		"ideation outside a run": {summaryEnvironment("implementation", "implementation", ""), "--ideation-file", ideation},
	}
	for name, test := range tests {
		test.environment["FACTORY_RESULT_DIR"] = t.TempDir()
		var errorsOutput bytes.Buffer
		status := reportcli.Run(reportcli.Request{
			Args:         []string{"--outcome", "completed", "--summary", "done", test.flag, writePayload(t, test.payload)},
			Environment:  test.environment,
			ErrorsOutput: &errorsOutput,
		})
		if status == 0 {
			t.Fatalf("%s: Run() status = 0, want refusal", name)
		}
	}
}
