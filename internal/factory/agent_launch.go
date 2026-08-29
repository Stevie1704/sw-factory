package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/prompt"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
	"github.com/Stevie1704/sw-factory/internal/workflow"
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
			if active.AttachRequired {
				return AgentLaunchResult{}, fmt.Errorf("run %q has a manually resumed invocation that requires `factory attach`", run.ID)
			}
			if !s.invocationStartedHere(active.ID) && active.RecoveryResumeCount > 0 {
				s.markInvocationStarted(active.ID)
				resumedRoute, _ := routeForRun(*run)
				return AgentLaunchResult{Invocation: *active, TestPolicyMode: testPolicyModeForRun(*run), Route: resumedRoute}, nil
			}
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
	if !testRolePolicySatisfied(packet) {
		return AgentLaunchResult{}, errors.New("frozen repository packet lacks the mandatory test-stage role policy")
	}
	request, err = selectAgentRole(*run, request)
	if err != nil {
		return AgentLaunchResult{}, err
	}
	if err := validateAgentRequest(request); err != nil {
		return AgentLaunchResult{}, err
	}
	roleDefinition, roleDeclared := workflow.DefaultRegistry().Role(request.Role)
	if !roleDeclared {
		return AgentLaunchResult{}, fmt.Errorf("agent role %q is not declared by the workflow registry", request.Role)
	}
	if len(request.PermittedPaths) == 0 {
		request.PermittedPaths = append([]string(nil), roleDefinition.DefaultPermittedPaths...)
	}
	if err := report.ValidatePermittedPaths(request.PermittedPaths); err != nil {
		return AgentLaunchResult{}, err
	}
	if latestStore, ok := runStore.(LatestInvocationStore); ok {
		latest, latestErr := latestStore.LatestInvocation(ctx, run.ID)
		if latestErr != nil {
			return AgentLaunchResult{}, fmt.Errorf("look up latest invocation before launch: %w", latestErr)
		}
		if latest != nil && latest.Status != store.InvocationStatusSuperseded && latest.Role == request.Role && latest.Stage == request.Stage {
			return AgentLaunchResult{}, fmt.Errorf("run %q already has invocation history for %s/%s", run.ID, request.Role, request.Stage)
		}
	}
	isReview := roleDefinition.Kind == workflow.RoleKindReview
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
		if err := s.publishSpecificationReviewStatus(ctx, registration, runStore, *run, github.CommitStatusPending, "specification review in progress"); err != nil {
			return AgentLaunchResult{}, fmt.Errorf("publish pending specification review status: %w", err)
		}
	}
	if roleDefinition.Kind == workflow.RoleKindTest && !independentTestStageDeclared(packet) {
		return AgentLaunchResult{}, errors.New("test role is unavailable in advisory mode without a selected route; implementation owns TDD")
	}
	if roleDefinition.RequiresTestHandoff && independentTestStageDeclared(packet) && run.Stage == store.StageClaim && !run.TestStageSkipped {
		return AgentLaunchResult{}, errors.New("implementation agent cannot bypass the configured test stage")
	}
	designHandoff := designHandoffForInvocation(*run, packet, roleDefinition)
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
		PromptVersion:       roleDefinition.PromptVersion,
		TestPolicyMode:      packet.RepositoryConfig.TestPolicy.Mode,
		Route:               packet.Route,
		DesignHandoff:       designHandoff,
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
		TestPolicyMode:          string(packet.RepositoryConfig.TestPolicy.Mode),
		Route:                   packet.Route,
		DesignHandoff:           designHandoff,
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
		PromptVersion:       roleDefinition.PromptVersion,
		Status:              store.InvocationStatusActive,
		CreatedAt:           createdAt,
		UpdatedAt:           createdAt,
	}
	invocationPersisted := false
	workerStarted := false
	preserveInvocation := false
	cleanupSurface := terminal.Surface{}
	defer func() {
		if returnErr == nil || !invocationPersisted || preserveInvocation {
			return
		}
		if journal, journaled := runStore.(PendingEffectStore); journaled {
			pending, pendingErr := journal.PendingEffect(context.WithoutCancel(ctx), run.ID)
			if pendingErr == nil && pending != nil {
				// Leave the active invocation and its reserved effect intact. A
				// restart must replay the exact worker boundary before deciding
				// whether the native session can be resumed or needs human review.
				return
			}
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
	workerRequest := worker.StartRequest{
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
	}
	if err := s.startWorkerWithEffect(ctx, runStore, workerRequest); err != nil {
		return AgentLaunchResult{}, fmt.Errorf("start worker for visible agent: %w", err)
	}
	workerStarted = true
	if isReview {
		reviewContext.CurrentDiff, err = s.captureReviewDiff(ctx, *run, role)
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
			return AgentLaunchResult{}, newCredentialProjectionError(string(policy.Harness))
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
	var agentSurface terminal.Surface
	switch roleDefinition.Surface {
	case workflow.SurfaceChecks:
		agentSurface = runWorkspace.Checks
	case workflow.SurfaceRole:
		// The harness adapter creates a fresh command-backed surface for a
		// role-owned surface, rather than reusing the implementation surface.
		agentSurface = terminal.Surface{WorkspaceID: runWorkspace.ID, Name: role}
	default:
		agentSurface = runWorkspace.Implementation
	}
	cleanupSurface = agentSurface
	invocation.WorkspaceID = string(runWorkspace.ID)
	invocation.StatusSurfaceID = string(runWorkspace.Status.ID)
	setInvocationSurface(&invocation, agentSurface)
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
		classified := harness.ClassifyError(err, string(policy.Harness))
		if harness.IsRateLimited(classified) {
			preserveInvocation = true
			invocation.Status = store.InvocationStatusSuperseded
			invocation.UpdatedAt = s.deps.Now().UTC()
			if saveErr := invocationStore.SaveInvocation(ctx, invocation); saveErr != nil {
				return AgentLaunchResult{}, fmt.Errorf("persist rate-limited invocation: %w", saveErr)
			}
			if _, waitErr := s.pauseForHarnessCapacity(ctx, registration, runStore, *run, string(policy.Harness)); waitErr != nil {
				return AgentLaunchResult{Invocation: invocation}, errors.Join(classified, waitErr)
			}
			return AgentLaunchResult{Invocation: invocation}, classified
		}
		if harness.IsAuthenticationExpired(classified) {
			preserveInvocation = true
			invocation.Status = store.InvocationStatusSuperseded
			invocation.UpdatedAt = s.deps.Now().UTC()
			if saveErr := invocationStore.SaveInvocation(ctx, invocation); saveErr != nil {
				return AgentLaunchResult{}, fmt.Errorf("persist authentication failure state: %w", saveErr)
			}
			if _, pauseErr := s.pauseForAuthentication(ctx, registration, runStore, *run, string(policy.Harness)); pauseErr != nil {
				return AgentLaunchResult{Invocation: invocation}, errors.Join(classified, pauseErr)
			}
			return AgentLaunchResult{Invocation: invocation}, classified
		}
		if harness.IsUnexpectedExit(classified) {
			preserveInvocation = true
			invocation.Status = store.InvocationStatusSuperseded
			invocation.UpdatedAt = s.deps.Now().UTC()
			if saveErr := invocationStore.SaveInvocation(ctx, invocation); saveErr != nil {
				return AgentLaunchResult{}, fmt.Errorf("persist unexpected harness exit state: %w", saveErr)
			}
			if _, pauseErr := s.pauseForManualRecovery(ctx, registration, runStore, *run, string(policy.Harness), classified); pauseErr != nil {
				return AgentLaunchResult{Invocation: invocation}, errors.Join(classified, pauseErr)
			}
			return AgentLaunchResult{Invocation: invocation}, classified
		}
		return AgentLaunchResult{}, fmt.Errorf("launch %s %s agent: %w", policy.Harness, role, classified)
	}
	if session.NativeSessionID != "" {
		invocation.NativeSessionID = session.NativeSessionID
	}
	if session.Surface.ID != "" {
		agentSurface = session.Surface
		cleanupSurface = agentSurface
		setInvocationSurface(&invocation, agentSurface)
	}
	if err := invocationStore.SaveInvocation(ctx, invocation); err != nil {
		return AgentLaunchResult{}, fmt.Errorf("persist harness session identity: %w", err)
	}
	previousRun := *run
	run.Stage = stage
	run.Status = store.StatusActive
	run.ActiveInvocationID = invocation.ID
	run.UpdatedAt = s.deps.Now().UTC()
	if err := s.persistAgentRunState(ctx, registration, runStore, previousRun, *run); err != nil {
		return AgentLaunchResult{}, fmt.Errorf("persist %s stage: %w", role, err)
	}
	if isReview {
		if err := s.refreshSpecificationReviewPullRequest(ctx, registration, runStore, *run); err != nil {
			return AgentLaunchResult{}, err
		}
	}
	s.markInvocationStarted(invocation.ID)
	return AgentLaunchResult{Invocation: invocation, Prompt: promptText, TestPolicyMode: packet.RepositoryConfig.TestPolicy.Mode, Route: packet.Route}, nil
}

// resumePersistedInvocation restores one active invocation after a coordinator
// restart. It reuses the persisted worker, workspace, surface, harness, and
// native session identity; it never creates a second invocation for the run.
// The worker is stopped before reattachment so a cmux restart cannot leave an
// orphaned harness process alongside the resumed native session; its worktree
// and role volumes remain intact.
func (s *Service) resumePersistedInvocation(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, invocation store.Invocation) (store.Invocation, error) {
	return s.resumePersistedInvocationWithMode(ctx, registration, runStore, run, invocation, true)
}

// resumePersistedInvocationManually performs an operator-requested native
// resume. Manual recovery is allowed after the one automatic attempt, but it
// records that the resulting session must be attached before progression.
func (s *Service) resumePersistedInvocationManually(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, invocation store.Invocation) (store.Invocation, error) {
	return s.resumePersistedInvocationWithMode(ctx, registration, runStore, run, invocation, false)
}

// resumePersistedInvocationWithMode restores one invocation through either the
// bounded automatic recovery path or the explicit operator path.
func (s *Service) resumePersistedInvocationWithMode(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, invocation store.Invocation, automatic bool) (store.Invocation, error) {
	if automatic && invocation.RecoveryResumeCount > 0 {
		return invocation, nil
	}
	invocationStore, ok := runStore.(InvocationStore)
	if !ok {
		return invocation, errors.New("operational store does not support invocation recovery")
	}
	if strings.TrimSpace(invocation.NativeSessionID) == "" {
		return invocation, errors.New("active invocation has no persisted native session identifier")
	}
	roleDefinition, err := roleDefinitionForInvocation(invocation)
	if err != nil {
		return invocation, err
	}
	if err := s.stopRunWorker(ctx, run.ID); err != nil {
		return invocation, err
	}
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return invocation, fmt.Errorf("decode specification packet for invocation recovery: %w", err)
	}
	terminalRuntime, harnessRuntime, err := s.ensureAgentRuntime(registration.Cmux.SocketPath, config.Harness(invocation.Harness))
	if err != nil {
		return invocation, err
	}
	capabilities := harnessRuntime.Capabilities()
	if capabilities.Name != invocation.Harness {
		return invocation, fmt.Errorf("harness %q cannot resume persisted %q session", capabilities.Name, invocation.Harness)
	}
	if !capabilities.InteractiveResume {
		return invocation, fmt.Errorf("harness %q does not support native session resume", invocation.Harness)
	}
	promptText, err := promptForPersistedInvocation(run, invocation, packet)
	if err != nil {
		return invocation, err
	}
	_, invocation, err = s.ensureWorkerForInvocation(ctx, registration, runStore, run, invocation)
	if err != nil {
		return invocation, err
	}
	if err := s.restoreCredentialProjection(ctx, registration, run, invocation); err != nil {
		return invocation, err
	}
	workspaceID, roleSurface, statusSurface, checksSurface, restored, err := s.restoreInvocationTerminal(ctx, terminalRuntime, run, invocation)
	if err != nil {
		return invocation, err
	}
	if restored {
		invocation.WorkspaceID = string(workspaceID)
		invocation.StatusSurfaceID = string(statusSurface.ID)
		setInvocationSurface(&invocation, roleSurface)
		invocation.ChecksSurfaceID = string(checksSurface.ID)
		if err := invocationStore.SaveInvocation(ctx, invocation); err != nil {
			return invocation, fmt.Errorf("persist restored terminal handles: %w", err)
		}
	}
	resumeRequest := harness.StartRequest{
		InvocationID:    invocation.ID,
		RunID:           run.ID,
		Role:            invocation.Role,
		Stage:           string(invocation.Stage),
		CheckpointSHA:   reviewCheckpointSHA(roleDefinition.Kind == workflow.RoleKindReview, run.CheckpointSHA),
		WorkspaceID:     workspaceID,
		Surface:         roleSurface,
		Prompt:          promptText,
		Model:           invocation.Model,
		ReasoningEffort: invocation.ReasoningEffort,
		ResumeSessionID: invocation.NativeSessionID,
	}
	var updated store.Invocation
	if automatic {
		updated, err = s.resumeHarnessWithEffect(ctx, runStore, invocationStore, registration.Cmux.SocketPath, harnessRuntime, invocation, resumeRequest)
	} else {
		updated, err = s.resumeHarnessManuallyWithEffect(ctx, runStore, invocationStore, registration.Cmux.SocketPath, harnessRuntime, invocation, resumeRequest)
	}
	if err != nil {
		return updated, err
	}
	return updated, nil
}

