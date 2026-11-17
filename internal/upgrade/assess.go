package upgrade

import (
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/onboarding"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// components derives the state of every component.
func (p planner) components() []ComponentStatus {
	return []ComponentStatus{p.binary(), p.worker(), p.policy(), p.smoke(), p.store()}
}

// binary compares the factory command on PATH with the target binary.
func (p planner) binary() ComponentStatus {
	o := p.observation
	switch {
	case o.InstalledIsTarget:
		return ComponentStatus{Name: ComponentBinary, State: StateNew}
	case o.InstalledBinary == "":
		return ComponentStatus{Name: ComponentBinary, State: StateUnknown, Detail: "PATH names no factory command"}
	default:
		return ComponentStatus{Name: ComponentBinary, State: StateOld, Detail: "PATH names " + o.InstalledBinary + ", not the target " + o.FactoryCommand}
	}
}

// worker compares the release label of the pinned worker image with the
// target release.
func (p planner) worker() ComponentStatus {
	w := p.observation.Worker
	status := ComponentStatus{Name: ComponentWorker}
	switch {
	case !p.observation.Registered || !p.observation.PolicyValid:
		status.State, status.Detail = StateUnknown, "factory.yaml does not name a usable worker image"
	case w.Problem != "":
		status.State, status.Detail = StateUnknown, w.Problem
	case !w.Present:
		status.State, status.Detail = StateIncomplete, "the pinned image "+w.Image+" is not available locally"
	case p.observation.Target.Build != "release":
		status.State, status.Detail = StateNew, "a development build accepts any worker image"
	case w.Release == p.observation.Target.Version:
		status.State = StateNew
	default:
		status.State, status.Detail = StateOld, fmt.Sprintf("the pinned image was built for release %q", w.Release)
	}
	return status
}

// policy reports whether factory.yaml is valid for the target release.
func (p planner) policy() ComponentStatus {
	switch {
	case !p.observation.Registered:
		return ComponentStatus{Name: ComponentPolicy, State: StateUnknown}
	case p.observation.PolicyValid:
		return ComponentStatus{Name: ComponentPolicy, State: StateNew}
	default:
		return ComponentStatus{Name: ComponentPolicy, State: StateIncomplete, Detail: p.observation.PolicyProblem}
	}
}

// smoke compares the recorded skill smoke of the pinned worker with the
// required skills. Evidence of an old worker is old, whatever it covers.
func (p planner) smoke() ComponentStatus {
	status := ComponentStatus{Name: ComponentSmoke}
	if len(p.observation.Smoke) == 0 {
		status.State, status.Detail = StateUnknown, "the harnesses that the policy selects are not known"
		return status
	}
	verified, missing := 0, 0
	for _, harness := range p.observation.Smoke {
		verified += len(harness.Verified)
		missing += len(harness.Missing)
	}
	switch {
	case p.worker().State != StateNew:
		status.State, status.Detail = StateOld, "the smoke belongs to the old worker"
	case missing == 0:
		status.State = StateNew
	case verified != 0:
		status.State, status.Detail = StateIncomplete, fmt.Sprintf("%d required skills have no smoke record for the pinned digest", missing)
	default:
		status.State, status.Detail = StateOld, "no smoke record exists for the pinned digest"
	}
	return status
}

// store classifies the schema of the operational store.
func (p planner) store() ComponentStatus {
	s := p.observation.Store
	status := ComponentStatus{Name: ComponentStore}
	switch {
	case p.observation.StoreProblem != "":
		status.State, status.Detail = StateUnknown, p.observation.StoreProblem
	case !s.Exists:
		status.State, status.Detail = StateUnknown, "no operational store exists at "+s.Path
	case s.InterruptedWrite:
		status.State, status.Detail = StateIncomplete, "a rollback journal shows an interrupted write"
	case s.SchemaVersion == store.CurrentSchemaVersion:
		status.State = StateNew
	case s.Supported():
		status.State, status.Detail = StateOld, fmt.Sprintf("schema %d; the target uses %d", s.SchemaVersion, store.CurrentSchemaVersion)
	default:
		status.State, status.Detail = StateUnsupported, fmt.Sprintf("schema %d; the target inspects %d to %d", s.SchemaVersion, store.MinimumUpgradeSchemaVersion, store.CurrentSchemaVersion)
	}
	return status
}

// blockers lists every condition that stops the pending operations.
func (p planner) blockers() []Blocker {
	o := p.observation
	if !o.Registered {
		return []Blocker{{Code: "registration.missing", Detail: o.RegistrationProblem,
			Action: "name the host configuration of the installation with --config; an upgrade needs a registered repository"}}
	}
	var blockers []Blocker
	blockers = append(blockers, p.storeBlockers()...)
	if !p.laterOperationsDone() {
		blockers = append(blockers, p.activityBlockers()...)
	}
	if !o.PolicyValid {
		blockers = append(blockers, Blocker{Code: "configuration.invalid", Detail: o.PolicyProblem,
			Action: "repair factory.yaml, then check it with " + p.command() + " config validate --file " + onboarding.Quote(o.RepositoryPath+"/factory.yaml") + " --json"})
	}
	blockers = append(blockers, p.workerBlockers()...)
	if o.OnboardingPlanProblem != "" {
		blockers = append(blockers, Blocker{Code: "onboarding.plan_unreadable", Detail: o.OnboardingPlanProblem,
			Action: "name the onboarding plan of this repository with --onboarding-plan, or omit the flag and create one with the onboarding_plan operation"})
	}
	return blockers
}

// storeBlockers lists the store conditions that the target cannot continue
// with. None of them is resolved by a backup or an older binary.
func (p planner) storeBlockers() []Blocker {
	o := p.observation
	s := o.Store
	switch {
	case o.StoreProblem != "":
		return []Blocker{{Code: "store.unreadable", Detail: o.StoreProblem,
			Action: "repair the access to the operational store, for example its private file modes, then plan again; follow factory guide debug"}}
	case !s.Exists:
		return []Blocker{{Code: "store.missing", Detail: "no operational store exists at " + s.Path,
			Action: "check operational_data_path in the host configuration; Factory has no supported procedure that recreates a lost store with its history (ADR 0032)"}}
	case s.InterruptedWrite:
		return []Blocker{{Code: "store.interrupted_write", Detail: "a rollback journal beside the store shows a write that stopped before its commit",
			Action: "run " + p.command() + " upgrade migrate-store " + p.configFlag() + " --json; it rolls the interrupted write back and checks quiescence before it migrates"}}
	case s.SchemaVersion > store.CurrentSchemaVersion:
		return []Blocker{{Code: "store.schema_newer", Detail: fmt.Sprintf("the store has schema %d; this build supports %d", s.SchemaVersion, store.CurrentSchemaVersion),
			Action: "use the release that migrated the store, or a newer one, as the target binary; the store is not changed"}}
	case !s.Supported():
		return []Blocker{{Code: "store.schema_unsupported", Detail: fmt.Sprintf("the store has schema %d; upgrade inspection reads %d to %d", s.SchemaVersion, store.MinimumUpgradeSchemaVersion, store.CurrentSchemaVersion),
			Action: "this build cannot prove that the store is quiescent; file a report with factory bug-report and keep the current binary"}}
	}
	return nil
}

// activityBlockers lists the live coordinator and the active work. Active
// work is finished with the binary that can read the store schema.
func (p planner) activityBlockers() []Blocker {
	o := p.observation
	var blockers []Blocker
	if o.CoordinatorProblem != "" {
		blockers = append(blockers, Blocker{Code: "coordinator.unobserved", Detail: o.CoordinatorProblem, Action: "repair the access to the coordinator lock directory, then plan again"})
	} else if o.CoordinatorRunning {
		blockers = append(blockers, Blocker{Code: "coordinator.running", Detail: "a live coordinator keeps its own release and would combine it with a worker or store of another release",
			Action: "stop it with Ctrl-C in its terminal, or run factory stop " + p.configFlag() + " with the binary that started it"})
	}
	if !o.Store.Supported() || o.Store.InterruptedWrite {
		return blockers
	}
	owner := p.storeOwner()
	if len(o.Store.ActiveRuns) != 0 {
		blockers = append(blockers, Blocker{Code: "run.active", Detail: "active runs: " + strings.Join(o.Store.ActiveRuns, ", "),
			Action: "finish each run, or cancel it deliberately, with " + owner + "; the upgrade never moves a claimed run to a new worker or policy"})
	}
	if o.Store.ActiveInvocations != 0 {
		blockers = append(blockers, Blocker{Code: "invocation.active", Detail: fmt.Sprintf("%d invocations are active", o.Store.ActiveInvocations),
			Action: "let the coordinator of " + owner + " finish them; never resume an old session with a new image"})
	}
	if o.Store.PendingEffects != 0 {
		blockers = append(blockers, Blocker{Code: "effect.pending", Detail: fmt.Sprintf("%d external effects are journaled", o.Store.PendingEffects),
			Action: "reconcile them with factory reconcile " + p.configFlag() + " of " + owner + ", or abandon an ambiguous effect after inspection as factory guide debug describes"})
	}
	return blockers
}

// storeOwner names the binary that reads the current store schema. Before
// the migration it is the old binary; after it, only the target binary.
func (p planner) storeOwner() string {
	if p.observation.Store.SchemaVersion == store.CurrentSchemaVersion {
		return "the target binary " + p.command()
	}
	return "the old binary that claimed it"
}

// workerBlockers lists the worker conditions that stop a rebuild.
func (p planner) workerBlockers() []Blocker {
	o := p.observation
	if p.states[ComponentWorker] == StateNew {
		return nil
	}
	if o.Worker.Problem != "" {
		return []Blocker{{Code: "worker.unobserved", Detail: o.Worker.Problem, Action: "make Docker reachable for this account, then plan again"}}
	}
	if o.BaseImage.Problem != "" {
		return []Blocker{{Code: "worker.base_image_unobserved", Detail: o.BaseImage.Problem, Action: "make Docker reachable for this account, then plan again"}}
	}
	if o.BaseImage.Reference != "" && !o.BaseImage.Present {
		return []Blocker{{Code: "worker.base_image_missing", Detail: "the target worker base image " + o.BaseImage.Reference + " is not available locally",
			Action: "pull it with docker pull " + o.BaseImage.Reference + " (network access); the plan never pulls an image"}}
	}
	return nil
}

// verification lists the checks of the final state.
func (p planner) verification() []Verification {
	o := p.observation
	return []Verification{
		{Code: "coordinator.identity", Description: "PATH names the target binary", Command: "factory version --json", Satisfied: o.InstalledIsTarget},
		{Code: "worker.release", Description: "the pinned worker image was built from the target worker base image", Command: p.command() + " doctor " + p.configFlag() + " --json", Satisfied: p.states[ComponentWorker] == StateNew},
		{Code: "configuration.valid", Description: "factory.yaml is valid for the target", Command: p.command() + " config validate --file " + onboarding.Quote(o.RepositoryPath+"/factory.yaml") + " --json", Satisfied: o.PolicyValid},
		{Code: "smoke.complete", Description: "every required skill has a smoke record for the pinned digest", Command: p.command() + " onboard status --plan " + p.planFile() + " --json", Satisfied: p.states[ComponentSmoke] == StateNew},
		{Code: "store.ready", Description: fmt.Sprintf("the operational store has schema %d", store.CurrentSchemaVersion), Command: p.command() + " doctor " + p.configFlag() + " --json", Satisfied: p.states[ComponentStore] == StateNew},
		{Code: "doctor.ready", Description: "the onboarding plan records a passed doctor of the target release", Command: p.command() + " onboard status --plan " + p.planFile() + " --json", Satisfied: p.onboardingVerified(onboarding.StepDoctor)},
		{Code: "supervisor.running", Description: "the coordinator of the target runs in the foreground and renews its heartbeat", Command: p.command() + " status " + p.configFlag() + " --json", Satisfied: o.CoordinatorRunning && p.laterOperationsDone()},
	}
}
