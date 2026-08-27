// Package workflow contains factory-owned role and stage declarations.
// Repository configuration may select policy for a declared role, but it
// cannot add or redefine any of these workflow authorities.
package workflow

import (
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
)

const (
	// RoleTest identifies the mandatory verified-test role.
	RoleTest = "test"
	// RoleImplementation identifies the production implementation role.
	RoleImplementation = "implementation"
	// RoleArchitecture identifies the document-producing architecture role.
	RoleArchitecture = "architecture"
	// RoleSpecificationReview identifies the independent specification reviewer.
	RoleSpecificationReview = "spec_review"
	// RoleStandardsReview identifies the legacy standards-review policy role.
	RoleStandardsReview = "standards_review"

	// PromptVersionTest identifies the immutable test-role prompt.
	PromptVersionTest = "test-v1"
	// PromptVersionImplementation identifies the immutable implementation prompt.
	PromptVersionImplementation = "implementation-v1"
	// PromptVersionArchitecture identifies the immutable architecture prompt.
	PromptVersionArchitecture = "architecture-v1"
	// PromptVersionSpecificationReview identifies the immutable review prompt.
	PromptVersionSpecificationReview = "specification-review-v1"
	// PromptVersionStandardsReview identifies the immutable standards-review prompt.
	PromptVersionStandardsReview = "standards-review-v1"

	// StageArchitecture identifies the optional architecture invocation stage.
	StageArchitecture store.Stage = store.StageArchitecture
	// StageStandardsReview identifies the legacy standards-review invocation stage.
	StageStandardsReview store.Stage = "standards_review"
)

const (
	// RoleKindHandoff is a role that produces the common completed handoff.
	RoleKindHandoff RoleKind = "handoff"
	// RoleKindTest is a role that produces verified red-test evidence.
	RoleKindTest RoleKind = "test"
	// RoleKindReview is a role that produces exact-checkpoint findings.
	RoleKindReview RoleKind = "review"
)

// RoleKind identifies the structured report contract a role uses.
type RoleKind string

const (
	// SurfaceImplementation selects the run workspace's implementation surface.
	SurfaceImplementation SurfaceKind = "implementation"
	// SurfaceChecks selects the run workspace's deterministic-check surface.
	SurfaceChecks SurfaceKind = "checks"
	// SurfaceRole creates a fresh surface named for the role.
	SurfaceRole SurfaceKind = "role"
)

// SurfaceKind identifies how a role receives its visible terminal surface.
type SurfaceKind string

// RoleDefinition is the complete factory-owned declaration for one role.
type RoleDefinition struct {
	// Name is the stable role identity recorded in packets and reports.
	Name string
	// Stage is the invocation stage owned by the role.
	Stage store.Stage
	// PromptVersion is the versioned factory prompt identity.
	PromptVersion string
	// DefaultPermittedPaths are the coordinator-owned handoff path prefixes.
	DefaultPermittedPaths []string
	// Kind selects the structured report contract.
	Kind RoleKind
	// Surface selects the visible terminal surface strategy.
	Surface SurfaceKind
	// StartStages lists run stages from which this role may be explicitly started.
	StartStages []store.Stage
	// RunStages lists run stages for which this role is the automatic selection.
	RunStages []store.Stage
	// RequiresTestHandoff reports whether the role may start only after the
	// required test-stage handoff has been accepted.
	RequiresTestHandoff bool
	// PromptInstructions contains role-specific instructions surrounded by the
	// common factory-owned safety and reporting rules.
	PromptInstructions string
}

// StageTransition is the coordinator-owned result of accepting one report
// outcome at a declared stage.
type StageTransition struct {
	// Stage is the next persisted workflow stage.
	Stage store.Stage
	// Status is the next persisted orthogonal run status.
	Status store.Status
}

// StageEvent identifies a coordinator-owned non-report boundary that advances
// a run through the declared stage graph.
type StageEvent string

const (
	// StageEventCheckpoint advances an implementation checkpoint into checks.
	StageEventCheckpoint StageEvent = "checkpoint"
	// StageEventDraftPullRequest advances successful checks into draft PR work.
	StageEventDraftPullRequest StageEvent = "draft_pull_request"
)

