package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/github/githubtest"
	"github.com/Stevie1704/sw-factory/internal/hostcmd"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// planRepository is the registered repository of the planning tests.
var planRepository = tracker.Repository{Owner: "example", Name: "project"}

// writableRepository is the repository response of a target where the
// account can open issues.
const writableRepository = githubtest.WritableRepository

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
const nativeCapabilities = githubtest.NativeCapabilities

// legacyCapabilities is the introspection answer of a GitHub host without
// either relationship.
const legacyCapabilities = githubtest.LegacyCapabilities

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

// TestPreflightPlanReportsTypedErrorsBeforeAnyWrite verifies that missing
// permission, unavailable capability, and GitHub limits fail preflight with
// a typed error that proves no change, and that preflight never writes.
func TestPreflightPlanReportsTypedErrorsBeforeAnyWrite(t *testing.T) {
	t.Parallel()

	wide := decomposedGraph()
	wide.Items = wide.Items[:1]
	for index := range 101 {
		node := "c-" + strconv.Itoa(index)
		wide.Items = append(wide.Items, tracker.PlanGraphItem{NodeID: node, Kind: "task", Title: "Child " + node, ParentID: "n-1", Correlation: "s/4/" + node})
	}
	deep := decomposedGraph()
	deep.Items = deep.Items[:1]
	for index := range 9 {
		parent := deep.Items[len(deep.Items)-1].NodeID
		node := "d-" + strconv.Itoa(index)
		deep.Items = append(deep.Items, tracker.PlanGraphItem{NodeID: node, Kind: "task", Title: "Level " + node, ParentID: parent, Correlation: "s/4/" + node})
	}
	longTitle := singleItemGraph()
	longTitle.Items[0].Title = strings.Repeat("t", 257)
	longBody := singleItemGraph()
	longBody.Items[0].Body = strings.Repeat("b", 65536)

	for _, tc := range []struct {
		name    string
		graph   tracker.PlanGraph
		outputs []string
		errs    []error
		want    tracker.PlanErrorKind
	}{
		{"no read permission", singleItemGraph(), []string{`{"has_issues":true,"permissions":{"pull":false}}`}, nil, tracker.PlanErrorPermission},
		{"archived repository", singleItemGraph(), []string{`{"has_issues":true,"archived":true,"permissions":{"pull":true}}`}, nil, tracker.PlanErrorPermission},
		{"issues disabled", singleItemGraph(), []string{`{"has_issues":false,"permissions":{"pull":true}}`}, nil, tracker.PlanErrorUnsupportedCapability},
		{"repository hidden", singleItemGraph(), []string{""}, []error{errors.New("gh api repos/example/project: HTTP 404: Not Found")}, tracker.PlanErrorPermission},
		{"title too long", longTitle, []string{writableRepository}, nil, tracker.PlanErrorInvalidMapping},
		{"body too long", longBody, []string{writableRepository}, nil, tracker.PlanErrorInvalidMapping},
		{"read-only account with native links", decomposedGraph(), []string{githubtest.ReadOnlyRepository, nativeCapabilities}, nil, tracker.PlanErrorPermission},
		{"too many sub-issues", wide, []string{writableRepository, nativeCapabilities}, nil, tracker.PlanErrorUnsupportedCapability},
		{"sub-issues nested too deep", deep, []string{writableRepository, nativeCapabilities}, nil, tracker.PlanErrorUnsupportedCapability},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner := &fakeCommandRunner{errs: tc.errs}
			for _, output := range tc.outputs {
				runner.outputs = append(runner.outputs, []byte(output))
			}
			_, err := (&github.GhClient{Runner: runner}).PreflightPlan(context.Background(), planRepository, tc.graph)
			if kind, ok := tracker.PlanErrorKindOf(err); !ok || kind != tc.want {
				t.Fatalf("PreflightPlan() error = %v, want kind %s", err, tc.want)
			}
			if !tracker.WriteNotApplied(err) {
				t.Fatalf("PreflightPlan() error = %v, want a proven no-change error", err)
			}
			assertReadOnly(t, runner.calls)
		})
	}
}

