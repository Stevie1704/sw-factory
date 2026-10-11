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

// TestValidateRejectsMalformedFactoryDefectProposals verifies that a
// proposal is bound to the invocation identity and stays bounded.
func TestValidateRejectsMalformedFactoryDefectProposals(t *testing.T) {
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
			err := report.Validate(value, report.ValidationContext{
				InvocationID: "inv-1",
				RunID:        "run-1",
				Harness:      "codex",
				Role:         "implementation",
				Stage:        "implementation",
			})
			if err == nil || !strings.Contains(err.Error(), "suspected_factory_defect."+test.want) {
				t.Fatalf("Validate() error = %v, want suspected_factory_defect.%s rejection", err, test.want)
			}
		})
	}
}
