package factory_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// TestStartAgentPersistsAReadOnlyPacketAndRecoverableSurfaceState verifies the
// high-level launch seam without depending on Docker or a live cmux process.
func TestStartAgentPersistsAReadOnlyPacketAndRecoverableSurfaceState(t *testing.T) {
	service, runStore, runtime, terminalRuntime, harnessRuntime := newAgentService(t)
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	if launch.Invocation.ID == "" || launch.Invocation.WorkspaceID != "workspace-run" || launch.Invocation.ImplementationSurfaceID != "surface-implementation" {
		t.Fatalf("launch invocation = %#v, want recoverable workspace and surface handles", launch.Invocation)
	}
	if len(runtime.starts) != 1 || runtime.starts[0].InvocationPath == "" || runtime.starts[0].ResultPath == "" {
		t.Fatalf("worker starts = %#v, want packet and result mounts", runtime.starts)
	}
	packetData, err := os.ReadFile(filepath.Join(runtime.starts[0].InvocationPath, "specification.json"))
	if err != nil {
		t.Fatalf("read invocation packet: %v", err)
	}
	var packet factory.InvocationPacket
	if err := json.Unmarshal(packetData, &packet); err != nil {
		t.Fatalf("decode invocation packet: %v", err)
	}
	if packet.InvocationID != launch.Invocation.ID || packet.SpecificationPacket == "" || packet.PromptVersion != workflow.PromptVersionImplementation || packet.TestPolicyMode != config.TestModeRequired {
		t.Fatalf("invocation packet = %#v, want frozen identity and prompt version", packet)
	}
	if len(terminalRuntime.notifications) != 1 || len(harnessRuntime.starts) != 1 {
		t.Fatalf("terminal notifications = %#v, harness starts = %#v", terminalRuntime.notifications, harnessRuntime.starts)
	}
	if len(runStore.invocations) != 1 || runStore.invocations[launch.Invocation.ID].Status != store.InvocationStatusActive {
		t.Fatalf("stored invocations = %#v, want active invocation", runStore.invocations)
	}
}

// TestAdvisoryImplementationOwnsTDDAndAcceptsTestEdits verifies advisory mode
// launches implementation directly and accepts behavioral tests as part of the
// implementation handoff without test-stage evidence or an exemption.
func TestAdvisoryImplementationOwnsTDDAndAcceptsTestEdits(t *testing.T) {
	service, runStore, _, _, _ := newAgentService(t)
	run := *runStore.current
	var packet factory.SpecificationPacket
	if err := json.Unmarshal([]byte(run.SpecificationPacket), &packet); err != nil {
		t.Fatalf("decode specification packet: %v", err)
	}
	packet.RepositoryConfig.TestPolicy.Mode = config.TestModeAdvisory
	delete(packet.RepositoryConfig.RoleHarnessDefaults, "test")
	delete(packet.RepositoryConfig.ModelOptions, "test")
	packetData, err := json.Marshal(packet)
	if err != nil {
		t.Fatalf("encode advisory specification packet: %v", err)
	}
	run.SpecificationPacket = string(packetData)
	run.Stage = store.StageImplementation
	run.TestStageSkipped = false
	run.TestExemption = nil
	run.TestHandoff = nil
	run.TestCheckpointSHA = ""
	run.ProtectedTestPaths = nil
	runStore.worktree.state.ChangedPaths = []string{"internal/factory/agent_test.go", "test-support/factory-fixture.go", "internal/factory/agent.go"}
	if err := runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("persist advisory implementation fixture: %v", err)
	}

	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() advisory error = %v", err)
	}
	if launch.Invocation.Role != "implementation" || launch.Invocation.Stage != store.StageImplementation || launch.TestPolicyMode != config.TestModeAdvisory || len(runStore.invocations) != 1 {
		t.Fatalf("advisory launch = %#v, invocations=%d, want one implementation invocation", launch, len(runStore.invocations))
	}
	packetData, err = os.ReadFile(filepath.Join(launch.Invocation.InvocationDirectory, "specification.json"))
	if err != nil {
		t.Fatalf("read advisory invocation packet: %v", err)
	}
	var invocationPacket factory.InvocationPacket
	if err := json.Unmarshal(packetData, &invocationPacket); err != nil {
		t.Fatalf("decode advisory invocation packet: %v", err)
	}
	if invocationPacket.TestPolicyMode != config.TestModeAdvisory || invocationPacket.TestHandoff != nil || invocationPacket.TestExemption != nil || len(invocationPacket.ProtectedTestPaths) != 0 {
		t.Fatalf("advisory invocation packet = %#v, want implementation-owned policy without test disposition", invocationPacket)
	}
	for _, marker := range []string{"Implementation-owned TDD:", "red/green/refactor", "focused behavioral test"} {
		if !strings.Contains(launch.Prompt, marker) {
			t.Fatalf("advisory implementation prompt missing %q:\n%s", marker, launch.Prompt)
		}
	}

	value := report.Report{
		SchemaVersion: report.SchemaVersion,
		InvocationID:  launch.Invocation.ID,
		RunID:         launch.Invocation.RunID,
		Harness:       launch.Invocation.Harness,
		Role:          launch.Invocation.Role,
		Stage:         string(launch.Invocation.Stage),
		Outcome:       report.OutcomeCompleted,
		Summary:       "implementation complete with focused behavior test",
		Handoff: &report.Handoff{
			ChangeSummary:          "implemented behavior and its focused test",
			AcceptanceMapping:      []report.AcceptanceMapping{{Criterion: "behavior", Evidence: "focused test"}},
			ProductionFilesChanged: []string{"internal/factory/agent_test.go", "test-support/factory-fixture.go", "internal/factory/agent.go"},
			FocusedCommands:        []string{"go test ./internal/factory -run TestBehavior"},
		},
		NativeSessionID: "advisory-session",
		ReportedAt:      time.Now().UTC(),
	}
	invalid := value
	invalid.Exemptions = []report.Exemption{report.ExemptionTechnical}
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, invalid); err != nil {
		t.Fatalf("WriteAtomicForInvocation() invalid advisory error = %v", err)
	}
	if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: run.ID, InvocationID: launch.Invocation.ID}); err == nil || !strings.Contains(err.Error(), "test-stage exemptions") {
		t.Fatalf("AcceptAgentReport() invalid advisory error = %v, want test-stage exemption rejection", err)
	}
	invalid.Exemptions = nil
	invalid.Escalations = []report.EscalationCategory{report.EscalationTestDispute}
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, invalid); err != nil {
		t.Fatalf("WriteAtomicForInvocation() invalid advisory dispute error = %v", err)
	}
	if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: run.ID, InvocationID: launch.Invocation.ID}); err == nil || !strings.Contains(err.Error(), "test-stage disputes") {
		t.Fatalf("AcceptAgentReport() invalid advisory dispute error = %v, want test-stage dispute rejection", err)
	}
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, value); err != nil {
		t.Fatalf("WriteAtomicForInvocation() advisory error = %v", err)
	}
	accepted, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: run.ID, InvocationID: launch.Invocation.ID})
	if err != nil {
		t.Fatalf("AcceptAgentReport() advisory error = %v", err)
	}
	if accepted.Invocation.Status != store.InvocationStatusCompleted || runStore.current.TestStageSkipped || runStore.current.TestExemption != nil || runStore.current.TestHandoff != nil {
		t.Fatalf("advisory accepted projection = invocation %#v run %#v, want completed implementation without test disposition", accepted.Invocation, runStore.current)
	}
}

// TestArchitectureRoleUsesItsDeclaredPromptSurfaceAndHandoff verifies the
// non-implementation role seam end to end without granting it implementation
// paths or making the coordinator special-case its report projection.
func TestArchitectureRoleUsesItsDeclaredPromptSurfaceAndHandoff(t *testing.T) {
	service, runStore, _, _, harnessRuntime := newAgentService(t)
	run := *runStore.current
	run.Stage = store.StageImplementation
	run.TestStageSkipped = true
	runStore.worktree.state.ChangedPaths = []string{"docs/architecture/issue-34.md"}
	if err := runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("persist architecture fixture: %v", err)
	}

	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{
		Role:  workflow.RoleArchitecture,
		Stage: workflow.StageArchitecture,
	})
	if err != nil {
		t.Fatalf("StartAgent(architecture) error = %v", err)
	}
	if launch.Invocation.PromptVersion != workflow.PromptVersionArchitecture || len(launch.Invocation.PermittedPaths) != 1 || launch.Invocation.PermittedPaths[0] != "docs/architecture" {
		t.Fatalf("architecture invocation = %#v, want declared prompt and path policy", launch.Invocation)
	}
	if len(harnessRuntime.starts) != 1 || harnessRuntime.starts[0].Surface.Name != workflow.RoleArchitecture || harnessRuntime.starts[0].Surface.ID != "" {
		t.Fatalf("architecture harness surface = %#v, want a fresh role-owned surface", harnessRuntime.starts[0].Surface)
	}

	value := report.Report{
		SchemaVersion: report.SchemaVersion,
		InvocationID:  launch.Invocation.ID,
		RunID:         launch.Invocation.RunID,
		Harness:       launch.Invocation.Harness,
		Role:          workflow.RoleArchitecture,
		Stage:         string(workflow.StageArchitecture),
		Outcome:       report.OutcomeCompleted,
		Summary:       "architecture design recorded",
		Handoff: &report.Handoff{
			ChangeSummary: "recorded the architecture design",
			AcceptanceMapping: []report.AcceptanceMapping{{
				Criterion: "architecture role produces a design document",
				Evidence:  "docs/architecture/issue-34.md",
			}},
			ProductionFilesChanged: []string{"docs/architecture/issue-34.md"},
			FocusedCommands:        []string{"test -f docs/architecture/issue-34.md"},
		},
		NativeSessionID: "session-architecture",
		ReportedAt:      time.Now().UTC(),
	}
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, value); err != nil {
		t.Fatalf("write architecture report: %v", err)
	}
	accepted, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{InvocationID: launch.Invocation.ID})
	if err != nil {
		t.Fatalf("AcceptAgentReport(architecture) error = %v", err)
	}
	if accepted.Invocation.Status != store.InvocationStatusCompleted || accepted.Invocation.Role != workflow.RoleArchitecture {
		t.Fatalf("accepted architecture invocation = %#v, want completed architecture role", accepted.Invocation)
	}
	if runStore.current.Stage != store.StageImplementation || runStore.current.Status != store.StatusActive {
		t.Fatalf("run after architecture handoff = stage %q/status %q, want implementation/active", runStore.current.Stage, runStore.current.Status)
	}
}

// TestStartAgentRejectsAnUndeclaredRoleWithAStablePolicyCode verifies the
// coordinator refuses role identities before opening a run or creating effects.
func TestStartAgentRejectsAnUndeclaredRoleWithAStablePolicyCode(t *testing.T) {
	service, _, _, _, _ := newAgentService(t)
	_, err := service.StartAgent(context.Background(), factory.AgentRequest{Role: "custom", Stage: store.StageImplementation})
	var rejection *factory.PolicyRejection
	if !errors.As(err, &rejection) {
		t.Fatalf("StartAgent() error = %v, want PolicyRejection", err)
	}
	if rejection.Code != factory.PolicyRejectionRoleUnavailable {
		t.Fatalf("policy rejection code = %q, want %q", rejection.Code, factory.PolicyRejectionRoleUnavailable)
	}
}

