package factory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/prompt"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// startAgentWithStore launches one visible role invocation using an already-open
// operational store. Command-driven resumes use this seam to avoid reopening
// SQLite while the command projection is being applied.
func (s *Service) startAgentWithStore(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run, request AgentRequest) (result AgentLaunchResult, returnErr error) {
	invocationStore, ok := runStore.(InvocationStore)
	if !ok {
		return AgentLaunchResult{}, errors.New("operational store does not support visible invocations")
	}
	if run == nil {
		return AgentLaunchResult{}, errors.New("no active run")
	}
	if request.RunID != "" && request.RunID != run.ID {
		return AgentLaunchResult{}, fmt.Errorf("active run is %s, not %s", run.ID, request.RunID)
	}
	if run.CheckRepairPendingAttempt != 0 {
		return AgentLaunchResult{}, fmt.Errorf("check-repair attempt %d is pending reconciliation", run.CheckRepairPendingAttempt)
	}
	if request.Harness == "" && run.HarnessOverride != "" {
		request.Harness = config.Harness(run.HarnessOverride)
	}
	if err := validateAgentRunState(*run); err != nil {
		return AgentLaunchResult{}, err
	}
	if activeStore, ok := invocationStore.(ActiveInvocationStore); ok {
		active, lookupErr := activeStore.ActiveInvocation(ctx, run.ID)
		if lookupErr != nil {
			return AgentLaunchResult{}, fmt.Errorf("look up active visible invocation: %w", lookupErr)
		}
		if active != nil {
			return AgentLaunchResult{}, fmt.Errorf("run %q already has active invocation %q", run.ID, active.ID)
		}
	}
	if !filepath.IsAbs(run.Worktree) {
		return AgentLaunchResult{}, errors.New("active run worktree must be absolute")
	}
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return AgentLaunchResult{}, err
	}
	if !testStageConfigured(packet.RepositoryConfig) {
		return AgentLaunchResult{}, errors.New("frozen repository packet lacks the mandatory test-stage role policy")
	}
	request, err = selectAgentRole(*run, request)
	if err != nil {
		return AgentLaunchResult{}, err
	}
	if err := validateAgentRequest(request); err != nil {
		return AgentLaunchResult{}, err
	}
	if err := s.ensureBaselineReady(ctx, runStore, *run, packet); err != nil {
		return AgentLaunchResult{}, err
	}
	if request.Role == "implementation" && run.Stage == store.StageClaim && !run.TestStageSkipped {
		return AgentLaunchResult{}, errors.New("implementation agent cannot bypass the configured test stage")
	}
	harnessName, model, err := resolveAgentPolicy(packet.RepositoryConfig, request)
	if err != nil {
		return AgentLaunchResult{}, err
	}
	if harnessName != config.HarnessCodex {
		return AgentLaunchResult{}, fmt.Errorf("harness %q is not supported by the Codex implementation agent", harnessName)
	}
	authPath := request.CodexAuthPath
	if authPath == "" {
		authPath = registration.Authentication.CodexAuthPath
	}
	credentialStoreID := ""
	var seeder worker.CredentialSeeder
	if authPath != "" {
		credentialStoreID = registration.Path
		var ok bool
		seeder, ok = s.deps.Worker.(worker.CredentialSeeder)
		if !ok {
			return AgentLaunchResult{}, errors.New("worker runtime does not support Codex credential seeding")
		}
	}
	terminalRuntime, harnessRuntime, err := s.ensureAgentRuntime(registration.Cmux.SocketPath)
	if err != nil {
		return AgentLaunchResult{}, fmt.Errorf("ensure agent runtime: %w", err)
	}
	runID, err := s.deps.NewRunID()
	if err != nil {
		return AgentLaunchResult{}, fmt.Errorf("generate invocation identifier: %w", err)
	}
	invocationID := "inv-" + runID
	createdAt := s.deps.Now().UTC()
	root := filepath.Join(filepath.Dir(run.Worktree), ".factory-agents", run.ID, invocationID)
	packetDirectory := filepath.Join(root, "packet")
	resultDirectory := filepath.Join(root, "results")
	if err := os.MkdirAll(packetDirectory, 0o700); err != nil {
		return AgentLaunchResult{}, fmt.Errorf("create invocation packet directory: %w", err)
	}
	if err := os.MkdirAll(resultDirectory, 0o700); err != nil {
		return AgentLaunchResult{}, fmt.Errorf("create invocation result directory: %w", err)
	}
	role := request.Role
	stage := request.Stage
	promptText, err := prompt.Build(prompt.Request{
		InvocationID:            invocationID,
		RunID:                   run.ID,
		Role:                    role,
		Stage:                   string(stage),
		SpecificationPacket:     run.SpecificationPacket,
		RepositoryGuidance:      packet.Issue.Body,
		TestHandoff:             run.TestHandoff,
		ProtectedTestPaths:      run.ProtectedTestPaths,
		TestExemption:           run.TestExemption,
		TestPaths:               packet.RepositoryConfig.TestPolicy.TestPaths,
		TestInfrastructurePaths: packet.RepositoryConfig.TestPolicy.InfrastructurePaths,
	})
	if err != nil {
		return AgentLaunchResult{}, err
	}
	if err := writeInvocationPacket(packetDirectory, InvocationPacket{
		SchemaVersion:       invocationPacketVersion,
		InvocationID:        invocationID,
		RunID:               run.ID,
		Role:                role,
		Stage:               stage,
		SpecificationPacket: run.SpecificationPacket,
		PromptVersion:       prompt.VersionFor(role, string(stage)),
		PermittedPaths:      append([]string(nil), request.PermittedPaths...),
		TestHandoff:         run.TestHandoff,
		ProtectedTestPaths:  append([]store.ProtectedTestPath(nil), run.ProtectedTestPaths...),
		TestExemption:       run.TestExemption,
	}); err != nil {
		return AgentLaunchResult{}, err
	}
	invocation := store.Invocation{
		ID:                  invocationID,
		RunID:               run.ID,
		Harness:             string(harnessName),
		Role:                role,
		Stage:               stage,
		Model:               model,
		ReasoningEffort:     request.ReasoningEffort,
		CredentialStoreID:   credentialStoreID,
		InvocationDirectory: packetDirectory,
		ResultDirectory:     resultDirectory,
		PermittedPaths:      append([]string(nil), request.PermittedPaths...),
		PromptVersion:       prompt.VersionFor(role, string(stage)),
		Status:              store.InvocationStatusActive,
		CreatedAt:           createdAt,
		UpdatedAt:           createdAt,
	}
	invocationPersisted := false
	workerStarted := false
	cleanupSurface := terminal.Surface{}
	defer func() {
		if returnErr == nil || !invocationPersisted {
			return
		}
		rollbackContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		invocation.Status = store.InvocationStatusCannotProceed
		invocation.UpdatedAt = s.deps.Now().UTC()
		_ = invocationStore.SaveInvocation(rollbackContext, invocation)
		if workerStarted {
			_ = s.deps.Worker.Stop(rollbackContext, run.ID)
		}
		if cleanupSurface.ID != "" {
			_ = terminalRuntime.CloseSurface(rollbackContext, cleanupSurface.ID)
		}
	}()
	if err := invocationStore.SaveInvocation(ctx, invocation); err != nil {
		return AgentLaunchResult{}, fmt.Errorf("persist visible invocation: %w", err)
	}
	invocationPersisted = true
	if recorder, ok := runStore.(evaluationRecorder); ok {
		if err := recorder.EnsureEvaluationSummary(ctx, *run); err != nil {
			return AgentLaunchResult{}, fmt.Errorf("ensure local evaluation summary: %w", err)
		}
		if err := recorder.RecordEvaluationInvocation(ctx, run.ID, invocation, run.ImageDigest, report.SchemaVersion); err != nil {
			return AgentLaunchResult{}, fmt.Errorf("record local evaluation invocation: %w", err)
		}
	}
	gitMetadataPath, err := prepareGitMetadataProjection(run.ID, registration.Path, run.Worktree)
	if err != nil {
		return AgentLaunchResult{}, fmt.Errorf("prepare worker Git metadata: %w", err)
	}
	if err := s.deps.Worker.Start(ctx, worker.StartRequest{
		RunID:             run.ID,
		WorktreePath:      run.Worktree,
		GitMetadataPath:   gitMetadataPath,
		Image:             packet.RepositoryConfig.WorkerBuild.Image,
		ImageDigest:       run.ImageDigest,
		Caches:            workerCaches(packet.RepositoryConfig.Caches),
		InvocationPath:    packetDirectory,
		ResultPath:        resultDirectory,
		CredentialStoreID: credentialStoreID,
		Role:              role,
	}); err != nil {
		return AgentLaunchResult{}, fmt.Errorf("start worker for visible agent: %w", err)
	}
	workerStarted = true
	if authPath != "" {
		if err := seeder.SeedCodexCredentials(ctx, worker.CredentialSeedRequest{RunID: run.ID, AuthPath: authPath}); err != nil {
			return AgentLaunchResult{}, err
		}
	}
	control, err := terminalRuntime.EnsureControlWorkspace(ctx, terminal.WorkspaceRequest{
		Name:             defaultString(registration.Cmux.ControlWorkspace, "factory-control"),
		Description:      "software factory coordinator",
		WorkingDirectory: registration.Path,
	})
	if err != nil {
		return AgentLaunchResult{}, err
	}
	runWorkspace, err := terminalRuntime.EnsureRunWorkspace(ctx, terminal.RunWorkspaceRequest{
		RunID:            run.ID,
		Name:             "factory-" + run.ID,
		Description:      fmt.Sprintf("factory run for issue #%d", run.IssueNumber),
		WorkingDirectory: run.Worktree,
	})
	if err != nil {
		return AgentLaunchResult{}, err
	}
	agentSurface := runWorkspace.Implementation
	if role == "test" {
		agentSurface = runWorkspace.Checks
	}
	cleanupSurface = agentSurface
	invocation.WorkspaceID = string(runWorkspace.ID)
	invocation.StatusSurfaceID = string(runWorkspace.Status.ID)
	invocation.ImplementationSurfaceID = string(agentSurface.ID)
	invocation.ChecksSurfaceID = string(runWorkspace.Checks.ID)
	if err := invocationStore.SaveInvocation(ctx, invocation); err != nil {
		return AgentLaunchResult{}, fmt.Errorf("persist visible terminal handles: %w", err)
	}
	if err := terminalRuntime.Notify(ctx, terminal.Notification{WorkspaceID: control.ID, Title: "factory run started", Body: fmt.Sprintf("%s %s agent active", run.ID, role)}); err != nil {
		return AgentLaunchResult{}, err
	}
	session, err := harnessRuntime.Start(ctx, harness.StartRequest{
		InvocationID:    invocationID,
		RunID:           run.ID,
		Role:            role,
		Stage:           string(stage),
		WorkspaceID:     runWorkspace.ID,
		Surface:         agentSurface,
		Prompt:          promptText,
		Model:           model,
		ReasoningEffort: request.ReasoningEffort,
	})
	if err != nil {
		return AgentLaunchResult{}, fmt.Errorf("launch Codex implementation agent: %w", err)
	}
	if session.NativeSessionID != "" {
		invocation.NativeSessionID = session.NativeSessionID
	}
	if err := invocationStore.SaveInvocation(ctx, invocation); err != nil {
		return AgentLaunchResult{}, fmt.Errorf("persist Codex session identity: %w", err)
	}
	previousRun := *run
	run.Stage = stage
	run.Status = store.StatusActive
	run.UpdatedAt = s.deps.Now().UTC()
	if err := s.persistAgentRunState(ctx, registration, runStore, previousRun, *run); err != nil {
		return AgentLaunchResult{}, fmt.Errorf("persist implementation stage: %w", err)
	}
	return AgentLaunchResult{Invocation: invocation, Prompt: promptText}, nil
}
