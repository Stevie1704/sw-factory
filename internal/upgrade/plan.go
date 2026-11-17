// Package upgrade derives a reviewable upgrade plan from a read-only
// observation of one installation. The plan describes work; it does not do
// it. Each operation is done by its own supported command, and a later plan
// observes the result again, so an agent that starts after an interruption
// can tell which component is old, new, or incomplete and how to continue.
package upgrade

import (
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/onboarding"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// PlanSchema is the version of the plan document. Increment it when a field
// changes meaning or is removed.
const PlanSchema = 1

// Result is the overall state of an upgrade plan.
type Result string

const (
	// ResultBlocked means a blocker must be resolved before any operation.
	ResultBlocked Result = "blocked"
	// ResultReady means no blocker exists and an operation is pending.
	ResultReady Result = "ready"
	// ResultComplete means every operation is done and verified.
	ResultComplete Result = "complete"
)

// Component names one part of the installation that an upgrade changes.
type Component string

const (
	ComponentBinary Component = "binary"
	ComponentWorker Component = "worker"
	ComponentPolicy Component = "policy"
	ComponentSmoke  Component = "smoke"
	ComponentStore  Component = "store"
)

// ComponentState tells whether a component matches the target release.
type ComponentState string

const (
	// StateOld means the component belongs to another release.
	StateOld ComponentState = "old"
	// StateNew means the component matches the target release.
	StateNew ComponentState = "new"
	// StateIncomplete means a change of the component started and did not
	// finish.
	StateIncomplete ComponentState = "incomplete"
	// StateUnsupported means the target release cannot use the component.
	StateUnsupported ComponentState = "unsupported"
	// StateUnknown means the component could not be observed.
	StateUnknown ComponentState = "unknown"
)

// OperationID names one upgrade operation.
type OperationID string

// The upgrade operations, in procedure order.
const (
	OperationQuiesce          OperationID = "quiesce"
	OperationInstallBinary    OperationID = "install_binary"
	OperationPrepareResources OperationID = "prepare_resources"
	OperationOnboardingPlan   OperationID = "onboarding_plan"
	OperationRebuildWorker    OperationID = "rebuild_worker"
	OperationGateProof        OperationID = "gate_proof"
	OperationSkillSmoke       OperationID = "skill_smoke"
	OperationMigrateStore     OperationID = "migrate_store"
	OperationDoctor           OperationID = "doctor"
	OperationStartCoordinator OperationID = "start_coordinator"
)

// OperationState is the evaluated state of one operation.
type OperationState string

const (
	// OperationDone means current evidence proves the operation.
	OperationDone OperationState = "done"
	// OperationPending means the operation is the next one to do.
	OperationPending OperationState = "pending"
	// OperationBlocked means the operation waits for a blocker or an earlier
	// operation.
	OperationBlocked OperationState = "blocked"
)

// Target identifies the release that the plan upgrades to: the running
// binary.
type Target struct {
	Version         string `json:"version"`
	Build           string `json:"build"`
	Revision        string `json:"revision"`
	WorkerBaseImage string `json:"worker_base_image,omitempty"`
}

// ImageObservation is the local presence of one digest-pinned image.
type ImageObservation struct {
	Reference string
	Present   bool
	// Problem tells why presence could not be observed, for example because
	// Docker is not reachable.
	Problem string
}

// WorkerObservation is the pinned worker image and the release it was built
// for.
type WorkerObservation struct {
	Image   string
	Present bool
	// Release is the release label of the image, or empty.
	Release string
	Problem string
}

// Observation is the read-only state of one installation, observed by the
// target binary.
type Observation struct {
	Target Target
	// FactoryCommand is the absolute path of the target binary.
	FactoryCommand string
	ConfigPath     string
	// Registered reports a usable registration; RegistrationProblem tells
	// why there is none.
	Registered          bool
	RegistrationProblem string
	RepositoryPath      string
	Store               store.UpgradeInspection
	// StoreProblem tells why the store could not be inspected.
	StoreProblem       string
	CoordinatorRunning bool
	CoordinatorProblem string
	// InstalledBinary is the factory command that PATH names, or empty.
	InstalledBinary   string
	InstalledIsTarget bool
	// BaseImage is the target worker base image that the worker builds on.
	BaseImage   ImageObservation
	Worker      WorkerObservation
	PolicyValid bool
	// PolicyProblem tells why factory.yaml is not valid for the target.
	PolicyProblem string
	// Smoke is the recorded skill smoke of the pinned worker per harness.
	Smoke []onboarding.HarnessSmoke
	// OnboardingPlanPath is the onboarding plan the operator named, or
	// empty; Onboarding is its evaluated status, or nil when it was not
	// read.
	OnboardingPlanPath    string
	OnboardingPlanProblem string
	Onboarding            []onboarding.StepStatus
}

// ComponentStatus is the state of one component.
type ComponentStatus struct {
	Name   Component      `json:"name"`
	State  ComponentState `json:"state"`
	Detail string         `json:"detail,omitempty"`
}

// Blocker is a condition that stops every pending operation until it is
// resolved.
type Blocker struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
	Action string `json:"action"`
}

