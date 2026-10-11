package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/cli"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestBugReportShowsWorkerTextOnlyOnTheTerminal verifies that the
// worker-authored text of a defect proposal is shown to the operator for
// review but is not collected into the draft file.
func TestBugReportShowsWorkerTextOnlyOnTheTerminal(t *testing.T) {
	t.Parallel()
	configPath, operationalPath, repositoryPath := registeredStatusFixture(t)
	started := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	opened, err := store.Open(context.Background(), operationalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	if err := opened.SaveRun(context.Background(), store.Run{
		ID: "run-defect", RepositoryPath: repositoryPath, IssueNumber: 5,
		Stage: store.StageImplementation, Status: store.StatusWaitingForHuman, CreatedAt: started, UpdatedAt: started,
	}); err != nil {
		t.Fatal(err)
	}
	resultDirectory := filepath.Join(t.TempDir(), "inv-defect")
	if err := opened.SaveInvocation(context.Background(), store.Invocation{
		ID: "inv-defect", RunID: "run-defect", Harness: "codex", Role: "implementation",
		Stage: store.StageImplementation, Status: store.InvocationStatusCannotProceed,
		ResultDirectory: resultDirectory, CreatedAt: started, UpdatedAt: started,
	}); err != nil {
		t.Fatal(err)
	}
	const workerText = "observed in /Users/alice/acme-private with token ghp_workertoken"
	if _, err := report.WriteAtomic(resultDirectory, report.Report{
		SchemaVersion: report.SchemaVersion, InvocationID: "inv-defect", RunID: "run-defect",
		Harness: "codex", Role: "implementation", Stage: "implementation",
		Outcome: report.OutcomeCannotProceed, Summary: "blocked",
		Evidence: []report.Evidence{{Kind: "command", Detail: "factory-report exited 1"}},
		SuspectedFactoryDefect: &report.FactoryDefectProposal{
			InvocationID: "inv-defect", Code: "report.handoff_rejected",
			Expected: "accepted", Observed: workerText, Reason: "no repository file",
		},
		ReportedAt: started,
	}); err != nil {
		t.Fatal(err)
	}
	draftPath := filepath.Join(t.TempDir(), "report.md")

	var output, errorsOutput bytes.Buffer
	if code := cli.Run(context.Background(), []string{"bug-report", "--config", configPath, "--run-id", "run-defect", "--output", draftPath}, &output, &errorsOutput); code != 0 {
		t.Fatalf("bug-report exit code = %d, stderr = %q", code, errorsOutput.String())
	}
	draft, err := os.ReadFile(draftPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(draft), "report.handoff_rejected") || strings.Contains(string(draft), "ghp_workertoken") || strings.Contains(string(draft), "/Users/alice") {
		t.Fatalf("draft = %s, want the proposal code without its worker text", draft)
	}
	if !strings.Contains(output.String(), workerText) || !strings.Contains(output.String(), "not in the draft") {
		t.Fatalf("stdout = %q, want the worker text for review, marked as not in the draft", output.String())
	}
}