// TestManualResumeRequiresAttachBeforeAnotherRecoveryCommand verifies an
// explicit native resume is durable, does not consume the automatic ceiling,
// and remains gated until the operator acknowledges the visible session.
func TestManualResumeRequiresAttachBeforeAnotherRecoveryCommand(t *testing.T) {
	service, runStore, runtime, _, harnessRuntime := newAgentService(t)
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}

	invocation := runStore.invocations[launch.Invocation.ID]
	invocation.NativeSessionID = "session-original"
	if err := runStore.SaveInvocation(context.Background(), invocation); err != nil {
		t.Fatalf("persist native session identity: %v", err)
	}
	workerRequest := runtime.starts[0]
	runtime.inspectResult = &worker.Inspection{
		Exists:           true,
		Running:          true,
		Image:            workerRequest.Image + "@" + workerRequest.ImageDigest,
		MountFingerprint: worker.MountContractFingerprint(workerRequest),
	}

	resumed, err := service.Resume(context.Background(), factory.ResumeRequest{RunID: launch.Invocation.RunID})
	var gateErr *factory.ManualResumeRequiredError
	if !errors.As(err, &gateErr) {
		t.Fatalf("Resume() error = %v, want ManualResumeRequiredError", err)
	}
	if !resumed.WaitingForAttach || resumed.Invocation.RecoveryResumeCount != 0 || !resumed.Invocation.AttachRequired {
		t.Fatalf("manual resume result = %#v, want attach gate without automatic budget use", resumed)
	}
	if resumed.Run.Status != store.StatusWaitingForHuman {
		t.Fatalf("run after manual resume = %q, want waiting_for_human", resumed.Run.Status)
	}
	if len(harnessRuntime.resumes) != 1 {
		t.Fatalf("native resume calls = %d, want one", len(harnessRuntime.resumes))
	}

	if _, err := service.Resume(context.Background(), factory.ResumeRequest{RunID: launch.Invocation.RunID}); !errors.As(err, &gateErr) {
		t.Fatalf("second Resume() error = %v, want the durable attach gate", err)
	}
	if len(harnessRuntime.resumes) != 1 {
		t.Fatalf("native resume calls after gated retry = %d, want no duplicate", len(harnessRuntime.resumes))
	}

	attached, err := service.Attach(context.Background(), factory.AttachRequest{RunID: launch.Invocation.RunID})
	if err != nil {
		t.Fatalf("Attach() error = %v", err)
	}
	if attached.Run.Status != store.StatusActive || attached.Invocation.AttachRequired {
		t.Fatalf("attach result = %#v, want active run with cleared gate", attached)
	}
	if got := runStore.invocations[launch.Invocation.ID]; got.AttachRequired {
		t.Fatalf("persisted invocation after attach = %#v, want cleared gate", got)
	}
}

// TestStartAgentRejectsADuplicateActiveInvocation verifies restart-safe
// coordination prevents a second visible session for one active run.
func TestStartAgentRejectsADuplicateActiveInvocation(t *testing.T) {
	service, runStore, runtime, _, _ := newAgentService(t)
	if _, err := service.StartAgent(context.Background(), factory.AgentRequest{}); err != nil {
		t.Fatalf("first StartAgent() error = %v", err)
	}
	if _, err := service.StartAgent(context.Background(), factory.AgentRequest{}); err == nil {
		t.Fatal("second StartAgent() succeeded with an active invocation")
	}
	if len(runtime.starts) != 1 || len(runStore.invocations) != 1 {
		t.Fatalf("duplicate launch side effects: starts=%d invocations=%d", len(runtime.starts), len(runStore.invocations))
	}
}

// TestStartAgentAllowsAClaimedRunAfterCoordinatorRestart verifies the clean
// claim-to-first-invocation handoff through a freshly opened coordinator.
func TestStartAgentAllowsAClaimedRunAfterCoordinatorRestart(t *testing.T) {
	_, runStore, runtime, terminalRuntime, harnessRuntime := newAgentService(t)
	run := *runStore.current
	worktree := &inspectingWorktree{
		fakeWorktree: fakeWorktree{workspace: gitadapter.Workspace{Worktree: run.Worktree}},
		state:        gitadapter.WorktreeState{RepositoryPath: run.RepositoryPath, Branch: run.Branch, HeadSHA: run.CheckpointSHA},
	}
	fresh := newFreshAgentService(t, runStore, worktree, runtime, terminalRuntime, harnessRuntime)

	launch, err := fresh.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("fresh StartAgent() error = %v, want clean claim handoff", err)
	}
	if launch.Invocation.ID == "" || len(runtime.starts) != 1 || len(runStore.invocations) != 1 {
		t.Fatalf("fresh launch = %#v, worker starts = %#v, invocations = %#v", launch, runtime.starts, runStore.invocations)
	}
}

// TestTransitionRejectsAChangedCheckCheckpointWithoutABaseline verifies a
// failed check cannot re-enter implementation with an unproven checkpoint.
func TestTransitionRejectsAChangedCheckCheckpointWithoutABaseline(t *testing.T) {
	service, runStore, _, _, _ := newAgentService(t)
	runStore.current.Stage = store.StageCheck
	runStore.current.CheckpointSHA = factoryGateCheckpoint

	if _, err := service.Transition(context.Background(), factory.TransitionRequest{Stage: store.StageImplementation, Status: store.StatusActive}); err == nil || !strings.Contains(err.Error(), "without complete baseline results") {
		t.Fatalf("Transition() error = %v, want incomplete-baseline refusal", err)
	}
	if runStore.current.Stage != store.StageCheck {
		t.Fatalf("run stage = %q, want unchanged check stage", runStore.current.Stage)
	}
}

// TestStartAgentPersistsInvocationBeforeStartingTheWorker verifies the
// durable invocation identity is published before any worker effect begins.
func TestStartAgentPersistsInvocationBeforeStartingTheWorker(t *testing.T) {
	service, runStore, runtime, _, _ := newAgentService(t)
	events := []string{}
	runStore.events = &events
	runtime.events = &events

	if _, err := service.StartAgent(context.Background(), factory.AgentRequest{}); err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	persistedAt := eventIndex(events, "persist invocation")
	startedAt := eventIndex(events, "start worker")
	if persistedAt < 0 || startedAt < 0 || persistedAt > startedAt {
		t.Fatalf("effect order = %#v, want invocation persistence before worker start", events)
	}
}

// TestStartAgentRefusesARecoveryDiscrepancyBeforeEffects verifies a fresh
// coordinator returns the typed recovery refusal without starting any effect.
func TestStartAgentRefusesARecoveryDiscrepancyBeforeEffects(t *testing.T) {
	_, runStore, runtime, terminalRuntime, harnessRuntime := newAgentService(t)
	run := *runStore.current
	worktree := &inspectingWorktree{
		fakeWorktree: fakeWorktree{workspace: gitadapter.Workspace{Worktree: run.Worktree}},
		state:        gitadapter.WorktreeState{RepositoryPath: run.RepositoryPath, Branch: run.Branch, HeadSHA: "different-checkpoint"},
	}
	fresh := newFreshAgentService(t, runStore, worktree, runtime, terminalRuntime, harnessRuntime)

	_, err := fresh.StartAgent(context.Background(), factory.AgentRequest{})
	var recoveryErr *factory.RecoveryRequiredError
	if !errors.As(err, &recoveryErr) {
		t.Fatalf("fresh StartAgent() error = %v, want RecoveryRequiredError", err)
	}
	if len(runtime.starts) != 0 || len(terminalRuntime.notifications) != 0 || len(harnessRuntime.starts) != 0 || len(runStore.invocations) != 0 {
		t.Fatalf("recovery refusal effects: workers=%d notifications=%d harnesses=%d invocations=%d, want none", len(runtime.starts), len(terminalRuntime.notifications), len(harnessRuntime.starts), len(runStore.invocations))
	}
}

// TestStartAgentRefusesAnyPreviouslyPersistedInvocation verifies that a
// terminal or non-terminal invocation keeps a fresh coordinator behind #24.
func TestStartAgentRefusesAnyPreviouslyPersistedInvocation(t *testing.T) {
	statuses := []store.InvocationStatus{
		store.InvocationStatusActive,
		store.InvocationStatusCompleted,
		store.InvocationStatusWaitingForHuman,
		store.InvocationStatusCannotProceed,
	}
	for _, status := range statuses {
		t.Run(string(status), func(t *testing.T) {
			_, runStore, runtime, terminalRuntime, harnessRuntime := newAgentService(t)
			run := *runStore.current
			if err := runStore.SaveInvocation(context.Background(), store.Invocation{
				ID: "inv-existing", RunID: run.ID, Harness: "codex", Role: "implementation", Stage: store.StageClaim,
				Status: status,
			}); err != nil {
				t.Fatalf("SaveInvocation() error = %v", err)
			}
			worktree := &inspectingWorktree{
				fakeWorktree: fakeWorktree{workspace: gitadapter.Workspace{Worktree: run.Worktree}},
				state:        gitadapter.WorktreeState{RepositoryPath: run.RepositoryPath, Branch: run.Branch, HeadSHA: run.CheckpointSHA},
			}
			fresh := newFreshAgentService(t, runStore, worktree, runtime, terminalRuntime, harnessRuntime)

			_, err := fresh.StartAgent(context.Background(), factory.AgentRequest{})
			var recoveryErr *factory.RecoveryRequiredError
			if !errors.As(err, &recoveryErr) {
				t.Fatalf("fresh StartAgent() error = %v, want RecoveryRequiredError", err)
			}
			if len(runtime.starts) != 0 || len(terminalRuntime.notifications) != 0 || len(harnessRuntime.starts) != 0 {
				t.Fatalf("existing invocation effects: workers=%d notifications=%d harnesses=%d, want none", len(runtime.starts), len(terminalRuntime.notifications), len(harnessRuntime.starts))
			}
		})
	}
}

// TestStartAgentAppliesTheExceptionOnlyToAClaimStage verifies implementation
// stage runs still use the #24 startup guard even without an invocation.
func TestStartAgentAppliesTheExceptionOnlyToAClaimStage(t *testing.T) {
	_, runStore, runtime, terminalRuntime, harnessRuntime := newAgentService(t)
	runStore.current.Stage = store.StageImplementation
	runStore.current.TestStageSkipped = false
	run := *runStore.current
	worktree := &inspectingWorktree{
		fakeWorktree: fakeWorktree{workspace: gitadapter.Workspace{Worktree: run.Worktree}},
		state:        gitadapter.WorktreeState{RepositoryPath: run.RepositoryPath, Branch: run.Branch, HeadSHA: run.CheckpointSHA},
	}
	fresh := newFreshAgentService(t, runStore, worktree, runtime, terminalRuntime, harnessRuntime)

	_, err := fresh.StartAgent(context.Background(), factory.AgentRequest{})
	var recoveryErr *factory.RecoveryRequiredError
	if !errors.As(err, &recoveryErr) {
		t.Fatalf("fresh StartAgent() error = %v, want RecoveryRequiredError", err)
	}
	if len(runtime.starts) != 0 || len(terminalRuntime.notifications) != 0 || len(harnessRuntime.starts) != 0 {
		t.Fatalf("non-claim effects: workers=%d notifications=%d harnesses=%d, want none", len(runtime.starts), len(terminalRuntime.notifications), len(harnessRuntime.starts))
	}
}

