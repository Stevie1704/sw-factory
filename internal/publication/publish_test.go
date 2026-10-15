package publication_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/publication"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/tracker/trackertest"
)

// TestPublishCreatesTheApprovedGraphInDependencyOrder verifies that one
// publication creates every approved item before the items that need it,
// links them after both ends exist, records each identity in the ledger, and
// leaves the tracker inert.
func TestPublishCreatesTheApprovedGraphInDependencyOrder(t *testing.T) {
	t.Parallel()

	h := newApprovedHarness(t)
	result, err := h.publisher().Publish(t.Context(), publication.Request{SessionID: h.sessionID, AttemptID: "attempt-1"})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if result.Receipt.Outcome != store.PublicationOutcomePublished || result.Session.State != store.IdeationStatePublished {
		t.Fatalf("Publish() = %#v", result)
	}
	var order []string
	for _, item := range h.fake.Items() {
		order = append(order, item.Rendered.NodeID)
	}
	if want := []string{"n-1", "n-2", "n-3"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("create order = %v, want %v", order, want)
	}
	items := h.itemsByNode()
	wantLinks := []tracker.PlanRelationship{
		{Kind: tracker.PlanRelationshipParent, Mode: tracker.PlanRelationshipNative, From: items["n-3"].Ref, To: items["n-1"].Ref},
		{Kind: tracker.PlanRelationshipDependsOn, Mode: tracker.PlanRelationshipNative, From: items["n-3"].Ref, To: items["n-2"].Ref},
		{Kind: tracker.PlanRelationshipParent, Mode: tracker.PlanRelationshipNative, From: items["n-2"].Ref, To: items["n-1"].Ref},
	}
	if got := h.fake.Relationships(); !reflect.DeepEqual(got, wantLinks) {
		t.Fatalf("relationships = %#v, want %#v", got, wantLinks)
	}
	if len(result.Receipt.Items) != 3 || len(result.Receipt.Relationships) != 3 || result.Receipt.ApprovalID != "approval-1" {
		t.Fatalf("receipt = %#v", result.Receipt)
	}
	for _, operation := range h.ledger() {
		if operation.State != store.PublicationOperationSucceeded || operation.Attempts != 1 {
			t.Fatalf("ledger operation = %#v, want succeeded after one attempt", operation)
		}
	}
	if got := h.operation("item/n-2"); got.ExternalID != items["n-2"].Ref.ExternalID || got.URL != items["n-2"].Ref.URL {
		t.Fatalf("ledger item/n-2 = %#v, want %#v", got, items["n-2"].Ref)
	}
	h.assertInertTracker()
}

// TestPublishReplaysAFinishedAttempt verifies that repeating an attempt
// identity returns the stored receipt without another tracker call.
func TestPublishReplaysAFinishedAttempt(t *testing.T) {
	t.Parallel()

	h := newApprovedHarness(t)
	request := publication.Request{SessionID: h.sessionID, AttemptID: "attempt-1"}
	first, err := h.publisher().Publish(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	writes := h.fake.Writes()
	again, err := h.publisher().Publish(t.Context(), request)
	if err != nil {
		t.Fatalf("replayed Publish() error = %v", err)
	}
	if again.Receipt.ID != first.Receipt.ID || h.fake.Writes() != writes {
		t.Fatalf("replayed Publish() = %#v with %d writes, want the stored receipt and %d writes", again.Receipt, h.fake.Writes(), writes)
	}
}

// TestPublishRefusesUnapprovedAndStaleApprovalsWithZeroWrites verifies that a
// missing approval, a plan change after approval, and provider drift after
// approval all fail before any tracker write and leave no ledger.
func TestPublishRefusesUnapprovedAndStaleApprovalsWithZeroWrites(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prepare func(*harness)
		want    error
	}{
		{name: "unapproved", prepare: func(*harness) {}, want: publication.ErrNotApproved},
		{name: "type mapping drift", prepare: func(h *harness) {
			h.approve("approval-1")
			h.fake.Types = map[string]string{"spec": "Epic", "task": "Task"}
		}, want: publication.ErrStaleApproval},
		{name: "relationship capability drift", prepare: func(h *harness) {
			h.approve("approval-1")
			h.fake.Mode = tracker.PlanRelationshipTextual
		}, want: publication.ErrStaleApproval},
		{name: "label drift", prepare: func(h *harness) {
			h.approve("approval-1")
			h.fake.Labels = []string{"enhancement"}
		}, want: publication.ErrStaleApproval},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			test.prepare(h)
			before := h.session()
			_, err := h.publisher().Publish(t.Context(), publication.Request{SessionID: h.sessionID, AttemptID: "attempt-1"})
			if !errors.Is(err, test.want) {
				t.Fatalf("Publish() error = %v, want %v", err, test.want)
			}
			if h.fake.Writes() != 0 || len(h.fake.Items()) != 0 {
				t.Fatalf("tracker writes = %d, want 0", h.fake.Writes())
			}
			if after := h.session(); after.State != before.State || after.Revision != before.Revision {
				t.Fatalf("session changed from %#v to %#v", before, after)
			}
			if ledger, err := h.store.PublicationLedger(t.Context(), h.sessionID, "approval-1"); err != nil || len(ledger) != 0 {
				t.Fatalf("ledger = %#v, %v; want none", ledger, err)
			}
		})
	}
}