// TestPreflightPlanTreatsTextualParentsWithoutSubIssueLimits verifies that
// the sub-issue limits apply only to native sub-issues: a wide plan on a host
// without sub-issues previews with the textual fallback.
func TestPreflightPlanTreatsTextualParentsWithoutSubIssueLimits(t *testing.T) {
	t.Parallel()

	wide := decomposedGraph()
	wide.Items = wide.Items[:1]
	for index := range 101 {
		node := "c-" + strconv.Itoa(index)
		wide.Items = append(wide.Items, tracker.PlanGraphItem{NodeID: node, Kind: "task", Title: "Child " + node, ParentID: "n-1", Correlation: "s/4/" + node})
	}
	runner := &fakeCommandRunner{outputs: [][]byte{[]byte(writableRepository), []byte(legacyCapabilities)}}
	preview, err := (&github.GhClient{Runner: runner}).PreflightPlan(context.Background(), planRepository, wide)
	if err != nil {
		t.Fatalf("PreflightPlan() error = %v", err)
	}
	if len(preview.Relationships) != 101 || preview.Relationships[0].Mode != tracker.PlanRelationshipTextual {
		t.Fatalf("relationships = %d (first %#v), want 101 textual parents", len(preview.Relationships), preview.Relationships[0])
	}
}

// issueListPage returns one slurped issue listing page with count issues
// that do not carry the correlation marker.
func issueListPage(first, count int) string {
	issues := make([]string, 0, count)
	for number := first; number < first+count; number++ {
		issues = append(issues, `{"number":`+strconv.Itoa(number)+`,"html_url":"https://github.com/example/project/issues/`+strconv.Itoa(number)+`","body":"other","user":{"login":"factory-bot"}}`)
	}
	return "[" + strings.Join(issues, ",") + "]"
}

// TestFindPlanItemsMatchesTheMarkerInTheAccountsRecentIssues verifies that
// the correlation lookup lists the issues the authenticated account created,
// newest first and in any state, instead of the lagging search index, and
// that it returns exact marker matches by that account only.
func TestFindPlanItemsMatchesTheMarkerInTheAccountsRecentIssues(t *testing.T) {
	t.Parallel()

	runner := &fakeCommandRunner{outputs: [][]byte{
		[]byte(`{"login":"factory-bot"}`),
		[]byte(`[` +
			`{"number":14,"html_url":"https://github.com/example/project/issues/14","body":"x\n<!-- factory-plan: session-1/4/n-2 -->","user":{"login":"factory-bot"}},` +
			`{"number":13,"html_url":"https://github.com/example/project/issues/13","body":"x\n<!-- factory-plan: session-1/4/n-22 -->","user":{"login":"factory-bot"}},` +
			`{"number":12,"html_url":"https://github.com/example/project/pull/12","body":"<!-- factory-plan: session-1/4/n-2 -->","pull_request":{},"user":{"login":"factory-bot"}},` +
			`{"number":11,"html_url":"https://github.com/example/project/issues/11","body":"<!-- factory-plan: session-1/4/n-2 -->","user":{"login":"mallory"}},` +
			`{"number":10,"html_url":"https://github.com/example/project/issues/10","body":"<!-- factory-plan: session-1/4/n-2 -->","user":{"login":"factory-bot"}}` +
			`]`),
	}}
	client := &github.GhClient{Runner: runner}

	found, err := client.FindPlanItems(context.Background(), planRepository, "session-1/4/n-2")
	if err != nil {
		t.Fatalf("FindPlanItems() error = %v", err)
	}
	want := []tracker.PlanItemRef{
		{ExternalID: "14", URL: "https://github.com/example/project/issues/14"},
		{ExternalID: "10", URL: "https://github.com/example/project/issues/10"},
	}
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("found = %#v, want both marker issues of the account", found)
	}
	listing := runner.calls[1].args
	if !containsArgs(listing, "api", "repos/example/project/issues") ||
		!hasArgs(listing, "state=all", "creator=factory-bot", "sort=created", "direction=desc", "per_page=100", "page=1", "GET") {
		t.Fatalf("listing = %#v, want the account's issues newest first", listing)
	}
	for _, call := range runner.calls {
		if hasArgs(call.args, "search/issues") || hasArgs(call.args, "--search") {
			t.Fatalf("call %#v uses the search index", call.args)
		}
	}
	assertReadOnly(t, runner.calls)
}

// TestFindPlanItemsStopsAtItsListingBound verifies that the lookup reads at
// most ten full pages and stops early on a short page.
func TestFindPlanItemsStopsAtItsListingBound(t *testing.T) {
	t.Parallel()

	short := &fakeCommandRunner{outputs: [][]byte{[]byte(`{"login":"factory-bot"}`), []byte(issueListPage(1, 100)), []byte(issueListPage(101, 3))}}
	found, err := (&github.GhClient{Runner: short}).FindPlanItems(context.Background(), planRepository, "session-1/4/n-2")
	if err != nil || len(found) != 0 {
		t.Fatalf("FindPlanItems() = %#v, %v; want no match", found, err)
	}
	if len(short.calls) != 3 || !hasArgs(short.calls[2].args, "page=2") {
		t.Fatalf("calls = %d, want the account and two pages", len(short.calls))
	}

	full := &fakeCommandRunner{outputs: [][]byte{[]byte(`{"login":"factory-bot"}`)}}
	for page := range 12 {
		full.outputs = append(full.outputs, []byte(issueListPage(page*100+1, 100)))
	}
	if _, err := (&github.GhClient{Runner: full}).FindPlanItems(context.Background(), planRepository, "session-1/4/n-2"); err != nil {
		t.Fatalf("FindPlanItems() error = %v", err)
	}
	if len(full.calls) != 11 {
		t.Fatalf("calls = %d, want the account and ten pages", len(full.calls))
	}
}