// TestStartAgentVoidsASetupFailure verifies a launch without a native session
// or structured report is superseded while its worker and surface are rolled
// back.
func TestStartAgentVoidsASetupFailure(t *testing.T) {
	service, runStore, runtime, terminalRuntime, harnessRuntime := newAgentService(t)
	harnessRuntime.startErr = errors.New("harness unavailable")
	if _, err := service.StartAgent(context.Background(), factory.AgentRequest{}); err == nil {
		t.Fatal("StartAgent() succeeded while the harness was unavailable")
	} else if !strings.Contains(err.Error(), "use `/factory retry`") {
		t.Fatalf("StartAgent() error = %v, want retry guidance", err)
	}
	invocation, ok := runStore.invocations["inv-generated"]
	if !ok || invocation.Status != store.InvocationStatusSuperseded {
		t.Fatalf("rolled-back invocation = %#v, want superseded", invocation)
	}
	if runtime.stops != 1 || len(terminalRuntime.closed) != 1 || terminalRuntime.closed[0] != "surface-implementation" {
		t.Fatalf("rollback side effects: stops=%d closed=%v", runtime.stops, terminalRuntime.closed)
	}
}

// TestStartAgentPreservesAnIndeterminateReportPath verifies a launch is kept
// protected when the coordinator cannot determine whether report.json exists.
func TestStartAgentPreservesAnIndeterminateReportPath(t *testing.T) {
	service, runStore, runtime, _, harnessRuntime := newAgentService(t)
	runtime.startHook = func(request worker.StartRequest) {
		if err := os.Remove(request.ResultPath); err != nil {
			t.Fatalf("remove result directory: %v", err)
		}
		if err := os.WriteFile(request.ResultPath, []byte("result path is not a directory"), 0o600); err != nil {
			t.Fatalf("replace result directory with a file: %v", err)
		}
	}
	harnessRuntime.startErr = errors.New("harness unavailable")
	if _, err := service.StartAgent(context.Background(), factory.AgentRequest{}); err == nil {
		t.Fatal("StartAgent() succeeded while the harness was unavailable")
	} else if strings.Contains(err.Error(), "use `/factory retry`") {
		t.Fatalf("StartAgent() exposed retry guidance for an indeterminate report path: %v", err)
	}
	invocation, ok := runStore.invocations["inv-generated"]
	if !ok || invocation.Status != store.InvocationStatusCannotProceed || invocation.LaunchVoided {
		t.Fatalf("indeterminate launch = %#v, want protected cannot_proceed invocation", invocation)
	}
}

// TestStartAgentPreservesAnIndeterminatePendingEffect verifies a launch is kept
// protected when the durable journal cannot prove the run reserved no effect.
func TestStartAgentPreservesAnIndeterminatePendingEffect(t *testing.T) {
	_, runStore, runtime, terminalRuntime, harnessRuntime := newAgentService(t)
	run := *runStore.current
	worktree := &inspectingWorktree{
		fakeWorktree: fakeWorktree{workspace: gitadapter.Workspace{Worktree: run.Worktree}},
		state:        gitadapter.WorktreeState{RepositoryPath: run.RepositoryPath, Branch: run.Branch, HeadSHA: run.CheckpointSHA},
	}
	journal := &journaledAgentRunStore{agentRunStore: runStore, armPendingErr: errors.New("effect journal unavailable")}
	fresh := newFreshAgentServiceWithStore(t, runStore, journal, worktree, runtime, terminalRuntime, harnessRuntime)
	harnessRuntime.startErr = errors.New("prompt exceeds launch limit")

	_, err := fresh.StartAgent(context.Background(), factory.AgentRequest{})
	if err == nil {
		t.Fatal("StartAgent() succeeded while the harness launch was unavailable")
	}
	if !strings.Contains(err.Error(), "invocation kept protected") {
		t.Fatalf("StartAgent() error = %v, want the recorded pending-effect discrepancy", err)
	}
	if strings.Contains(err.Error(), "use `/factory retry`") {
		t.Fatalf("StartAgent() exposed retry guidance for an indeterminate effect journal: %v", err)
	}
	invocation, ok := runStore.invocations["inv-generated"]
	if !ok || invocation.Status != store.InvocationStatusCannotProceed || invocation.LaunchVoided {
		t.Fatalf("indeterminate launch = %#v, want protected cannot_proceed invocation", invocation)
	}
}

// TestHandleCommandRetriesAWaitingRunAfterFailedLaunch verifies a launch that
// never produced a session can be retried without changing the specification
// packet or repeating the upstream stage.
func TestHandleCommandRetriesAWaitingRunAfterFailedLaunch(t *testing.T) {
	service, runStore, _, _, harnessRuntime := newAgentService(t)
	harnessRuntime.startErr = errors.New("prompt exceeds launch limit")
	if _, err := service.StartAgent(context.Background(), factory.AgentRequest{}); err == nil {
		t.Fatal("StartAgent() succeeded while the harness launch was unavailable")
	}

	failed := runStore.invocations["inv-generated"]
	if failed.Status != store.InvocationStatusSuperseded {
		t.Fatalf("failed invocation = %#v, want superseded before retry", failed)
	}
	run := *runStore.current
	run.Status = store.StatusWaitingForHuman
	run.ActiveInvocationIDs = nil
	run.LifecycleReason = "unattended progression stopped at start agent: harness launch produced no native session or structured report; use /factory retry"
	packetBeforeRetry := run.SpecificationPacket
	if err := runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("persist waiting launch failure: %v", err)
	}

	result, err := service.HandleCommand(context.Background(), factory.CommandRequest{
		IssueNumber: run.IssueNumber,
		Comment:     github.Comment{ID: "retry-failed-launch", Author: "alice", Body: "/factory retry"},
	})
	if err != nil {
		t.Fatalf("HandleCommand() error = %v", err)
	}
	if result.Outcome != factory.CommandAccepted || result.Run.Status != store.StatusActive {
		t.Fatalf("retry result = %#v, want accepted active run", result)
	}
	if result.Run.SpecificationPacket != packetBeforeRetry {
		t.Fatalf("specification packet changed during retry, want %q", packetBeforeRetry)
	}
	if got := runStore.invocations[failed.ID].Status; got != store.InvocationStatusSuperseded {
		t.Fatalf("failed invocation status = %q, want superseded", got)
	}

	harnessRuntime.startErr = nil
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() after retry error = %v", err)
	}
	if launch.Invocation.ID == failed.ID || launch.Invocation.Role != failed.Role || launch.Invocation.Stage != failed.Stage {
		t.Fatalf("recovered invocation = %#v, want a fresh %s/%s invocation", launch.Invocation, failed.Role, failed.Stage)
	}
}

// TestHandleCommandDoesNotVoidAReportedLaunch verifies a waiting run cannot
// use retry to discard a structured report merely because its invocation has
// no native session identity.
func TestHandleCommandDoesNotVoidAReportedLaunch(t *testing.T) {
	service, runStore, _, _, _ := newAgentService(t)
	run := *runStore.current
	resultDirectory := filepath.Join(t.TempDir(), "results")
	invocation := store.Invocation{
		ID:              "inv-reported-launch",
		RunID:           run.ID,
		Harness:         "codex",
		Role:            "implementation",
		Stage:           store.StageImplementation,
		ResultDirectory: resultDirectory,
		Status:          store.InvocationStatusCannotProceed,
		CreatedAt:       run.UpdatedAt,
		UpdatedAt:       run.UpdatedAt.Add(time.Minute),
	}
	if err := runStore.SaveInvocation(context.Background(), invocation); err != nil {
		t.Fatalf("SaveInvocation() error = %v", err)
	}
	reported := report.Report{
		SchemaVersion: report.SchemaVersion,
		InvocationID:  invocation.ID,
		RunID:         invocation.RunID,
		Harness:       invocation.Harness,
		Role:          invocation.Role,
		Stage:         string(invocation.Stage),
		Outcome:       report.OutcomeCannotProceed,
		Summary:       "launch evidence is available",
		Evidence:      []report.Evidence{{Kind: "harness", Detail: "launch diagnostic"}},
		ReportedAt:    time.Now().UTC(),
	}
	if _, err := report.WriteAtomicForInvocation(invocation.ResultDirectory, invocation.ID, reported); err != nil {
		t.Fatalf("WriteAtomicForInvocation() error = %v", err)
	}
	run.Status = store.StatusWaitingForHuman
	run.ActiveInvocationIDs = nil
	run.LifecycleReason = "unattended progression stopped at start agent"
	if err := runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("persist waiting reported launch: %v", err)
	}

	result, err := service.HandleCommand(context.Background(), factory.CommandRequest{
		IssueNumber: run.IssueNumber,
		Comment:     github.Comment{ID: "retry-reported-launch", Author: "alice", Body: "/factory retry"},
	})
	var rejection *factory.PolicyRejection
	if !errors.As(err, &rejection) || rejection.Code != factory.PolicyRejectionRetryState {
		t.Fatalf("HandleCommand() error = %v, want retry_state rejection", err)
	}
	if result.Outcome != factory.CommandRejected || result.Run.Status != store.StatusWaitingForHuman {
		t.Fatalf("retry result = %#v, want rejected waiting run", result)
	}
	if got := runStore.invocations[invocation.ID].Status; got != store.InvocationStatusCannotProceed {
		t.Fatalf("reported invocation status = %q, want unchanged cannot_proceed", got)
	}
}

// TestAcceptAgentReportUsesOnlyTheStructuredReportAndFinishesTheSession verifies
// that a valid completion changes state only after report validation.
func TestAcceptAgentReportUsesOnlyTheStructuredReportAndFinishesTheSession(t *testing.T) {
	service, runStore, runtime, _, harnessRuntime := newAgentService(t)
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	value := report.Report{
		SchemaVersion: report.SchemaVersion,
		InvocationID:  launch.Invocation.ID,
		RunID:         launch.Invocation.RunID,
		Harness:       launch.Invocation.Harness,
		Role:          launch.Invocation.Role,
		Stage:         string(launch.Invocation.Stage),
		Outcome:       report.OutcomeCompleted,
		Summary:       "implementation complete",
		Handoff: &report.Handoff{
			ChangeSummary:          "implemented behavior",
			AcceptanceMapping:      []report.AcceptanceMapping{{Criterion: "criterion", Evidence: "focused test"}},
			ProductionFilesChanged: []string{"internal/factory/agent.go"},
			FocusedCommands:        []string{"go test ./internal/factory"},
		},
		NativeSessionID: "session-1",
		ReportedAt:      time.Now().UTC(),
	}
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, value); err != nil {
		t.Fatalf("WriteAtomicForInvocation() error = %v", err)
	}
	accepted, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{InvocationID: launch.Invocation.ID})
	if err != nil {
		t.Fatalf("AcceptAgentReport() error = %v", err)
	}
	if accepted.Report.Outcome != report.OutcomeCompleted || accepted.Invocation.Status != store.InvocationStatusCompleted {
		t.Fatalf("accepted result = %#v, want completed report and invocation", accepted)
	}
	if runStore.runs[launch.Invocation.RunID].Status != store.StatusActive || len(harnessRuntime.finished) != 1 || harnessRuntime.finished[0].Surface.ID != "surface-implementation" {
		t.Fatalf("run state = %#v, finished = %#v, want active run and recoverable surface", runStore.runs[launch.Invocation.RunID], harnessRuntime.finished)
	}
	if runtime.starts[0].WorktreePath == "" {
		t.Fatal("worker start did not receive the run worktree")
	}
}

