package report_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/report"
)

// summaryReport returns a completed PR-writer report with the given body.
func summaryReport(body string) report.Report {
	return report.Report{
		SchemaVersion:      report.SchemaVersion,
		InvocationID:       "inv-pr",
		RunID:              "run-1",
		Harness:            "codex",
		Role:               "pr_writer",
		Stage:              "pr_summary",
		Outcome:            report.OutcomeCompleted,
		Summary:            "wrote the pull-request summary",
		PullRequestSummary: &report.PullRequestSummary{Body: body},
		ReportedAt:         time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC),
	}
}

// summaryContext returns the coordinator validation context of a PR writer.
func summaryContext() report.ValidationContext {
	return report.ValidationContext{
		InvocationID: "inv-pr", RunID: "run-1", Harness: "codex",
		Role: "pr_writer", Stage: "pr_summary", RoleKind: "summary",
	}
}

// TestValidateAcceptsAMultilinePullRequestSummary verifies a completed PR
// writer returns a markdown body that may span several lines.
func TestValidateAcceptsAMultilinePullRequestSummary(t *testing.T) {
	value := summaryReport("## Summary\n\nAdds a role.\n\n## Merge Danger\n\n**Door:** two-way")
	if err := report.Validate(value, summaryContext()); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	// The worker-side writer has no registry projection and must still accept
	// the PR-writer identity.
	if _, err := report.WriteAtomicForInvocation(t.TempDir(), "inv-pr", value); err != nil {
		t.Fatalf("WriteAtomicForInvocation() error = %v", err)
	}
}

// TestValidateRejectsAnInvalidPullRequestSummary verifies an empty, oversized,
// missing, or misplaced summary body is refused.
func TestValidateRejectsAnInvalidPullRequestSummary(t *testing.T) {
	missing := summaryReport("")
	missing.PullRequestSummary = nil
	misplaced := completedReport()
	misplaced.PullRequestSummary = &report.PullRequestSummary{Body: "text"}
	clarification := summaryReport("text")
	clarification.Outcome = report.OutcomeNeedsClarification
	clarification.Questions = []report.Question{{ID: "q1", Prompt: "Which issue?"}}
	cases := map[string]struct {
		value   report.Report
		context report.ValidationContext
	}{
		"empty body":         {summaryReport(" \n "), summaryContext()},
		"oversized body":     {summaryReport(strings.Repeat("x", report.MaxPullRequestSummaryRunes+1)), summaryContext()},
		"NUL in body":        {summaryReport("a\x00b"), summaryContext()},
		"missing summary":    {missing, summaryContext()},
		"non-writer summary": {misplaced, report.ValidationContext{InvocationID: "inv-1", RunID: "run-1", Harness: "codex", Role: "implementation", Stage: "implementation", RoleKind: "handoff"}},
		"incomplete summary": {clarification, summaryContext()},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			if err := report.Validate(test.value, test.context); err == nil {
				t.Fatal("Validate() error = nil, want a refused pull-request summary")
			}
		})
	}
}
