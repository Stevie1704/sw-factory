package workflow

import (
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// Route identifies the factory-owned contract-first stage sequence an
// authorized issue author selects before claim. Route selection is a
// deliberate human decision; it is never inferred from changed files, issue
// prose, or model judgment. Repository guidance and agents can neither add a
// route nor redefine the transitions a route selects.
type Route string

const (
	// RouteDefault means the issue selected no route. The run then follows the
	// frozen repository test policy.
	RouteDefault Route = ""
	// RouteAcceptance runs the independent test role before implementation.
	RouteAcceptance Route = "acceptance"
	// RouteDesignAcceptance runs architecture, then the independent test role,
	// then implementation.
	RouteDesignAcceptance Route = "design-acceptance"
)

// SelectableRoutes returns every route an issue may select, in declaration
// order. The default route is absent because it is the absence of a marker.
func SelectableRoutes() []Route {
	return []Route{RouteAcceptance, RouteDesignAcceptance}
}

// ParseRoute converts one issue-marker value into a declared route. It rejects
// every value outside the bounded factory-owned grammar, including the empty
// value, so an unrecognized marker can never fall back to a default route.
func ParseRoute(value string) (Route, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return RouteDefault, fmt.Errorf("workflow route value must not be empty; declared routes are %s", strings.Join(routeNames(), ", "))
	}
	for _, route := range SelectableRoutes() {
		if strings.EqualFold(trimmed, string(route)) {
			return route, nil
		}
	}
	return RouteDefault, fmt.Errorf("workflow route %q is not declared; declared routes are %s", trimmed, strings.Join(routeNames(), ", "))
}

// Declared reports whether a route value is one the factory declares. The
// default route is declared because an absent marker is a legal selection.
func (r Route) Declared() bool {
	if r == RouteDefault {
		return true
	}
	for _, route := range SelectableRoutes() {
		if r == route {
			return true
		}
	}
	return false
}

// EntryStage returns the stage a healthy baseline enters for a selected route,
// and reports false for the default route, whose entry stage follows the
// frozen repository test policy instead.
func (r Route) EntryStage() (store.Stage, bool) {
	switch r {
	case RouteAcceptance:
		return store.StageTest, true
	case RouteDesignAcceptance:
		return store.StageArchitecture, true
	default:
		return "", false
	}
}

// RequiresIndependentTestStage reports whether the route runs the
// factory-owned test role before implementation, regardless of whether the
// repository test policy would otherwise require it.
func (r Route) RequiresIndependentTestStage() bool {
	return r == RouteAcceptance || r == RouteDesignAcceptance
}

// RequiredRoles lists the factory roles a repository must configure before it
// can claim an issue that selects the route.
func (r Route) RequiredRoles() []string {
	switch r {
	case RouteAcceptance:
		return []string{RoleTest}
	case RouteDesignAcceptance:
		return []string{RoleArchitecture, RoleTest}
	default:
		return nil
	}
}

// Description renders the stable user-facing meaning of a route for status
// projections.
func (r Route) Description() string {
	switch r {
	case RouteDefault:
		return "default (repository test policy)"
	case RouteAcceptance:
		return "acceptance (independent acceptance test before implementation)"
	case RouteDesignAcceptance:
		return "design-acceptance (architecture, then independent acceptance test, then implementation)"
	default:
		return "unknown"
	}
}

// ResolveRouteReportTransition resolves a validated report outcome through the
// declared stage graph, applying the only transition a factory-owned route may
// reshape: the design-acceptance route hands an accepted architecture design
// to the test role instead of directly to implementation.
func (r Registry) ResolveRouteReportTransition(route Route, stage store.Stage, outcome report.Outcome) (StageTransition, error) {
	if !route.Declared() {
		return StageTransition{}, fmt.Errorf("workflow route %q is not declared", route)
	}
	transition, err := r.ResolveReportTransition(stage, outcome)
	if err != nil {
		return StageTransition{}, err
	}
	if route == RouteDesignAcceptance && stage == store.StageArchitecture && outcome == report.OutcomeCompleted {
		return StageTransition{Stage: store.StageTest, Status: store.StatusActive}, nil
	}
	return transition, nil
}

// routeNames renders the selectable route values for typed policy errors.
func routeNames() []string {
	names := make([]string, 0, len(SelectableRoutes()))
	for _, route := range SelectableRoutes() {
		names = append(names, string(route))
	}
	return names
}
