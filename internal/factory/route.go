package factory

import (
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// routeMarkerPrefix identifies the exact frozen issue marker that selects one
// factory-owned contract-first route. Ordinary issue prose cannot select a
// route, and the coordinator never infers one from changed files.
const routeMarkerPrefix = "<!-- factory-route:"

// unreadableRunRoute keeps status projections from presenting a corrupt
// specification packet as the default workflow route.
const unreadableRunRoute workflow.Route = "unknown"

// parseIssueRoute reads the bounded route marker from a frozen issue body. An
// absent marker selects the default route. A duplicate, unterminated, or
// undeclared marker is a typed policy rejection rather than a silent default.
func parseIssueRoute(body string) (workflow.Route, error) {
	start := indexFold(body, routeMarkerPrefix)
	if start < 0 {
		return workflow.RouteDefault, nil
	}
	remainder := body[start+len(routeMarkerPrefix):]
	if indexFold(remainder, routeMarkerPrefix) >= 0 {
		return workflow.RouteDefault, &PolicyRejection{
			Code:    PolicyRejectionRouteDuplicate,
			Problem: "frozen issue declares more than one factory route marker; keep exactly one",
		}
	}
	end := strings.Index(remainder, "-->")
	if end < 0 {
		return workflow.RouteDefault, &PolicyRejection{
			Code:    PolicyRejectionRouteInvalid,
			Problem: fmt.Sprintf("frozen issue route marker is not terminated; write %s <route> -->", routeMarkerPrefix),
		}
	}
	route, err := workflow.ParseRoute(remainder[:end])
	if err != nil {
		return workflow.RouteDefault, &PolicyRejection{
			Code:    PolicyRejectionRouteInvalid,
			Problem: err.Error(),
		}
	}
	return route, nil
}

// resolveClaimRoute freezes the selected route for one claim. It rejects a
// route whose factory roles the repository has not configured, so the refusal
// happens before any agent invocation rather than at launch.
func resolveClaimRoute(body string, repository config.RepositoryConfig) (workflow.Route, error) {
	route, err := parseIssueRoute(body)
	if err != nil {
		return workflow.RouteDefault, err
	}
	for _, role := range route.RequiredRoles() {
		if !roleConfigured(repository, role) {
			return workflow.RouteDefault, &PolicyRejection{
				Code:    PolicyRejectionRouteUnavailable,
				Problem: fmt.Sprintf("route %q needs the %q role; declare role_harness_defaults.%s and model_options.%s", route, role, role, role),
			}
		}
	}
	return route, nil
}

// roleConfigured reports whether repository policy declares both a harness and
// at least one model for one factory-owned role.
func roleConfigured(repository config.RepositoryConfig, role string) bool {
	if repository.RoleHarnessDefaults == nil || repository.ModelOptions == nil {
		return false
	}
	if _, hasHarness := repository.RoleHarnessDefaults[role]; !hasHarness {
		return false
	}
	models, hasModels := repository.ModelOptions[role]
	return hasModels && len(models) > 0
}

// routeForRun returns the route frozen in a run's specification packet and
// reports whether the packet could be read. A caller that would change the
// stage graph must treat a false result as fail-closed rather than as the
// default route, because a corrupt packet would otherwise silently downgrade a
// selected route.
func routeForRun(run store.Run) (workflow.Route, bool) {
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return workflow.RouteDefault, false
	}
	return packet.Route, true
}

// statusRouteForRun preserves an unreadable packet as an explicit unknown
// route in the existing status result field.
func statusRouteForRun(run store.Run) workflow.Route {
	route, readable := routeForRun(run)
	if !readable {
		return unreadableRunRoute
	}
	return route
}

// routeDescriptionForRun renders the stable user-facing meaning of a run's
// frozen route. An unreadable packet reports an unknown route rather than
// presenting the default route as authoritative.
func routeDescriptionForRun(run store.Run) string {
	return statusRouteForRun(run).Description()
}

// designHandoffForInvocation returns the accepted architecture design that the
// design-acceptance route hands to the test role alongside the frozen
// specification packet. Every other role and route receives none.
func designHandoffForInvocation(run store.Run, packet SpecificationPacket, role workflow.RoleDefinition) *store.RoleHandoff {
	if packet.Route != workflow.RouteDesignAcceptance || role.Kind != workflow.RoleKindTest {
		return nil
	}
	return run.RoleHandoff
}
