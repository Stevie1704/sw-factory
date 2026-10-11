package bugreport_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/bugreport"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/version"
)

// testDraft returns a draft with a Run, a worker proposal, and a limitation.
func testDraft() bugreport.Draft {
	return bugreport.Draft{
		ID: "fbr-0123456789abcdef0123456789abcdef",
		Identity: version.Identity{
			SchemaVersion: version.SchemaVersion, Version: "v1.2.3", Build: "release",
			Revision: "abc123", Platform: "linux/amd64",
		},
		Evidence: bugreport.Evidence{
			Configuration: bugreport.AvailabilityAvailable,
			Store:         bugreport.AvailabilityAvailable,
			Provider:      "azure_devops",
			Run: &bugreport.RunEvidence{
				ID: "run-1", Stage: "implementation", Status: "waiting_for_human",
				FailedGates: []bugreport.GateEvidence{{Phase: "checkpoint", Outcome: "failed"}},
				Invocations: []bugreport.InvocationEvidence{{ID: "inv-1", Role: "implementation", Stage: "implementation", Harness: "codex", Status: "cannot_proceed", PromptVersion: "implementation-v3"}},
				DefectProposals: []report.FactoryDefectProposal{{
					InvocationID: "inv-1", Code: "report.handoff_rejected",
					Expected: "factory-report accepts the handoff", Observed: "factory-report exits 1", Reason: "no repository file is involved",
				}},
			},
			Limitations: []string{"The retained report of Invocation inv-2 is unavailable."},
		},
		CreatedAt: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC),
	}
}

// fillDraft replaces the placeholders an operator must complete.
func fillDraft(t *testing.T, rendered string) string {
	t.Helper()
	replacements := map[string]string{
		"## Expected behavior\n\n": "## Expected behavior\n\n`factory status` reports the Run.\n\n",
		"## Observed behavior\n\n": "## Observed behavior\n\n`factory status` exits 1.\n\n",
		"## Reproduction\n\n":      "## Reproduction\n\n1. Run `factory status`.\n\n",
		bugreport.TitlePlaceholder: "status exits 1 after a rejected handoff",
	}
	for old, replacement := range replacements {
		if !strings.Contains(rendered, old) {
			t.Fatalf("rendered draft lacks %q:\n%s", old, rendered)
		}
		rendered = strings.Replace(rendered, old, replacement, 1)
	}
	return rendered
}

// TestRenderedDraftCarriesTheEvidenceAndItsIdentity verifies that a draft
// shows the release identity, the allowlisted Run evidence, the worker
// proposal marked for review, the limitations, and the report identity.
func TestRenderedDraftCarriesTheEvidenceAndItsIdentity(t *testing.T) {
	rendered := string(bugreport.Render(testDraft()))
	for _, want := range []string{
		"fbr-0123456789abcdef0123456789abcdef",
		"v1.2.3", "abc123", "linux/amd64",
		"Harness versions: unavailable",
		"azure_devops",
		"run-1", "waiting_for_human", "phase=checkpoint outcome=failed",
		"inv-1", "implementation-v3",
		"Invocation inv-1 proposed code report.handoff_rejected",
		"The retained report of Invocation inv-2 is unavailable.",
		"not a confirmed root cause",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered draft lacks %q:\n%s", want, rendered)
		}
	}
	for _, workerText := range []string{"factory-report accepts the handoff", "factory-report exits 1", "no repository file is involved"} {
		if strings.Contains(rendered, workerText) {
			t.Errorf("rendered draft contains worker-authored text %q", workerText)
		}
	}
}

// TestParseRequiresTheCoreSections verifies that an unedited draft cannot be
// submitted, and names every section the operator still has to write.
func TestParseRequiresTheCoreSections(t *testing.T) {
	_, err := bugreport.Parse(bugreport.Render(testDraft()))
	if err == nil {
		t.Fatal("Parse(unedited draft) error = nil, want missing sections")
	}
	for _, section := range []string{"title", "Expected behavior", "Observed behavior", "Reproduction"} {
		if !strings.Contains(err.Error(), section) {
			t.Errorf("Parse() error = %v, want it to name %q", err, section)
		}
	}
}

// TestParseReturnsTheExactReviewedSubmission verifies that a completed draft
// yields its title, its identity, and a body that is the reviewed file
// without the title line.
func TestParseReturnsTheExactReviewedSubmission(t *testing.T) {
	completed := fillDraft(t, string(bugreport.Render(testDraft())))
	submission, err := bugreport.Parse([]byte(completed))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if submission.ID != "fbr-0123456789abcdef0123456789abcdef" {
		t.Fatalf("Parse().ID = %q", submission.ID)
	}
	if submission.Title != "Suspected Factory defect: status exits 1 after a rejected handoff" {
		t.Fatalf("Parse().Title = %q", submission.Title)
	}
	lines := strings.SplitN(completed, "\n", 3)
	if submission.Body != lines[0]+"\n"+lines[2] {
		t.Fatalf("Parse().Body is not the reviewed file without its title line:\n%s", submission.Body)
	}
}

// TestParseRejectsADraftWithoutAValidIdentity verifies that the report
// identity marker is required, because retry reconciliation depends on it.
func TestParseRejectsADraftWithoutAValidIdentity(t *testing.T) {
	completed := fillDraft(t, string(bugreport.Render(testDraft())))
	for name, content := range map[string]string{
		"missing marker":  strings.SplitN(completed, "\n", 2)[1],
		"edited identity": strings.Replace(completed, "fbr-0123456789abcdef0123456789abcdef", "fbr-edited", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := bugreport.Parse([]byte(content)); err == nil || !strings.Contains(err.Error(), "identity") {
				t.Fatalf("Parse() error = %v, want an identity error", err)
			}
		})
	}
}
