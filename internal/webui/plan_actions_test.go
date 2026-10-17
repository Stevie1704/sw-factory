package webui_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/tracker/trackertest"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// TestNewerPreviewRefusalWithdrawsTheOlderApproval verifies that the page
// offers no approval of an older preview after a newer preview of the same
// plan revision was refused, and shows the refusal instead.
func TestNewerPreviewRefusalWithdrawsTheOlderApproval(t *testing.T) {
	t.Parallel()

	ui, _ := previewedPlanUI(t)
	assertContains(t, ui.page(t), `data-session-command="approve"`)
	ui.fake.Inject(trackertest.Fault{Op: trackertest.OpPreflight, Err: tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "no type is mapped for kind task")})
	ui.send(t, "preview-2", sessioncommand.KindPreviewPlan, ui.history(t).Session.Revision, `{"plan_revision":1}`)
	ui.drain(t)

	page := ui.page(t)
	assertContains(t, page, "data-preview-refused", "no type is mapped for kind task")
	if strings.Contains(page, `data-session-command="approve"`) {
		t.Fatal("page offers approval of an older preview after a newer refusal of the same revision")
	}
}

// approvedPlanUI returns the UI with plan revision 1 approved and publication
// not started.
func approvedPlanUI(t *testing.T) planUI {
	t.Helper()
	ui, result := previewedPlanUI(t)
	opened, err := store.Open(t.Context(), ui.storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	env := ui.env(opened)
	binding, err := env.Publisher.Preview(t.Context(), "session-1")
	if err != nil || binding.PlanRevision != result.PlanRevision {
		t.Fatalf("Preview() = %#v, %v", binding, err)
	}
	write, err := env.Lifecycle.Write(ui.history(t).Session, workflow.SessionEventApproved, "", fixtureNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := opened.ApprovePlan(t.Context(), write, binding.Approval("approval-1")); err != nil {
		t.Fatal(err)
	}
	return ui
}

// TestApprovedPlanNamesOnlyAllowedActions verifies that the approved page
// names the edit it offers and not a regeneration, which the lifecycle does
// not allow in approved.
func TestApprovedPlanNamesOnlyAllowedActions(t *testing.T) {
	t.Parallel()

	page := approvedPlanUI(t).page(t)
	assertContains(t, page, "Edit the draft to review a new revision.", `data-session-command="publish"`)
	for _, unwanted := range []string{"regenerate the draft to review", `data-session-command="regenerate_draft"`, `data-session-command="approve"`} {
		if strings.Contains(strings.ToLower(page), strings.ToLower(unwanted)) {
			t.Fatalf("approved page offers %q", unwanted)
		}
	}
}

// TestUnknownCreateIsCountedAsUncertain verifies that a create whose write
// was sent but never recorded, as a coordinator stop leaves it, counts as an
// unknown outcome and not as pending.
func TestUnknownCreateIsCountedAsUncertain(t *testing.T) {
	t.Parallel()

	ui, result := previewedPlanUI(t)
	ui.fake.Inject(trackertest.Fault{Op: trackertest.OpCreateItem, Key: "n-3", Err: errors.New("connection reset")})
	ui.send(t, "approve-1", sessioncommand.KindApprove, ui.history(t).Session.Revision, approveBody(result))
	ui.drain(t)
	history := ui.history(t)
	opened, err := store.Open(t.Context(), ui.storePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range history.Operations {
		if operation.ID == "item/n-3" {
			operation.State, operation.Detail = store.PublicationOperationAttempted, ""
			if _, err := opened.RecordPublicationOperation(t.Context(), operation); err != nil {
				t.Fatal(err)
			}
		}
	}
	_ = opened.Close()

	page := ui.page(t)
	assertContains(t, page,
		"2 items created · 0 relationships created · 3 pending · 0 failed · 1 unknown outcome",
		`data-operation="item/n-3" data-state="attempted"`, "<td>unknown outcome", `data-reconcile="item/n-3"`)
}
