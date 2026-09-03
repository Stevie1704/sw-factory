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
	"github.com/Stevie1704/sw-factory/internal/store"
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
		InvocationID:     "inv-1",
		RunID:            "run-1",
		Harness:          "codex",
		Role:             "implementation",
		Stage:            "implementation",
		WorktreeObserved: true,
		ObservedChanges:  []string{"internal/factory/agent.go"},
	})
	if err == nil || !strings.Contains(err.Error(), "invocation_id") {
		t.Fatalf("Validate() error = %v, want invocation identity error", err)
	}
}

// TestReportedUsageRoundTripsOnlyReliableMeasurements verifies optional
// harness usage is accepted as numeric metadata and remains separate from the
// ordinary human-readable handoff text.
func TestReportedUsageRoundTripsOnlyReliableMeasurements(t *testing.T) {
	want := completedReport()
	want.Usage = &report.Usage{Available: true, InputTokens: 12, OutputTokens: 8, TotalTokens: 20, CostReported: true, CostMicros: 15, Currency: "USD"}
	path, err := report.WriteAtomicForInvocation(filepath.Join(t.TempDir(), "results"), "inv-1", want)
	if err != nil {
		t.Fatalf("WriteAtomicForInvocation() error = %v", err)
	}
	got, err := report.Read(path)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got.Usage == nil || !got.Usage.Available || got.Usage.TotalTokens != 20 || got.Usage.CostMicros != 15 || got.Usage.Currency != "USD" {
		t.Fatalf("usage = %#v, want reported numeric usage", got.Usage)
	}
}

// TestReportedEvaluationSignalsRoundTripAsFixedMetadata verifies a harness can
// report budget, escalation, exemption, and blocker categories without adding
// finding or blocker text to the report envelope.
func TestReportedEvaluationSignalsRoundTripAsFixedMetadata(t *testing.T) {
	want := completedReport()
	want.BudgetExhausted = true
	want.Exemptions = []report.Exemption{report.ExemptionTechnical}
	want.Escalations = []report.EscalationCategory{report.EscalationReviewFinding}
	want.Blockers = []report.BlockerCategory{report.BlockerReview, report.BlockerReview}
	path, err := report.WriteAtomicForInvocation(filepath.Join(t.TempDir(), "results"), "inv-1", want)
	if err != nil {
		t.Fatalf("WriteAtomicForInvocation() error = %v", err)
	}
	got, err := report.Read(path)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if !got.BudgetExhausted || len(got.Exemptions) != 1 || len(got.Escalations) != 1 || len(got.Blockers) != 2 {
		t.Fatalf("evaluation signals = %#v, want fixed metadata signals", got)
	}
}

// TestValidateRejectsUnknownEvaluationSignals verifies report categories are
// closed vocabularies rather than a path for arbitrary work content.
func TestValidateRejectsUnknownEvaluationSignals(t *testing.T) {
	value := completedReport()
	value.Escalations = []report.EscalationCategory{"review finding text"}
	if err := report.Validate(value, report.ValidationContext{InvocationID: "inv-1", RunID: "run-1", Harness: "codex", Role: "implementation", Stage: "implementation"}); err == nil || !strings.Contains(err.Error(), "unsupported report escalation") {
		t.Fatalf("Validate() error = %v, want fixed escalation vocabulary error", err)
	}
}

