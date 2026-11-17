package upgrade_test

import (
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/onboarding"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/upgrade"
)

const (
	targetRelease = "v1.4.0"
	targetFactory = "/opt/factory-v1.4.0/bin/factory"
	configPath    = "/home/operator/.config/factory/config.yaml"
	planPath      = "/home/operator/.cache/sw-factory/onboarding-service.json"
)

// idleBeforeUpgrade is a healthy, idle installation of the previous release
// on which the operator runs the target binary for the first time.
func idleBeforeUpgrade() upgrade.Observation {
	return upgrade.Observation{
		Target:             upgrade.Target{Version: targetRelease, Build: "release", Revision: "abc123", WorkerBaseImage: "ghcr.io/stevie1704/sw-factory-base@sha256:1111"},
		FactoryCommand:     targetFactory,
		ConfigPath:         configPath,
		Registered:         true,
		RepositoryPath:     "/work/service",
		Store:              store.UpgradeInspection{Path: "/home/operator/.config/factory/data/factory.db", Exists: true, SchemaVersion: store.CurrentSchemaVersion - 1},
		InstalledBinary:    "/opt/factory-v1.3.0/bin/factory",
		BaseImage:          upgrade.ImageObservation{Reference: "ghcr.io/stevie1704/sw-factory-base@sha256:1111", Present: true},
		Worker:             upgrade.WorkerObservation{Image: "ghcr.io/example/service-worker@sha256:2222", Present: true, Release: "v1.3.0"},
		PolicyValid:        true,
		Smoke:              []onboarding.HarnessSmoke{{Harness: "codex", Required: []string{"tdd"}, Verified: []string{}, Missing: []string{"tdd"}}},
		OnboardingPlanPath: planPath,
	}
}

// upgradedAndRunning is the same installation after a complete upgrade with
// the target coordinator started in the foreground.
func upgradedAndRunning() upgrade.Observation {
	observation := idleBeforeUpgrade()
	observation.InstalledBinary, observation.InstalledIsTarget = targetFactory, true
	observation.Worker.Release = targetRelease
	observation.Smoke = []onboarding.HarnessSmoke{{Harness: "codex", Required: []string{"tdd"}, Verified: []string{"tdd"}, Missing: []string{}}}
	observation.Store.SchemaVersion = store.CurrentSchemaVersion
	observation.Onboarding = verifiedOnboarding()
	observation.CoordinatorRunning = true
	return observation
}

// verifiedOnboarding is an onboarding status whose evidence all matches the
// target release.
func verifiedOnboarding() []onboarding.StepStatus {
	var steps []onboarding.StepStatus
	for _, step := range []onboarding.Step{onboarding.StepPrepare, onboarding.StepConfiguration, onboarding.StepImage, onboarding.StepGateProof, onboarding.StepRegistration, onboarding.StepSkillSmoke, onboarding.StepDoctor} {
		steps = append(steps, onboarding.StepStatus{Name: step, State: onboarding.StateVerified})
	}
	return steps
}

// withStep replaces the state and action of one onboarding step.
func withStep(steps []onboarding.StepStatus, name onboarding.Step, state onboarding.State, action string) []onboarding.StepStatus {
	result := append([]onboarding.StepStatus{}, steps...)
	for index := range result {
		if result[index].Name == name {
			result[index].State, result[index].Action = state, action
		}
	}
	return result
}

// operation returns one operation of a plan by its ID.
func operation(t *testing.T, plan upgrade.Plan, id upgrade.OperationID) upgrade.Operation {
	t.Helper()
	for _, candidate := range plan.Operations {
		if candidate.ID == id {
			return candidate
		}
	}
	t.Fatalf("plan has no operation %s", id)
	return upgrade.Operation{}
}

// component returns the state of one component.
func component(t *testing.T, plan upgrade.Plan, name upgrade.Component) upgrade.ComponentState {
	t.Helper()
	for _, candidate := range plan.Components {
		if candidate.Name == name {
			return candidate.State
		}
	}
	t.Fatalf("plan has no component %s", name)
	return ""
}

// blocker returns the blocker with a code, or fails.
func blocker(t *testing.T, plan upgrade.Plan, code string) upgrade.Blocker {
	t.Helper()
	for _, candidate := range plan.Blockers {
		if candidate.Code == code {
			return candidate
		}
	}
	t.Fatalf("blockers = %+v, want %s", plan.Blockers, code)
	return upgrade.Blocker{}
}

