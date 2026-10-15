package trackertest_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/tracker/trackertest"
)

// repository is the registered repository the fake serves in these tests.
var repository = tracker.Repository{Owner: "owner", Name: "repo"}

// graphForTest returns a parent with one dependent child.
func graphForTest() tracker.PlanGraph {
	return tracker.PlanGraph{
		SessionID: "session-1", PlanRevision: 2, ContextRevision: "abcdef1",
		Items: []tracker.PlanGraphItem{
			{NodeID: "spec", Kind: "spec", Title: "Spec", Body: "Why.", Correlation: tracker.PlanCorrelation("session-1", 2, "spec")},
			{NodeID: "task", Kind: "task", Title: "Task", Body: "What.", AcceptanceCriteria: []string{"It works."}, ParentID: "spec",
				DependsOn: []string{"spec"}, Correlation: tracker.PlanCorrelation("session-1", 2, "task")},
		},
	}
}

// TestFakePreflightRendersADeterministicReadOnlyPreview verifies that the fake
// renders the same preview twice, renders the correlation into each body,
// maps both relationship kinds, and makes no write.
func TestFakePreflightRendersADeterministicReadOnlyPreview(t *testing.T) {
	t.Parallel()

	fake := trackertest.NewPlanTracker()
	first, err := fake.PreflightPlan(t.Context(), repository, graphForTest())
	if err != nil {
		t.Fatalf("PreflightPlan() error = %v", err)
	}
	second, err := fake.PreflightPlan(t.Context(), repository, graphForTest())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("previews differ:\n%#v\n%#v", first, second)
	}
	if first.Target != "fake:owner/repo" || len(first.Items) != 2 {
		t.Fatalf("preview = %#v", first)
	}
	if !strings.Contains(first.Items[1].Body, "session-1/2/task") || !strings.Contains(first.Items[1].Body, "It works.") {
		t.Fatalf("task body = %q, want correlation and acceptance criteria", first.Items[1].Body)
	}
	want := []tracker.PlanRelationshipMapping{
		{Kind: tracker.PlanRelationshipParent, FromNodeID: "task", ToNodeID: "spec", Mode: tracker.PlanRelationshipNative},
		{Kind: tracker.PlanRelationshipDependsOn, FromNodeID: "task", ToNodeID: "spec", Mode: tracker.PlanRelationshipNative},
	}
	if !reflect.DeepEqual(first.Relationships, want) {
		t.Fatalf("relationships = %#v, want %#v", first.Relationships, want)
	}
	if fake.Writes() != 0 {
		t.Fatalf("Writes() = %d after preflight, want 0", fake.Writes())
	}
}

// TestFakePreflightRefusesFactoryLabelsAndUnmappedKinds verifies the typed
// preflight errors for a configured agent-ready label and a kind without a
// mapped type.
func TestFakePreflightRefusesFactoryLabelsAndUnmappedKinds(t *testing.T) {
	t.Parallel()

	labelled := trackertest.NewPlanTracker()
	labelled.Labels = []string{"enhancement", "Agent-Ready"}
	if _, err := labelled.PreflightPlan(t.Context(), repository, graphForTest()); !isPlanError(err, tracker.PlanErrorInvalidMapping) {
		t.Fatalf("PreflightPlan() with agent-ready error = %v, want invalid_mapping", err)
	}
	unmapped := trackertest.NewPlanTracker()
	unmapped.Types = map[string]string{"spec": "Epic"}
	if _, err := unmapped.PreflightPlan(t.Context(), repository, graphForTest()); !isPlanError(err, tracker.PlanErrorInvalidMapping) {
		t.Fatalf("PreflightPlan() with unmapped kind error = %v, want invalid_mapping", err)
	}
}

// TestFakeInjectsFailuresBeforeAndAfterTheWrite verifies that an injected
// fault before the write leaves no item, that a lost response leaves exactly
// one item, and that a seeded duplicate makes the lookup ambiguous.
func TestFakeInjectsFailuresBeforeAndAfterTheWrite(t *testing.T) {
	t.Parallel()

	fake := trackertest.NewPlanTracker()
	preview, err := fake.PreflightPlan(t.Context(), repository, graphForTest())
	if err != nil {
		t.Fatal(err)
	}
	spec := preview.Items[0]
	denied := tracker.NewPlanError(tracker.PlanErrorPermission, "no write access")
	fake.Inject(trackertest.Fault{Op: trackertest.OpCreateItem, Key: "spec", Err: denied})
	if _, err := fake.CreatePlanItem(t.Context(), repository, spec); !errors.Is(err, denied) {
		t.Fatalf("CreatePlanItem() error = %v, want injected", err)
	}
	if found, _ := fake.FindPlanItems(t.Context(), repository, spec.Correlation); len(found) != 0 {
		t.Fatalf("FindPlanItems() after a failure before the write = %#v, want none", found)
	}

	fake.Inject(trackertest.Fault{Op: trackertest.OpCreateItem, Key: "spec", AfterWrite: true, Err: errors.New("timeout")})
	if _, err := fake.CreatePlanItem(t.Context(), repository, spec); err == nil {
		t.Fatal("CreatePlanItem() lost response returned no error")
	}
	found, err := fake.FindPlanItems(t.Context(), repository, spec.Correlation)
	if err != nil || len(found) != 1 {
		t.Fatalf("FindPlanItems() after a lost response = %#v, %v; want one item", found, err)
	}

	fake.Seed(spec)
	if found, _ := fake.FindPlanItems(t.Context(), repository, spec.Correlation); len(found) != 2 {
		t.Fatalf("FindPlanItems() after a seeded duplicate = %#v, want two items", found)
	}
	if fake.Writes() != 2 {
		t.Fatalf("Writes() = %d, want 2 create calls", fake.Writes())
	}
}

// TestFakeRelationshipsAreFoundAfterCreation verifies relationship creation
// and verification by kind and endpoints.
func TestFakeRelationshipsAreFoundAfterCreation(t *testing.T) {
	t.Parallel()

	fake := trackertest.NewPlanTracker()
	preview, err := fake.PreflightPlan(t.Context(), repository, graphForTest())
	if err != nil {
		t.Fatal(err)
	}
	parent, err := fake.CreatePlanItem(t.Context(), repository, preview.Items[0])
	if err != nil {
		t.Fatal(err)
	}
	child, err := fake.CreatePlanItem(t.Context(), repository, preview.Items[1])
	if err != nil {
		t.Fatal(err)
	}
	link := tracker.PlanRelationship{Kind: tracker.PlanRelationshipParent, Mode: tracker.PlanRelationshipNative, From: child, To: parent}
	if exists, err := fake.FindPlanRelationship(t.Context(), repository, link); err != nil || exists {
		t.Fatalf("FindPlanRelationship() before creation = %v, %v", exists, err)
	}
	if err := fake.CreatePlanRelationship(t.Context(), repository, link); err != nil {
		t.Fatal(err)
	}
	if exists, err := fake.FindPlanRelationship(t.Context(), repository, link); err != nil || !exists {
		t.Fatalf("FindPlanRelationship() after creation = %v, %v", exists, err)
	}
	if got := fake.Relationships(); len(got) != 1 || got[0] != link {
		t.Fatalf("Relationships() = %#v", got)
	}
}

// isPlanError reports whether err is a tracker.PlanError of kind.
func isPlanError(err error, kind tracker.PlanErrorKind) bool {
	got, ok := tracker.PlanErrorKindOf(err)
	return ok && got == kind
}