// TestAcceptAgentReportPublishesClarificationAndPausesTheRun verifies a
// clarification report becomes visible human work without consuming repair
// budget or leaving the worker running.
func TestAcceptAgentReportPublishesClarificationAndPausesTheRun(t *testing.T) {
	service, runStore, runtime, terminalRuntime, harnessRuntime := newAgentService(t)
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	reportValue := report.Report{
		SchemaVersion: report.SchemaVersion,
		InvocationID:  launch.Invocation.ID,
		RunID:         launch.Invocation.RunID,
		Harness:       launch.Invocation.Harness,
		Role:          launch.Invocation.Role,
		Stage:         string(launch.Invocation.Stage),
		Outcome:       report.OutcomeNeedsClarification,
		Summary:       "product intent is ambiguous",
		Questions: []report.Question{{
			ID:     "clarification-1",
			Prompt: "Should the response use the existing JSON format?",
		}},
		NativeSessionID: "session-clarification",
		ReportedAt:      time.Now().UTC(),
	}
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, reportValue); err != nil {
		t.Fatalf("WriteAtomicForInvocation() error = %v", err)
	}

	accepted, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{InvocationID: launch.Invocation.ID})
	if err != nil {
		t.Fatalf("AcceptAgentReport() error = %v", err)
	}
	if accepted.Invocation.Status != store.InvocationStatusWaitingForHuman {
		t.Fatalf("accepted invocation status = %q, want waiting_for_human", accepted.Invocation.Status)
	}
	paused := runStore.runs[launch.Invocation.RunID]
	if paused.Status != store.StatusWaitingForHuman || len(paused.PendingQuestions) != 1 || paused.PendingQuestions[0].ID != "clarification-1" {
		t.Fatalf("paused run = %#v, want one pending clarification", paused)
	}
	if paused.CheckRepairAttempts != 0 {
		t.Fatalf("check-repair attempts = %d, want clarification pause to consume none", paused.CheckRepairAttempts)
	}
	if len(runStore.github.createdComments) != 2 || !strings.Contains(runStore.github.createdComments[1], "clarification-1") || !strings.Contains(runStore.github.createdComments[1], "Should the response use the existing JSON format?") {
		t.Fatalf("clarification comments = %#v, want one question comment after claim", runStore.github.createdComments)
	}
	if len(runStore.github.editedComments) == 0 || !strings.Contains(runStore.github.editedComments[len(runStore.github.editedComments)-1].body, "clarification-1") {
		t.Fatalf("status comment edits = %#v, want pending question", runStore.github.editedComments)
	}
	if runtime.stops != 1 || len(terminalRuntime.notifications) != 2 || len(harnessRuntime.finished) != 1 {
		t.Fatalf("clarification effects = stops=%d notifications=%d finished=%d, want 1/2/1", runtime.stops, len(terminalRuntime.notifications), len(harnessRuntime.finished))
	}
}

// TestClarificationPublicationCanRetryFromAStatusCommand verifies durable
// publication markers make a GitHub/cmux failure recoverable after the report
// itself has already moved the run to waiting_for_human.
func TestClarificationPublicationCanRetryFromAStatusCommand(t *testing.T) {
	service, runStore, runtime, terminalRuntime, harnessRuntime := newAgentService(t)
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	reportValue := report.Report{
		SchemaVersion:   report.SchemaVersion,
		InvocationID:    launch.Invocation.ID,
		RunID:           launch.Invocation.RunID,
		Harness:         launch.Invocation.Harness,
		Role:            launch.Invocation.Role,
		Stage:           string(launch.Invocation.Stage),
		Outcome:         report.OutcomeNeedsClarification,
		Summary:         "needs product input",
		Questions:       []report.Question{{ID: "clarification-retry", Prompt: "Which behavior is intended?"}},
		NativeSessionID: "session-clarification",
		ReportedAt:      time.Now().UTC(),
	}
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, reportValue); err != nil {
		t.Fatalf("WriteAtomicForInvocation() error = %v", err)
	}
	runStore.github.createCommentErr = errors.New("GitHub temporarily unavailable")
	if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{InvocationID: launch.Invocation.ID}); err == nil {
		t.Fatal("AcceptAgentReport() succeeded while clarification publication failed")
	}
	if runStore.current.ClarificationCommentID != "" || runStore.current.ClarificationNotificationSent {
		t.Fatalf("failed publication markers = %#v, want no completed effects", runStore.current)
	}
	runStore.github.createCommentErr = nil
	result, err := service.HandleCommand(context.Background(), factory.CommandRequest{
		IssueNumber: 6,
		Comment:     github.Comment{ID: "status-retry", Author: "alice", Body: "/factory status"},
	})
	if err != nil {
		t.Fatalf("HandleCommand() retry error = %v", err)
	}
	if result.Outcome != factory.CommandAccepted || runStore.current.ClarificationCommentID == "" || !runStore.current.ClarificationNotificationSent {
		t.Fatalf("retry result = %#v, persisted run = %#v, want published clarification", result, runStore.current)
	}
	if len(runStore.github.createdComments) != 2 || len(terminalRuntime.notifications) != 2 || runtime.stops != 1 || len(harnessRuntime.finished) != 1 {
		t.Fatalf("retry effects = comments=%d notifications=%d stops=%d finished=%d, want 2/2/1/1", len(runStore.github.createdComments), len(terminalRuntime.notifications), runtime.stops, len(harnessRuntime.finished))
	}
}

// TestAnswerCommandVersionsThePacketAndResumesImplementation verifies only an
// authorized answer can resolve a pending question and launch a new packet.
func TestAnswerCommandVersionsThePacketAndResumesImplementation(t *testing.T) {
	service, runStore, runtime, terminalRuntime, harnessRuntime := newAgentService(t)
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	clarification := report.Report{
		SchemaVersion: report.SchemaVersion,
		InvocationID:  launch.Invocation.ID,
		RunID:         launch.Invocation.RunID,
		Harness:       launch.Invocation.Harness,
		Role:          launch.Invocation.Role,
		Stage:         string(launch.Invocation.Stage),
		Outcome:       report.OutcomeNeedsClarification,
		Summary:       "intent is ambiguous",
		Questions: []report.Question{
			{ID: "clarification-1", Prompt: "Which output format should be used?"},
			{ID: "clarification-2", Prompt: "Should the response include examples?"},
		},
		NativeSessionID: "session-clarification",
		ReportedAt:      time.Now().UTC(),
	}
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, clarification); err != nil {
		t.Fatalf("WriteAtomicForInvocation() error = %v", err)
	}
	if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{InvocationID: launch.Invocation.ID}); err != nil {
		t.Fatalf("AcceptAgentReport() error = %v", err)
	}

	result, err := service.HandleCommand(context.Background(), factory.CommandRequest{
		IssueNumber: 6,
		Comment:     github.Comment{ID: "answer-1", Author: "alice", Body: "/factory answer clarification-1 use the existing JSON format"},
	})
	if err != nil {
		t.Fatalf("HandleCommand() error = %v", err)
	}
	if result.Outcome != factory.CommandAccepted || result.Run.Status != store.StatusActive || len(result.Run.PendingQuestions) != 1 || result.Run.PendingQuestions[0].ID != "clarification-2" {
		t.Fatalf("first answer result = %#v, want active run with the second question pending", result)
	}
	if runStore.invocations[launch.Invocation.ID].Status != store.InvocationStatusSuperseded {
		t.Fatalf("old invocation = %#v, want superseded", runStore.invocations[launch.Invocation.ID])
	}
	if len(runtime.starts) != 2 || len(harnessRuntime.starts) != 2 || len(terminalRuntime.notifications) != 3 {
		t.Fatalf("first resume effects = workers=%d harnesses=%d notifications=%d, want 2/2/3", len(runtime.starts), len(harnessRuntime.starts), len(terminalRuntime.notifications))
	}
	result, err = service.HandleCommand(context.Background(), factory.CommandRequest{
		IssueNumber: 6,
		Comment:     github.Comment{ID: "answer-2", Author: "alice", Body: "/factory answer clarification-2 omit examples"},
	})
	if err != nil {
		t.Fatalf("second HandleCommand() error = %v", err)
	}
	if result.Outcome != factory.CommandAccepted || result.Run.Status != store.StatusActive || len(result.Run.PendingQuestions) != 0 {
		t.Fatalf("second answer result = %#v, want active run without pending questions", result)
	}
	if len(runtime.starts) != 3 || len(harnessRuntime.starts) != 3 || len(terminalRuntime.notifications) != 4 {
		t.Fatalf("second resume effects = workers=%d harnesses=%d notifications=%d, want 3/3/4", len(runtime.starts), len(harnessRuntime.starts), len(terminalRuntime.notifications))
	}
	packetData, err := os.ReadFile(filepath.Join(runtime.starts[2].InvocationPath, "specification.json"))
	if err != nil {
		t.Fatalf("read resumed invocation packet: %v", err)
	}
	var invocationPacket factory.InvocationPacket
	if err := json.Unmarshal(packetData, &invocationPacket); err != nil {
		t.Fatalf("decode resumed invocation packet: %v", err)
	}
	var packet factory.SpecificationPacket
	if err := json.Unmarshal([]byte(invocationPacket.SpecificationPacket), &packet); err != nil {
		t.Fatalf("decode resumed specification packet: %v", err)
	}
	if packet.Version != 3 || len(packet.Clarifications) != 2 || packet.Clarifications[0].Answer != "use the existing JSON format" || packet.Clarifications[1].Answer != "omit examples" {
		t.Fatalf("resumed specification packet = %#v, want both versioned answers", packet)
	}
}

