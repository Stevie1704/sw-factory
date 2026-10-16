package report_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/report"
)

// ideationReport returns a valid completed ideation report for session
// "session-1" at revision 4.
func ideationReport() report.Report {
	return report.Report{
		SchemaVersion:   report.SchemaVersion,
		InvocationID:    "sinv-1",
		SessionID:       "session-1",
		SessionRevision: 4,
		Harness:         "claude",
		Role:            "ideation",
		Stage:           "ideation",
		Outcome:         report.OutcomeCompleted,
		Summary:         "asked about the audience",
		Ideation: &report.IdeationHandoff{
			Message:   "The repository has no export command yet.\nWho needs the export?",
			Questions: []report.Question{{ID: "audience", Prompt: "Who uses the export?"}},
			Brief: &report.Brief{
				Problem:  "Operators cannot export run history.",
				Evidence: []report.BriefItem{{ID: "ev-1", Text: "internal/cli has no export command"}},
				Decisions: []report.BriefDecision{
					{ID: "dec-1", Decision: "Export JSON only", Rationale: "Existing tools read JSON"},
				},
			},
			Readiness: &report.ReadinessProposal{Proposal: report.ReadinessInvestigationNeeded, Rationale: "The audience is unknown."},
		},
		ReportedAt: time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC),
	}
}

// planningReport returns a valid completed planning report for session
// "session-1" at revision 7.
func planningReport() report.Report {
	return report.Report{
		SchemaVersion:   report.SchemaVersion,
		InvocationID:    "sinv-2",
		SessionID:       "session-1",
		SessionRevision: 7,
		Harness:         "codex",
		Role:            "planning",
		Stage:           "planning",
		Outcome:         report.OutcomeCompleted,
		Summary:         "drafted one parent and two children",
		PlanDraft: &report.PlanDraft{Nodes: []report.PlanNode{
			{ID: "epic", Kind: "parent", Title: "Export run history", Body: "Parent item.\nLinks the brief.", BriefRefs: []string{"dec-1"}},
			{ID: "cli", Kind: "task", Title: "Add the export command", AcceptanceCriteria: []string{"factory export writes JSON"}, ParentID: "epic"},
			{ID: "docs", Kind: "task", Title: "Document the export", ParentID: "epic", DependsOn: []string{"cli"}},
		}},
		ReportedAt: time.Date(2026, 10, 15, 11, 0, 0, 0, time.UTC),
	}
}

// sessionContext returns the coordinator expectation that matches value.
func sessionContext(value report.Report) report.ValidationContext {
	return report.ValidationContext{
		InvocationID: value.InvocationID, SessionID: value.SessionID, SessionRevision: value.SessionRevision,
		Harness: value.Harness, Role: value.Role, Stage: value.Stage, RoleKind: value.Role,
	}
}

// TestSessionReportsRoundTripThroughTheInvocationResultDirectory verifies that
// ideation and planning reports carry explicit session ownership instead of a
// run identity and survive the atomic write and strict read.
func TestSessionReportsRoundTripThroughTheInvocationResultDirectory(t *testing.T) {
	t.Parallel()

	for _, value := range []report.Report{ideationReport(), planningReport()} {
		path, err := report.WriteAtomicForInvocation(filepath.Join(t.TempDir(), "results"), value.InvocationID, value)
		if err != nil {
			t.Fatalf("WriteAtomicForInvocation(%s) error = %v", value.Role, err)
		}
		got, err := report.Read(path)
		if err != nil {
			t.Fatalf("Read(%s) error = %v", value.Role, err)
		}
		if got.RunID != "" || got.SessionID != "session-1" || got.SessionRevision != value.SessionRevision {
			t.Fatalf("Read(%s) identity = run %q session %q@%d, want explicit session ownership", value.Role, got.RunID, got.SessionID, got.SessionRevision)
		}
		if err := report.Validate(got, sessionContext(value)); err != nil {
			t.Fatalf("Validate(%s) error = %v", value.Role, err)
		}
	}
}

