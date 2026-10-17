package workflow_test

import (
	"testing"

	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// TestSessionRolesDeclareReadOnlyIdeationAndPlanning verifies that the
// factory declares the ideation and planning roles with versioned prompts,
// their report contracts, the session states they may start from, and
// read-only capabilities without tracker authority.
func TestSessionRolesDeclareReadOnlyIdeationAndPlanning(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		kind          workflow.SessionRoleKind
		promptVersion string
		startStates   []store.IdeationState
	}{
		{workflow.RoleIdeation, workflow.SessionRoleKindIdeation, "ideation-v2", []store.IdeationState{store.IdeationStateAwaitingUser}},
		{workflow.RolePlanning, workflow.SessionRoleKindPlanning, "planning-v2", []store.IdeationState{store.IdeationStateAwaitingUser, store.IdeationStateDraftReview}},
	}
	for _, test := range tests {
		definition, ok := workflow.SessionRole(test.name)
		if !ok {
			t.Fatalf("SessionRole(%q) is not declared", test.name)
		}
		if definition.Kind != test.kind || definition.PromptVersion != test.promptVersion || definition.Stage != test.name {
			t.Fatalf("SessionRole(%q) = %#v, want kind %q, prompt %q, and stage %q", test.name, definition, test.kind, test.promptVersion, test.name)
		}
		capabilities := definition.Capabilities
		if capabilities.RepositoryAccess != workflow.RepositoryAccessReadOnlySnapshot || capabilities.TrackerMutation || capabilities.WorktreeMutation {
			t.Fatalf("SessionRole(%q) capabilities = %#v, want read-only snapshot without mutation authority", test.name, capabilities)
		}
		for _, state := range store.IdeationStates() {
			want := false
			for _, allowed := range test.startStates {
				want = want || allowed == state
			}
			if got := definition.CanStartFrom(state); got != want {
				t.Errorf("SessionRole(%q).CanStartFrom(%s) = %v, want %v", test.name, state, got, want)
			}
		}
	}
	if len(workflow.SessionRoles()) != 2 {
		t.Fatalf("SessionRoles() = %#v, want exactly ideation and planning", workflow.SessionRoles())
	}
}

// TestSessionRolesStayOutOfTheRunRegistry verifies that a session role cannot
// be selected for, or started from, any run stage: sessions are not runs.
func TestSessionRolesStayOutOfTheRunRegistry(t *testing.T) {
	t.Parallel()

	registry := workflow.DefaultRegistry()
	for _, definition := range workflow.SessionRoles() {
		if _, ok := registry.Role(definition.Name); ok {
			t.Fatalf("run registry declares session role %q", definition.Name)
		}
		if _, ok := workflow.SessionRole(definition.Name); !ok || !workflow.IsDeclaredRole(definition.Name) {
			t.Fatalf("IsDeclaredRole(%q) = false, want a declared factory role", definition.Name)
		}
	}
	if !workflow.IsDeclaredRole(workflow.RoleImplementation) || workflow.IsDeclaredRole("grilling") {
		t.Fatal("IsDeclaredRole() must accept run roles and refuse undeclared names")
	}
	if _, ok := workflow.SessionRole(workflow.RoleImplementation); ok {
		t.Fatal("SessionRole() must not return a run role")
	}
}