// nextOperation returns the first pending operation.
func nextOperation(plan upgrade.Plan) upgrade.OperationID {
	for _, candidate := range plan.Operations {
		if candidate.State == upgrade.OperationPending {
			return candidate.ID
		}
	}
	return ""
}

// TestBuildPlansAHealthyIdleUpgrade verifies the reviewable plan of an idle
// installation: no blockers, the documented order, and the old components.
func TestBuildPlansAHealthyIdleUpgrade(t *testing.T) {
	plan := upgrade.Build(idleBeforeUpgrade())

	if plan.SchemaVersion != upgrade.PlanSchema || plan.Result != upgrade.ResultReady || len(plan.Blockers) != 0 {
		t.Fatalf("plan = schema %d, result %s, blockers %+v; want a ready plan", plan.SchemaVersion, plan.Result, plan.Blockers)
	}
	var order []string
	for _, step := range plan.Operations {
		order = append(order, string(step.ID))
	}
	want := "quiesce,install_binary,prepare_resources,onboarding_plan,rebuild_worker,gate_proof,skill_smoke,migrate_store,doctor,start_coordinator"
	if got := strings.Join(order, ","); got != want {
		t.Fatalf("operation order = %s, want %s", got, want)
	}
	if operation(t, plan, upgrade.OperationQuiesce).State != upgrade.OperationDone || nextOperation(plan) != upgrade.OperationInstallBinary {
		t.Fatalf("operations = %+v, want quiesce done and install_binary next", plan.Operations)
	}
	for name, state := range map[upgrade.Component]upgrade.ComponentState{
		upgrade.ComponentBinary: upgrade.StateOld, upgrade.ComponentWorker: upgrade.StateOld, upgrade.ComponentPolicy: upgrade.StateNew,
		upgrade.ComponentSmoke: upgrade.StateOld, upgrade.ComponentStore: upgrade.StateOld,
	} {
		if got := component(t, plan, name); got != state {
			t.Fatalf("component %s = %s, want %s", name, got, state)
		}
	}
	migrate := operation(t, plan, upgrade.OperationMigrateStore)
	if !strings.Contains(migrate.Command, targetFactory+" upgrade migrate-store --config "+configPath+" --json") || migrate.Effect != "local_mutation" || len(migrate.ExpectedEffects) == 0 {
		t.Fatalf("migrate_store = %+v, want the explicit target entry point and its effects", migrate)
	}
	if !operation(t, plan, upgrade.OperationSkillSmoke).Paid {
		t.Fatal("skill_smoke is not marked as paid")
	}
	if len(plan.Verification) == 0 || len(plan.RecoveryLimits) == 0 {
		t.Fatalf("plan has no verification (%d) or recovery limits (%d)", len(plan.Verification), len(plan.RecoveryLimits))
	}
}

// TestBuildBlocksActiveWorkWithTheOldBinary verifies the first-version
// policy: active runs, journaled effects, and active invocations block the
// upgrade and are finished with the binary that owns the store schema.
func TestBuildBlocksActiveWorkWithTheOldBinary(t *testing.T) {
	observation := idleBeforeUpgrade()
	observation.Store.ActiveRuns = []string{"run-7"}
	observation.Store.PendingEffects = 1
	observation.Store.ActiveInvocations = 1
	plan := upgrade.Build(observation)

	if plan.Result != upgrade.ResultBlocked {
		t.Fatalf("result = %s, want blocked", plan.Result)
	}
	active := blocker(t, plan, "run.active")
	if !strings.Contains(active.Detail, "run-7") || !strings.Contains(active.Action, "old") {
		t.Fatalf("run.active = %+v, want the run and the old binary", active)
	}
	blocker(t, plan, "effect.pending")
	blocker(t, plan, "invocation.active")
	for _, step := range plan.Operations {
		if step.State == upgrade.OperationPending {
			t.Fatalf("operation %s is pending while the installation is not quiescent", step.ID)
		}
	}
}