// issueRefs returns the neutral references of two created issues.
func issueRefs(from, to int) (tracker.PlanItemRef, tracker.PlanItemRef) {
	ref := func(number int) tracker.PlanItemRef {
		return tracker.PlanItemRef{ExternalID: strconv.Itoa(number), URL: "https://github.com/example/project/issues/" + strconv.Itoa(number)}
	}
	return ref(from), ref(to)
}

// TestCreatePlanRelationshipUsesNativeSubIssuesAndDependencies verifies that
// a native parent becomes a sub-issue of the parent issue and a native
// dependency becomes a blocked-by dependency of the dependent issue, both
// addressed by the database identity GitHub requires.
func TestCreatePlanRelationshipUsesNativeSubIssuesAndDependencies(t *testing.T) {
	t.Parallel()

	runner := &fakeCommandRunner{outputs: [][]byte{
		[]byte(`{"id":5013,"number":13}`), []byte(`{}`),
		[]byte(`{"id":5012,"number":12}`), []byte(`{}`),
	}}
	client := &github.GhClient{Runner: runner}
	child, parent := issueRefs(13, 11)
	if err := client.CreatePlanRelationship(context.Background(), planRepository, tracker.PlanRelationship{
		Kind: tracker.PlanRelationshipParent, Mode: tracker.PlanRelationshipNative, From: child, To: parent,
	}); err != nil {
		t.Fatalf("CreatePlanRelationship(parent) error = %v", err)
	}
	dependent, dependency := issueRefs(13, 12)
	if err := client.CreatePlanRelationship(context.Background(), planRepository, tracker.PlanRelationship{
		Kind: tracker.PlanRelationshipDependsOn, Mode: tracker.PlanRelationshipNative, From: dependent, To: dependency,
	}); err != nil {
		t.Fatalf("CreatePlanRelationship(depends_on) error = %v", err)
	}

	if !containsArgs(runner.calls[0].args, "api", "repos/example/project/issues/13") {
		t.Fatalf("call 0 = %#v, want the child issue read", runner.calls[0].args)
	}
	if !containsArgs(runner.calls[1].args, "api", "repos/example/project/issues/11/sub_issues", "--method", "POST") ||
		string(runner.calls[1].input) != `{"sub_issue_id":5013}` {
		t.Fatalf("call 1 = %#v %s, want the child added as a sub-issue of 11", runner.calls[1].args, runner.calls[1].input)
	}
	if !containsArgs(runner.calls[2].args, "api", "repos/example/project/issues/12") {
		t.Fatalf("call 2 = %#v, want the dependency issue read", runner.calls[2].args)
	}
	if !containsArgs(runner.calls[3].args, "api", "repos/example/project/issues/13/dependencies/blocked_by", "--method", "POST") ||
		string(runner.calls[3].input) != `{"issue_id":5012}` {
		t.Fatalf("call 3 = %#v %s, want 13 blocked by 12", runner.calls[3].args, runner.calls[3].input)
	}
}

// TestFindPlanRelationshipVerifiesNativeLinks verifies that native links are
// read back from the parent's sub-issues and the dependent's blockers.
func TestFindPlanRelationshipVerifiesNativeLinks(t *testing.T) {
	t.Parallel()

	runner := &fakeCommandRunner{outputs: [][]byte{
		[]byte(`[[{"id":5014,"number":14},{"id":5013,"number":13}]]`),
		[]byte(`[[{"id":5010,"number":10}]]`),
	}}
	client := &github.GhClient{Runner: runner}
	child, parent := issueRefs(13, 11)
	exists, err := client.FindPlanRelationship(context.Background(), planRepository, tracker.PlanRelationship{
		Kind: tracker.PlanRelationshipParent, Mode: tracker.PlanRelationshipNative, From: child, To: parent,
	})
	if err != nil || !exists {
		t.Fatalf("FindPlanRelationship(parent) = %t, %v; want an existing sub-issue", exists, err)
	}
	dependent, dependency := issueRefs(13, 12)
	exists, err = client.FindPlanRelationship(context.Background(), planRepository, tracker.PlanRelationship{
		Kind: tracker.PlanRelationshipDependsOn, Mode: tracker.PlanRelationshipNative, From: dependent, To: dependency,
	})
	if err != nil || exists {
		t.Fatalf("FindPlanRelationship(depends_on) = %t, %v; want no blocker 12", exists, err)
	}
	if !containsArgs(runner.calls[0].args, "api", "repos/example/project/issues/11/sub_issues") ||
		!containsArgs(runner.calls[1].args, "api", "repos/example/project/issues/13/dependencies/blocked_by") {
		t.Fatalf("calls = %#v, want the sub-issue and blocker listings", runner.calls)
	}
	assertReadOnly(t, runner.calls)
}

