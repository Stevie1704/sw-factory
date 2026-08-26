package factory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/github"
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
	isReview := request.Role == "spec_review" && request.Stage == store.StageReview
	var reviewContext *prompt.ReviewContext
	if isReview {
		if err := s.ensureReviewStart(ctx, *run); err != nil {
			return AgentLaunchResult{}, err
		}
		reviewContext, err = reviewContextForRun(ctx, *run, runStore)
		if err != nil {
			return AgentLaunchResult{}, err
		}
	}
	if err := s.ensureBaselineReady(ctx, runStore, *run, packet); err != nil {
		return AgentLaunchResult{}, err
	}
	if isReview {
		if err := s.publishSpecificationReviewStatus(ctx, registration, *run, github.CommitStatusPending, "specification review in progress"); err != nil {
			return AgentLaunchResult{}, fmt.Errorf("publish pending specification review status: %w", err)
		}
	}
	if request.Role == "implementation" && run.Stage == store.StageClaim && !run.TestStageSkipped {
		return AgentLaunchResult{}, errors.New("implementation agent cannot bypass the configured test stage")
	}
	policy, err := resolveAgentPolicy(packet.RepositoryConfig, request)
	if err != nil {
		return AgentLaunchResult{}, err
	}
	seedCredentials, credentialStoreID, err := s.credentialSeeding(registration, request, policy.Harness)
	if err != nil {
		return AgentLaunchResult{}, err
	}
	terminalRuntime, harnessRuntime, err := s.ensureAgentRuntime(registration.Cmux.SocketPath, policy.Harness)
	if err != nil {
		return AgentLaunchResult{}, fmt.Errorf("ensure agent runtime: %w", err)
	}
	// A setting the selected harness cannot honor is refused here, before the
	// launch creates any directory, worker, credential copy, or surface. The
	// adapter refuses it again at launch as a fail-closed backstop.
	if policy.ReasoningEffort != "" && !harnessRuntime.Capabilities().ReasoningEffort {
		return AgentLaunchResult{}, &PolicyRejection{
			Code:    PolicyRejectionReasoningEffortUnsupported,
			Problem: fmt.Sprintf("harness %q cannot honor a reasoning-effort selection for role %q", policy.Harness, request.Role),
		}
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
	invocationPacket := InvocationPacket{
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
		ReviewContext:       reviewContext,
	}
	promptRequest := prompt.Request{
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
		ReviewContext:           reviewContext,
	}
	promptText, err := prompt.Build(promptRequest)
	if err != nil {
		return AgentLaunchResult{}, err
	}
	if err := writeInvocationPacket(packetDirectory, invocationPacket); err != nil {
		return AgentLaunchResult{}, err
	}
	invocation := store.Invocation{
		ID:                  invocationID,
		RunID:               run.ID,
		Harness:             string(policy.Harness),
		Role:                role,
		Stage:               stage,
		Model:               policy.Model,
		ReasoningEffort:     policy.ReasoningEffort,
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
	if isReview {
		reviewContext.CurrentDiff, err = s.captureReviewDiff(ctx, *run)
		if err != nil {
			return AgentLaunchResult{}, err
		}
		invocationPacket.ReviewContext = reviewContext
		if err := writeInvocationPacket(packetDirectory, invocationPacket); err != nil {
			return AgentLaunchResult{}, err
		}
		promptRequest.ReviewContext = reviewContext
		promptText, err = prompt.Build(promptRequest)
		if err != nil {
			return AgentLaunchResult{}, err
		}
	}
	if seedCredentials != nil {
		if err := seedCredentials(ctx, run.ID); err != nil {
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
	} else if isReview {
		// The harness adapter creates a fresh command-backed surface for the
		// reviewer, rather than reusing the implementation surface.
		agentSurface = terminal.Surface{WorkspaceID: runWorkspace.ID, Name: "spec-review"}
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
		CheckpointSHA:   reviewCheckpointSHA(isReview, run.CheckpointSHA),
		WorkspaceID:     runWorkspace.ID,
		Surface:         agentSurface,
		Prompt:          promptText,
		Model:           policy.Model,
		ReasoningEffort: policy.ReasoningEffort,
	})
	if err != nil {
		return AgentLaunchResult{}, fmt.Errorf("launch %s %s agent: %w", policy.Harness, role, err)
	}
	if session.NativeSessionID != "" {
		invocation.NativeSessionID = session.NativeSessionID
	}
	if session.Surface.ID != "" {
		agentSurface = session.Surface
		cleanupSurface = agentSurface
		invocation.ImplementationSurfaceID = string(agentSurface.ID)
	}
	if err := invocationStore.SaveInvocation(ctx, invocation); err != nil {
		return AgentLaunchResult{}, fmt.Errorf("persist harness session identity: %w", err)
	}
	previousRun := *run
	run.Stage = stage
	run.Status = store.StatusActive
	run.UpdatedAt = s.deps.Now().UTC()
	if err := s.persistAgentRunState(ctx, registration, runStore, previousRun, *run); err != nil {
		return AgentLaunchResult{}, fmt.Errorf("persist %s stage: %w", role, err)
	}
	if isReview {
		if err := s.refreshSpecificationReviewPullRequest(ctx, registration, *run); err != nil {
			return AgentLaunchResult{}, err
		}
	}
	return AgentLaunchResult{Invocation: invocation, Prompt: promptText}, nil
}

// reviewCheckpointSHA supplies an exact checkpoint only to the review harness.
func reviewCheckpointSHA(review bool, checkpoint string) string {
	if !review {
		return ""
	}
	return checkpoint
}

// credentialSeeding resolves the narrowly scoped credential source for one
// harness and returns the seeding step to run after the worker starts. Each
// harness keeps its own host source, because a host can hold one harness
// credential as a file without holding the other. It returns a nil step when
// no source is registered, leaving the credential the worker itself persisted
// in its role volume in place.
func (s *Service) credentialSeeding(registration config.RepositoryRegistration, request AgentRequest, harnessName config.Harness) (func(context.Context, string) error, string, error) {
	var authPath string
	var seed func(context.Context, worker.CredentialSeedRequest) error
	switch harnessName {
	case config.HarnessCodex:
		authPath = defaultString(request.CodexAuthPath, registration.Authentication.CodexAuthPath)
		if seeder, ok := s.deps.Worker.(worker.CredentialSeeder); ok {
			seed = seeder.SeedCodexCredentials
		}
	case config.HarnessClaude:
		authPath = defaultString(request.ClaudeAuthPath, registration.Authentication.ClaudeAuthPath)
		if seeder, ok := s.deps.Worker.(worker.ClaudeCredentialSeeder); ok {
			seed = seeder.SeedClaudeCredentials
		}
	}
	if authPath == "" {
		return nil, "", nil
	}
	if seed == nil {
		return nil, "", fmt.Errorf("worker runtime does not support %s credential seeding", harnessName)
	}
	return func(ctx context.Context, runID string) error {
		return seed(ctx, worker.CredentialSeedRequest{RunID: runID, AuthPath: authPath})
	}, registration.Path, nil
}