// TestBuildReportsExplicitBlockers verifies the blockers of an unsupported
// schema, a missing target base image, and a live coordinator.
func TestBuildReportsExplicitBlockers(t *testing.T) {
	for name, test := range map[string]struct {
		change func(*upgrade.Observation)
		code   string
	}{
		"newer schema":       {func(o *upgrade.Observation) { o.Store.SchemaVersion = store.CurrentSchemaVersion + 1 }, "store.schema_newer"},
		"schema too old":     {func(o *upgrade.Observation) { o.Store.SchemaVersion = store.MinimumUpgradeSchemaVersion - 1 }, "store.schema_unsupported"},
		"missing base image": {func(o *upgrade.Observation) { o.BaseImage.Present = false }, "worker.base_image_missing"},
		"live coordinator":   {func(o *upgrade.Observation) { o.CoordinatorRunning = true }, "coordinator.running"},
		"old coordinator with new worker": {func(o *upgrade.Observation) {
			o.CoordinatorRunning, o.Worker.Release = true, targetRelease
		}, "coordinator.worker_mismatch"},
		"no registration": {func(o *upgrade.Observation) { o.Registered = false; o.RegistrationProblem = "no host configuration" }, "registration.missing"},
	} {
		t.Run(name, func(t *testing.T) {
			observation := idleBeforeUpgrade()
			test.change(&observation)
			plan := upgrade.Build(observation)
			found := blocker(t, plan, test.code)
			if plan.Result != upgrade.ResultBlocked || found.Action == "" {
				t.Fatalf("plan = %s with %+v, want blocked with an action", plan.Result, found)
			}
			for _, forbidden := range []string{".bak", "downgrade", "restore the"} {
				if strings.Contains(strings.ToLower(found.Action), forbidden) {
					t.Fatalf("action %q advises %q", found.Action, forbidden)
				}
			}
		})
	}
}

// TestBuildContinuesAfterSmokeWithoutRepeatingPaidCalls verifies an
// interruption after the paid smoke: valid evidence is reused, and the store
// migration is the supported continuation.
func TestBuildContinuesAfterSmokeWithoutRepeatingPaidCalls(t *testing.T) {
	observation := upgradedAndRunning()
	observation.CoordinatorRunning = false
	observation.Store.SchemaVersion = store.CurrentSchemaVersion - 1
	observation.Onboarding = withStep(verifiedOnboarding(), onboarding.StepDoctor, onboarding.StatePending, "run doctor")
	plan := upgrade.Build(observation)

	if smoke := operation(t, plan, upgrade.OperationSkillSmoke); smoke.State != upgrade.OperationDone {
		t.Fatalf("skill_smoke = %+v, want done from reused evidence", smoke)
	}
	if next := nextOperation(plan); next != upgrade.OperationMigrateStore {
		t.Fatalf("next operation = %s, want migrate_store", next)
	}
	if component(t, plan, upgrade.ComponentSmoke) != upgrade.StateNew || component(t, plan, upgrade.ComponentStore) != upgrade.StateOld {
		t.Fatalf("components = %+v, want smoke new and store old", plan.Components)
	}
}

// TestBuildReportsPartialSmokeAsIncomplete verifies that a partly recorded
// smoke of the new worker asks only for the missing paid calls.
func TestBuildReportsPartialSmokeAsIncomplete(t *testing.T) {
	observation := upgradedAndRunning()
	observation.CoordinatorRunning = false
	observation.Smoke = []onboarding.HarnessSmoke{{Harness: "codex", Required: []string{"tdd", "review"}, Verified: []string{"tdd"}, Missing: []string{"review"}}}
	observation.Onboarding = withStep(verifiedOnboarding(), onboarding.StepSkillSmoke, onboarding.StatePending, "run these paid smoke calls: MANDATORY_SKILLS=review")
	plan := upgrade.Build(observation)

	smoke := operation(t, plan, upgrade.OperationSkillSmoke)
	if component(t, plan, upgrade.ComponentSmoke) != upgrade.StateIncomplete || smoke.State != upgrade.OperationPending || !strings.Contains(smoke.Command, "MANDATORY_SKILLS=review") {
		t.Fatalf("skill_smoke = %+v, want the onboarding action for the missing skill only", smoke)
	}
}

// TestBuildNeverOffersTheOldBinaryAfterMigration verifies an interruption
// after the store migration while PATH still names the old binary: the plan
// continues forward with the target binary and states that the old binary
// is no rollback.
func TestBuildNeverOffersTheOldBinaryAfterMigration(t *testing.T) {
	observation := idleBeforeUpgrade()
	observation.Store.SchemaVersion = store.CurrentSchemaVersion
	observation.Store.ActiveRuns = []string{"run-9"}
	plan := upgrade.Build(observation)

	if component(t, plan, upgrade.ComponentStore) != upgrade.StateNew || component(t, plan, upgrade.ComponentBinary) != upgrade.StateOld {
		t.Fatalf("components = %+v, want store new and binary old", plan.Components)
	}
	active := blocker(t, plan, "run.active")
	if strings.Contains(active.Action, "old") || !strings.Contains(active.Action, targetFactory) {
		t.Fatalf("run.active action = %q, want the target binary, never the old one", active.Action)
	}
	limits := strings.Join(plan.RecoveryLimits, " ")
	for _, want := range []string{"old binary", "ADR 0032"} {
		if !strings.Contains(limits, want) {
			t.Fatalf("recovery limits %q do not state %q", limits, want)
		}
	}
}

