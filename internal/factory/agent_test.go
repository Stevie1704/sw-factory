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
	if packet.InvocationID != launch.Invocation.ID || packet.SpecificationPacket == "" || packet.PromptVersion != "implementation-v1" || packet.TestPolicyMode != config.TestModeRequired {
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

// TestStartAgentRollsBackASetupFailure verifies a partially launched session
// cannot remain active after the harness fails to start.
func TestStartAgentRollsBackASetupFailure(t *testing.T) {
	service, runStore, runtime, terminalRuntime, harnessRuntime := newAgentService(t)
	harnessRuntime.startErr = errors.New("harness unavailable")
	if _, err := service.StartAgent(context.Background(), factory.AgentRequest{}); err == nil {
		t.Fatal("StartAgent() succeeded while the harness was unavailable")
	}
	invocation, ok := runStore.invocations["inv-generated"]
	if !ok || invocation.Status != store.InvocationStatusCannotProceed {
		t.Fatalf("rolled-back invocation = %#v, want cannot_proceed", invocation)
	}
	if runtime.stops != 1 || len(terminalRuntime.closed) != 1 || terminalRuntime.closed[0] != "surface-implementation" {
		t.Fatalf("rollback side effects: stops=%d closed=%v", runtime.stops, terminalRuntime.closed)
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

// newFreshAgentService recreates a coordinator around the persisted claim so
// tests exercise the cross-process startup seam rather than cached service state.
func newFreshAgentService(t *testing.T, runStore *agentRunStore, worktree *inspectingWorktree, runtime *agentWorker, terminalRuntime *agentTerminal, harnessRuntime *agentHarness) *factory.Service {
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
		OpenStore:      func(context.Context, string) (factory.OperationalStore, error) { return runStore, nil },
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