// StageDefinition declares one stage and its coordinator transitions.
type StageDefinition struct {
	// Name is the stable persisted stage identity.
	Name store.Stage
	// AgentRole identifies the role that owns this invocation stage, when any.
	AgentRole string
	// Transitions maps every structured report outcome to a coordinator result.
	Transitions map[report.Outcome]StageTransition
	// Events maps non-report coordinator boundaries to a stage transition.
	Events map[StageEvent]StageTransition
}

// Registry is an immutable-in-use collection of factory-owned role and stage
// declarations. New instances defensively copy caller-owned maps and slices.
type Registry struct {
	roles      map[string]RoleDefinition
	roleOrder  []string
	stages     map[store.Stage]StageDefinition
	stageOrder []store.Stage
}

// NewRegistry validates and constructs a role/stage registry. It is intended
// for factory-owned declarations and tests of the extensibility seam.
func NewRegistry(roles []RoleDefinition, stages []StageDefinition) (Registry, error) {
	registry := Registry{
		roles:      make(map[string]RoleDefinition, len(roles)),
		roleOrder:  make([]string, 0, len(roles)),
		stages:     make(map[store.Stage]StageDefinition, len(stages)),
		stageOrder: make([]store.Stage, 0, len(stages)),
	}
	for index, definition := range stages {
		if err := validateStageDefinition(index, definition); err != nil {
			return Registry{}, err
		}
		if _, exists := registry.stages[definition.Name]; exists {
			return Registry{}, fmt.Errorf("workflow stage %q is declared more than once", definition.Name)
		}
		definition.Transitions = cloneTransitions(definition.Transitions)
		definition.Events = cloneEventTransitions(definition.Events)
		registry.stages[definition.Name] = definition
		registry.stageOrder = append(registry.stageOrder, definition.Name)
	}
	for index, definition := range roles {
		if err := validateRoleDefinition(index, definition, registry); err != nil {
			return Registry{}, err
		}
		if _, exists := registry.roles[definition.Name]; exists {
			return Registry{}, fmt.Errorf("workflow role %q is declared more than once", definition.Name)
		}
		definition.DefaultPermittedPaths = cloneStrings(definition.DefaultPermittedPaths)
		definition.StartStages = cloneStages(definition.StartStages)
		definition.RunStages = cloneStages(definition.RunStages)
		registry.roles[definition.Name] = definition
		registry.roleOrder = append(registry.roleOrder, definition.Name)
	}
	for _, definition := range registry.roles {
		stage, exists := registry.stages[definition.Stage]
		if !exists {
			return Registry{}, fmt.Errorf("workflow role %q references undeclared invocation stage %q", definition.Name, definition.Stage)
		}
		if stage.AgentRole != definition.Name {
			return Registry{}, fmt.Errorf("workflow stage %q does not name role %q as its owner", definition.Stage, definition.Name)
		}
	}
	for _, definition := range registry.stages {
		for outcome, transition := range definition.Transitions {
			if _, exists := registry.stages[transition.Stage]; !exists {
				return Registry{}, fmt.Errorf("workflow stage %q outcome %q transitions to undeclared stage %q", definition.Name, outcome, transition.Stage)
			}
		}
		for event, transition := range definition.Events {
			if _, exists := registry.stages[transition.Stage]; !exists {
				return Registry{}, fmt.Errorf("workflow stage %q event %q transitions to undeclared stage %q", definition.Name, event, transition.Stage)
			}
		}
	}
	return registry, nil
}

