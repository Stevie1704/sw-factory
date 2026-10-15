package github_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// planRepository is the registered repository of the planning tests.
var planRepository = tracker.Repository{Owner: "example", Name: "project"}

// writableRepository is the repository response of a target where the
// account can open issues.
const writableRepository = `{"has_issues":true,"archived":false,"permissions":{"pull":true,"push":true,"triage":true}}`

// singleItemGraph is a plan with one node and no relationships.
func singleItemGraph() tracker.PlanGraph {
	return tracker.PlanGraph{
		SessionID: "session-1", PlanRevision: 3, ContextRevision: "0123456789abcdef0123456789abcdef01234567",
		Items: []tracker.PlanGraphItem{{
			NodeID: "n-1", Kind: "task", Title: "Cache worker images", Body: "Reuse the pulled image.",
			AcceptanceCriteria: []string{"A second run pulls nothing."}, BriefRefs: []string{"scope-1"},
			Correlation: tracker.PlanCorrelation("session-1", 3, "n-1"),
		}},
	}
}

// TestPreflightPlanRendersOneReadOnlyIssuePreview verifies that a single-item
// plan renders one complete issue with traceability metadata and the hidden
// correlation marker, through reads only, and byte-identically on repeat.
func TestPreflightPlanRendersOneReadOnlyIssuePreview(t *testing.T) {
	t.Parallel()

	runner := &fakeCommandRunner{outputs: [][]byte{[]byte(writableRepository), []byte(writableRepository)}}
	client := &github.GhClient{Runner: runner}

	preview, err := client.PreflightPlan(context.Background(), planRepository, singleItemGraph())
	if err != nil {
		t.Fatalf("PreflightPlan() error = %v", err)
	}
	again, err := client.PreflightPlan(context.Background(), planRepository, singleItemGraph())
	if err != nil {
		t.Fatalf("second PreflightPlan() error = %v", err)
	}
	if !reflect.DeepEqual(preview, again) {
		t.Fatalf("preview is not deterministic:\n%#v\n%#v", preview, again)
	}
	if preview.Target != "github:example/project" || len(preview.Items) != 1 || len(preview.Relationships) != 0 {
		t.Fatalf("preview = %#v, want one issue for github:example/project", preview)
	}
	item := preview.Items[0]
	if item.NodeID != "n-1" || item.Type != "issue" || item.Title != "Cache worker images" || len(item.Labels) != 0 {
		t.Fatalf("item = %#v, want one unlabeled issue", item)
	}
	for _, want := range []string{
		"Reuse the pulled image.",
		"- [ ] A second run pulls nothing.",
		"session `session-1`, plan revision 3, node `n-1`",
		"`0123456789abcdef0123456789abcdef01234567`",
		"scope-1",
		"<!-- factory-plan: session-1/3/n-1 -->",
	} {
		if !strings.Contains(item.Body, want) {
			t.Errorf("body does not contain %q:\n%s", want, item.Body)
		}
	}
	if item.Correlation != "session-1/3/n-1" {
		t.Fatalf("correlation = %q, want session-1/3/n-1", item.Correlation)
	}
	assertReadOnly(t, runner.calls)
}

// TestCreatePlanItemCreatesExactlyTheRenderedIssueWithoutFactoryLabels
// verifies decision D6: the create request carries the approved title and
// body and no label, and an item with a factory state label is refused
// before any gh call.
func TestCreatePlanItemCreatesExactlyTheRenderedIssueWithoutFactoryLabels(t *testing.T) {
	t.Parallel()

	runner := &fakeCommandRunner{outputs: [][]byte{[]byte(`{"id":9001,"number":12,"html_url":"https://github.com/example/project/issues/12"}`)}}
	client := &github.GhClient{Runner: runner}
	item := tracker.RenderedPlanItem{NodeID: "n-1", Type: "issue", Title: "Cache worker images", Body: "body\n<!-- factory-plan: s/1/n-1 -->", Correlation: "s/1/n-1"}

	ref, err := client.CreatePlanItem(context.Background(), planRepository, item)
	if err != nil {
		t.Fatalf("CreatePlanItem() error = %v", err)
	}
	if ref.ExternalID != "12" || ref.URL != "https://github.com/example/project/issues/12" {
		t.Fatalf("ref = %#v, want issue 12", ref)
	}
	if len(runner.calls) != 1 || !containsArgs(runner.calls[0].args, "api", "repos/example/project/issues", "--method", "POST") {
		t.Fatalf("calls = %#v, want one issue create", runner.calls)
	}
	var payload map[string]any
	if err := json.Unmarshal(runner.calls[0].input, &payload); err != nil {
		t.Fatalf("decode create request: %v", err)
	}
	if payload["title"] != item.Title || payload["body"] != item.Body {
		t.Fatalf("create request = %#v, want the rendered title and body", payload)
	}
	if _, ok := payload["labels"]; ok {
		t.Fatalf("create request = %#v, want no labels", payload)
	}

	for _, label := range tracker.FactoryStateLabels {
		labeled := item
		labeled.Labels = []string{label}
		_, err := client.CreatePlanItem(context.Background(), planRepository, labeled)
		if kind, _ := tracker.PlanErrorKindOf(err); kind != tracker.PlanErrorInvalidMapping {
			t.Fatalf("CreatePlanItem(label %q) error = %v, want invalid mapping", label, err)
		}
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %d, want no gh call for a factory label", len(runner.calls))
	}
}

