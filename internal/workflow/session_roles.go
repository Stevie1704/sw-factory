package workflow

import "github.com/Stevie1704/sw-factory/internal/store"

const (
	// RoleIdeation identifies the conversational role that asks questions and
	// proposes brief updates for an ideation session.
	RoleIdeation = "ideation"
	// RolePlanning identifies the role that turns a frozen brief into a draft
	// plan graph for an ideation session.
	RolePlanning = "planning"

	// PromptVersionIdeation identifies the immutable ideation prompt.
	PromptVersionIdeation = "ideation-v1"
	// PromptVersionPlanning identifies the immutable planning prompt.
	PromptVersionPlanning = "planning-v1"
)

// SessionRoleKind identifies the structured report contract of a session role.
type SessionRoleKind string

const (
	// SessionRoleKindIdeation reports questions, a brief update, and an
	// optional readiness proposal.
	SessionRoleKindIdeation SessionRoleKind = "ideation"
	// SessionRoleKindPlanning reports a draft plan graph.
	SessionRoleKindPlanning SessionRoleKind = "planning"
)

// RepositoryAccess identifies how a role may see the repository.
type RepositoryAccess string

const (
	// RepositoryAccessReadOnlySnapshot gives the role a read-only checkout of
	// one recorded commit. The role never sees the user's working tree.
	RepositoryAccessReadOnlySnapshot RepositoryAccess = "read_only_snapshot"
)

// SessionRoleCapabilities is the explicit authority of a session role. The
// coordinator enforces it; a prompt only describes it.
type SessionRoleCapabilities struct {
	// RepositoryAccess is the only repository view the role receives.
	RepositoryAccess RepositoryAccess
	// WorktreeMutation reports whether the role may change a checkout.
	WorktreeMutation bool
	// TrackerMutation reports whether the role may change tracker content.
	TrackerMutation bool
}

// SessionRoleDefinition is the factory-owned declaration of one role that
// serves an ideation session. Session roles are not run roles: no run stage
// selects them and repositories cannot redefine them (ADR 0003, ADR 0023).
type SessionRoleDefinition struct {
	// Name is the stable role identity recorded in packets and reports.
	Name string
	// Stage is the invocation stage label recorded in packets and reports.
	Stage string
	// PromptVersion is the versioned factory prompt identity.
	PromptVersion string
	// Kind selects the structured report contract.
	Kind SessionRoleKind
	// StartStates lists the session states from which the coordinator may
	// start the role.
	StartStates []store.IdeationState
	// Capabilities is the explicit authority of the role.
	Capabilities SessionRoleCapabilities
}

// CanStartFrom reports whether the coordinator may start the role while the
// session is in state.
func (d SessionRoleDefinition) CanStartFrom(state store.IdeationState) bool {
	for _, allowed := range d.StartStates {
		if allowed == state {
			return true
		}
	}
	return false
}

// readOnlySessionCapabilities is the authority every current session role has.
var readOnlySessionCapabilities = SessionRoleCapabilities{RepositoryAccess: RepositoryAccessReadOnlySnapshot}

// SessionRoles returns every factory-owned session role in a stable order.
func SessionRoles() []SessionRoleDefinition {
	return []SessionRoleDefinition{
		{
			Name:          RoleIdeation,
			Stage:         RoleIdeation,
			PromptVersion: PromptVersionIdeation,
			Kind:          SessionRoleKindIdeation,
			StartStates:   []store.IdeationState{store.IdeationStateAwaitingUser},
			Capabilities:  readOnlySessionCapabilities,
		},
		{
			Name:          RolePlanning,
			Stage:         RolePlanning,
			PromptVersion: PromptVersionPlanning,
			Kind:          SessionRoleKindPlanning,
			StartStates:   []store.IdeationState{store.IdeationStateAwaitingUser, store.IdeationStateDraftReview},
			Capabilities:  readOnlySessionCapabilities,
		},
	}
}

// SessionRole returns the declaration of one session role.
func SessionRole(name string) (SessionRoleDefinition, bool) {
	for _, definition := range SessionRoles() {
		if definition.Name == name {
			return definition, true
		}
	}
	return SessionRoleDefinition{}, false
}

// IsDeclaredRole reports whether name is a factory-owned run role or session
// role. Repository policy may select harness and model only for such a role.
func IsDeclaredRole(name string) bool {
	if _, ok := DefaultRegistry().Role(name); ok {
		return true
	}
	_, ok := SessionRole(name)
	return ok
}