// Quiescence is one condition that a quiescent installation satisfies.
type Quiescence struct {
	Code        string `json:"code"`
	Description string `json:"description"`
	Satisfied   bool   `json:"satisfied"`
}

// Operation is one ordered upgrade step.
type Operation struct {
	ID          OperationID    `json:"id"`
	State       OperationState `json:"state"`
	Description string         `json:"description"`
	// Command is the exact command to run, or empty for a manual step.
	Command string        `json:"command,omitempty"`
	Effect  doctor.Effect `json:"effect"`
	// Paid reports that the operation makes paid model calls.
	Paid            bool     `json:"paid,omitempty"`
	ExpectedEffects []string `json:"expected_effects"`
	Reason          string   `json:"reason,omitempty"`
}

// Verification is one check of the final state.
type Verification struct {
	Code        string `json:"code"`
	Description string `json:"description"`
	Command     string `json:"command"`
	Satisfied   bool   `json:"satisfied"`
}

// Plan is the versioned upgrade plan.
type Plan struct {
	SchemaVersion  int               `json:"schema_version"`
	Target         Target            `json:"target"`
	Result         Result            `json:"result"`
	Components     []ComponentStatus `json:"components"`
	Blockers       []Blocker         `json:"blockers"`
	Quiescence     []Quiescence      `json:"quiescence"`
	Operations     []Operation       `json:"operations"`
	Verification   []Verification    `json:"verification"`
	RecoveryLimits []string          `json:"recovery_limits"`
}

// RecoveryLimits states what an upgrade cannot undo. ADR 0032 owns the
// backup and restore boundary.
var RecoveryLimits = []string{
	"Factory has no supported restore. A pre-migration backup <store>.bak-* is migration provenance, not a restore point: never copy it over the store (ADR 0032).",
	"After migrate_store, the old binary refuses the store because its schema is newer. Running the old binary is not a rollback; continue forward with the target binary.",
	"A database copy does not rewind branches, pull requests, comments, labels, workers, or harness sessions.",
	"A claimed run keeps its frozen worker and policy. The upgrade never moves an active run to the new worker or resumes an old session with a new image.",
}

// Build derives the plan from one observation. It never changes anything.
func Build(observation Observation) Plan {
	planner := planner{observation: observation}
	components := planner.components()
	planner.states = map[Component]ComponentState{}
	for _, status := range components {
		planner.states[status.Name] = status.State
	}
	operations := planner.operations()
	blockers := planner.blockers()
	sequence(operations, blockers)
	plan := Plan{
		SchemaVersion:  PlanSchema,
		Target:         observation.Target,
		Result:         result(operations, blockers),
		Components:     components,
		Blockers:       blockers,
		Quiescence:     planner.quiescence(),
		Operations:     operations,
		Verification:   planner.verification(),
		RecoveryLimits: RecoveryLimits,
	}
	if plan.Blockers == nil {
		plan.Blockers = []Blocker{}
	}
	return plan
}

// planner holds one observation and the facts derived from it so far.
type planner struct {
	observation Observation
	states      map[Component]ComponentState
}

// command returns the target binary, quoted for a POSIX shell.
func (p planner) command() string {
	if p.observation.FactoryCommand == "" {
		return "factory"
	}
	return onboarding.Quote(p.observation.FactoryCommand)
}

// configFlag returns the --config flag of the observed installation.
func (p planner) configFlag() string {
	return "--config " + onboarding.Quote(p.observation.ConfigPath)
}