// TestRefreshCommandReReadsTheIssueAndInvalidatesCheckpointResults verifies a
// refresh creates a new packet while preserving the independent baseline.
func TestRefreshCommandReReadsTheIssueAndInvalidatesCheckpointResults(t *testing.T) {
	service, runStore, runtime, _, _ := newAgentService(t)
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	if _, err := writeAgentTestReport(t, launch, "internal/factory/agent.go"); err != nil {
		t.Fatalf("writeAgentTestReport() error = %v", err)
	}
	if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{InvocationID: launch.Invocation.ID}); err != nil {
		t.Fatalf("AcceptAgentReport() error = %v", err)
	}
	run := *runStore.current
	runStore.gateResults[run.ID] = append(runStore.gateResults[run.ID], store.GateResult{
		RunID: run.ID, CheckpointSHA: run.CheckpointSHA, Phase: store.GatePhaseCheckpoint,
		Ordinal: 0, GateName: "tests", Outcome: store.GateOutcomePassed,
	})
	runStore.github.issueValue.Body = "refreshed product intent"
	result, err := service.HandleCommand(context.Background(), factory.CommandRequest{
		IssueNumber: 6,
		Comment:     github.Comment{ID: "refresh-1", Author: "alice", Body: "/factory refresh"},
	})
	if err != nil {
		t.Fatalf("HandleCommand() error = %v", err)
	}
	if result.Outcome != factory.CommandAccepted || result.Run.Status != store.StatusActive || len(runtime.starts) != 2 {
		t.Fatalf("refresh result = %#v, worker starts = %d, want accepted active resume", result, len(runtime.starts))
	}
	if runStore.invocations[launch.Invocation.ID].Status != store.InvocationStatusSuperseded {
		t.Fatalf("refreshed invocation = %#v, want superseded", runStore.invocations[launch.Invocation.ID])
	}
	results := runStore.gateResults[run.ID]
	if len(results) != 1 || results[0].Phase != store.GatePhaseBaseline {
		t.Fatalf("remaining gate results = %#v, want baseline only", results)
	}
	var packet factory.SpecificationPacket
	packetData, err := os.ReadFile(filepath.Join(runtime.starts[1].InvocationPath, "specification.json"))
	if err != nil {
		t.Fatalf("read refreshed invocation packet: %v", err)
	}
	var invocationPacket factory.InvocationPacket
	if err := json.Unmarshal(packetData, &invocationPacket); err != nil {
		t.Fatalf("decode refreshed invocation packet: %v", err)
	}
	if err := json.Unmarshal([]byte(invocationPacket.SpecificationPacket), &packet); err != nil {
		t.Fatalf("decode refreshed specification packet: %v", err)
	}
	if packet.Version != 2 || packet.Issue.Body != "refreshed product intent" {
		t.Fatalf("refreshed packet = %#v, want issue snapshot version 2", packet)
	}
}

// TestRefreshAfterAnAcceptedImplementationCheckpointRestartsFromTheCheckpoint
// verifies the exact recovery sequence for an unchanged specification: an
// accepted implementation checkpoint is treated as the new baseline before
// the implementation session is resumed, and its next report remains active.
func TestRefreshAfterAnAcceptedImplementationCheckpointRestartsFromTheCheckpoint(t *testing.T) {
	service, runStore, workspace, workerRuntime, harnessRuntime, pullRequests := newCheckpointRefreshService(t)

	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	if _, err := writeAgentTestReport(t, launch, "internal/factory/agent.go"); err != nil {
		t.Fatalf("write initial implementation report: %v", err)
	}
	if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{InvocationID: launch.Invocation.ID}); err != nil {
		t.Fatalf("AcceptAgentReport() error = %v", err)
	}

	checkpointed, err := service.CreateDraftPullRequest(context.Background(), factory.DraftPullRequestRequest{RunID: launch.Invocation.RunID})
	if err != nil {
		t.Fatalf("CreateDraftPullRequest() error = %v", err)
	}
	if checkpointed.Run.Stage != store.StageDraftPR || checkpointed.Run.CheckpointSHA != implementationCheckpoint {
		t.Fatalf("checkpointed run = %#v, want draft_pr at the implementation checkpoint", checkpointed.Run)
	}
	if len(workspace.state.ChangedPaths) != 0 {
		t.Fatalf("checkpoint worktree changes = %#v, want clean worktree", workspace.state.ChangedPaths)
	}
	pullRequests.existing = checkpointed.PullRequest

	result, err := service.HandleCommand(context.Background(), factory.CommandRequest{
		IssueNumber: runStore.current.IssueNumber,
		Comment:     github.Comment{ID: "refresh-checkpoint", Author: "alice", Body: "/factory refresh"},
	})
	if err != nil {
		t.Fatalf("HandleCommand() refresh error = %v", err)
	}
	if result.Outcome != factory.CommandAccepted || result.Run.Status != store.StatusActive || result.Run.Stage != store.StageImplementation || result.Run.BaseCheckpointSHA != implementationCheckpoint || result.Run.AcceptedImplementationCheckpointSHA != implementationCheckpoint {
		t.Fatalf("refresh result = %#v, want accepted active implementation restart", result)
	}
	if !strings.Contains(result.Run.LifecycleReason, "/factory refresh") || !strings.Contains(factory.StatusCommentBody(result.Run), "/factory refresh") {
		t.Fatalf("refresh lifecycle projection = %q, want the applying command in the status comment", result.Run.LifecycleReason)
	}
	if len(harnessRuntime.resumes) != 1 {
		t.Fatalf("native resumes = %d, want one revision-style implementation resume", len(harnessRuntime.resumes))
	}
	if len(harnessRuntime.starts) != 1 {
		t.Fatalf("fresh harness starts after refresh = %d, want only the initial session", len(harnessRuntime.starts))
	}
	var baselineAtCheckpoint bool
	for _, gateResult := range runStore.gateResults[launch.Invocation.RunID] {
		if gateResult.Phase == store.GatePhaseBaseline && gateResult.CheckpointSHA == implementationCheckpoint {
			baselineAtCheckpoint = true
		}
	}
	if !baselineAtCheckpoint {
		t.Fatalf("gate results = %#v, want a baseline result for the accepted implementation checkpoint", runStore.gateResults[launch.Invocation.RunID])
	}
	if len(workerRuntime.commands) < 4 {
		t.Fatalf("worker commands = %#v, want checkpoint gates and refreshed baseline gates", workerRuntime.commands)
	}

	refreshed := runStore.invocations[result.Run.ActiveInvocationIDs[0]]
	value := report.Report{
		SchemaVersion: report.SchemaVersion,
		InvocationID:  refreshed.ID,
		RunID:         refreshed.RunID,
		Harness:       refreshed.Harness,
		Role:          refreshed.Role,
		Stage:         string(refreshed.Stage),
		Outcome:       report.OutcomeCompleted,
		Summary:       "verified the unchanged issue against the accepted checkpoint",
		Handoff: &report.Handoff{
			ChangeSummary:     "verified the accepted implementation checkpoint",
			AcceptanceMapping: []report.AcceptanceMapping{{Criterion: "unchanged issue", Evidence: "checkpoint baseline and focused verification"}},
			FocusedCommands:   []string{"go test ./internal/factory"},
		},
		NativeSessionID: refreshed.NativeSessionID,
		ReportedAt:      time.Now().UTC(),
	}
	invalid := value
	invalid.Handoff = &report.Handoff{
		ChangeSummary:          value.Handoff.ChangeSummary,
		AcceptanceMapping:      append([]report.AcceptanceMapping(nil), value.Handoff.AcceptanceMapping...),
		ProductionFilesChanged: []string{"internal/factory/agent.go"},
		FocusedCommands:        append([]string(nil), value.Handoff.FocusedCommands...),
	}
	if _, err := report.WriteAtomicForInvocation(refreshed.ResultDirectory, refreshed.ID, invalid); err != nil {
		t.Fatalf("write invalid refreshed implementation report: %v", err)
	}
	if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{InvocationID: refreshed.ID}); err == nil || !strings.Contains(err.Error(), "was not observed in the worktree") {
		t.Fatalf("AcceptAgentReport() invalid refreshed error = %v, want unobserved production path rejection", err)
	}
	paused := *runStore.current
	paused.Status = store.StatusWaitingForHuman
	paused.LifecycleReason = "unattended progression stopped at accept agent report: production_files_changed[0] was not observed in the worktree"
	if err := runStore.SaveRun(context.Background(), paused); err != nil {
		t.Fatalf("save invalid-report waiting state: %v", err)
	}

	secondResult, err := service.HandleCommand(context.Background(), factory.CommandRequest{
		IssueNumber: runStore.current.IssueNumber,
		Comment:     github.Comment{ID: "refresh-checkpoint-again", Author: "alice", Body: "/factory refresh"},
	})
	if err != nil {
		t.Fatalf("HandleCommand() second refresh error = %v", err)
	}
	if secondResult.Outcome != factory.CommandAccepted || secondResult.Run.Status != store.StatusActive || secondResult.Run.Stage != store.StageImplementation || secondResult.Run.BaseCheckpointSHA != implementationCheckpoint || secondResult.Run.AcceptedImplementationCheckpointSHA != implementationCheckpoint {
		t.Fatalf("second refresh result = %#v, want accepted active implementation restart", secondResult)
	}
	if len(harnessRuntime.resumes) != 2 {
		t.Fatalf("native resumes after rejected-report refresh = %d, want two revision-style implementation resumes", len(harnessRuntime.resumes))
	}
	if len(harnessRuntime.starts) != 1 {
		t.Fatalf("fresh harness starts after rejected-report refresh = %d, want only the initial session", len(harnessRuntime.starts))
	}
	secondRefreshed := runStore.invocations[secondResult.Run.ActiveInvocationIDs[0]]
	if secondRefreshed.ID == refreshed.ID {
		t.Fatalf("rejected-report refresh reused invocation %q, want a new resumed invocation", secondRefreshed.ID)
	}
	secondValue := value
	secondValue.InvocationID = secondRefreshed.ID
	secondValue.RunID = secondRefreshed.RunID
	secondValue.Harness = secondRefreshed.Harness
	secondValue.Role = secondRefreshed.Role
	secondValue.Stage = string(secondRefreshed.Stage)
	secondValue.NativeSessionID = secondRefreshed.NativeSessionID
	secondValue.ReportedAt = time.Now().UTC()
	if _, err := report.WriteAtomicForInvocation(secondRefreshed.ResultDirectory, secondRefreshed.ID, secondValue); err != nil {
		t.Fatalf("write second refreshed implementation report: %v", err)
	}
	if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{InvocationID: secondRefreshed.ID}); err != nil {
		t.Fatalf("AcceptAgentReport() second refreshed error = %v", err)
	}
	if runStore.current.Status != store.StatusActive {
		t.Fatalf("run after second refreshed report = %#v, want active rather than waiting", runStore.current)
	}
}

// TestRefreshDoesNotPromoteACleanFailedCheckpoint verifies a checkpoint gate
// failure remains repairable through ordinary implementation refresh rather
// than being mistaken for an accepted checkpoint baseline.
func TestRefreshDoesNotPromoteACleanFailedCheckpoint(t *testing.T) {
	service, runStore, workspace, _, harnessRuntime, _ := newCheckpointRefreshService(t)
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	if _, err := writeAgentTestReport(t, launch, "internal/factory/agent.go"); err != nil {
		t.Fatalf("write initial implementation report: %v", err)
	}
	if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{InvocationID: launch.Invocation.ID}); err != nil {
		t.Fatalf("AcceptAgentReport() error = %v", err)
	}

	run := *runStore.current
	run.Stage = store.StageCheck
	run.Status = store.StatusActive
	run.CheckpointSHA = implementationCheckpoint
	run.BaseCheckpointSHA = factoryGateCheckpoint
	run.LifecycleReason = "checkpoint gate failure"
	workspace.state.HeadSHA = implementationCheckpoint
	workspace.state.ChangedPaths = nil
	runStore.gateResults[run.ID] = append(runStore.gateResults[run.ID], store.GateResult{
		RunID: run.ID, CheckpointSHA: implementationCheckpoint, Phase: store.GatePhaseCheckpoint,
		Ordinal: 0, GateName: "test", Outcome: store.GateOutcomeFailed,
		Status: string(github.CommitStatusFailure), Blocking: true,
	})
	if err := runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun() failed-checkpoint fixture error = %v", err)
	}

	result, err := service.HandleCommand(context.Background(), factory.CommandRequest{
		IssueNumber: run.IssueNumber,
		Comment:     github.Comment{ID: "refresh-failed-checkpoint", Author: "alice", Body: "/factory refresh"},
	})
	if err != nil {
		t.Fatalf("HandleCommand() refresh error = %v", err)
	}
	if result.Outcome != factory.CommandAccepted || result.Run.Status != store.StatusActive || result.Run.Stage != store.StageImplementation {
		t.Fatalf("refresh result = %#v, want ordinary active implementation restart", result)
	}
	if result.Run.BaseCheckpointSHA != factoryGateCheckpoint {
		t.Fatalf("refresh base checkpoint = %q, want the original baseline %q", result.Run.BaseCheckpointSHA, factoryGateCheckpoint)
	}
	if len(harnessRuntime.starts) != 2 || len(harnessRuntime.resumes) != 0 {
		t.Fatalf("harness launches after failed checkpoint refresh = starts=%d resumes=%d, want a fresh repair session", len(harnessRuntime.starts), len(harnessRuntime.resumes))
	}
	for _, gateResult := range runStore.gateResults[run.ID] {
		if gateResult.Phase == store.GatePhaseBaseline && gateResult.CheckpointSHA == implementationCheckpoint {
			t.Fatalf("gate results = %#v, want no baseline promotion of failed checkpoint", runStore.gateResults[run.ID])
		}
	}
}