// promptForPersistedInvocation rebuilds the same role prompt from the frozen
// packet and invocation packet after a process restart.
func promptForPersistedInvocation(run store.Run, invocation store.Invocation, packet SpecificationPacket) (string, error) {
	data, err := os.ReadFile(filepath.Join(invocation.InvocationDirectory, invocationPacketFileName))
	if err != nil {
		return "", fmt.Errorf("read persisted invocation packet: %w", err)
	}
	persisted, err := decodePersistedInvocationPacket(data)
	if err != nil {
		return "", fmt.Errorf("decode persisted invocation packet: %w", err)
	}
	if err := validatePersistedInvocationPacket(run, invocation, persisted); err != nil {
		return "", err
	}
	return prompt.Build(prompt.Request{
		InvocationID:            invocation.ID,
		RunID:                   run.ID,
		Role:                    invocation.Role,
		Stage:                   string(invocation.Stage),
		SpecificationPacket:     run.SpecificationPacket,
		RepositoryGuidance:      packet.Issue.Body,
		TestPolicyMode:          string(packet.RepositoryConfig.TestPolicy.Mode),
		Route:                   packet.Route,
		DesignHandoff:           persisted.DesignHandoff,
		TestHandoff:             persisted.TestHandoff,
		ProtectedTestPaths:      persisted.ProtectedTestPaths,
		TestExemption:           persisted.TestExemption,
		TestPaths:               packet.RepositoryConfig.TestPolicy.TestPaths,
		TestInfrastructurePaths: packet.RepositoryConfig.TestPolicy.InfrastructurePaths,
		CheckRepairAttempt:      checkRepairAttempt(persisted.CheckRepair),
		CheckRepairBudget:       checkRepairBudget(persisted.CheckRepair),
		ReviewContext:           persisted.ReviewContext,
	})
}