// DefaultRegistry returns the immutable factory-owned role and stage graph.
func DefaultRegistry() Registry {
	roles := []RoleDefinition{
		{
			Name:                  RoleTest,
			Stage:                 store.StageTest,
			PromptVersion:         PromptVersionTest,
			DefaultPermittedPaths: []string{"."},
			Kind:                  RoleKindTest,
			Surface:               SurfaceChecks,
			StartStages:           []store.Stage{store.StageTest},
			RunStages:             []store.Stage{store.StageTest},
		},
		{
			Name:                  RoleImplementation,
			Stage:                 store.StageImplementation,
			PromptVersion:         PromptVersionImplementation,
			DefaultPermittedPaths: []string{"."},
			Kind:                  RoleKindHandoff,
			Surface:               SurfaceImplementation,
			StartStages:           []store.Stage{store.StageClaim, store.StageImplementation},
			RunStages:             []store.Stage{store.StageClaim, store.StageImplementation},
			RequiresTestHandoff:   true,
		},
		{
			Name:                  RoleArchitecture,
			Stage:                 store.StageArchitecture,
			PromptVersion:         PromptVersionArchitecture,
			DefaultPermittedPaths: []string{"docs/architecture"},
			Kind:                  RoleKindHandoff,
			Surface:               SurfaceRole,
			StartStages:           []store.Stage{store.StageArchitecture, store.StageImplementation},
			RunStages:             []store.Stage{store.StageArchitecture},
			PromptInstructions: `Architecture-role ownership:
- Produce a concise design document under the permitted architecture path.
- Describe the proposed boundaries, invariants, and observable acceptance evidence.
- Do not implement production behavior or edit tests unless the frozen specification explicitly requires the document itself.
- List the design document in the completed handoff's changed-file field.
`,
		},
		{
			Name:                  RoleSpecificationReview,
			Stage:                 store.StageReview,
			PromptVersion:         PromptVersionSpecificationReview,
			DefaultPermittedPaths: []string{"."},
			Kind:                  RoleKindReview,
			Surface:               SurfaceRole,
			StartStages:           []store.Stage{store.StageDraftPR},
			RunStages:             []store.Stage{store.StageDraftPR},
		},
		{
			Name:                  RoleStandardsReview,
			Stage:                 StageStandardsReview,
			PromptVersion:         PromptVersionStandardsReview,
			DefaultPermittedPaths: []string{"."},
			Kind:                  RoleKindReview,
			Surface:               SurfaceRole,
			StartStages:           []store.Stage{store.StageDraftPR},
			RunStages:             []store.Stage{StageStandardsReview},
		},
	}

	stages := []StageDefinition{
		{Name: store.StageClaim},
		{Name: store.StagePreflight},
		{
			Name:      store.StageTest,
			AgentRole: RoleTest,
			Transitions: reportTransitions(
				StageTransition{Stage: store.StageImplementation, Status: store.StatusActive},
				StageTransition{Stage: store.StageTest, Status: store.StatusWaitingForHuman},
				StageTransition{Stage: store.StageTest, Status: store.StatusFailed},
			),
		},
		{
			Name:      store.StageImplementation,
			AgentRole: RoleImplementation,
			Events: map[StageEvent]StageTransition{
				StageEventCheckpoint: {Stage: store.StageCheck, Status: store.StatusActive},
			},
			Transitions: reportTransitions(
				StageTransition{Stage: store.StageImplementation, Status: store.StatusActive},
				StageTransition{Stage: store.StageImplementation, Status: store.StatusWaitingForHuman},
				StageTransition{Stage: store.StageImplementation, Status: store.StatusFailed},
			),
		},
		{
			Name: store.StageCheck,
			Events: map[StageEvent]StageTransition{
				StageEventDraftPullRequest: {Stage: store.StageDraftPR, Status: store.StatusActive},
			},
		},
		{Name: store.StageDraftPR},
		{
			Name:      store.StageReview,
			AgentRole: RoleSpecificationReview,
			Transitions: reportTransitions(
				StageTransition{Stage: store.StageReady, Status: store.StatusActive},
				StageTransition{Stage: store.StageReview, Status: store.StatusWaitingForHuman},
				StageTransition{Stage: store.StageReview, Status: store.StatusWaitingForHuman},
			),
		},
		{
			Name:      StageStandardsReview,
			AgentRole: RoleStandardsReview,
			Transitions: reportTransitions(
				StageTransition{Stage: store.StageReady, Status: store.StatusActive},
				StageTransition{Stage: StageStandardsReview, Status: store.StatusWaitingForHuman},
				StageTransition{Stage: StageStandardsReview, Status: store.StatusWaitingForHuman},
			),
		},
		{Name: store.StageReady},
		{
			Name:      store.StageArchitecture,
			AgentRole: RoleArchitecture,
			Transitions: reportTransitions(
				StageTransition{Stage: store.StageImplementation, Status: store.StatusActive},
				StageTransition{Stage: store.StageArchitecture, Status: store.StatusWaitingForHuman},
				StageTransition{Stage: store.StageArchitecture, Status: store.StatusFailed},
			),
		},
	}
	registry, err := NewRegistry(roles, stages)
	if err != nil {
		panic(fmt.Sprintf("invalid default workflow registry: %v", err))
	}
	return registry
}