// result derives the overall result.
func result(operations []Operation, blockers []Blocker) Result {
	if len(blockers) != 0 {
		return ResultBlocked
	}
	for _, operation := range operations {
		if operation.State != OperationDone {
			return ResultReady
		}
	}
	return ResultComplete
}

// sequence makes the first operation that is not done pending and every
// later one blocked. With a blocker, every operation that is not done is
// blocked.
func sequence(operations []Operation, blockers []Blocker) {
	var next OperationID
	for index := range operations {
		operation := &operations[index]
		if operation.State == OperationDone {
			continue
		}
		switch {
		case len(blockers) != 0:
			operation.State, operation.Reason = OperationBlocked, "resolve the blockers first"
		case next == "":
			operation.State, next = OperationPending, operation.ID
		default:
			operation.State, operation.Reason = OperationBlocked, "waiting for "+string(next)
		}
	}
}

// onboardingStep returns the evaluated onboarding step, or nil when the
// onboarding plan was not read.
func (p planner) onboardingStep(name onboarding.Step) *onboarding.StepStatus {
	for index := range p.observation.Onboarding {
		if p.observation.Onboarding[index].Name == name {
			return &p.observation.Onboarding[index]
		}
	}
	return nil
}

// onboardingVerified reports whether an onboarding step is verified for the
// current inputs, so its evidence is reused.
func (p planner) onboardingVerified(name onboarding.Step) bool {
	step := p.onboardingStep(name)
	return step != nil && step.State == onboarding.StateVerified
}

// onboardingAction returns the onboarding action of a step, or fallback.
func (p planner) onboardingAction(name onboarding.Step, fallback string) string {
	if step := p.onboardingStep(name); step != nil && step.Action != "" {
		return strings.TrimPrefix(step.Action, "run ")
	}
	return fallback
}

// planFile returns the onboarding plan path for commands.
func (p planner) planFile() string {
	if p.observation.OnboardingPlanPath == "" {
		return "<onboarding plan file>"
	}
	return onboarding.Quote(p.observation.OnboardingPlanPath)
}

// done returns the state of an operation that current evidence proves.
func done(proved bool) OperationState {
	if proved {
		return OperationDone
	}
	return OperationPending
}