// decodePersistedInvocationPacket accepts the current and retained historical
// append-only packet shapes. Fields introduced after an older version remain at
// their zero value, while missing, future, and unsupported versions fail closed.
func decodePersistedInvocationPacket(data []byte) (InvocationPacket, error) {
	var persisted InvocationPacket
	if err := json.Unmarshal(data, &persisted); err != nil {
		return InvocationPacket{}, err
	}
	if !supportedInvocationPacketVersion(persisted.SchemaVersion) {
		return InvocationPacket{}, fmt.Errorf("unsupported persisted invocation packet schema version %d", persisted.SchemaVersion)
	}
	return persisted, nil
}

// supportedInvocationPacketVersion reports whether a packet belongs to the
// contiguous compatibility range maintained by the current factory.
func supportedInvocationPacketVersion(version int) bool {
	return version >= invocationPacketMinimumSupportedVersion && version <= invocationPacketVersion
}

// validatePersistedInvocationPacket verifies that a decoded packet still
// describes the durable invocation and frozen run being resumed. Review
// invocations additionally require their exact review context to be present.
func validatePersistedInvocationPacket(run store.Run, invocation store.Invocation, persisted InvocationPacket) error {
	if persisted.InvocationID != invocation.ID || persisted.RunID != run.ID {
		return errors.New("persisted invocation packet identity does not match the run")
	}
	if strings.TrimSpace(persisted.Role) == "" || strings.TrimSpace(string(persisted.Stage)) == "" {
		return errors.New("persisted invocation packet role and stage are required")
	}
	if persisted.Role != invocation.Role || persisted.Stage != invocation.Stage {
		return errors.New("persisted invocation packet role and stage do not match the invocation")
	}
	if strings.TrimSpace(persisted.SpecificationPacket) == "" {
		return errors.New("persisted invocation packet specification is required")
	}
	if persisted.SpecificationPacket != run.SpecificationPacket {
		return errors.New("persisted invocation packet specification does not match the run")
	}
	if strings.TrimSpace(persisted.PromptVersion) == "" {
		return errors.New("persisted invocation packet prompt version is required")
	}
	if invocation.PromptVersion != "" && persisted.PromptVersion != invocation.PromptVersion {
		return errors.New("persisted invocation packet prompt version does not match the invocation")
	}
	if persisted.TestPolicyMode != "" {
		packet, err := decodeSpecificationPacket(run.SpecificationPacket)
		if err != nil {
			return fmt.Errorf("decode persisted invocation test policy: %w", err)
		}
		if persisted.TestPolicyMode != packet.RepositoryConfig.TestPolicy.Mode {
			return errors.New("persisted invocation packet test policy does not match the frozen repository policy")
		}
		if persisted.Route != packet.Route {
			return errors.New("persisted invocation packet route does not match the frozen specification packet route")
		}
	}
	if invocation.Role == workflow.RoleSpecificationReview {
		if persisted.ReviewContext == nil {
			return errors.New("persisted specification-review invocation packet has no review context")
		}
		if strings.TrimSpace(persisted.ReviewContext.CheckpointSHA) == "" {
			return errors.New("persisted specification-review invocation packet has no review checkpoint")
		}
		if persisted.ReviewContext.CheckpointSHA != run.CheckpointSHA {
			return errors.New("persisted specification-review invocation packet review checkpoint does not match the run")
		}
	}
	return nil
}

