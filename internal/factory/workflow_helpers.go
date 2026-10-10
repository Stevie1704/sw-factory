package factory

import (
	"fmt"

	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

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

// invocationReadOnly reports whether a persisted invocation belongs to a role
// that must not change the run worktree.
func invocationReadOnly(invocation store.Invocation) bool {
	definition, err := roleDefinitionForInvocation(invocation)
	return err == nil && definition.ReadOnly()
}

// workerIDForInvocation gives each review invocation its own worker,
// role-home volume, and temporary filesystem while retaining the historical
// run-scoped worker for ordinary roles.
func workerIDForInvocation(invocation store.Invocation) string {
	if invocationReadOnly(invocation) {
		return invocation.RunID + "-" + invocation.ID
	}
	return invocation.RunID
}