// TestPreviewIsDeterministicAndIsWhatTheApprovalBinds verifies that the
// preview renders the same bytes twice, binds the current plan revision and
// target, carries no conversation text, and surfaces typed preflight errors.
func TestPreviewIsDeterministicAndIsWhatTheApprovalBinds(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	first, err := h.publisher().Preview(t.Context(), h.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.publisher().Preview(t.Context(), h.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Payload, second.Payload) || !bytes.Equal(first.RelationshipMapping, second.RelationshipMapping) || first.Target != second.Target {
		t.Fatal("Preview() is not deterministic")
	}
	if first.PlanRevision != 1 || first.Target != "fake:owner/repo" || len(first.Preview.Items) != 3 || len(first.Preview.Relationships) != 3 {
		t.Fatalf("Preview() = %#v", first)
	}
	if bytes.Contains(first.Payload, []byte(conversationSecret)) {
		t.Fatal("preview payload contains raw conversation text")
	}
	if !strings.Contains(string(first.Payload), "session-1/1/n-1") {
		t.Fatalf("payload %s lacks the correlation key", first.Payload)
	}
	approval := first.Approval("approval-1")
	if approval.ID != "approval-1" || approval.PlanRevision != 1 || approval.Target != first.Target {
		t.Fatalf("Approval() = %#v", approval)
	}

	h.fake.Inject(trackertest.Fault{Op: trackertest.OpPreflight, Err: tracker.NewPlanError(tracker.PlanErrorPermission, "read-only token")})
	if _, err := h.publisher().Preview(t.Context(), h.sessionID); !isPlanError(err, tracker.PlanErrorPermission) {
		t.Fatalf("Preview() error = %v, want a typed permission error", err)
	}
}

// TestPreviewRefusesFactoryStateLabels verifies decision D6 at the
// publication boundary: a rendered item with a factory state label cannot be
// previewed, so it cannot be approved.
func TestPreviewRefusesFactoryStateLabels(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	labelling := &labellingTrackerForTest{PlanTracker: h.fake, label: tracker.LabelAgentReady}
	if _, err := h.publisherWith(h.store, labelling).Preview(t.Context(), h.sessionID); !errors.Is(err, publication.ErrInvalidPayload) {
		t.Fatalf("Preview() error = %v, want ErrInvalidPayload", err)
	}
}

// labellingTrackerForTest adds one label to every rendered item, as a broken
// adapter would.
type labellingTrackerForTest struct {
	*trackertest.PlanTracker
	label string
}

// PreflightPlan renders through the fake and then adds the label.
func (l *labellingTrackerForTest) PreflightPlan(ctx context.Context, repository tracker.Repository, graph tracker.PlanGraph) (tracker.PlanPreview, error) {
	preview, err := l.PlanTracker.PreflightPlan(ctx, repository, graph)
	for index := range preview.Items {
		preview.Items[index].Labels = append(preview.Items[index].Labels, l.label)
	}
	return preview, err
}

// isPlanError reports whether err is a tracker.PlanError of kind.
func isPlanError(err error, kind tracker.PlanErrorKind) bool {
	got, ok := tracker.PlanErrorKindOf(err)
	return ok && got == kind
}