// TestTextualRelationshipIsOneMarkedBacklinkComment verifies the approved
// textual fallback: one comment on the sub-issue or dependent issue names
// the other issue number, carries a stable marker, and is found again only
// when the authenticated account wrote it.
func TestTextualRelationshipIsOneMarkedBacklinkComment(t *testing.T) {
	t.Parallel()

	runner := &fakeCommandRunner{outputs: [][]byte{
		[]byte(`{"id":1,"body":"ok"}`),
		[]byte(`{"login":"factory-bot"}`),
		[]byte(`[[{"id":7,"body":"Depends on #12\n\n<!-- factory-plan-link: depends_on #13 -> #12 -->","user":{"login":"mallory"}}]]`),
		[]byte(`{"login":"factory-bot"}`),
		[]byte(`[[{"id":8,"body":"Part of #11\n\n<!-- factory-plan-link: parent #13 -> #11 -->","user":{"login":"factory-bot"}}]]`),
	}}
	client := &github.GhClient{Runner: runner}
	child, parent := issueRefs(13, 11)
	parentLink := tracker.PlanRelationship{Kind: tracker.PlanRelationshipParent, Mode: tracker.PlanRelationshipTextual, From: child, To: parent}
	if err := client.CreatePlanRelationship(context.Background(), planRepository, parentLink); err != nil {
		t.Fatalf("CreatePlanRelationship(textual) error = %v", err)
	}
	if !containsArgs(runner.calls[0].args, "api", "repos/example/project/issues/13/comments", "--method", "POST") {
		t.Fatalf("call 0 = %#v, want a comment on the sub-issue", runner.calls[0].args)
	}
	var comment map[string]string
	if err := json.Unmarshal(runner.calls[0].input, &comment); err != nil {
		t.Fatalf("decode comment: %v", err)
	}
	if comment["body"] != "Part of #11\n\n<!-- factory-plan-link: parent #13 -> #11 -->" {
		t.Fatalf("comment body = %q, want the marked backlink", comment["body"])
	}

	dependent, dependency := issueRefs(13, 12)
	forged, err := client.FindPlanRelationship(context.Background(), planRepository, tracker.PlanRelationship{
		Kind: tracker.PlanRelationshipDependsOn, Mode: tracker.PlanRelationshipTextual, From: dependent, To: dependency,
	})
	if err != nil || forged {
		t.Fatalf("FindPlanRelationship(forged) = %t, %v; want a comment by another account ignored", forged, err)
	}
	exists, err := client.FindPlanRelationship(context.Background(), planRepository, parentLink)
	if err != nil || !exists {
		t.Fatalf("FindPlanRelationship(textual) = %t, %v; want the factory comment", exists, err)
	}
	if len(runner.calls) != 5 {
		t.Fatalf("calls = %d, want five", len(runner.calls))
	}
}