// checkRepairAttempt extracts the bounded repair attempt from a persisted
// invocation packet while keeping the prompt package independent of factory
// packet types.
func checkRepairAttempt(packet *CheckRepairPacket) int {
	if packet == nil {
		return 0
	}
	return packet.Attempt
}

// checkRepairBudget extracts the configured repair ceiling from a persisted
// invocation packet.
func checkRepairBudget(packet *CheckRepairPacket) int {
	if packet == nil {
		return 0
	}
	return packet.Budget
}

// workerImageMatches accepts Docker's image@digest formatting while requiring
// the frozen digest whenever the runtime exposes an image identity.
func workerImageMatches(observed, image, digest string) bool {
	observed = strings.TrimSpace(observed)
	if observed == "" {
		return true
	}
	if strings.TrimSpace(digest) == "" {
		return observed == image
	}
	return observed == image+"@"+digest || strings.HasSuffix(observed, "@"+digest)
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
	var authPath, registeredAuthPath, overrideAuthPath string
	var seed func(context.Context, worker.CredentialSeedRequest) error
	switch harnessName {
	case config.HarnessCodex:
		registeredAuthPath = registration.Authentication.CodexAuthPath
		overrideAuthPath = request.CodexAuthPath
		if seeder, ok := s.deps.Worker.(worker.CredentialSeeder); ok {
			seed = seeder.SeedCodexCredentials
		}
	case config.HarnessClaude:
		registeredAuthPath = registration.Authentication.ClaudeAuthPath
		overrideAuthPath = request.ClaudeAuthPath
		if seeder, ok := s.deps.Worker.(worker.ClaudeCredentialSeeder); ok {
			seed = seeder.SeedClaudeCredentials
		}
	}
	if overrideAuthPath != "" && (registeredAuthPath == "" || filepath.Clean(overrideAuthPath) != filepath.Clean(registeredAuthPath)) {
		return nil, "", fmt.Errorf("%s auth source override is not restart-safe; configure the source on the registered repository", harnessName)
	}
	authPath = defaultString(overrideAuthPath, registeredAuthPath)
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

// ensureCredentialStoreIdentity records the factory-managed credential store
// identity when recovery discovers that a currently configured source belongs
// to an older invocation that predates the persisted identity.
func (s *Service) ensureCredentialStoreIdentity(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, invocation store.Invocation) (store.Invocation, error) {
	_, credentialStoreID, err := s.credentialSeeding(registration, AgentRequest{}, config.Harness(invocation.Harness))
	if err != nil {
		return invocation, newCredentialProjectionError(invocation.Harness)
	}
	if strings.TrimSpace(credentialStoreID) == "" || strings.TrimSpace(invocation.CredentialStoreID) != "" {
		return invocation, nil
	}
	invocationStore, ok := runStore.(InvocationStore)
	if !ok {
		return invocation, errors.New("operational store does not support credential store persistence")
	}
	invocation.CredentialStoreID = credentialStoreID
	invocation.UpdatedAt = s.deps.Now().UTC()
	if err := invocationStore.SaveInvocation(ctx, invocation); err != nil {
		return invocation, fmt.Errorf("persist credential store identity during recovery: %w", err)
	}
	return invocation, nil
}

// restoreCredentialProjection reseeds the configured host source into the
// already-mounted factory-managed volume. An absent source is a no-op only
// when the invocation has no persisted factory-managed credential store.
func (s *Service) restoreCredentialProjection(ctx context.Context, registration config.RepositoryRegistration, run store.Run, invocation store.Invocation) error {
	seed, _, err := s.credentialSeeding(registration, AgentRequest{}, config.Harness(invocation.Harness))
	if err != nil || seed == nil {
		if err == nil && strings.TrimSpace(invocation.CredentialStoreID) == "" {
			return nil
		}
		return newCredentialProjectionError(invocation.Harness)
	}
	if err := seed(ctx, run.ID); err != nil {
		return newCredentialProjectionError(invocation.Harness)
	}
	return nil
}

// credentialProjectionError reports a credential restoration refusal without
// retaining a host path, credential contents, or adapter output.
type credentialProjectionError struct {
	// Harness identifies the non-secret adapter whose projection is unavailable.
	Harness string
}

// Error returns bounded operator guidance for restoring the managed projection.
func (e *credentialProjectionError) Error() string {
	if e == nil {
		return "credential projection could not be restored; retry `factory auth refresh`"
	}
	return fmt.Sprintf("%s credential projection could not be restored; restore the configured source and retry `factory auth refresh`", credentialHarnessLabel(e.Harness))
}

// Unwrap lets the normal recovery policy pause the run in the auth state.
func (e *credentialProjectionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return harness.NewAuthenticationExpiredError(credentialHarnessLabel(e.Harness))
}

// newCredentialProjectionError constructs the redacted credential recovery
// error used at every host-source and worker-projection boundary.
func newCredentialProjectionError(harnessName string) error {
	return &credentialProjectionError{Harness: credentialHarnessLabel(harnessName)}
}

// credentialHarnessLabel bounds the adapter identity included in recovery
// guidance so malformed persisted data cannot become an output channel.
func credentialHarnessLabel(harnessName string) string {
	switch config.Harness(harnessName) {
	case config.HarnessCodex:
		return string(config.HarnessCodex)
	case config.HarnessClaude:
		return string(config.HarnessClaude)
	default:
		return "harness"
	}
}
