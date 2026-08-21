package report_test

import (
	"bytes"
	"encoding/json"
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

// TestWriteAtomicForInvocationWritesAReadableReport verifies the worker-mounted
// result-directory entry point binds the explicit invocation identity.
func TestWriteAtomicForInvocationWritesAReadableReport(t *testing.T) {
	resultDirectory := filepath.Join(t.TempDir(), "results")
	path, err := report.WriteAtomicForInvocation(resultDirectory, "inv-1", completedReport())
	if err != nil {
		t.Fatalf("WriteAtomicForInvocation() error = %v", err)
	}
	if path != filepath.Join(resultDirectory, report.ReportFileName) {
		t.Fatalf("WriteAtomicForInvocation() path = %q, want report path", path)
	}
	value, err := report.Read(path)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if value.InvocationID != "inv-1" || value.RunID != "run-1" {
		t.Fatalf("Read() = %#v, want invocation-bound report", value)
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

// TestReadRejectsUnknownFieldsAndTrailingJSON verifies the report reader is a
// strict single-envelope parser rather than a permissive JSON prefix reader.
func TestReadRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	root := t.TempDir()
	encoded, err := json.Marshal(completedReport())
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatal(err)
	}
	object["unexpected"] = true
	unknown, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	unknownPath := filepath.Join(root, "unknown.json")
	if err := os.WriteFile(unknownPath, unknown, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := report.Read(unknownPath); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Read(unknown) error = %v, want unknown-field rejection", err)
	}
	trailingPath := filepath.Join(root, "trailing.json")
	if err := os.WriteFile(trailingPath, append(encoded, []byte("\n{}")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := report.Read(trailingPath); err == nil || !strings.Contains(err.Error(), "more than one JSON value") {
		t.Fatalf("Read(trailing) error = %v, want trailing-value rejection", err)
	}
}

// TestReportContentLimitsRejectOversizedFieldsAndFiles verifies bounded text,
// collection, and serialized-file inputs are rejected before acceptance.
func TestReportContentLimitsRejectOversizedFieldsAndFiles(t *testing.T) {
	longText := strings.Repeat("x", 5000)
	for _, test := range []struct {
		name   string
		mutate func(*report.Report)
	}{
		{name: "summary", mutate: func(value *report.Report) { value.Summary = longText }},
		{name: "change summary", mutate: func(value *report.Report) { value.Handoff.ChangeSummary = longText }},
		{name: "mapping evidence", mutate: func(value *report.Report) { value.Handoff.AcceptanceMapping[0].Evidence = longText }},
		{name: "focused command", mutate: func(value *report.Report) { value.Handoff.FocusedCommands[0] = longText }},
		{name: "limitation", mutate: func(value *report.Report) { value.Handoff.KnownLimitations = []string{longText} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := report.Validate(func() report.Report {
				value := completedReport()
				test.mutate(&value)
				return value
			}(), report.ValidationContext{InvocationID: "inv-1", RunID: "run-1", Harness: "codex", Role: "implementation", Stage: "implementation"}); err == nil {
				t.Fatal("Validate() accepted oversized content")
			}
		})
	}
	path := filepath.Join(t.TempDir(), "oversized.json")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), report.MaxReportBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := report.Read(path); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("Read(oversized) error = %v, want byte-limit rejection", err)
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
