package workflow_test

import (
	"testing"

	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// TestDefaultRegistryDeclaresArchitectureAsAFactoryOwnedRole verifies the
// non-implementation proof role carries its complete coordinator declaration.
func TestDefaultRegistryDeclaresArchitectureAsAFactoryOwnedRole(t *testing.T) {
	registry := workflow.DefaultRegistry()

	role, ok := registry.Role(workflow.RoleArchitecture)
	if !ok {
		t.Fatal("architecture role is not declared")
	}
	if role.Stage != workflow.StageArchitecture || role.PromptVersion != workflow.PromptVersionArchitecture {
		t.Fatalf("architecture role = %#v, want architecture stage and prompt identity", role)
	}
	if len(role.DefaultPermittedPaths) != 1 || role.DefaultPermittedPaths[0] != "docs/architecture" {
		t.Fatalf("architecture permitted paths = %#v, want docs/architecture", role.DefaultPermittedPaths)
	}
	if role.Surface != workflow.SurfaceRole {
		t.Fatalf("architecture surface = %q, want a role-owned surface", role.Surface)
	}
}

// TestDefaultRegistryResolvesArchitectureReportTransitions verifies a new
// role's report outcomes use the declared stage graph rather than coordinator
// branches dedicated to that role name.
func TestDefaultRegistryResolvesArchitectureReportTransitions(t *testing.T) {
	registry := workflow.DefaultRegistry()

	completed, err := registry.ResolveReportTransition(workflow.StageArchitecture, report.OutcomeCompleted)
	if err != nil {
		t.Fatalf("ResolveReportTransition(completed) error = %v", err)
	}
	if completed.Stage != store.StageImplementation || completed.Status != store.StatusActive {
		t.Fatalf("completed transition = %#v, want implementation/active", completed)
	}

	clarification, err := registry.ResolveReportTransition(workflow.StageArchitecture, report.OutcomeNeedsClarification)
	if err != nil {
		t.Fatalf("ResolveReportTransition(clarification) error = %v", err)
	}
	if clarification.Stage != workflow.StageArchitecture || clarification.Status != store.StatusWaitingForHuman {
		t.Fatalf("clarification transition = %#v, want architecture/waiting_for_human", clarification)
	}
}

// TestDefaultRegistryResolvesCoordinatorStageEvents verifies checkpoint and
// draft-PR boundaries are also declared in the factory-owned stage graph.
func TestDefaultRegistryResolvesCoordinatorStageEvents(t *testing.T) {
	registry := workflow.DefaultRegistry()

	checkpoint, err := registry.ResolveEventTransition(store.StageImplementation, workflow.StageEventCheckpoint)
	if err != nil {
		t.Fatalf("ResolveEventTransition(checkpoint) error = %v", err)
	}
	if checkpoint.Stage != store.StageCheck || checkpoint.Status != store.StatusActive {
		t.Fatalf("checkpoint transition = %#v, want check/active", checkpoint)
	}

	draftPullRequest, err := registry.ResolveEventTransition(store.StageCheck, workflow.StageEventDraftPullRequest)
	if err != nil {
		t.Fatalf("ResolveEventTransition(draft PR) error = %v", err)
	}
	if draftPullRequest.Stage != store.StageDraftPR || draftPullRequest.Status != store.StatusActive {
		t.Fatalf("draft PR transition = %#v, want draft_pr/active", draftPullRequest)
	}
}

// TestRegistryAcceptsAFactoryOwnedCustomRole verifies the registry seam can
// declare another role without changing coordinator control flow.
func TestRegistryAcceptsAFactoryOwnedCustomRole(t *testing.T) {
	const customStage store.Stage = "documentation"
	registry, err := workflow.NewRegistry(
		[]workflow.RoleDefinition{{
			Name:                  "documentation",
			Stage:                 customStage,
			PromptVersion:         "documentation-v1",
			DefaultPermittedPaths: []string{"docs"},
			Kind:                  workflow.RoleKindHandoff,
			Surface:               workflow.SurfaceRole,
			StartStages:           []store.Stage{customStage},
			RunStages:             []store.Stage{customStage},
		}},
		[]workflow.StageDefinition{{
			Name:      customStage,
			AgentRole: "documentation",
			Transitions: map[report.Outcome]workflow.StageTransition{
				report.OutcomeCompleted:          {Stage: store.StageReady, Status: store.StatusActive},
				report.OutcomeNeedsClarification: {Stage: customStage, Status: store.StatusWaitingForHuman},
				report.OutcomeCannotProceed:      {Stage: customStage, Status: store.StatusFailed},
			},
		}, {Name: store.StageReady}},
	)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	role, ok := registry.Role("documentation")
	if !ok || role.Stage != customStage {
		t.Fatalf("custom role = %#v, found=%t", role, ok)
	}
	transition, err := registry.ResolveReportTransition(customStage, report.OutcomeCompleted)
	if err != nil {
		t.Fatalf("ResolveReportTransition(custom) error = %v", err)
	}
	if transition.Stage != store.StageReady {
		t.Fatalf("custom transition = %#v, want ready stage", transition)
	}
}

// TestNewRegistryRejectsInvalidDeclarations verifies malformed factory-owned
// role and stage declarations fail before they can enter the registry.
func TestNewRegistryRejectsInvalidDeclarations(t *testing.T) {
	const stage store.Stage = "stage"

	validRole := func() workflow.RoleDefinition {
		return workflow.RoleDefinition{
			Name:                  "role",
			Stage:                 stage,
			PromptVersion:         "role-v1",
			DefaultPermittedPaths: []string{"docs"},
			Kind:                  workflow.RoleKindHandoff,
			Surface:               workflow.SurfaceRole,
			StartStages:           []store.Stage{stage},
			RunStages:             []store.Stage{stage},
		}
	}
	validStage := func(owner string) workflow.StageDefinition {
		return workflow.StageDefinition{Name: stage, AgentRole: owner}
	}

	tests := []struct {
		name   string
		roles  []workflow.RoleDefinition
		stages []workflow.StageDefinition
	}{
		{
			name:   "duplicate role names",
			roles:  []workflow.RoleDefinition{validRole(), validRole()},
			stages: []workflow.StageDefinition{validStage("role")},
		},
		{
			name: "undeclared stage reference",
			roles: []workflow.RoleDefinition{func() workflow.RoleDefinition {
				role := validRole()
				role.Stage = "missing"
				return role
			}()},
			stages: []workflow.StageDefinition{validStage("role")},
		},
		{
			name: "unsupported report kind",
			roles: []workflow.RoleDefinition{func() workflow.RoleDefinition {
				role := validRole()
				role.Kind = "unsupported"
				return role
			}()},
			stages: []workflow.StageDefinition{validStage("role")},
		},
		{
			name: "unsupported surface",
			roles: []workflow.RoleDefinition{func() workflow.RoleDefinition {
				role := validRole()
				role.Surface = "unsupported"
				return role
			}()},
			stages: []workflow.StageDefinition{validStage("role")},
		},
		{
			name: "empty default permitted paths",
			roles: []workflow.RoleDefinition{func() workflow.RoleDefinition {
				role := validRole()
				role.DefaultPermittedPaths = nil
				return role
			}()},
			stages: []workflow.StageDefinition{validStage("role")},
		},
		{
			name:   "mismatched stage owner",
			roles:  []workflow.RoleDefinition{validRole()},
			stages: []workflow.StageDefinition{validStage("other")},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := workflow.NewRegistry(test.roles, test.stages); err == nil {
				t.Fatal("NewRegistry() error = nil, want invalid-declaration error")
			}
		})
	}
}