// TestAcceptAgentReportTrustsThePersistedNativeSession verifies a report
// cannot replace the native session identity already bound to the invocation.
func TestAcceptAgentReportTrustsThePersistedNativeSession(t *testing.T) {
	service, runStore, _, _, harnessRuntime := newAgentService(t)
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	persisted := runStore.invocations[launch.Invocation.ID]
	persisted.NativeSessionID = "session-persisted"
	runStore.invocations[launch.Invocation.ID] = persisted
	path, err := writeAgentTestReport(t, launch, "internal/factory/agent.go")
	if err != nil {
		t.Fatal(err)
	}
	value, err := report.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	value.NativeSessionID = ""
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, value); err != nil {
		t.Fatalf("WriteAtomicForInvocation() error = %v", err)
	}

	accepted, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{InvocationID: launch.Invocation.ID})
	if err != nil {
		t.Fatalf("AcceptAgentReport() error = %v", err)
	}
	if accepted.Invocation.NativeSessionID != "session-persisted" || len(harnessRuntime.finished) != 1 || harnessRuntime.finished[0].NativeSessionID != "session-persisted" {
		t.Fatalf("accepted native session = %#v, finished = %#v, want persisted identity", accepted.Invocation.NativeSessionID, harnessRuntime.finished)
	}
}

// TestAcceptAgentReportRejectsANativeSessionMismatch verifies a supplied
// report identity must agree with the persisted invocation identity.
func TestAcceptAgentReportRejectsANativeSessionMismatch(t *testing.T) {
	service, runStore, _, _, harnessRuntime := newAgentService(t)
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	persisted := runStore.invocations[launch.Invocation.ID]
	persisted.NativeSessionID = "session-persisted"
	runStore.invocations[launch.Invocation.ID] = persisted
	if _, err := writeAgentTestReport(t, launch, "internal/factory/agent.go"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{InvocationID: launch.Invocation.ID}); err == nil || !strings.Contains(err.Error(), "does not match persisted invocation") {
		t.Fatalf("AcceptAgentReport() error = %v, want persisted-session mismatch", err)
	}
	if len(harnessRuntime.finished) != 0 {
		t.Fatalf("finished sessions = %#v, want none after mismatch", harnessRuntime.finished)
	}
}

// TestAcceptAgentReportRejectsAWorktreeCheckpointMismatch verifies report
// acceptance refuses a worktree whose HEAD no longer matches the run claim.
func TestAcceptAgentReportRejectsAWorktreeCheckpointMismatch(t *testing.T) {
	service, runStore, _, _, _ := newAgentService(t)
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	runStore.current.CheckpointSHA = "different-checkpoint"
	if _, err := writeAgentTestReport(t, launch, "internal/factory/agent.go"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{InvocationID: launch.Invocation.ID}); err == nil || !strings.Contains(err.Error(), "does not match checkpoint") {
		t.Fatalf("AcceptAgentReport() error = %v, want checkpoint rejection", err)
	}
	if runStore.invocations[launch.Invocation.ID].Status != store.InvocationStatusActive {
		t.Fatalf("invocation after checkpoint rejection = %#v, want active", runStore.invocations[launch.Invocation.ID])
	}
}

// TestAcceptAgentReportRejectsAProductionPathOutsidePolicy verifies the
// coordinator's persisted permitted-path policy is applied to the handoff.
func TestAcceptAgentReportRejectsAProductionPathOutsidePolicy(t *testing.T) {
	service, runStore, _, _, _ := newAgentService(t)
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{PermittedPaths: []string{"internal/factory"}})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	if _, err := writeAgentTestReport(t, launch, "docs/outside.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{InvocationID: launch.Invocation.ID}); err == nil || !strings.Contains(err.Error(), "outside permitted paths") {
		t.Fatalf("AcceptAgentReport() error = %v, want permitted-path rejection", err)
	}
	if runStore.invocations[launch.Invocation.ID].Status != store.InvocationStatusActive {
		t.Fatalf("invocation after path rejection = %#v, want active", runStore.invocations[launch.Invocation.ID])
	}
}

// TestAcceptAgentReportRejectsAPermittedPathRequestMismatch verifies an
// operator cannot replace the invocation's persisted path policy at accept.
func TestAcceptAgentReportRejectsAPermittedPathRequestMismatch(t *testing.T) {
	service, runStore, _, _, _ := newAgentService(t)
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{PermittedPaths: []string{"internal/factory"}})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	if _, err := writeAgentTestReport(t, launch, "internal/factory/agent.go"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{InvocationID: launch.Invocation.ID, PermittedPaths: []string{"internal/other"}}); err == nil || !strings.Contains(err.Error(), "permitted paths do not match") {
		t.Fatalf("AcceptAgentReport() error = %v, want request-policy rejection", err)
	}
	if runStore.invocations[launch.Invocation.ID].Status != store.InvocationStatusActive {
		t.Fatalf("invocation after request-policy rejection = %#v, want active", runStore.invocations[launch.Invocation.ID])
	}
}

// TestAcceptAgentReportRejectsATerminalInvocation verifies a completed
// invocation cannot be accepted again or finish its harness twice.
func TestAcceptAgentReportRejectsATerminalInvocation(t *testing.T) {
	service, runStore, _, _, _ := newAgentService(t)
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	completed := runStore.invocations[launch.Invocation.ID]
	completed.Status = store.InvocationStatusCompleted
	runStore.invocations[launch.Invocation.ID] = completed
	if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{InvocationID: launch.Invocation.ID}); err == nil || !strings.Contains(err.Error(), "already completed") {
		t.Fatalf("AcceptAgentReport() error = %v, want terminal-invocation rejection", err)
	}
}

// writeAgentTestReport writes a valid completed report with one selected
// production path for the acceptance rejection tests.
func writeAgentTestReport(t *testing.T, launch factory.AgentLaunchResult, productionPath string) (string, error) {
	t.Helper()
	value := report.Report{
		SchemaVersion: report.SchemaVersion,
		InvocationID:  launch.Invocation.ID,
		RunID:         launch.Invocation.RunID,
		Harness:       launch.Invocation.Harness,
		Role:          launch.Invocation.Role,
		Stage:         string(launch.Invocation.Stage),
		Outcome:       report.OutcomeCompleted,
		Summary:       "implementation complete",
		Handoff: &report.Handoff{
			ChangeSummary:          "implemented behavior",
			AcceptanceMapping:      []report.AcceptanceMapping{{Criterion: "criterion", Evidence: "focused test"}},
			ProductionFilesChanged: []string{productionPath},
			FocusedCommands:        []string{"go test ./internal/factory"},
		},
		NativeSessionID: "session-1",
		ReportedAt:      time.Now().UTC(),
	}
	return report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, value)
}

// newAgentService creates one isolated high-level service with fake external
// adapters and a real packet/result filesystem.
func newAgentService(t *testing.T) (*factory.Service, *agentRunStore, *agentWorker, *agentTerminal, *agentHarness) {
	t.Helper()
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	worktreePath := filepath.Join(root, "worktree")
	if err := os.MkdirAll(filepath.Join(repositoryPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryPath, ".git", "HEAD"), []byte("ref: refs/heads/factory/run-agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := validRepositoryConfig()
	policy.RoleHarnessDefaults["test"] = config.HarnessCodex
	policy.ModelOptions["test"] = []string{"gpt-5"}
	policy.TestPolicy.AllowTechnicalExemption = true
	created := time.Date(2026, 8, 21, 8, 0, 0, 0, time.UTC)
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{Path: repositoryPath, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"}, AuthorizedUsers: []string{"alice"}, Polling: config.PollingConfig{Interval: "30s", Backoff: "5m"}, Cmux: config.CmuxConfig{ControlWorkspace: "factory-control"}, OperationalDataPath: filepath.Join(root, "state", "factory.db"), RepositoryConfigPath: filepath.Join(repositoryPath, "factory.yaml")}}}
	runStore := &agentRunStore{runs: map[string]store.Run{}, invocations: map[string]store.Invocation{}, gateResults: map[string][]store.GateResult{}}
	runtime := &agentWorker{}
	terminalRuntime := &agentTerminal{}
	harnessRuntime := &agentHarness{}
	issue := githubIssueFixture()
	issue.Labels = []string{github.LabelAgentReady}
	githubRuntime := &fakeGitHub{issueValue: issue}
	runStore.github = githubRuntime
	worktree := &inspectingWorktree{
		fakeWorktree: fakeWorktree{workspace: gitadapter.Workspace{BaseSHA: "base", Branch: "factory/run-agent", Worktree: worktreePath}},
		state:        gitadapter.WorktreeState{Branch: "factory/run-agent", HeadSHA: "base", ChangedPaths: []string{"internal/factory/agent.go"}},
	}
	runStore.worktree = worktree
	ids := []string{"run-agent", "generated", "generated-2", "generated-3"}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:         &fakeConfig{value: host},
		OpenStore:      func(context.Context, string) (factory.OperationalStore, error) { return runStore, nil },
		LoadRepository: func(string) (config.RepositoryConfig, error) { return policy, nil },
		Worker:         runtime,
		Terminal:       terminalRuntime,
		Harness:        harnessRuntime,
		GitHub:         githubRuntime,
		Now:            func() time.Time { return created },
		NewRunID: func() (string, error) {
			if len(ids) == 0 {
				return "", errors.New("run id fixture exhausted")
			}
			id := ids[0]
			ids = ids[1:]
			return id, nil
		},
		Worktree: worktree,
	})
	if _, err := service.ClaimIssue(context.Background(), 6); err != nil {
		t.Fatalf("ClaimIssue() fixture setup error = %v", err)
	}
	run := *runStore.current
	run.TestStageSkipped = true
	run.TestExemption = &store.TestExemption{Kind: "human", Justification: "implementation seam fixture"}
	if err := runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("persist implementation-only test fixture: %v", err)
	}
	runStore.gateResults[run.ID] = []store.GateResult{{
		RunID: run.ID, CheckpointSHA: run.CheckpointSHA, Phase: store.GatePhaseBaseline,
		Ordinal: 0, GateName: policy.Gates[0].Name, Outcome: store.GateOutcomePassed,
		Status: string(github.CommitStatusSuccess), Blocking: policy.Gates[0].Blocking,
	}}
	return service, runStore, runtime, terminalRuntime, harnessRuntime
}