// TestValidateBindsSessionReportsToTheExactInvocationAndRevision verifies that
// a session report for another session, an older revision, or a run is
// rejected, and that a run report cannot claim a session.
func TestValidateBindsSessionReportsToTheExactInvocationAndRevision(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*report.Report, *report.ValidationContext)
		want   string
	}{
		{"other session", func(value *report.Report, _ *report.ValidationContext) { value.SessionID = "session-2" }, "session_id"},
		{"old revision", func(value *report.Report, _ *report.ValidationContext) { value.SessionRevision = 3 }, "session_revision"},
		{"run identity", func(value *report.Report, _ *report.ValidationContext) { value.RunID = "run-1" }, "run_id"},
		{"other role", func(_ *report.Report, context *report.ValidationContext) { context.Role = "planning" }, "role"},
		{"run context", func(_ *report.Report, context *report.ValidationContext) {
			context.SessionID, context.SessionRevision, context.RunID = "", 0, "run-1"
		}, "session"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			value := ideationReport()
			context := sessionContext(value)
			test.mutate(&value, &context)
			err := report.Validate(value, context)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want rejection naming %q", err, test.want)
			}
		})
	}
	run := completedReport()
	run.Ideation = ideationReport().Ideation
	if err := report.Validate(run, report.ValidationContext{InvocationID: run.InvocationID, RunID: run.RunID, Harness: run.Harness, Role: run.Role, Stage: run.Stage}); err == nil {
		t.Fatal("Validate() accepted a run report with an ideation payload")
	}
}

// TestValidateRequiresTheSessionPayloadOfEachRole verifies the structured
// contract: ideation reports a message with questions, a brief update, and a
// readiness proposal; planning reports a draft plan graph or questions; both
// report evidence when they cannot proceed.
func TestValidateRequiresTheSessionPayloadOfEachRole(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		base   func() report.Report
		mutate func(*report.Report)
		valid  bool
	}{
		{"ideation without payload", ideationReport, func(value *report.Report) { value.Ideation = nil }, false},
		{"ideation without message", ideationReport, func(value *report.Report) { value.Ideation.Message = " " }, false},
		{"ideation with a plan", ideationReport, func(value *report.Report) { value.PlanDraft = planningReport().PlanDraft }, false},
		{"ideation message only", ideationReport, func(value *report.Report) {
			value.Ideation = &report.IdeationHandoff{Message: "I read the CLI package."}
		}, true},
		{"unknown readiness", ideationReport, func(value *report.Report) { value.Ideation.Readiness.Proposal = "approved" }, false},
		{"duplicate brief item", ideationReport, func(value *report.Report) {
			value.Ideation.Brief.Scope = []report.BriefItem{{ID: "ev-1", Text: "again"}}
		}, false},
		{"affected decisions", ideationReport, func(value *report.Report) { value.Ideation.AffectedDecisions = []string{"dec-1", "dec-2"} }, true},
		{"unsafe affected decision", ideationReport, func(value *report.Report) { value.Ideation.AffectedDecisions = []string{"dec 1"} }, false},
		{"duplicate affected decision", ideationReport, func(value *report.Report) { value.Ideation.AffectedDecisions = []string{"dec-1", "dec-1"} }, false},
		{"decision without rationale", ideationReport, func(value *report.Report) { value.Ideation.Brief.Decisions[0].Rationale = "" }, false},
		{"ideation clarification", ideationReport, func(value *report.Report) {
			value.Outcome = report.OutcomeNeedsClarification
			value.Questions = []report.Question{{ID: "q", Prompt: "Who?"}}
			value.Ideation = nil
		}, false},
		{"cannot proceed", ideationReport, func(value *report.Report) {
			value.Outcome = report.OutcomeCannotProceed
			value.Ideation = nil
			value.Evidence = []report.Evidence{{Kind: "repository", Detail: "snapshot is empty"}}
		}, true},
		{"planning without plan", planningReport, func(value *report.Report) { value.PlanDraft = nil }, false},
		{"planning empty plan", planningReport, func(value *report.Report) { value.PlanDraft.Nodes = nil }, false},
		{"planning node without title", planningReport, func(value *report.Report) { value.PlanDraft.Nodes[1].Title = "" }, false},
		{"planning duplicate node", planningReport, func(value *report.Report) { value.PlanDraft.Nodes[2].ID = "cli" }, false},
		{"planning unsafe reference", planningReport, func(value *report.Report) { value.PlanDraft.Nodes[2].DependsOn = []string{"../cli"} }, false},
		{"planning with ideation", planningReport, func(value *report.Report) { value.Ideation = ideationReport().Ideation }, false},
		{"planning clarification", planningReport, func(value *report.Report) {
			value.Outcome = report.OutcomeNeedsClarification
			value.PlanDraft = nil
			value.Questions = []report.Question{{ID: "scope", Prompt: "Is the import in scope?"}}
		}, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			value := test.base()
			test.mutate(&value)
			err := report.Validate(value, sessionContext(value))
			if (err == nil) != test.valid {
				t.Fatalf("Validate() error = %v, want valid %v", err, test.valid)
			}
		})
	}
}