// TestValidateRejectsUnavailableReportedUsage verifies a report cannot disguise
// an unavailable measurement as a reliable value.
func TestValidateRejectsUnavailableReportedUsage(t *testing.T) {
	value := completedReport()
	value.Usage = &report.Usage{Available: false, TotalTokens: 20}
	if err := report.Validate(value, report.ValidationContext{InvocationID: "inv-1", RunID: "run-1", Harness: "codex", Role: "implementation", Stage: "implementation"}); err == nil || !strings.Contains(err.Error(), "available") {
		t.Fatalf("Validate() error = %v, want unavailable-usage rejection", err)
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

// TestValidateAcceptsTheTestStageHandoff verifies the test role reports
// acceptance coverage and independently verifiable red-test evidence instead
// of using the implementation handoff shape.
func TestValidateAcceptsTheTestStageHandoff(t *testing.T) {
	value := testStageReport()
	if err := report.Validate(value, report.ValidationContext{
		InvocationID:     "inv-test",
		RunID:            "run-test",
		Harness:          "codex",
		Role:             "test",
		Stage:            "test",
		WorktreeObserved: true,
		ObservedChanges:  []string{"internal/factory/agent_test.go", "test-support/fixtures.go"},
	}); err != nil {
		t.Fatalf("Validate() error = %v, want accepted test handoff", err)
	}
}

// TestValidateRejectsAProductionPathInTheTestHandoff verifies the test role
// cannot smuggle a production behavior change through its changed-file list.
func TestValidateRejectsAProductionPathInTheTestHandoff(t *testing.T) {
	value := testStageReport()
	value.TestHandoff.ChangedFiles = append(value.TestHandoff.ChangedFiles, "internal/factory/agent.go")
	err := report.Validate(value, report.ValidationContext{
		InvocationID:     "inv-test",
		RunID:            "run-test",
		Harness:          "codex",
		Role:             "test",
		Stage:            "test",
		WorktreeObserved: true,
		ObservedChanges:  []string{"internal/factory/agent_test.go", "test-support/fixtures.go", "internal/factory/agent.go"},
	})
	if err == nil || !strings.Contains(err.Error(), "test changed_files") {
		t.Fatalf("Validate() error = %v, want test production-path rejection", err)
	}
}

// TestValidateRequiresRedEvidenceForATestHandoff verifies an empty or
// unverifiable focused failure cannot authorize implementation handoff.
func TestValidateRequiresRedEvidenceForATestHandoff(t *testing.T) {
	value := testStageReport()
	value.TestHandoff.ObservedFailureEvidence = nil
	err := report.Validate(value, report.ValidationContext{
		InvocationID:     "inv-test",
		RunID:            "run-test",
		Harness:          "codex",
		Role:             "test",
		Stage:            "test",
		WorktreeObserved: true,
		ObservedChanges:  []string{"internal/factory/agent_test.go", "test-support/fixtures.go"},
	})
	if err == nil || !strings.Contains(err.Error(), "observed_failure_evidence") {
		t.Fatalf("Validate() error = %v, want red-evidence requirement", err)
	}
}

// TestValidateRequiresTheExpectedFailureReason verifies the coordinator has a
// stable failure identifier to compare with its independent rerun output.
func TestValidateRequiresTheExpectedFailureReason(t *testing.T) {
	value := testStageReport()
	value.TestHandoff.ExpectedFailureReason = ""
	err := report.Validate(value, report.ValidationContext{
		InvocationID:     "inv-test",
		RunID:            "run-test",
		Harness:          "codex",
		Role:             "test",
		Stage:            "test",
		WorktreeObserved: true,
		ObservedChanges:  []string{"internal/factory/agent_test.go", "test-support/fixtures.go"},
	})
	if err == nil || !strings.Contains(err.Error(), "expected_failure_reason") {
		t.Fatalf("Validate() error = %v, want expected-failure-reason requirement", err)
	}
}

// TestValidateRequiresTheTestHandoffForTestCompletion verifies a test agent
// cannot complete using the implementation handoff envelope.
func TestValidateRequiresTheTestHandoffForTestCompletion(t *testing.T) {
	value := testStageReport()
	value.TestHandoff = nil
	err := report.Validate(value, report.ValidationContext{
		InvocationID: "inv-test",
		RunID:        "run-test",
		Harness:      "codex",
		Role:         "test",
		Stage:        "test",
	})
	if err == nil || !strings.Contains(err.Error(), "test handoff") {
		t.Fatalf("Validate() error = %v, want missing-test-handoff rejection", err)
	}
}

// TestValidateRejectsAReviewHandoffForTestCompletion verifies a test role
// cannot combine its test handoff with a review envelope.
func TestValidateRejectsAReviewHandoffForTestCompletion(t *testing.T) {
	value := testStageReport()
	value.ReviewHandoff = &report.ReviewHandoff{}
	err := report.Validate(value, report.ValidationContext{
		InvocationID: "inv-test",
		RunID:        "run-test",
		Harness:      "codex",
		Role:         "test",
		Stage:        "test",
	})
	if err == nil || !strings.Contains(err.Error(), "only a test handoff") {
		t.Fatalf("Validate() error = %v, want mixed-review-handoff rejection", err)
	}
}

// TestValidateAcceptsTheStructuredTestObjectionProtocol verifies an
// implementation objection and both bounded test-role response shapes remain
// strict report-level contracts.
func TestValidateAcceptsTheStructuredTestObjectionProtocol(t *testing.T) {
	implementation := completedReport()
	implementation.Handoff.TestObjections = []report.TestObjection{{
		Test:     "internal/factory/agent_test.go:TestBehavior",
		Claim:    "the assertion over-specifies an implementation detail",
		Evidence: "the public behavior is correct while the assertion fails",
	}}
	if err := report.Validate(implementation, report.ValidationContext{InvocationID: "inv-1", RunID: "run-1", Harness: "codex", Role: "implementation", Stage: "implementation"}); err != nil {
		t.Fatalf("Validate(implementation objection) error = %v", err)
	}

	accepted := testStageReport()
	accepted.TestObjectionResponse = &report.TestObjectionResponse{Decision: report.TestObjectionAccepted, Reason: "the public contract is stable"}
	if err := report.Validate(accepted, report.ValidationContext{InvocationID: "inv-test", RunID: "run-test", Harness: "codex", Role: "test", Stage: "test", WorktreeObserved: true, ObservedChanges: accepted.TestHandoff.ChangedFiles}); err != nil {
		t.Fatalf("Validate(accepted objection) error = %v", err)
	}

	rejected := testStageReport()
	rejected.TestHandoff = nil
	rejected.TestObjectionResponse = &report.TestObjectionResponse{Decision: report.TestObjectionRejected, Reason: "the frozen specification requires this behavior"}
	if err := report.Validate(rejected, report.ValidationContext{InvocationID: "inv-test", RunID: "run-test", Harness: "codex", Role: "test", Stage: "test"}); err != nil {
		t.Fatalf("Validate(rejected objection) error = %v", err)
	}
}

// TestValidateRejectsMalformedTestObjectionResponses verifies unknown
// decisions and unexplained objection responses cannot reach the coordinator.
func TestValidateRejectsMalformedTestObjectionResponses(t *testing.T) {
	for name, response := range map[string]report.TestObjectionResponse{
		"unknown decision":          {Decision: "maybe", Reason: "unclear"},
		"missing acceptance reason": {Decision: report.TestObjectionAccepted},
		"missing rejection reason":  {Decision: report.TestObjectionRejected},
	} {
		t.Run(name, func(t *testing.T) {
			value := testStageReport()
			value.TestObjectionResponse = &response
			if err := report.Validate(value, report.ValidationContext{InvocationID: "inv-test", RunID: "run-test", Harness: "codex", Role: "test", Stage: "test"}); err == nil {
				t.Fatal("Validate() accepted malformed objection response")
			}
		})
	}
}

// TestValidateAcceptsACompleteSpecificationReview verifies a review binds its
// findings to the exact immutable checkpoint and preserves advisory findings.
func TestValidateAcceptsACompleteSpecificationReview(t *testing.T) {
	value := specificationReviewReport()
	if err := report.Validate(value, report.ValidationContext{
		InvocationID:  "inv-review",
		RunID:         "run-review",
		Harness:       "codex",
		Role:          "spec_review",
		Stage:         "review",
		CheckpointSHA: value.ReviewHandoff.ReviewedSHA,
	}); err != nil {
		t.Fatalf("Validate() error = %v, want accepted specification review", err)
	}
	if !report.ReviewFindingBlocks(value.ReviewHandoff.Findings[0]) {
		t.Fatal("concrete blocker was classified as advisory")
	}
	if report.ReviewFindingBlocks(value.ReviewHandoff.Findings[1]) {
		t.Fatal("scope finding blocked readiness")
	}
}

// TestValidateRequiresStandardsFindingProvenance verifies each standards-axis
// finding names a guidance document or heuristic and binds that source to its
// exact reported hunk.
func TestValidateRequiresStandardsFindingProvenance(t *testing.T) {
	tests := []struct {
		name     string
		evidence string
		wantErr  bool
	}{
		{
			name:     "documented guidance",
			evidence: "source=guidance:AGENTS.md;hunk=internal/factory/review.go:20;named repository rule is violated",
		},
		{
			name:     "heuristic baseline",
			evidence: "source=heuristic:duplicated logic shape;hunk=internal/factory/review.go:20;the same decision is repeated",
		},
		{
			name:     "missing source",
			evidence: "the same decision is repeated",
			wantErr:  true,
		},
		{
			name:     "mismatched hunk",
			evidence: "source=guidance:AGENTS.md;hunk=internal/factory/review.go:21;named repository rule is violated",
			wantErr:  true,
		},
		{
			name:     "uncaptured guidance document",
			evidence: "source=guidance:CONTEXT.md;hunk=internal/factory/review.go:20;named repository rule is violated",
			wantErr:  true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := specificationReviewReport()
			value.Role = "standards_review"
			value.Stage = "standards_review"
			value.ReviewHandoff.Findings = []report.ReviewFinding{{
				Location:            "internal/factory/review.go:20",
				Claim:               "the checkpoint has a review issue",
				Evidence:            test.evidence,
				Severity:            report.ReviewSeverityAdvisory,
				Category:            report.ReviewCategoryDocumentedStandards,
				SuggestedResolution: "address the finding",
				SuggestedOwner:      "implementation",
			}}
			err := report.Validate(value, report.ValidationContext{
				InvocationID: "inv-review", RunID: "run-review", Harness: "codex", Role: "standards_review", Stage: "standards_review",
				CheckpointSHA: value.ReviewHandoff.ReviewedSHA, RepositoryGuidancePaths: []string{"AGENTS.md"}, RepositoryGuidanceBound: true,
			})
			if test.wantErr && err == nil {
				t.Fatal("Validate() accepted malformed standards provenance")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("Validate() error = %v, want accepted standards provenance", err)
			}
		})
	}
}

// TestValidateRejectsAnIncompleteReviewFinding verifies every finding field is
// required before a reviewer can influence readiness.
func TestValidateRejectsAnIncompleteReviewFinding(t *testing.T) {
	value := specificationReviewReport()
	value.ReviewHandoff.Findings[0].SuggestedOwner = ""
	err := report.Validate(value, report.ValidationContext{
		InvocationID: "inv-review", RunID: "run-review", Harness: "codex", Role: "spec_review", Stage: "review",
	})
	if err == nil || !strings.Contains(err.Error(), "suggested_owner") {
		t.Fatalf("Validate() error = %v, want missing-owner rejection", err)
	}
}

// TestValidateRejectsAReviewForAStaleCheckpoint verifies review output cannot
// be reused after the reviewed commit changes.
func TestValidateRejectsAReviewForAStaleCheckpoint(t *testing.T) {
	value := specificationReviewReport()
	err := report.Validate(value, report.ValidationContext{
		InvocationID: "inv-review", RunID: "run-review", Harness: "codex", Role: "spec_review", Stage: "review",
		CheckpointSHA: strings.Repeat("b", 40),
	})
	if err == nil || !strings.Contains(err.Error(), "does not match checkpoint") {
		t.Fatalf("Validate() error = %v, want stale-checkpoint rejection", err)
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

// TestValidateRejectsACompletedHandoffWithoutProductionFiles verifies that a
// completed handoff names at least one repository file changed by the role.
func TestValidateRejectsACompletedHandoffWithoutProductionFiles(t *testing.T) {
	value := completedReport()
	value.Handoff.ProductionFilesChanged = nil
	err := report.Validate(value, report.ValidationContext{
		InvocationID:     "inv-1",
		RunID:            "run-1",
		Harness:          "codex",
		Role:             "implementation",
		Stage:            "implementation",
		WorktreeObserved: true,
		ObservedChanges:  []string{"internal/factory/agent.go"},
	})
	if err == nil || !strings.Contains(err.Error(), "production_files_changed") {
		t.Fatalf("Validate() error = %v, want missing-production-files rejection", err)
	}
}

// TestReportAndStoreValidatorsAgreeOnCompletedHandoffFiles verifies both
// validation seams reject the same incomplete completed handoff.
func TestReportAndStoreValidatorsAgreeOnCompletedHandoffFiles(t *testing.T) {
	value := completedReport()
	value.Handoff.ProductionFilesChanged = nil
	reportErr := report.Validate(value, report.ValidationContext{
		InvocationID:     "inv-1",
		RunID:            "run-1",
		Harness:          "codex",
		Role:             "implementation",
		Stage:            "implementation",
		WorktreeObserved: true,
		ObservedChanges:  []string{"internal/factory/agent.go"},
	})
	storeErr := store.ValidateRun(store.Run{
		ID:             "run-1",
		RepositoryPath: "/work/repository",
		Status:         store.StatusActive,
		RoleHandoff: &store.RoleHandoff{
			ChangeSummary:          value.Handoff.ChangeSummary,
			AcceptanceMapping:      []store.HandoffAcceptance{{Criterion: "criterion", Evidence: "test"}},
			ProductionFilesChanged: nil,
			FocusedCommands:        []string{"go test ./internal/factory"},
		},
	})
	if (reportErr == nil) != (storeErr == nil) {
		t.Fatalf("validator agreement = report error %v, store error %v", reportErr, storeErr)
	}
	if reportErr == nil {
		t.Fatal("report and store validators accepted an empty production-file list")
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

// testStageReport returns the smallest valid completed test-stage report.
func testStageReport() report.Report {
	return report.Report{
		SchemaVersion: report.SchemaVersion,
		InvocationID:  "inv-test",
		RunID:         "run-test",
		Harness:       "codex",
		Role:          "test",
		Stage:         "test",
		Outcome:       report.OutcomeCompleted,
		Summary:       "verified the new behavior is red on the base implementation",
		TestHandoff: &report.TestHandoff{
			AcceptanceCoverage:    []report.AcceptanceMapping{{Criterion: "test-stage handoff", Evidence: "focused red test"}},
			ChangedFiles:          []string{"internal/factory/agent_test.go", "test-support/fixtures.go"},
			FocusedTestCommand:    "go test ./internal/factory -run TestNewBehavior",
			ExpectedFailureReason: "expected behavior assertion",
			ObservedFailureEvidence: []report.Evidence{{
				Kind:   "exit_code",
				Detail: "focused command exited with code 1",
			}},
			InfrastructureChanges: []string{"test-support/fixtures.go"},
		},
		ReportedAt: time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC),
	}
}

// specificationReviewReport returns a valid review report with one blocker
// and one advisory scope observation.
func specificationReviewReport() report.Report {
	return report.Report{
		SchemaVersion: report.SchemaVersion,
		InvocationID:  "inv-review",
		RunID:         "run-review",
		Harness:       "codex",
		Role:          "spec_review",
		Stage:         "review",
		Outcome:       report.OutcomeCompleted,
		Summary:       "review complete",
		ReviewHandoff: &report.ReviewHandoff{
			ReviewedSHA: strings.Repeat("a", 40),
			Findings: []report.ReviewFinding{
				{
					Location:            "internal/factory/agent.go:42",
					Claim:               "the implementation accepts stale review output",
					Evidence:            "checkpoint identity is not compared",
					Severity:            report.ReviewSeverityBlocker,
					Category:            report.ReviewCategoryCorrectness,
					SuggestedResolution: "compare the reviewed SHA before acceptance",
					SuggestedOwner:      "implementation",
				},
				{
					Location:            "internal/factory/review.go",
					Claim:               "the review packet could explain the scope boundary more clearly",
					Evidence:            "the packet already contains the frozen issue",
					Severity:            report.ReviewSeverityAdvisory,
					Category:            report.ReviewCategoryScope,
					SuggestedResolution: "clarify the prompt if later scope remains ambiguous",
					SuggestedOwner:      "human",
				},
			},
		},
		ReportedAt: time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC),
	}
}