// newCheckpointRefreshService creates an advisory implementation run with a
// real checkpoint/draft-PR boundary so refresh can be exercised from the same
// clean committed state that caused the production failure.
func newCheckpointRefreshService(t *testing.T) (*factory.Service, *agentRunStore, *draftGitWorkspace, *agentWorker, *agentHarness, *fakePullRequests) {
	t.Helper()
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	worktreePath := filepath.Join(root, "worktree")
	if err := os.MkdirAll(filepath.Join(repositoryPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryPath, ".git", "HEAD"), []byte("ref: refs/heads/factory/run-refresh-checkpoint\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	policy := validRepositoryConfig()
	policy.TestPolicy.Mode = config.TestModeAdvisory
	delete(policy.RoleHarnessDefaults, workflow.RoleTest)
	delete(policy.ModelOptions, workflow.RoleTest)
	runStore := &agentRunStore{runs: map[string]store.Run{}, invocations: map[string]store.Invocation{}, gateResults: map[string][]store.GateResult{}}
	workerRuntime := &agentWorker{}
	terminalRuntime := &agentTerminal{}
	harnessRuntime := &agentHarness{}
	issue := github.Issue{Number: 6, Title: "Unchanged implementation", Body: "The accepted implementation already satisfies this issue.", State: "open", Labels: []string{github.LabelAgentReady}}
	githubRuntime := &fakeGitHubWithPullRequests{fakeGitHub: &fakeGitHub{issueValue: issue}}
	workspace := &draftGitWorkspace{
		workspace: gitadapter.Workspace{BaseSHA: factoryGateCheckpoint, Branch: "factory/run-refresh-checkpoint", Worktree: worktreePath},
		state: gitadapter.WorktreeState{
			RepositoryPath: repositoryPath,
			Branch:         "factory/run-refresh-checkpoint",
			HeadSHA:        factoryGateCheckpoint,
			ChangedPaths:   []string{"internal/factory/agent.go"},
		},
	}
	pullRequests := &fakePullRequests{created: github.PullRequest{
		Number: 17, URL: "https://github.com/example/project/pull/17", State: "open", Draft: true,
		HeadBranch: "factory/run-refresh-checkpoint", BaseBranch: "main",
	}}
	operationalPath := filepath.Join(root, "state", "factory.db")
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{
		Path:                 repositoryPath,
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		AuthorizedUsers:      []string{"alice"},
		OperationalDataPath:  operationalPath,
		RepositoryConfigPath: filepath.Join(repositoryPath, "factory.yaml"),
		Cmux:                 config.CmuxConfig{ControlWorkspace: "factory-control"},
	}}}
	ids := []string{"run-refresh-checkpoint", "initial-implementation", "refreshed-implementation", "second-refreshed-implementation"}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config: &fakeConfig{value: host},
		OpenStore: func(context.Context, string) (factory.OperationalStore, error) {
			return runStore, nil
		},
		LoadRepository: func(string) (config.RepositoryConfig, error) { return policy, nil },
		GitHub:         githubRuntime,
		PullRequests:   pullRequests,
		Worktree:       workspace,
		GitWorkspace:   workspace,
		Worker:         workerRuntime,
		Terminal:       terminalRuntime,
		Harness:        harnessRuntime,
		CommitStatuses: &gateStatuses{},
		Now:            func() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) },
		NewRunID: func() (string, error) {
			if len(ids) == 0 {
				return "", errors.New("refresh checkpoint test identifiers exhausted")
			}
			id := ids[0]
			ids = ids[1:]
			return id, nil
		},
	})
	claimed, err := service.ClaimIssue(context.Background(), issue.Number)
	if err != nil {
		t.Fatalf("ClaimIssue() fixture setup error = %v", err)
	}
	run := claimed.Run
	var packet factory.SpecificationPacket
	if err := json.Unmarshal([]byte(run.SpecificationPacket), &packet); err != nil {
		t.Fatal(err)
	}
	packet.RepositoryConfig.TestPolicy.Mode = config.TestModeAdvisory
	delete(packet.RepositoryConfig.RoleHarnessDefaults, workflow.RoleTest)
	delete(packet.RepositoryConfig.ModelOptions, workflow.RoleTest)
	packetData, err := json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	run.SpecificationPacket = string(packetData)
	run.Stage = store.StageImplementation
	run.Status = store.StatusActive
	run.TestStageSkipped = false
	run.TestExemption = nil
	run.TestHandoff = nil
	runStore.gateResults[run.ID] = []store.GateResult{{
		RunID: run.ID, CheckpointSHA: factoryGateCheckpoint, Phase: store.GatePhaseBaseline,
		Ordinal: 0, GateName: policy.Gates[0].Name, Outcome: store.GateOutcomePassed,
		Status: string(github.CommitStatusSuccess), Blocking: policy.Gates[0].Blocking,
	}}
	if err := runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	return service, runStore, workspace, workerRuntime, harnessRuntime, pullRequests
}

// newFreshAgentService recreates a coordinator around the persisted claim so
// tests exercise the cross-process startup seam rather than cached service state.
func newFreshAgentService(t *testing.T, runStore *agentRunStore, worktree *inspectingWorktree, runtime *agentWorker, terminalRuntime *agentTerminal, harnessRuntime *agentHarness) *factory.Service {
	t.Helper()
	return newFreshAgentServiceWithStore(t, runStore, runStore, worktree, runtime, terminalRuntime, harnessRuntime)
}

// newFreshAgentServiceWithStore rebuilds the same coordinator around an
// explicit operational store, so a journal-capable wrapper can take part in
// the launch seam while the fixture keeps its in-memory run view.
func newFreshAgentServiceWithStore(t *testing.T, runStore *agentRunStore, operational factory.OperationalStore, worktree *inspectingWorktree, runtime *agentWorker, terminalRuntime *agentTerminal, harnessRuntime *agentHarness) *factory.Service {
	t.Helper()
	if runStore.current == nil {
		t.Fatal("fresh coordinator fixture requires a persisted run")
	}
	run := *runStore.current
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{
		Path:                 run.RepositoryPath,
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		Polling:              config.PollingConfig{Interval: "30s", Backoff: "5m"},
		Cmux:                 config.CmuxConfig{ControlWorkspace: "factory-control"},
		OperationalDataPath:  filepath.Join(filepath.Dir(run.RepositoryPath), "state", "factory.db"),
		RepositoryConfigPath: filepath.Join(run.RepositoryPath, "factory.yaml"),
	}}}
	githubRuntime := &fakeGitHub{
		issueValue:    github.Issue{Number: run.IssueNumber, State: "open", Labels: []string{github.LabelAgentRunning}},
		statusComment: github.Comment{ID: run.StatusCommentID, Body: factory.StatusCommentBody(run)},
	}
	return factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:         &fakeConfig{value: host},
		OpenStore:      func(context.Context, string) (factory.OperationalStore, error) { return operational, nil },
		LoadRepository: func(string) (config.RepositoryConfig, error) { return validRepositoryConfig(), nil },
		Worker:         runtime,
		Terminal:       terminalRuntime,
		Harness:        harnessRuntime,
		GitHub:         githubRuntime,
		Worktree:       worktree,
		Now:            func() time.Time { return time.Date(2026, 8, 21, 8, 0, 0, 0, time.UTC) },
		NewRunID:       func() (string, error) { return "generated", nil },
	})
}

// eventIndex returns the first position of one recorded test effect.
func eventIndex(events []string, wanted string) int {
	for index, event := range events {
		if event == wanted {
			return index
		}
	}
	return -1
}

// inspectingWorktree adds the read-only report inspection seam to the shared
// claim-test worktree fake.
type inspectingWorktree struct {
	fakeWorktree
	state gitadapter.WorktreeState
}

// Inspect returns the deterministic state used by report acceptance tests.
func (w *inspectingWorktree) Inspect(context.Context, string) (gitadapter.WorktreeState, error) {
	return w.state, nil
}

// githubIssueFixture returns the frozen issue used by agent tests.
func githubIssueFixture() github.Issue {
	return github.Issue{Number: 6, Title: "Visible implementation", Body: "Repository guidance", State: "open"}
}

// journaledAgentRunStore adds the durable pending-effect journal to the agent
// store fake, so the launch seam can be exercised on the journaled path with
// an injectable lookup failure.
type journaledAgentRunStore struct {
	*agentRunStore
	pending *store.PendingEffect
	// pendingErr fails every journal lookup while it is set.
	pendingErr error
	// armPendingErr becomes pendingErr once one invocation is persisted, so a
	// pre-launch recovery diagnosis still reads a healthy journal.
	armPendingErr error
}

// SaveInvocation persists the invocation and arms the pending journal failure
// the launch rollback must meet.
func (s *journaledAgentRunStore) SaveInvocation(ctx context.Context, invocation store.Invocation) error {
	if err := s.agentRunStore.SaveInvocation(ctx, invocation); err != nil {
		return err
	}
	if s.armPendingErr != nil {
		s.pendingErr, s.armPendingErr = s.armPendingErr, nil
	}
	return nil
}

// PendingEffect returns the reserved effect, or the injected failure that
// leaves the coordinator unable to prove the run reserved nothing.
func (s *journaledAgentRunStore) PendingEffect(context.Context, string) (*store.PendingEffect, error) {
	if s.pendingErr != nil {
		return nil, s.pendingErr
	}
	return s.pending, nil
}

// SavePendingEffect reserves one effect before its external mutation.
func (s *journaledAgentRunStore) SavePendingEffect(_ context.Context, effect store.PendingEffect) error {
	s.pending = &effect
	return nil
}

// ClearPendingEffect releases the reserved effect after it completed.
func (s *journaledAgentRunStore) ClearPendingEffect(context.Context, string, string) error {
	s.pending = nil
	return nil
}

// agentRunStore is an in-memory invocation-capable operational-store fake.
type agentRunStore struct {
	runs            map[string]store.Run
	current         *store.Run
	invocations     map[string]store.Invocation
	gateResults     map[string][]store.GateResult
	lifecycleClaims map[string]map[store.Status]bool
	events          *[]string
	github          *fakeGitHub
	worktree        *inspectingWorktree
}

// CurrentRun returns the configured active run.
func (s *agentRunStore) CurrentRun(context.Context) (*store.Run, error) { return s.current, nil }

// SaveRun stores one updated run state.
func (s *agentRunStore) SaveRun(_ context.Context, run store.Run) error {
	s.runs[run.ID] = run
	s.current = &run
	return nil
}

// ClaimLifecycleNotification atomically claims notification delivery.
func (s *agentRunStore) ClaimLifecycleNotification(_ context.Context, runID string, terminalStatus store.Status) (bool, error) {
	if s.lifecycleClaims == nil {
		s.lifecycleClaims = make(map[string]map[store.Status]bool)
	}
	if s.lifecycleClaims[runID] == nil {
		s.lifecycleClaims[runID] = make(map[store.Status]bool)
	}
	if s.lifecycleClaims[runID][terminalStatus] {
		return false, nil
	}
	s.lifecycleClaims[runID][terminalStatus] = true
	return true, nil
}

