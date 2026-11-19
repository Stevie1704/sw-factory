package upgrade

import (
	"fmt"

	"github.com/Stevie1704/sw-factory/internal/onboarding"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// components derives the state of every component.
// Each state is recorded before the next is derived, because the smoke
// state depends on the worker state.
func (p planner) components() []ComponentStatus {
	var statuses []ComponentStatus
	for _, derive := range []func() ComponentStatus{p.binary, p.worker, p.policy, p.smoke, p.store} {
		status := derive()
		p.states[status.Name] = status.State
		statuses = append(statuses, status)
	}
	return statuses
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
		status.State, status.Detail = StateIncomplete, "the pinned image "+w.Image+" is not available locally, or Docker is not reachable: "+w.Absence
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
	case p.states[ComponentWorker] != StateNew:
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
	finding := s.SchemaFinding()
	switch {
	case p.observation.StoreProblem != "":
		status.State, status.Detail = StateUnknown, p.observation.StoreProblem
	case s.InterruptedWrite:
		status.State, status.Detail = StateIncomplete, "a rollback journal shows an interrupted write"
	case finding != nil && !s.Exists:
		status.State, status.Detail = StateUnknown, finding.Detail
	case finding != nil:
		status.State, status.Detail = StateUnsupported, finding.Detail
	case s.SchemaVersion == store.CurrentSchemaVersion:
		status.State = StateNew
	default:
		status.State, status.Detail = StateOld, fmt.Sprintf("schema %d; the target uses %d", s.SchemaVersion, store.CurrentSchemaVersion)
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
	if coordinator := p.coordinatorBlocker(); coordinator != nil {
		blockers = append(blockers, *coordinator)
	}
	for _, activity := range p.activityBlockers() {
		if !p.activityTolerated(activity.Code) {
			blockers = append(blockers, activity)
		}
	}
	if !o.PolicyValid {
		blockers = append(blockers, Blocker{Code: "configuration.invalid", Detail: o.PolicyProblem,
			Action: "repair factory.yaml, then check it with " + p.validateCommand()})
	}
	blockers = append(blockers, p.workerBlockers()...)
	if o.OnboardingPlanForeign != "" {
		blockers = append(blockers, Blocker{Code: "onboarding.plan_registration_mismatch", Detail: o.OnboardingPlanForeign,
			Action: "name the onboarding plan of the registration that --config selects, or omit --onboarding-plan and create one with the onboarding_plan operation"})
	}
	if o.OnboardingPlanProblem != "" {
		blockers = append(blockers, Blocker{Code: "onboarding.plan_unreadable", Detail: o.OnboardingPlanProblem,
			Action: "name the onboarding plan of this repository with --onboarding-plan, or omit the flag and create one with the onboarding_plan operation"})
	}
	return blockers
}

// storeActions are the actions of the store findings. None of them is a
// backup or an older binary.
var storeActions = map[string]string{
	"store.missing":            "check operational_data_path in the host configuration; Factory has no supported procedure that recreates a lost store with its history (ADR 0032)",
	"store.schema_newer":       "use the release that migrated the store, or a newer one, as the target binary; the store is not changed",
	"store.schema_unsupported": "this build cannot prove that the store passes the migration gate; file a report with factory bug-report and keep the current binary",
}

// storeBlockers lists the store conditions that the target cannot continue
// with.
func (p planner) storeBlockers() []Blocker {
	o := p.observation
	switch {
	case o.StoreProblem != "":
		return []Blocker{{Code: "store.unreadable", Detail: o.StoreProblem,
			Action: "repair the access to the operational store, for example its private file modes, then plan again; follow factory guide debug"}}
	case o.Store.InterruptedWrite:
		return []Blocker{{Code: "store.interrupted_write", Detail: "a rollback journal beside the store shows a write that stopped before its commit",
			Action: "run " + p.command() + " upgrade migrate-store " + p.configFlag() + " --json; it rolls the interrupted write back and checks the migration gate before it migrates"}}
	}
	if finding := o.Store.SchemaFinding(); finding != nil {
		return []Blocker{{Code: finding.Code, Detail: finding.Detail, Action: storeActions[finding.Code]}}
	}
	return nil
}

// workerEvidenceCodes are the activity findings that only the coordinator
// of the target can resolve: a live harness helper, a session-role worker
// that is not released, and Docker evidence that is missing. Evidence of
// the later operations never proves them resolved (ADR 0032).
var workerEvidenceCodes = map[string]bool{
	"session_invocation.unreleased": true,
	"worker.active":                 true,
	"docker.unreachable":            true,
	"worker.unobserved":             true,
}

// activityTolerated reports whether an activity finding is ordinary
// operation instead of a blocker. Active work is ordinary once every later
// operation is done. A worker finding is ordinary only while the target
// coordinator also runs, because before that no process can own it.
func (p planner) activityTolerated(code string) bool {
	return p.laterOperationsDone() && (p.targetCoordinator() || !workerEvidenceCodes[code])
}

// activityBlockers lists the active work that fails the migration gate.
// Active work is finished with the binary that can read the store schema.
func (p planner) activityBlockers() []Blocker {
	o := p.observation
	var blockers []Blocker
	if !o.Store.Supported() || o.Store.InterruptedWrite {
		return blockers
	}
	owner := "the binary that claimed it"
	actions := map[string]string{
		"run.active":                    "finish each run, or cancel it deliberately, with " + owner + "; the upgrade never moves a claimed run to a new worker or policy",
		"invocation.active":             "let the coordinator of " + owner + " finish them; never resume an old session with a new image",
		"effect.pending":                "reconcile them with factory reconcile " + p.configFlag() + " of " + owner + ", or abandon an ambiguous effect after inspection as factory guide debug describes",
		"session_invocation.active":     "let the coordinator of " + owner + " finish the session role, or cancel the session as factory guide ideation describes",
		"session_invocation.unreleased": "start the coordinator of " + owner + " until it has released the session-role workers, then stop it",
		"session_command.queued":        "let the coordinator of " + owner + " complete the queued session commands, then stop it",
		"worker.active":                 "let the coordinator of " + owner + " finish or cancel the harness process; never stop a worker by hand while its process writes",
		"docker.unreachable":            "start Docker or repair the access to it, then plan again; the plan never passes without proof that no worker process runs",
		"worker.unobserved":             "check the named worker with docker ps and docker logs, then plan again",
	}
	for _, finding := range append(o.Store.ActivityFindings(), o.WorkerActivity...) {
		blockers = append(blockers, Blocker{Code: finding.Code, Detail: finding.Detail, Action: actions[finding.Code]})
	}
	return blockers
}

// coordinatorBlocker returns the blocker of the process that holds the
// coordinator lock, or nil. It uses the identity that the holder recorded:
// a store migration or a reset is maintenance, a coordinator of another
// release mismatches the target worker and store, and a target coordinator
// is a blocker only before the upgrade is done.
func (p planner) coordinatorBlocker() *Blocker {
	lock := p.observation.Lock
	stop := "stop it with Ctrl-C in its terminal, or run factory stop " + p.configFlag()
	switch {
	case lock.Problem != "":
		return &Blocker{Code: "coordinator.unobserved", Detail: lock.Problem, Action: "repair the access to the coordinator lock directory, then plan again"}
	case !lock.Held:
		return nil
	case lock.Holder != LockHolderCoordinator:
		return &Blocker{Code: "maintenance.running", Detail: "the repository lock is held by " + lock.Holder + " of release " + lock.Version,
			Action: "wait until that command ends, then plan again"}
	case !p.targetCoordinator():
		return &Blocker{Code: "coordinator.worker_mismatch", Detail: fmt.Sprintf("a coordinator of release %q (revision %q) runs; the target worker and store need %s (revision %s)", lock.Version, lock.Revision, p.observation.Target.Version, p.observation.Target.Revision),
			Action: stop + "; start the target coordinator only at start_coordinator"}
	case !p.laterOperationsDone():
		return &Blocker{Code: "coordinator.running", Detail: "a coordinator of the target runs before the upgrade is done", Action: stop}
	}
	return nil
}

// targetCoordinator reports whether the lock holder is a coordinator of the
// target build.
func (p planner) targetCoordinator() bool {
	lock, target := p.observation.Lock, p.observation.Target
	return lock.Held && lock.Holder == LockHolderCoordinator && lock.Version == target.Version && lock.Revision == target.Revision
}

// workerBlockers lists the worker conditions that stop a rebuild.
func (p planner) workerBlockers() []Blocker {
	o := p.observation
	if p.states[ComponentWorker] == StateNew {
		return nil
	}
	if o.Worker.Problem != "" {
		return []Blocker{{Code: "worker.release_unreadable", Detail: o.Worker.Problem, Action: "check that this account can inspect the pinned worker image with docker image inspect, then plan again"}}
	}
	if o.BaseImage.Reference != "" && !o.BaseImage.Present {
		return []Blocker{{Code: "worker.base_image_missing", Detail: "the target worker base image " + o.BaseImage.Reference + " is not available locally, or Docker is not reachable: " + o.BaseImage.Absence,
			Action: "pull it with docker pull " + o.BaseImage.Reference + " (network access); the plan never pulls an image"}}
	}
	return nil
}

// validateCommand returns the offline check of factory.yaml.
func (p planner) validateCommand() string {
	return p.command() + " config validate --file " + onboarding.Quote(p.observation.RepositoryPath+"/factory.yaml") + " --json"
}

// verification lists the checks of the final state.
func (p planner) verification() []Verification {
	o := p.observation
	return []Verification{
		{Code: "coordinator.identity", Description: "PATH names the target binary", Command: "factory version --json", Satisfied: p.proved[OperationInstallBinary]},
		{Code: "worker.release", Description: "the pinned worker image was built from the target worker base image", Command: p.command() + " doctor " + p.configFlag() + " --json", Satisfied: p.proved[OperationRebuildWorker]},
		{Code: "configuration.valid", Description: "factory.yaml is valid for the target", Command: p.validateCommand(), Satisfied: o.PolicyValid},
		{Code: "smoke.complete", Description: "every required skill has a smoke record for the pinned digest", Command: p.command() + " onboard status --plan " + p.planFile() + " --json", Satisfied: p.proved[OperationSkillSmoke]},
		{Code: "store.ready", Description: fmt.Sprintf("the operational store has schema %d", store.CurrentSchemaVersion), Command: p.command() + " doctor " + p.configFlag() + " --json", Satisfied: p.proved[OperationMigrateStore]},
		{Code: "doctor.ready", Description: "the onboarding plan records a passed doctor of the target release", Command: p.command() + " onboard status --plan " + p.planFile() + " --json", Satisfied: p.proved[OperationDoctor]},
		{Code: "supervisor.running", Description: "the repository lock is held by a coordinator that records the target release and revision, after every other check holds", Command: p.command() + " status " + p.configFlag() + " --json", Satisfied: p.targetCoordinator() && p.laterOperationsDone()},
	}
}
