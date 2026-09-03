package factory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// TestGatherLaunchPerformsNoMutation verifies the gather seam reads the
// projections it needs without saving a run, invocation, gate result, or
// notification before admission.
func TestGatherLaunchPerformsNoMutation(t *testing.T) {
	packet := launchPlanningPacket()
	packetData, err := json.Marshal(packet)
	if err != nil {
		t.Fatalf("marshal planning packet: %v", err)
	}
	storeValue := &gatherReadOnlyStore{}
	run := &store.Run{ID: "run-gather", Status: store.StatusActive, Stage: store.StageImplementation, Worktree: "/worktree", SpecificationPacket: string(packetData), TestStageSkipped: true}
	module := newInvocationLifecycle(nil, nil, nil, nil, nil, nil, invocationLifecycleHooks{})
	_, err = module.gatherLaunch(context.Background(), InvocationLaunchRequest{RunStore: storeValue, Run: run, Request: AgentRequest{Role: workflow.RoleImplementation, Stage: store.StageImplementation}, InvocationID: "inv-gather"})
	if err != nil {
		t.Fatalf("gatherLaunch() error = %v", err)
	}
	if storeValue.saveRuns != 0 || storeValue.saveInvocations != 0 || storeValue.saveGates != 0 || storeValue.claimNotifications != 0 {
		t.Fatalf("gather mutations = runs:%d invocations:%d gates:%d notifications:%d, want none", storeValue.saveRuns, storeValue.saveInvocations, storeValue.saveGates, storeValue.claimNotifications)
	}
}

// TestPlanLaunchRejectsAnInactiveRunFromAGatheredSnapshot verifies the pure
// admission seam rejects a run without requiring a context or any adapter.
func TestPlanLaunchRejectsAnInactiveRunFromAGatheredSnapshot(t *testing.T) {
	snapshot := LaunchSnapshot{
		Run: store.Run{ID: "run-admission", Status: store.StatusWaitingForHuman},
	}
	_, err := PlanLaunch(snapshot, AgentRequest{Role: "implementation", Stage: store.StageImplementation})
	if err == nil || !strings.Contains(err.Error(), `cannot start visible agent from run status "waiting_for_human"`) {
		t.Fatalf("PlanLaunch() error = %v, want inactive-run rejection", err)
	}
}

// TestPlanLaunchAdoptsARecoveredInvocationAsItsThirdOutcome verifies adoption
// is distinct from both rejection and creation, and remains write-free.
func TestPlanLaunchAdoptsARecoveredInvocationAsItsThirdOutcome(t *testing.T) {
	run := store.Run{ID: "run-adoption", Status: store.StatusActive, Stage: store.StageImplementation, Worktree: "/worktree"}
	snapshot := LaunchSnapshot{
		Run:                        run,
		Packet:                     launchPlanningPacket(),
		ActiveInvocationsSupported: true,
		ActiveInvocations: []ActiveLaunchObservation{{
			Invocation:  store.Invocation{ID: "inv-recovered", RunID: run.ID, Role: workflow.RoleImplementation, Stage: store.StageImplementation, Status: store.InvocationStatusActive, RecoveryResumeCount: 1},
			StartedHere: false,
		}},
	}
	plan, err := PlanLaunch(snapshot, AgentRequest{Role: workflow.RoleImplementation, Stage: store.StageImplementation})
	if err != nil {
		t.Fatalf("PlanLaunch() error = %v", err)
	}
	if plan.Outcome != LaunchOutcomeAdopt || plan.AdoptedInvocation == nil || plan.AdoptedInvocation.ID != "inv-recovered" {
		t.Fatalf("PlanLaunch() = %#v, want recovered invocation adoption", plan)
	}
}

// launchPlanningPacket returns the smallest frozen policy that can admit an
// implementation invocation in a pure planning test.
func launchPlanningPacket() SpecificationPacket {
	return SpecificationPacket{Version: specificationPacketVersion, RepositoryConfig: config.RepositoryConfig{
		TestPolicy: config.TestPolicy{Mode: config.TestModeRequired},
		RoleHarnessDefaults: map[string]config.Harness{
			workflow.RoleTest:           config.HarnessCodex,
			workflow.RoleImplementation: config.HarnessCodex,
		},
		ModelOptions: map[string][]string{
			workflow.RoleTest:           {"model"},
			workflow.RoleImplementation: {"model"},
		},
	}}
}

// gatherReadOnlyStore is the smallest invocation/gate projection used to
// prove that gather only invokes read seams.
type gatherReadOnlyStore struct {
	saveRuns, saveInvocations, saveGates, claimNotifications int
}

func (*gatherReadOnlyStore) CurrentRun(context.Context) (*store.Run, error) { return nil, nil }
func (*gatherReadOnlyStore) Close() error                                   { return nil }
func (s *gatherReadOnlyStore) SaveRun(context.Context, store.Run) error {
	s.saveRuns++
	return nil
}
func (s *gatherReadOnlyStore) ClaimLifecycleNotification(context.Context, string, store.Status) (bool, error) {
	s.claimNotifications++
	return true, nil
}
func (*gatherReadOnlyStore) ReleaseLifecycleNotification(context.Context, string, store.Status) error {
	return nil
}
func (s *gatherReadOnlyStore) SaveInvocation(context.Context, store.Invocation) error {
	s.saveInvocations++
	return nil
}
func (*gatherReadOnlyStore) Invocation(context.Context, string, string) (*store.Invocation, error) {
	return nil, nil
}
func (*gatherReadOnlyStore) ActiveInvocations(context.Context, string) ([]store.Invocation, error) {
	return nil, nil
}
func (*gatherReadOnlyStore) GateResults(context.Context, string, store.GatePhase, string) ([]store.GateResult, error) {
	return nil, nil
}
func (s *gatherReadOnlyStore) SaveGateResults(context.Context, []store.GateResult) error {
	s.saveGates++
	return nil
}