// ReleaseLifecycleNotification removes a notification claim.
func (s *agentRunStore) ReleaseLifecycleNotification(_ context.Context, runID string, terminalStatus store.Status) error {
	if s.lifecycleClaims != nil && s.lifecycleClaims[runID] != nil {
		delete(s.lifecycleClaims[runID], terminalStatus)
	}
	return nil
}

// SaveInvocation stores one updated invocation state.
func (s *agentRunStore) SaveInvocation(_ context.Context, invocation store.Invocation) error {
	if s.events != nil {
		*s.events = append(*s.events, "persist invocation")
	}
	s.invocations[invocation.ID] = invocation
	return nil
}

// HasInvocation reports whether any invocation history exists for one run.
func (s *agentRunStore) HasInvocation(_ context.Context, runID string) (bool, error) {
	for _, value := range s.invocations {
		if value.RunID == runID {
			return true, nil
		}
	}
	return false, nil
}

// Invocation loads one invocation only for its owning run.
func (s *agentRunStore) Invocation(_ context.Context, runID, invocationID string) (*store.Invocation, error) {
	value, ok := s.invocations[invocationID]
	if !ok || value.RunID != runID {
		return nil, nil
	}
	return &value, nil
}

// LatestInvocation returns the newest persisted invocation for one run.
func (s *agentRunStore) LatestInvocation(_ context.Context, runID string) (*store.Invocation, error) {
	var latest *store.Invocation
	for _, value := range s.invocations {
		if value.RunID != runID {
			continue
		}
		if latest == nil || value.UpdatedAt.After(latest.UpdatedAt) || (value.UpdatedAt.Equal(latest.UpdatedAt) && value.ID > latest.ID) {
			copy := value
			latest = &copy
		}
	}
	return latest, nil
}

// LatestInvocationByRole returns the newest invocation for one run and role,
// allowing revision tests to verify native implementation-session recovery.
func (s *agentRunStore) LatestInvocationByRole(_ context.Context, runID, role string) (*store.Invocation, error) {
	var latest *store.Invocation
	for _, value := range s.invocations {
		if value.RunID != runID || value.Role != role {
			continue
		}
		if latest == nil || value.UpdatedAt.After(latest.UpdatedAt) || (value.UpdatedAt.Equal(latest.UpdatedAt) && value.ID > latest.ID) {
			copy := value
			latest = &copy
		}
	}
	return latest, nil
}

// ActiveInvocation returns the only active invocation for the fake run.
func (s *agentRunStore) ActiveInvocation(_ context.Context, runID string) (*store.Invocation, error) {
	for _, value := range s.invocations {
		if value.RunID == runID && value.Status == store.InvocationStatusActive {
			copy := value
			return &copy, nil
		}
	}
	return nil, nil
}

// InvalidateRunResults supersedes prior invocations and clears checkpoint
// results when a new specification packet becomes authoritative.
func (s *agentRunStore) InvalidateRunResults(_ context.Context, runID string) error {
	for id, invocation := range s.invocations {
		if invocation.RunID == runID && invocation.Status != store.InvocationStatusSuperseded {
			invocation.Status = store.InvocationStatusSuperseded
			s.invocations[id] = invocation
		}
	}
	results := s.gateResults[runID]
	baseline := make([]store.GateResult, 0, len(results))
	for _, result := range results {
		if result.Phase == store.GatePhaseBaseline {
			baseline = append(baseline, result)
		}
	}
	s.gateResults[runID] = baseline
	return nil
}

// InvalidateAllRunResults supersedes invocation history and removes baseline
// results as an amended specification requires a fresh baseline.
func (s *agentRunStore) InvalidateAllRunResults(_ context.Context, runID string) error {
	for id, invocation := range s.invocations {
		if invocation.RunID == runID && invocation.Status != store.InvocationStatusSuperseded {
			invocation.Status = store.InvocationStatusSuperseded
			s.invocations[id] = invocation
		}
	}
	delete(s.gateResults, runID)
	return nil
}

// SaveGateResults upserts the fixture's exact gate projections by durable identity.
func (s *agentRunStore) SaveGateResults(_ context.Context, results []store.GateResult) error {
	if len(results) == 0 {
		return errors.New("at least one gate result is required")
	}
	for _, incoming := range results {
		runResults := s.gateResults[incoming.RunID]
		replaced := false
		for index, existing := range runResults {
			if existing.RunID == incoming.RunID && existing.CheckpointSHA == incoming.CheckpointSHA && existing.Phase == incoming.Phase && existing.GateName == incoming.GateName {
				runResults[index] = incoming
				replaced = true
				break
			}
		}
		if !replaced {
			runResults = append(runResults, incoming)
		}
		s.gateResults[incoming.RunID] = runResults
	}
	return nil
}

// GateResults returns the fixture's exact phase and checkpoint projection.
func (s *agentRunStore) GateResults(_ context.Context, runID string, phase store.GatePhase, checkpointSHA string) ([]store.GateResult, error) {
	results := s.gateResults[runID]
	filtered := make([]store.GateResult, 0, len(results))
	for _, result := range results {
		if result.Phase == phase && result.CheckpointSHA == checkpointSHA {
			filtered = append(filtered, result)
		}
	}
	return filtered, nil
}

// Close implements the operational-store seam.
func (*agentRunStore) Close() error { return nil }

// agentWorker records the worker start request without running Docker.
type agentWorker struct {
	starts        []worker.StartRequest
	startHook     func(worker.StartRequest)
	stops         int
	commands      []worker.CommandRequest
	results       []worker.CommandResult
	events        *[]string
	interactive   []worker.InteractiveRequest
	codexSeeds    []worker.CredentialSeedRequest
	claudeSeeds   []worker.CredentialSeedRequest
	inspectResult *worker.Inspection
	inspectErr    error
	seedErr       error
}

// Start records one worker start.
func (w *agentWorker) Start(_ context.Context, request worker.StartRequest) error {
	if w.events != nil {
		*w.events = append(*w.events, "start worker")
	}
	w.starts = append(w.starts, request)
	if w.startHook != nil {
		w.startHook(request)
	}
	return nil
}

// Resume implements WorkerRuntime.
func (*agentWorker) Resume(context.Context, worker.ResumeRequest) error { return nil }

// RunCommand implements WorkerRuntime and consumes optional deterministic test
// results in declaration order.
func (w *agentWorker) RunCommand(_ context.Context, request worker.CommandRequest) (worker.CommandResult, error) {
	w.commands = append(w.commands, request)
	if len(w.results) == 0 {
		return worker.CommandResult{}, nil
	}
	result := w.results[0]
	w.results = w.results[1:]
	return result, nil
}

// Stop implements WorkerRuntime.
func (w *agentWorker) Stop(context.Context, string) error {
	w.stops++
	return nil
}

// Inspect implements WorkerRuntime and optionally returns a configured worker
// projection for recovery contract tests.
func (w *agentWorker) Inspect(context.Context, string) (worker.Inspection, error) {
	if w.inspectErr != nil {
		return worker.Inspection{}, w.inspectErr
	}
	if w.inspectResult != nil {
		return *w.inspectResult, nil
	}
	return worker.Inspection{Exists: true, Running: true}, nil
}

// agentTerminal records terminal handles and notifications.
type agentTerminal struct {
	notifications []terminal.Notification
	closed        []terminal.SurfaceID
}

// EnsureControlWorkspace returns the control workspace handle.
func (*agentTerminal) EnsureControlWorkspace(context.Context, terminal.WorkspaceRequest) (terminal.Workspace, error) {
	return terminal.Workspace{ID: "workspace-control"}, nil
}

// EnsureRunWorkspace returns the required run surface handles.
func (*agentTerminal) EnsureRunWorkspace(context.Context, terminal.RunWorkspaceRequest) (terminal.RunWorkspace, error) {
	return terminal.RunWorkspace{Workspace: terminal.Workspace{ID: "workspace-run"}, Status: terminal.Surface{ID: "surface-status"}, Implementation: terminal.Surface{ID: "surface-implementation"}, Checks: terminal.Surface{ID: "surface-checks"}}, nil
}

// CreateSurface implements TerminalRuntime.
func (*agentTerminal) CreateSurface(context.Context, terminal.SurfaceRequest) (terminal.Surface, error) {
	return terminal.Surface{ID: "surface-extra"}, nil
}

// LaunchSurface implements TerminalRuntime for the layout-created agent surface.
func (*agentTerminal) LaunchSurface(context.Context, terminal.SurfaceID, terminal.Command) error {
	return nil
}

// SendInput implements TerminalRuntime.
func (*agentTerminal) SendInput(context.Context, terminal.SurfaceID, []byte) error { return nil }

// Notify records one operator-visible notification.
func (t *agentTerminal) Notify(_ context.Context, notification terminal.Notification) error {
	t.notifications = append(t.notifications, notification)
	return nil
}

// CloseSurface implements TerminalRuntime.
func (t *agentTerminal) CloseSurface(_ context.Context, surfaceID terminal.SurfaceID) error {
	t.closed = append(t.closed, surfaceID)
	return nil
}

// CloseWorkspace implements TerminalRuntime.
func (*agentTerminal) CloseWorkspace(context.Context, terminal.WorkspaceID) error { return nil }

// agentHarness records visible session lifecycle operations.
type agentHarness struct {
	starts    []harness.StartRequest
	resumes   []harness.StartRequest
	finished  []harness.Session
	startErr  error
	resumeErr error
}

// Capabilities reports the harness identity this fake stands in for.
func (*agentHarness) Capabilities() harness.Capabilities {
	return harness.Capabilities{Name: harness.NameCodex, InteractiveResume: true}
}

// Start records the coordinator-owned prompt request.
func (h *agentHarness) Start(_ context.Context, request harness.StartRequest) (harness.Session, error) {
	h.starts = append(h.starts, request)
	if h.startErr != nil {
		return harness.Session{}, h.startErr
	}
	return harness.Session{InvocationID: request.InvocationID, Surface: request.Surface}, nil
}

// Resume implements harness.Runtime and records native session continuation.
func (h *agentHarness) Resume(_ context.Context, request harness.StartRequest) (harness.Session, error) {
	h.resumes = append(h.resumes, request)
	if h.resumeErr != nil {
		return harness.Session{}, h.resumeErr
	}
	return harness.Session{InvocationID: request.InvocationID, NativeSessionID: "session-repaired", Surface: request.Surface}, nil
}

// Finish records the accepted session close.
func (h *agentHarness) Finish(_ context.Context, session harness.Session) error {
	h.finished = append(h.finished, session)
	return nil
}

var _ factory.InvocationStore = (*agentRunStore)(nil)
var _ factory.InvocationHistoryStore = (*agentRunStore)(nil)
var _ factory.LatestInvocationStore = (*agentRunStore)(nil)
var _ worker.WorkerRuntime = (*agentWorker)(nil)
var _ terminal.TerminalRuntime = (*agentTerminal)(nil)
var _ harness.Runtime = (*agentHarness)(nil)
