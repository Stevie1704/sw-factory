package workflow_test

import (
	"testing"

	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// TestParseRouteAcceptsOnlyDeclaredValues verifies the bounded route grammar
// rejects every value the factory does not declare.
func TestParseRouteAcceptsOnlyDeclaredValues(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  workflow.Route
		valid bool
	}{
		{name: "acceptance", value: "acceptance", want: workflow.RouteAcceptance, valid: true},
		{name: "design acceptance", value: "design-acceptance", want: workflow.RouteDesignAcceptance, valid: true},
		{name: "surrounding space", value: "  acceptance  ", want: workflow.RouteAcceptance, valid: true},
		{name: "mixed case", value: "Design-Acceptance", want: workflow.RouteDesignAcceptance, valid: true},
		{name: "empty", value: "   "},
		{name: "unknown", value: "architecture"},
		{name: "implementation owned", value: "fast"},
		{name: "multiline", value: "acceptance\ndesign-acceptance"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			route, err := workflow.ParseRoute(test.value)
			if test.valid {
				if err != nil || route != test.want {
					t.Fatalf("ParseRoute(%q) = %q, %v; want %q, nil", test.value, route, err, test.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("ParseRoute(%q) error = nil, want rejection", test.value)
			}
		})
	}
}

// TestRouteEntryStageSelectsTheContractStage verifies each declared route
// names the stage a healthy baseline enters before implementation.
func TestRouteEntryStageSelectsTheContractStage(t *testing.T) {
	tests := []struct {
		name     string
		route    workflow.Route
		want     store.Stage
		selected bool
	}{
		{name: "default", route: workflow.RouteDefault},
		{name: "acceptance", route: workflow.RouteAcceptance, want: store.StageTest, selected: true},
		{name: "design acceptance", route: workflow.RouteDesignAcceptance, want: store.StageArchitecture, selected: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stage, ok := test.route.EntryStage()
			if ok != test.selected || stage != test.want {
				t.Fatalf("EntryStage() = %q, %t; want %q, %t", stage, ok, test.want, test.selected)
			}
			if got := test.route.RequiresIndependentTestStage(); got != test.selected {
				t.Fatalf("RequiresIndependentTestStage() = %t, want %t", got, test.selected)
			}
		})
	}
}

// TestRouteRequiredRolesDeclareTheContractRoles verifies a route names every
// factory role a repository must configure before the route can be claimed.
func TestRouteRequiredRolesDeclareTheContractRoles(t *testing.T) {
	if roles := workflow.RouteDefault.RequiredRoles(); len(roles) != 0 {
		t.Fatalf("default required roles = %#v, want none", roles)
	}
	acceptance := workflow.RouteAcceptance.RequiredRoles()
	if len(acceptance) != 1 || acceptance[0] != workflow.RoleTest {
		t.Fatalf("acceptance required roles = %#v, want the test role", acceptance)
	}
	design := workflow.RouteDesignAcceptance.RequiredRoles()
	if len(design) != 2 || design[0] != workflow.RoleArchitecture || design[1] != workflow.RoleTest {
		t.Fatalf("design-acceptance required roles = %#v, want architecture then test", design)
	}
}

// TestResolveRouteReportTransitionHandsDesignToTest verifies the
// design-acceptance route sends an accepted architecture handoff to the test
// role instead of the default implementation transition.
func TestResolveRouteReportTransitionHandsDesignToTest(t *testing.T) {
	registry := workflow.DefaultRegistry()

	design, err := registry.ResolveRouteReportTransition(workflow.RouteDesignAcceptance, store.StageArchitecture, report.OutcomeCompleted)
	if err != nil {
		t.Fatalf("ResolveRouteReportTransition(design) error = %v", err)
	}
	if design.Stage != store.StageTest || design.Status != store.StatusActive {
		t.Fatalf("design-acceptance architecture transition = %#v, want test/active", design)
	}

	for _, route := range []workflow.Route{workflow.RouteDefault, workflow.RouteAcceptance} {
		transition, err := registry.ResolveRouteReportTransition(route, store.StageArchitecture, report.OutcomeCompleted)
		if err != nil {
			t.Fatalf("ResolveRouteReportTransition(%q) error = %v", route, err)
		}
		if transition.Stage != store.StageImplementation {
			t.Fatalf("route %q architecture transition = %#v, want implementation", route, transition)
		}
	}
}

// TestResolveRouteReportTransitionKeepsEveryOtherDeclaredTransition verifies a
// route cannot reshape stages it does not own.
func TestResolveRouteReportTransitionKeepsEveryOtherDeclaredTransition(t *testing.T) {
	registry := workflow.DefaultRegistry()

	for _, stage := range []store.Stage{store.StageTest, store.StageImplementation, store.StageReview} {
		for _, outcome := range []report.Outcome{report.OutcomeCompleted, report.OutcomeNeedsClarification, report.OutcomeCannotProceed} {
			want, err := registry.ResolveReportTransition(stage, outcome)
			if err != nil {
				t.Fatalf("ResolveReportTransition(%q, %q) error = %v", stage, outcome, err)
			}
			for _, route := range []workflow.Route{workflow.RouteDefault, workflow.RouteAcceptance, workflow.RouteDesignAcceptance} {
				got, err := registry.ResolveRouteReportTransition(route, stage, outcome)
				if err != nil || got != want {
					t.Fatalf("route %q at %q/%q = %#v, %v; want %#v", route, stage, outcome, got, err, want)
				}
			}
		}
	}
}

// TestResolveRouteReportTransitionRejectsUndeclaredRoutes verifies an
// unrecognized route value cannot resolve a transition at all.
func TestResolveRouteReportTransitionRejectsUndeclaredRoutes(t *testing.T) {
	if _, err := workflow.DefaultRegistry().ResolveRouteReportTransition("invented", store.StageArchitecture, report.OutcomeCompleted); err == nil {
		t.Fatal("ResolveRouteReportTransition(invented) error = nil, want rejection")
	}
}