// operations lists every operation in procedure order with its own
// evidence. sequence later decides which pending operation is next.
func (p planner) operations() []Operation {
	o := p.observation
	return []Operation{
		{ID: OperationQuiesce, State: done(p.quiescent() || p.laterOperationsDone()), Effect: doctor.EffectLocalMutation,
			Description:     "finish or cancel every active run with the binary that claimed it, then stop the foreground coordinator with Ctrl-C or factory stop " + p.configFlag(),
			ExpectedEffects: []string{"no coordinator holds the repository lock", "no run, invocation, or pending effect is active"}},
		{ID: OperationInstallBinary, State: done(o.InstalledIsTarget), Effect: doctor.EffectLocalMutation,
			Description:     "install the archive of " + o.Target.Version + " in place of the old one, so that command -v factory names " + o.FactoryCommand,
			ExpectedEffects: []string{"factory version --json reports the target version and revision"}},
		{ID: OperationPrepareResources, State: done(p.onboardingVerified(onboarding.StepPrepare)), Effect: doctor.EffectLocalMutation,
			Description:     "export the release resources of the target to a new directory below $HOME",
			Command:         p.command() + " onboard prepare --output <new directory below $HOME> --json",
			ExpectedEffects: []string{"a new resource directory that the operator owns"}},
		{ID: OperationOnboardingPlan, State: done(o.Onboarding != nil), Effect: doctor.EffectLocalMutation,
			Description:     "create or refresh the onboarding plan with the target binary, so its evidence is evaluated against the target release",
			Command:         p.command() + " onboard plan --repository " + onboarding.Quote(o.RepositoryPath) + " --output " + p.planFile() + " " + p.configFlag() + " --resources <prepared directory> --json",
			ExpectedEffects: []string{"the onboarding plan file and its lock"}},
		{ID: OperationRebuildWorker, State: done(p.states[ComponentWorker] == StateNew), Effect: doctor.EffectLocalMutation,
			Description:     "rebuild the worker image from the target worker base image " + o.Target.WorkerBaseImage + " and record its digest in factory.yaml, as Step 3 of the onboarding procedure describes",
			Command:         p.onboardingAction(onboarding.StepImage, ""),
			ExpectedEffects: []string{"a new local worker image", "a changed worker_build.digest in factory.yaml"}},
		{ID: OperationGateProof, State: done(p.onboardingVerified(onboarding.StepGateProof)), Effect: doctor.EffectLocalMutation,
			Description:     "prove setup and every gate in the new worker; valid evidence is reused",
			Command:         p.onboardingAction(onboarding.StepGateProof, p.command()+" onboard record --plan "+p.planFile()+" gate-proof --json"),
			ExpectedEffects: []string{"gate proof evidence in the onboarding plan"}},
		{ID: OperationSkillSmoke, State: done(p.states[ComponentSmoke] == StateNew), Effect: doctor.EffectExternalMutation, Paid: true,
			Description:     "record the skill smoke of every selectable harness for the new worker digest; only missing skills are called again",
			Command:         p.onboardingAction(onboarding.StepSkillSmoke, ""),
			ExpectedEffects: []string{"paid model calls", "new records in worker/skill-smoke.json"}},
		{ID: OperationMigrateStore, State: done(p.states[ComponentStore] == StateNew), Effect: doctor.EffectLocalMutation,
			Description:     "migrate the operational store with the explicit entry point; the old binary refuses the store afterwards",
			Command:         p.command() + " upgrade migrate-store " + p.configFlag() + " --json",
			ExpectedEffects: []string{"a private pre-migration backup <store>.bak-<timestamp>", fmt.Sprintf("store schema version %d", store.CurrentSchemaVersion)}},
		{ID: OperationDoctor, State: done(p.onboardingVerified(onboarding.StepDoctor)), Effect: doctor.EffectInspection,
			Description:     "run the startup diagnosis of the target and record it in the onboarding plan",
			Command:         p.onboardingAction(onboarding.StepDoctor, p.command()+" onboard record --plan "+p.planFile()+" doctor --json"),
			ExpectedEffects: []string{"doctor evidence in the onboarding plan"}},
		{ID: OperationStartCoordinator, State: done(o.CoordinatorRunning && p.laterOperationsDone()), Effect: doctor.EffectExternalMutation,
			Description:     "start the coordinator of the target in the foreground and keep its terminal open; a supervised service is not supported yet",
			Command:         p.command() + " start " + p.configFlag(),
			ExpectedEffects: []string{"the coordinator claims eligible issues and continues runs"}},
	}
}

// laterOperationsDone reports whether every operation from install_binary
// to doctor is proved by evidence. Then the upgrade is done, and active work
// and a live coordinator are ordinary operation, not blockers.
func (p planner) laterOperationsDone() bool {
	return p.observation.InstalledIsTarget && p.onboardingVerified(onboarding.StepPrepare) && p.observation.Onboarding != nil &&
		p.states[ComponentWorker] == StateNew && p.onboardingVerified(onboarding.StepGateProof) &&
		p.states[ComponentSmoke] == StateNew && p.states[ComponentStore] == StateNew && p.onboardingVerified(onboarding.StepDoctor)
}

// quiescent reports whether no coordinator runs and the store holds no
// active work.
func (p planner) quiescent() bool {
	for _, condition := range p.quiescence() {
		if !condition.Satisfied {
			return false
		}
	}
	return true
}

// quiescence evaluates each condition of a quiescent installation.
func (p planner) quiescence() []Quiescence {
	o := p.observation
	readable := o.Store.Supported() && !o.Store.InterruptedWrite && o.StoreProblem == ""
	return []Quiescence{
		{Code: "coordinator.stopped", Description: "no coordinator holds the repository lock", Satisfied: !o.CoordinatorRunning && o.CoordinatorProblem == ""},
		{Code: "runs.terminal", Description: "every run is complete, cancelled, or failed", Satisfied: readable && len(o.Store.ActiveRuns) == 0},
		{Code: "invocations.inactive", Description: "no invocation is active", Satisfied: readable && o.Store.ActiveInvocations == 0},
		{Code: "effects.empty", Description: "the pending-effect journal is empty", Satisfied: readable && o.Store.PendingEffects == 0},
	}
}
