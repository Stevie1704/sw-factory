package factory

import (
	"errors"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// TestParseIssueRouteAcceptsAtMostOneDeclaredMarker verifies the frozen issue
// grammar admits one bounded factory-owned route marker and rejects every
// other shape with a typed policy code.
func TestParseIssueRouteAcceptsAtMostOneDeclaredMarker(t *testing.T) {
	tests := []struct {
		name string
		body string
		want workflow.Route
		code PolicyRejectionCode
	}{
		{name: "absent marker", body: "plain issue prose", want: workflow.RouteDefault},
		{name: "acceptance", body: "intro\n<!-- factory-route: acceptance -->\n", want: workflow.RouteAcceptance},
		{name: "design acceptance", body: "<!-- factory-route: design-acceptance -->", want: workflow.RouteDesignAcceptance},
		{name: "mixed case marker", body: "<!-- Factory-Route: Acceptance -->", want: workflow.RouteAcceptance},
		{name: "prose naming a route", body: "use the acceptance route for this change", want: workflow.RouteDefault},
		{name: "unknown value", body: "<!-- factory-route: architecture -->", code: PolicyRejectionRouteInvalid},
		{name: "empty value", body: "<!-- factory-route: -->", code: PolicyRejectionRouteInvalid},
		{name: "unterminated marker", body: "<!-- factory-route: acceptance", code: PolicyRejectionRouteInvalid},
		{name: "duplicate identical markers", body: "<!-- factory-route: acceptance -->\n<!-- factory-route: acceptance -->", code: PolicyRejectionRouteDuplicate},
		{name: "nested second marker", body: "<!-- factory-route: <!-- factory-route: acceptance --> -->", code: PolicyRejectionRouteDuplicate},
		{name: "duplicate before the terminator", body: "<!-- factory-route: acceptance <!-- factory-route: design-acceptance -->", code: PolicyRejectionRouteDuplicate},
		{name: "duplicate conflicting markers", body: "<!-- factory-route: acceptance -->\n<!-- factory-route: design-acceptance -->", code: PolicyRejectionRouteDuplicate},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			route, err := parseIssueRoute(test.body)
			if test.code == "" {
				if err != nil || route != test.want {
					t.Fatalf("parseIssueRoute() = %q, %v; want %q, nil", route, err, test.want)
				}
				return
			}
			var rejection *PolicyRejection
			if !errors.As(err, &rejection) {
				t.Fatalf("parseIssueRoute() error = %v, want typed policy rejection", err)
			}
			if rejection.Code != test.code {
				t.Fatalf("policy rejection code = %q, want %q", rejection.Code, test.code)
			}
			if strings.TrimSpace(rejection.Problem) == "" {
				t.Fatal("policy rejection problem must be actionable")
			}
		})
	}
}

// TestResolveClaimRouteRequiresTheRolesTheRouteRuns verifies a route cannot be
// claimed against a repository that declares no harness or model for the roles
// the route invokes.
func TestResolveClaimRouteRequiresTheRolesTheRouteRuns(t *testing.T) {
	advisory := config.RepositoryConfig{
		TestPolicy:          config.TestPolicy{Mode: config.TestModeAdvisory},
		RoleHarnessDefaults: map[string]config.Harness{workflow.RoleImplementation: config.HarnessCodex},
		ModelOptions:        map[string][]string{workflow.RoleImplementation: {"model"}},
	}
	complete := config.RepositoryConfig{
		TestPolicy: config.TestPolicy{Mode: config.TestModeAdvisory},
		RoleHarnessDefaults: map[string]config.Harness{
			workflow.RoleImplementation: config.HarnessCodex,
			workflow.RoleTest:           config.HarnessCodex,
			workflow.RoleArchitecture:   config.HarnessCodex,
		},
		ModelOptions: map[string][]string{
			workflow.RoleImplementation: {"model"},
			workflow.RoleTest:           {"model"},
			workflow.RoleArchitecture:   {"model"},
		},
	}

	tests := []struct {
		name       string
		body       string
		repository config.RepositoryConfig
		want       workflow.Route
		code       PolicyRejectionCode
	}{
		{name: "no route needs no extra role", body: "prose", repository: advisory, want: workflow.RouteDefault},
		{name: "acceptance without test role", body: "<!-- factory-route: acceptance -->", repository: advisory, code: PolicyRejectionRouteUnavailable},
		{name: "design acceptance without roles", body: "<!-- factory-route: design-acceptance -->", repository: advisory, code: PolicyRejectionRouteUnavailable},
		{name: "acceptance with test role", body: "<!-- factory-route: acceptance -->", repository: complete, want: workflow.RouteAcceptance},
		{name: "design acceptance with roles", body: "<!-- factory-route: design-acceptance -->", repository: complete, want: workflow.RouteDesignAcceptance},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			route, err := resolveClaimRoute(test.body, test.repository)
			if test.code == "" {
				if err != nil || route != test.want {
					t.Fatalf("resolveClaimRoute() = %q, %v; want %q, nil", route, err, test.want)
				}
				return
			}
			var rejection *PolicyRejection
			if !errors.As(err, &rejection) || rejection.Code != test.code {
				t.Fatalf("resolveClaimRoute() error = %v, want %q rejection", err, test.code)
			}
		})
	}
}

// TestAgentReportProjectionFailsClosedOnAnUnreadablePacket verifies a corrupt
// frozen packet cannot silently downgrade a selected route by resolving the
// default route's transition.
func TestAgentReportProjectionFailsClosedOnAnUnreadablePacket(t *testing.T) {
	previous := store.Run{Stage: store.StageArchitecture, Status: store.StatusActive, SpecificationPacket: "{not json"}
	value := report.Report{Outcome: report.OutcomeCompleted, Handoff: &report.Handoff{ChangeSummary: "design"}}

	next := agentReportRunProjection(previous, store.StageArchitecture, value)

	if next.Stage != store.StageArchitecture || next.Status != store.StatusFailed {
		t.Fatalf("projection = stage %q/status %q, want a failed architecture stage", next.Stage, next.Status)
	}
}

// TestRouteProjectionsReportAnUnreadablePacket verifies the visible route
// projections never present the default route for a packet they cannot read.
func TestRouteProjectionsReportAnUnreadablePacket(t *testing.T) {
	run := store.Run{Stage: store.StageArchitecture, SpecificationPacket: "{not json"}

	if route, readable := routeForRun(run); readable || route != workflow.RouteDefault {
		t.Fatalf("routeForRun() = %q, %t; want the zero route and an unreadable result", route, readable)
	}
	if got := routeDescriptionForRun(run); got != "unknown" {
		t.Fatalf("routeDescriptionForRun() = %q, want unknown", got)
	}
	if isCleanStartStage(run) {
		t.Fatal("isCleanStartStage() = true for an unreadable packet, want a fail-closed refusal")
	}
}
