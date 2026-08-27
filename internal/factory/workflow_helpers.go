package factory

import (
	"fmt"

	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// roleHandoffForRun returns the role-neutral handoff while accepting rows and
// fixtures that still populate the legacy implementation field.
func roleHandoffForRun(run store.Run) *store.RoleHandoff {
	if run.RoleHandoff != nil {
		return run.RoleHandoff
	}
	return run.ImplementationHandoff
}

// roleDefinitionForInvocation resolves the factory declaration attached to a
// persisted invocation and verifies that its role owns its stage.
func roleDefinitionForInvocation(invocation store.Invocation) (workflow.RoleDefinition, error) {
	definition, exists := workflow.DefaultRegistry().Role(invocation.Role)
	if !exists {
		return workflow.RoleDefinition{}, fmt.Errorf("invocation role %q is not declared by the workflow registry", invocation.Role)
	}
	if definition.Stage != invocation.Stage {
		return workflow.RoleDefinition{}, fmt.Errorf("invocation role %q does not own stage %q", invocation.Role, invocation.Stage)
	}
	return definition, nil
}

// roleIsKind reports whether an invocation belongs to a declared report kind.
func roleIsKind(invocation store.Invocation, wanted workflow.RoleKind) bool {
	definition, err := roleDefinitionForInvocation(invocation)
	return err == nil && definition.Kind == wanted
}

// invocationSurface returns the role-owned terminal surface, falling back to
// the legacy implementation field for rows created before schema 25.
func invocationSurface(invocation store.Invocation) terminal.Surface {
	id := invocation.RoleSurfaceID
	if id == "" {
		id = invocation.ImplementationSurfaceID
	}
	return terminal.Surface{
		ID:          terminal.SurfaceID(id),
		WorkspaceID: terminal.WorkspaceID(invocation.WorkspaceID),
		Name:        invocation.Role,
	}
}

// setInvocationSurface records a role-owned surface while retaining the
// legacy implementation column as a compatibility projection.
func setInvocationSurface(invocation *store.Invocation, surface terminal.Surface) {
	if invocation == nil {
		return
	}
	invocation.RoleSurfaceID = string(surface.ID)
	invocation.ImplementationSurfaceID = string(surface.ID)
}