// TestBuildReportsAnInterruptedStoreWrite verifies that a rollback journal
// makes the store incomplete with the explicit migration entry point as its
// continuation.
func TestBuildReportsAnInterruptedStoreWrite(t *testing.T) {
	observation := idleBeforeUpgrade()
	observation.Store = store.UpgradeInspection{Path: observation.Store.Path, Exists: true, InterruptedWrite: true}
	plan := upgrade.Build(observation)

	if component(t, plan, upgrade.ComponentStore) != upgrade.StateIncomplete {
		t.Fatalf("store = %s, want incomplete", component(t, plan, upgrade.ComponentStore))
	}
	interrupted := blocker(t, plan, "store.interrupted_write")
	if !strings.Contains(interrupted.Action, "upgrade migrate-store") {
		t.Fatalf("store.interrupted_write action = %q, want the migration entry point", interrupted.Action)
	}
}

// TestBuildVerifiesACompleteUpgrade verifies the final state: every
// operation is done, every verification holds, and the live target
// coordinator is not a blocker.
func TestBuildVerifiesACompleteUpgrade(t *testing.T) {
	plan := upgrade.Build(upgradedAndRunning())

	if plan.Result != upgrade.ResultComplete || len(plan.Blockers) != 0 {
		t.Fatalf("plan = %s with %+v, want complete", plan.Result, plan.Blockers)
	}
	codes := map[string]bool{}
	for _, check := range plan.Verification {
		codes[check.Code] = true
		if !check.Satisfied {
			t.Fatalf("verification %s is not satisfied", check.Code)
		}
	}
	for _, want := range []string{"coordinator.identity", "worker.release", "configuration.valid", "smoke.complete", "store.ready", "doctor.ready", "supervisor.running"} {
		if !codes[want] {
			t.Fatalf("verification lacks %s", want)
		}
	}
}

// TestBuildContinuesAfterEachStep verifies that an interruption after each
// operation is diagnosed from evidence alone: the plan reports the
// operation as done and names the following one as the continuation.
func TestBuildContinuesAfterEachStep(t *testing.T) {
	staleProofAndDoctor := withStep(withStep(verifiedOnboarding(), onboarding.StepGateProof, onboarding.StateStale, ""), onboarding.StepDoctor, onboarding.StateStale, "")
	steps := []struct {
		done func(*upgrade.Observation)
		next upgrade.OperationID
	}{
		{func(o *upgrade.Observation) { o.InstalledBinary, o.InstalledIsTarget = targetFactory, true }, upgrade.OperationPrepareResources},
		{func(o *upgrade.Observation) { o.Onboarding = staleProofAndDoctor }, upgrade.OperationRebuildWorker},
		{func(o *upgrade.Observation) { o.Worker.Release = targetRelease }, upgrade.OperationGateProof},
		{func(o *upgrade.Observation) {
			o.Onboarding = withStep(verifiedOnboarding(), onboarding.StepDoctor, onboarding.StateStale, "")
		}, upgrade.OperationSkillSmoke},
		{func(o *upgrade.Observation) {
			o.Smoke = []onboarding.HarnessSmoke{{Harness: "codex", Required: []string{"tdd"}, Verified: []string{"tdd"}, Missing: []string{}}}
		}, upgrade.OperationMigrateStore},
		{func(o *upgrade.Observation) { o.Store.SchemaVersion = store.CurrentSchemaVersion }, upgrade.OperationDoctor},
		{func(o *upgrade.Observation) { o.Onboarding = verifiedOnboarding() }, upgrade.OperationStartCoordinator},
	}
	observation := idleBeforeUpgrade()
	for index, step := range steps {
		step.done(&observation)
		plan := upgrade.Build(observation)
		if next := nextOperation(plan); next != step.next || len(plan.Blockers) != 0 {
			t.Fatalf("after step %d: next = %s, blockers %+v; want %s", index, next, plan.Blockers, step.next)
		}
	}
}
