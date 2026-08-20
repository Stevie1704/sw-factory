package report_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/report"
)

// TestWriteAtomicWritesOneInvocationReport verifies that a valid report is
// published as one JSON file below the invocation-specific result directory.
func TestWriteAtomicWritesOneInvocationReport(t *testing.T) {
	resultRoot := t.TempDir()
	want := report.Report{
		SchemaVersion: report.SchemaVersion,
		InvocationID:  "inv-1",
		RunID:         "run-1",
		Harness:       "codex",
		Role:          "implementation",
		Stage:         "implementation",
		Outcome:       report.OutcomeCompleted,
		Summary:       "implemented the requested behavior",
		Handoff: &report.Handoff{
			ChangeSummary:          "added the implementation",
			AcceptanceMapping:      []report.AcceptanceMapping{{Criterion: "visible agent", Evidence: "contract test"}},
			ProductionFilesChanged: []string{"internal/factory/agent.go"},
			FocusedCommands:        []string{"go test ./internal/factory"},
			KnownLimitations:       []string{"live cmux rendering needs an operator check"},
		},
		ReportedAt: time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC),
	}

	path, err := report.WriteAtomic(filepath.Join(resultRoot, "inv-1"), want)
	if err != nil {
		t.Fatalf("WriteAtomic() error = %v", err)
	}
	if path != filepath.Join(resultRoot, "inv-1", "report.json") {
		t.Fatalf("WriteAtomic() path = %q, want invocation report path", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("report file was not published: %v", err)
	}
	got, err := report.Read(path)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got.InvocationID != want.InvocationID || got.Outcome != want.Outcome || got.Handoff == nil {
		t.Fatalf("Read() = %#v, want %#v", got, want)
	}
	if entries, err := os.ReadDir(filepath.Join(resultRoot, "inv-1")); err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	} else if len(entries) != 1 || entries[0].Name() != "report.json" {
		t.Fatalf("invocation result directory contains %#v, want only report.json", entries)
	}
}

// TestValidateRejectsAReportThatDoesNotMatchTheInvocation verifies that a
// stale report cannot be accepted for a different run or stage.
func TestValidateRejectsAReportThatDoesNotMatchTheInvocation(t *testing.T) {
	value := completedReport()
	value.InvocationID = "stale-invocation"
	err := report.Validate(value, report.ValidationContext{
		InvocationID: "inv-1",
		RunID:        "run-1",
		Harness:      "codex",
		Role:         "implementation",
		Stage:        "implementation",
	})
	if err == nil || !strings.Contains(err.Error(), "invocation_id") {
		t.Fatalf("Validate() error = %v, want invocation identity error", err)
	}
}

// TestValidateRequiresTheStructuredPayloadForEachOutcome verifies that each
// outcome carries the handoff or evidence needed by the coordinator.
func TestValidateRequiresTheStructuredPayloadForEachOutcome(t *testing.T) {
	tests := []struct {
		name    string
		outcome report.Outcome
		mutate  func(*report.Report)
		want    string
	}{
		{
			name:    "completed handoff",
			outcome: report.OutcomeCompleted,
			mutate:  func(value *report.Report) { value.Handoff = nil },
			want:    "handoff",
		},
		{
			name:    "clarification questions",
			outcome: report.OutcomeNeedsClarification,
			mutate:  func(value *report.Report) { value.Questions = nil },
			want:    "question",
		},
		{
			name:    "cannot proceed evidence",
			outcome: report.OutcomeCannotProceed,
			mutate:  func(value *report.Report) { value.Evidence = nil },
			want:    "evidence",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := completedReport()
			value.Outcome = test.outcome
			value.Handoff = completedReport().Handoff
			value.Questions = []report.Question{{ID: "q-1", Prompt: "Which behavior is intended?"}}
			value.Evidence = []report.Evidence{{Kind: "command", Detail: "go test ./..."}}
			test.mutate(&value)
			err := report.Validate(value, report.ValidationContext{
				InvocationID: "inv-1",
				RunID:        "run-1",
				Harness:      "codex",
				Role:         "implementation",
				Stage:        "implementation",
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %q error", err, test.want)
			}
		})
	}
}

// TestValidateChecksPermittedFilesAgainstTheObservedWorktree verifies that a
// handoff cannot claim a path outside the coordinator-approved worktree state.
func TestValidateChecksPermittedFilesAgainstTheObservedWorktree(t *testing.T) {
	value := completedReport()
	value.Handoff.ProductionFilesChanged = []string{"internal/factory/agent.go", "../outside.go"}
	err := report.Validate(value, report.ValidationContext{
		InvocationID:    "inv-1",
		RunID:           "run-1",
		Harness:         "codex",
		Role:            "implementation",
		Stage:           "implementation",
		WorktreePath:    t.TempDir(),
		PermittedPaths:  []string{"internal"},
		ObservedChanges: []string{"internal/factory/agent.go"},
	})
	if err == nil || !strings.Contains(err.Error(), "production_files_changed") {
		t.Fatalf("Validate() error = %v, want permitted path error", err)
	}
}

// TestValidateRejectsAProductionFileFromAnInspectedCleanWorktree verifies that
// an empty observed change set is authoritative rather than an instruction to
// skip worktree validation.
func TestValidateRejectsAProductionFileFromAnInspectedCleanWorktree(t *testing.T) {
	value := completedReport()
	err := report.Validate(value, report.ValidationContext{
		InvocationID:     "inv-1",
		RunID:            "run-1",
		Harness:          "codex",
		Role:             "implementation",
		Stage:            "implementation",
		WorktreeObserved: true,
	})
	if err == nil || !strings.Contains(err.Error(), "was not observed") {
		t.Fatalf("Validate() error = %v, want clean-worktree mismatch", err)
	}
}

// completedReport returns an independent valid fixture for report validation.
func completedReport() report.Report {
	return report.Report{
		SchemaVersion: report.SchemaVersion,
		InvocationID:  "inv-1",
		RunID:         "run-1",
		Harness:       "codex",
		Role:          "implementation",
		Stage:         "implementation",
		Outcome:       report.OutcomeCompleted,
		Summary:       "done",
		Handoff: &report.Handoff{
			ChangeSummary:          "changed the implementation",
			AcceptanceMapping:      []report.AcceptanceMapping{{Criterion: "criterion", Evidence: "test"}},
			ProductionFilesChanged: []string{"internal/factory/agent.go"},
			FocusedCommands:        []string{"go test ./internal/factory"},
		},
		ReportedAt: time.Now().UTC(),
	}
}