// assertReadOnly fails when any recorded gh call can write.
func assertReadOnly(t *testing.T, calls []commandCall) {
	t.Helper()
	for _, call := range calls {
		for index, arg := range call.args {
			if arg == "--method" && index+1 < len(call.args) && call.args[index+1] != "GET" {
				t.Fatalf("call %#v writes; preflight must be read-only", call.args)
			}
		}
		if len(call.args) > 0 && call.args[0] != "api" {
			t.Fatalf("call %#v is not a gh api read", call.args)
		}
		if len(call.input) > 0 && !(len(call.args) > 1 && call.args[1] == "graphql") {
			t.Fatalf("call %#v sends a request body; preflight must be read-only", call.args)
		}
	}
}

// nativeCapabilities is the GraphQL introspection answer of a GitHub host
// with native sub-issues and blocking dependencies.
const nativeCapabilities = `{"data":{"__type":{"fields":[{"name":"title"},{"name":"subIssues"},{"name":"parent"},{"name":"blockedBy"},{"name":"blocking"}]}}}`

// legacyCapabilities is the introspection answer of a GitHub host without
// either relationship.
const legacyCapabilities = `{"data":{"__type":{"fields":[{"name":"title"},{"name":"body"}]}}}`

// decomposedGraph is a parent specification n-1 with children n-2 and n-3,
// where n-3 depends on n-2.
func decomposedGraph() tracker.PlanGraph {
	item := func(node, kind, title, parent string, depends ...string) tracker.PlanGraphItem {
		return tracker.PlanGraphItem{
			NodeID: node, Kind: kind, Title: title, Body: title + " body.", ParentID: parent, DependsOn: depends,
			Correlation: tracker.PlanCorrelation("session-1", 4, node),
		}
	}
	return tracker.PlanGraph{
		SessionID: "session-1", PlanRevision: 4, ContextRevision: "0123456789abcdef0123456789abcdef01234567",
		Items: []tracker.PlanGraphItem{
			item("n-1", "spec", "Faster onboarding", ""),
			item("n-2", "task", "Cache images", "n-1"),
			item("n-3", "task", "Document setup", "n-1", "n-2"),
		},
	}
}

// TestPreflightPlanMapsTheGraphToNativeRelationships verifies that a
// decomposed plan maps parents to sub-issues and dependencies to blocking
// dependencies when GitHub offers both, without a textual section.
func TestPreflightPlanMapsTheGraphToNativeRelationships(t *testing.T) {
	t.Parallel()

	runner := &fakeCommandRunner{outputs: [][]byte{[]byte(writableRepository), []byte(nativeCapabilities)}}
	client := &github.GhClient{Runner: runner}

	preview, err := client.PreflightPlan(context.Background(), planRepository, decomposedGraph())
	if err != nil {
		t.Fatalf("PreflightPlan() error = %v", err)
	}
	want := []tracker.PlanRelationshipMapping{
		{Kind: tracker.PlanRelationshipParent, FromNodeID: "n-2", ToNodeID: "n-1", Mode: tracker.PlanRelationshipNative},
		{Kind: tracker.PlanRelationshipParent, FromNodeID: "n-3", ToNodeID: "n-1", Mode: tracker.PlanRelationshipNative},
		{Kind: tracker.PlanRelationshipDependsOn, FromNodeID: "n-3", ToNodeID: "n-2", Mode: tracker.PlanRelationshipNative},
	}
	if !reflect.DeepEqual(preview.Relationships, want) {
		t.Fatalf("relationships = %#v, want %#v", preview.Relationships, want)
	}
	if len(preview.Items) != 3 {
		t.Fatalf("items = %d, want the parent and both children", len(preview.Items))
	}
	for _, item := range preview.Items {
		if strings.Contains(item.Body, "## Plan relationships") {
			t.Fatalf("item %s body has a textual fallback with native relationships:\n%s", item.NodeID, item.Body)
		}
	}
	if !containsArgs(runner.calls[1].args, "api", "graphql") {
		t.Fatalf("capability call = %#v, want a GraphQL introspection", runner.calls[1].args)
	}
	assertReadOnly(t, runner.calls)
}

// TestPreflightPlanShowsTheTextualFallbackInThePreview verifies that
// unavailable native relationships become an explicit textual fallback in
// the relationship mapping and in the rendered bodies, so the approval binds
// it.
func TestPreflightPlanShowsTheTextualFallbackInThePreview(t *testing.T) {
	t.Parallel()

	runner := &fakeCommandRunner{outputs: [][]byte{[]byte(writableRepository), []byte(legacyCapabilities)}}
	client := &github.GhClient{Runner: runner}

	preview, err := client.PreflightPlan(context.Background(), planRepository, decomposedGraph())
	if err != nil {
		t.Fatalf("PreflightPlan() error = %v", err)
	}
	if len(preview.Relationships) != 3 {
		t.Fatalf("relationships = %#v, want three", preview.Relationships)
	}
	for _, relationship := range preview.Relationships {
		if relationship.Mode != tracker.PlanRelationshipTextual {
			t.Fatalf("relationship = %#v, want the textual fallback", relationship)
		}
	}
	bodies := map[string]string{}
	for _, item := range preview.Items {
		bodies[item.NodeID] = item.Body
	}
	for node, wants := range map[string][]string{
		"n-1": {"## Plan relationships", "- [ ] Sub-issue: Cache images (`n-2`)", "- [ ] Sub-issue: Document setup (`n-3`)"},
		"n-2": {"## Plan relationships", "- Part of: Faster onboarding (`n-1`)"},
		"n-3": {"- Part of: Faster onboarding (`n-1`)", "- Depends on: Cache images (`n-2`)"},
	} {
		for _, want := range wants {
			if !strings.Contains(bodies[node], want) {
				t.Errorf("body of %s does not contain %q:\n%s", node, want, bodies[node])
			}
		}
	}
	assertReadOnly(t, runner.calls)
}