// Role returns one declared role and a defensive copy of its collections.
func (r Registry) Role(name string) (RoleDefinition, bool) {
	definition, ok := r.roles[name]
	if !ok {
		return RoleDefinition{}, false
	}
	definition.DefaultPermittedPaths = cloneStrings(definition.DefaultPermittedPaths)
	definition.StartStages = cloneStages(definition.StartStages)
	definition.RunStages = cloneStages(definition.RunStages)
	return definition, true
}

// Roles returns every role in factory declaration order.
func (r Registry) Roles() []RoleDefinition {
	roles := make([]RoleDefinition, 0, len(r.roleOrder))
	for _, name := range r.roleOrder {
		role, _ := r.Role(name)
		roles = append(roles, role)
	}
	return roles
}

// Stage returns one declared stage and a defensive copy of its transitions.
func (r Registry) Stage(name store.Stage) (StageDefinition, bool) {
	definition, ok := r.stages[name]
	if !ok {
		return StageDefinition{}, false
	}
	definition.Transitions = cloneTransitions(definition.Transitions)
	definition.Events = cloneEventTransitions(definition.Events)
	return definition, true
}

// Stages returns every stage in factory declaration order.
func (r Registry) Stages() []StageDefinition {
	stages := make([]StageDefinition, 0, len(r.stageOrder))
	for _, name := range r.stageOrder {
		stage, _ := r.Stage(name)
		stages = append(stages, stage)
	}
	return stages
}

// RoleForInvocationStage finds the role whose declaration owns an invocation
// stage. It is the registry-backed replacement for role/stage conditionals.
func (r Registry) RoleForInvocationStage(stage store.Stage) (RoleDefinition, bool) {
	definition, ok := r.Stage(stage)
	if !ok || definition.AgentRole == "" {
		return RoleDefinition{}, false
	}
	return r.Role(definition.AgentRole)
}

// RoleForRunStage finds the automatic role for a persisted run stage.
func (r Registry) RoleForRunStage(stage store.Stage) (RoleDefinition, bool) {
	for _, name := range r.roleOrder {
		definition, _ := r.Role(name)
		if containsStage(definition.RunStages, stage) {
			return definition, true
		}
	}
	return RoleDefinition{}, false
}

// CanStartFrom reports whether a declared role may be explicitly launched
// while the run is at the supplied stage.
func (r Registry) CanStartFrom(role string, runStage store.Stage) bool {
	definition, ok := r.Role(role)
	return ok && containsStage(definition.StartStages, runStage)
}

// ResolveReportTransition resolves a validated report outcome through the
// declaration for the invocation stage.
func (r Registry) ResolveReportTransition(stage store.Stage, outcome report.Outcome) (StageTransition, error) {
	definition, ok := r.Stage(stage)
	if !ok {
		return StageTransition{}, fmt.Errorf("workflow stage %q is not declared", stage)
	}
	transition, ok := definition.Transitions[outcome]
	if !ok {
		return StageTransition{}, fmt.Errorf("workflow stage %q has no transition for report outcome %q", stage, outcome)
	}
	return transition, nil
}

// ResolveEventTransition resolves a coordinator boundary through the
// declaration for the current stage.
func (r Registry) ResolveEventTransition(stage store.Stage, event StageEvent) (StageTransition, error) {
	definition, ok := r.Stage(stage)
	if !ok {
		return StageTransition{}, fmt.Errorf("workflow stage %q is not declared", stage)
	}
	transition, ok := definition.Events[event]
	if !ok {
		return StageTransition{}, fmt.Errorf("workflow stage %q has no transition for coordinator event %q", stage, event)
	}
	return transition, nil
}