// TestPlanWritesClassifyGhFailuresByProvenOutcome verifies that only an
// HTTP rejection proves that GitHub made no change, that a deadline expiry
// is ambiguous, and that a rate limit or a server error stays an unknown
// outcome.
func TestPlanWritesClassifyGhFailuresByProvenOutcome(t *testing.T) {
	t.Parallel()

	item := tracker.RenderedPlanItem{NodeID: "n-1", Type: "issue", Title: "Cache worker images", Body: "body", Correlation: "s/1/n-1"}
	for _, tc := range []struct {
		name       string
		err        error
		kind       tracker.PlanErrorKind
		notApplied bool
	}{
		{"timeout", &hostcmd.TimeoutError{Operation: "gh api", Timeout: github.CommandTimeout}, tracker.PlanErrorAmbiguous, false},
		{"forbidden", errors.New("gh api repos/example/project/issues --method POST: HTTP 403: Resource not accessible by integration"), tracker.PlanErrorPermission, true},
		{"issues disabled", errors.New("gh api repos/example/project/issues: HTTP 410: Issues are disabled for this repo"), tracker.PlanErrorUnsupportedCapability, true},
		{"validation", errors.New("gh api repos/example/project/issues: HTTP 422: Validation Failed"), tracker.PlanErrorInvalidMapping, true},
		{"rate limit", errors.New("gh api repos/example/project/issues: HTTP 403: API rate limit exceeded"), "", false},
		{"server error", errors.New("gh api repos/example/project/issues: HTTP 502: Bad Gateway"), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner := &fakeCommandRunner{outputs: [][]byte{nil}, errs: []error{tc.err}}
			_, err := (&github.GhClient{Runner: runner}).CreatePlanItem(context.Background(), planRepository, item)
			if err == nil {
				t.Fatal("CreatePlanItem() error = nil, want the gh failure")
			}
			if kind, _ := tracker.PlanErrorKindOf(err); kind != tc.kind {
				t.Fatalf("CreatePlanItem() error = %v, want kind %q", err, tc.kind)
			}
			if tracker.WriteNotApplied(err) != tc.notApplied {
				t.Fatalf("WriteNotApplied(%v) = %t, want %t", err, !tc.notApplied, tc.notApplied)
			}
		})
	}
}

// TestRetryAfterAnUncertainCreateFindsTheIssueInsteadOfCreatingAgain
// verifies the reconciliation path at the adapter seam: a create whose
// response is lost is ambiguous, and the following correlation lookup finds
// the issue GitHub created, so the caller adopts it and sends no second
// create.
func TestRetryAfterAnUncertainCreateFindsTheIssueInsteadOfCreatingAgain(t *testing.T) {
	t.Parallel()

	runner := &fakeCommandRunner{
		outputs: [][]byte{
			nil,
			[]byte(`{"login":"factory-bot"}`),
			[]byte(`[{"number":21,"html_url":"https://github.com/example/project/issues/21","body":"body\n<!-- factory-plan: s/1/n-1 -->","user":{"login":"factory-bot"}}]`),
		},
		errs: []error{&hostcmd.TimeoutError{Operation: "gh api", Timeout: github.CommandTimeout}},
	}
	client := &github.GhClient{Runner: runner}
	item := tracker.RenderedPlanItem{NodeID: "n-1", Type: "issue", Title: "Cache worker images", Body: "body\n<!-- factory-plan: s/1/n-1 -->", Correlation: "s/1/n-1"}

	_, err := client.CreatePlanItem(context.Background(), planRepository, item)
	if kind, _ := tracker.PlanErrorKindOf(err); kind != tracker.PlanErrorAmbiguous || tracker.WriteNotApplied(err) {
		t.Fatalf("CreatePlanItem() error = %v, want an ambiguous outcome", err)
	}
	found, err := client.FindPlanItems(context.Background(), planRepository, "s/1/n-1")
	if err != nil || len(found) != 1 || found[0].ExternalID != "21" {
		t.Fatalf("FindPlanItems() = %#v, %v; want issue 21", found, err)
	}
	creates := 0
	for _, call := range runner.calls {
		if hasArgs(call.args, "POST") {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("creates = %d, want exactly one", creates)
	}
}

// TestPartialLinkFailureIsVerifiedBeforeARetry verifies that a relationship
// create with an unknown outcome is not proven unapplied, and that the next
// verification sees the link GitHub created, so a retry adds no duplicate.
func TestPartialLinkFailureIsVerifiedBeforeARetry(t *testing.T) {
	t.Parallel()

	runner := &fakeCommandRunner{
		outputs: [][]byte{[]byte(`{"id":5013,"number":13}`), nil, []byte(`[[{"id":5013,"number":13}]]`)},
		errs:    []error{nil, errors.New("gh api repos/example/project/issues/11/sub_issues --method POST: HTTP 502: Bad Gateway")},
	}
	client := &github.GhClient{Runner: runner}
	child, parent := issueRefs(13, 11)
	link := tracker.PlanRelationship{Kind: tracker.PlanRelationshipParent, Mode: tracker.PlanRelationshipNative, From: child, To: parent}

	err := client.CreatePlanRelationship(context.Background(), planRepository, link)
	if err == nil || tracker.WriteNotApplied(err) {
		t.Fatalf("CreatePlanRelationship() error = %v, want an unknown outcome", err)
	}
	exists, err := client.FindPlanRelationship(context.Background(), planRepository, link)
	if err != nil || !exists {
		t.Fatalf("FindPlanRelationship() = %t, %v; want the link GitHub created", exists, err)
	}
}
