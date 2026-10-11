package report_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/report"
)

// cannotProceedWithDefect returns a cannot_proceed report that proposes a
// suspected Factory defect for its own invocation.
func cannotProceedWithDefect() report.Report {
	value := completedReport()
	value.Outcome = report.OutcomeCannotProceed
	value.Handoff = nil
	value.Evidence = []report.Evidence{{Kind: "command", Detail: "factory-report rejected a valid handoff"}}
	value.SuspectedFactoryDefect = &report.FactoryDefectProposal{
		InvocationID: "inv-1",
		Code:         "report.handoff_rejected",
		Expected:     "factory-report accepts a handoff with one production file",
		Observed:     "factory-report exits 1 with a schema error",
		Reason:       "the same handoff passes the documented contract and no repository file is involved",
	}
	return value
}

// TestSuspectedFactoryDefectProposalRoundTrips verifies that a worker can
// publish a bounded defect proposal beside its normal outcome, and that the
// proposal does not replace the outcome.
func TestSuspectedFactoryDefectProposalRoundTrips(t *testing.T) {
	path, err := report.WriteAtomic(filepath.Join(t.TempDir(), "inv-1"), cannotProceedWithDefect())
	if err != nil {
		t.Fatalf("WriteAtomic() error = %v", err)
	}
	got, err := report.Read(path)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got.Outcome != report.OutcomeCannotProceed {
		t.Fatalf("Read().Outcome = %q, want cannot_proceed", got.Outcome)
	}
	if got.SuspectedFactoryDefect == nil || got.SuspectedFactoryDefect.Code != "report.handoff_rejected" {
		t.Fatalf("Read().SuspectedFactoryDefect = %#v, want the proposal", got.SuspectedFactoryDefect)
	}
}

// TestWriteAtomicRejectsMalformedFactoryDefectProposals verifies that the
// worker-side writer refuses a proposal that is not bound to the invocation
// identity or is not bounded, so the worker sees the error at once.
func TestWriteAtomicRejectsMalformedFactoryDefectProposals(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*report.FactoryDefectProposal)
		want   string
	}{
		{"wrong invocation", func(p *report.FactoryDefectProposal) { p.InvocationID = "inv-other" }, "invocation_id"},
		{"missing observed", func(p *report.FactoryDefectProposal) { p.Observed = " " }, "observed"},
		{"missing expected", func(p *report.FactoryDefectProposal) { p.Expected = "" }, "expected"},
		{"missing reason", func(p *report.FactoryDefectProposal) { p.Reason = "" }, "reason"},
		{"unsafe code", func(p *report.FactoryDefectProposal) { p.Code = "code with spaces" }, "code"},
		{"multiline text", func(p *report.FactoryDefectProposal) { p.Observed = "line one\nline two" }, "observed"},
		{"oversized text", func(p *report.FactoryDefectProposal) { p.Reason = strings.Repeat("r", 1001) }, "reason"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := cannotProceedWithDefect()
			test.mutate(value.SuspectedFactoryDefect)
			_, err := report.WriteAtomic(filepath.Join(t.TempDir(), "inv-1"), value)
			if err == nil || !strings.Contains(err.Error(), "suspected_factory_defect."+test.want) {
				t.Fatalf("WriteAtomic() error = %v, want suspected_factory_defect.%s rejection", err, test.want)
			}
		})
	}
}

// TestCoordinatorValidationIgnoresTheDefectProposal verifies that a bad
// proposal does not change report acceptance: the coordinator validates the
// report as if the proposal were absent, and consumers that surface
// proposals reject it on their own.
func TestCoordinatorValidationIgnoresTheDefectProposal(t *testing.T) {
	value := cannotProceedWithDefect()
	value.SuspectedFactoryDefect.InvocationID = "inv-other"
	err := report.Validate(value, report.ValidationContext{
		InvocationID: "inv-1",
		RunID:        "run-1",
		Harness:      "codex",
		Role:         "implementation",
		Stage:        "implementation",
	})
	if err != nil {
		t.Fatalf("Validate() error = %v, want the report accepted without regard to its proposal", err)
	}
}