// validateRoleDefinition checks the role-owned declaration before it enters a
// registry, including all stage references and prompt identity metadata.
func validateRoleDefinition(index int, definition RoleDefinition, registry Registry) error {
	if strings.TrimSpace(definition.Name) == "" || strings.ContainsAny(definition.Name, "\x00\r\n \t") {
		return fmt.Errorf("workflow role %d name must be a nonempty single token", index)
	}
	if strings.TrimSpace(string(definition.Stage)) == "" {
		return fmt.Errorf("workflow role %q stage is required", definition.Name)
	}
	if strings.TrimSpace(definition.PromptVersion) == "" || strings.ContainsAny(definition.PromptVersion, "\x00\r\n \t") {
		return fmt.Errorf("workflow role %q prompt version must be a nonempty single token", definition.Name)
	}
	switch definition.Kind {
	case RoleKindHandoff, RoleKindTest, RoleKindReview:
	default:
		return fmt.Errorf("workflow role %q has unsupported report kind %q", definition.Name, definition.Kind)
	}
	switch definition.Surface {
	case SurfaceImplementation, SurfaceChecks, SurfaceRole:
	default:
		return fmt.Errorf("workflow role %q has unsupported surface kind %q", definition.Name, definition.Surface)
	}
	if len(definition.DefaultPermittedPaths) == 0 {
		return fmt.Errorf("workflow role %q must declare default permitted paths", definition.Name)
	}
	if len(definition.StartStages) == 0 || len(definition.RunStages) == 0 {
		return fmt.Errorf("workflow role %q must declare start and automatic run stages", definition.Name)
	}
	for _, stage := range append(cloneStages(definition.StartStages), definition.RunStages...) {
		if _, ok := registry.stages[stage]; !ok {
			return fmt.Errorf("workflow role %q references undeclared run stage %q", definition.Name, stage)
		}
	}
	return nil
}

// validateStageDefinition checks one stage declaration and its report keys.
func validateStageDefinition(index int, definition StageDefinition) error {
	if strings.TrimSpace(string(definition.Name)) == "" || strings.ContainsAny(string(definition.Name), "\x00\r\n \t") {
		return fmt.Errorf("workflow stage %d name must be a nonempty single token", index)
	}
	if definition.AgentRole != "" && strings.ContainsAny(definition.AgentRole, "\x00\r\n \t") {
		return fmt.Errorf("workflow stage %q agent role must be a single token", definition.Name)
	}
	for outcome, transition := range definition.Transitions {
		switch outcome {
		case report.OutcomeCompleted, report.OutcomeNeedsClarification, report.OutcomeCannotProceed:
		default:
			return fmt.Errorf("workflow stage %q has unsupported report outcome %q", definition.Name, outcome)
		}
		if transition.Stage == "" || transition.Status == "" {
			return fmt.Errorf("workflow stage %q outcome %q must declare a stage and status", definition.Name, outcome)
		}
	}
	for event, transition := range definition.Events {
		if strings.TrimSpace(string(event)) == "" || strings.ContainsAny(string(event), "\x00\r\n \t") {
			return fmt.Errorf("workflow stage %q event must be a nonempty single token", definition.Name)
		}
		if transition.Stage == "" || transition.Status == "" {
			return fmt.Errorf("workflow stage %q event %q must declare a stage and status", definition.Name, event)
		}
	}
	return nil
}

// reportTransitions constructs the standard outcome-keyed transition map in
// the order used by every role declaration.
func reportTransitions(completed, clarification, cannotProceed StageTransition) map[report.Outcome]StageTransition {
	return map[report.Outcome]StageTransition{
		report.OutcomeCompleted:          completed,
		report.OutcomeNeedsClarification: clarification,
		report.OutcomeCannotProceed:      cannotProceed,
	}
}

// containsStage reports whether a stage appears in a declaration list.
func containsStage(values []store.Stage, wanted store.Stage) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// cloneStrings returns a copy of a string declaration list.
func cloneStrings(values []string) []string {
	return append([]string(nil), values...)
}

// cloneStages returns a copy of a stage declaration list.
func cloneStages(values []store.Stage) []store.Stage {
	return append([]store.Stage(nil), values...)
}

// cloneTransitions returns a copy of a stage transition map.
func cloneTransitions(values map[report.Outcome]StageTransition) map[report.Outcome]StageTransition {
	if values == nil {
		return nil
	}
	copyValues := make(map[report.Outcome]StageTransition, len(values))
	for key, value := range values {
		copyValues[key] = value
	}
	return copyValues
}

// cloneEventTransitions returns a copy of a coordinator-event transition map.
func cloneEventTransitions(values map[StageEvent]StageTransition) map[StageEvent]StageTransition {
	if values == nil {
		return nil
	}
	copyValues := make(map[StageEvent]StageTransition, len(values))
	for key, value := range values {
		copyValues[key] = value
	}
	return copyValues
}
