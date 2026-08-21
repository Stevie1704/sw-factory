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
)

// InvocationStore is the operational-store seam required by visible agent
// launch and structured report acceptance.
type InvocationStore interface {
	RunStore
	SaveInvocation(context.Context, store.Invocation) error
	Invocation(context.Context, string, string) (*store.Invocation, error)
}

// ActiveInvocationStore is the optional restart-safe lookup used to prevent
// duplicate visible sessions for one active run.
type ActiveInvocationStore interface {
	ActiveInvocation(context.Context, string) (*store.Invocation, error)
}

// AgentRequest selects one coordinator-owned visible role invocation.
type AgentRequest struct {
	// RunID selects the active factory run. Empty selects the only active run.
	RunID string
	// Role is the workflow role, implementation for issue #6.
	Role string
	// Stage is the workflow stage, implementation for issue #6.
	Stage store.Stage
	// Harness is an optional issue-level selection constrained by repository policy.
	Harness config.Harness
	// Model is an optional policy-validated model override.
	Model string
	// ReasoningEffort is an optional policy-validated setting.
	ReasoningEffort string
	// CodexAuthPath overrides the registered narrow auth source when set.
	CodexAuthPath string
	// PermittedPaths constrains production paths in the accepted handoff.
	PermittedPaths []string
}

// AgentLaunchResult reports the persisted invocation and prompt after launch.
type AgentLaunchResult struct {
	// Invocation is the recoverable operational identity.
	Invocation store.Invocation
	// Prompt is returned for operator diagnostics and is not persisted as a transcript.
	Prompt string
}

// AgentReportRequest selects a previously launched invocation for acceptance.
type AgentReportRequest struct {
	// RunID selects the active run. Empty selects the only active run.
	RunID string
	// InvocationID selects the immutable invocation report directory.
	InvocationID string
	// PermittedPaths repeats the coordinator's path policy for this acceptance.
	PermittedPaths []string
}

// AgentResult contains the accepted invocation and its validated report.
type AgentResult struct {
	// Invocation is the updated recoverable invocation state.
	Invocation store.Invocation
	// Report is the structured proposal accepted by the coordinator.
	Report report.Report
}

// InvocationPacket is the read-only file mounted into the worker for one
// invocation. It includes no GitHub credentials or host authentication data.
type InvocationPacket struct {
	// SchemaVersion identifies this packet shape.
	SchemaVersion int `json:"schema_version"`
	// InvocationID binds the packet to one invocation.
	InvocationID string `json:"invocation_id"`
	// RunID binds the packet to one run.
	RunID string `json:"run_id"`
	// Role identifies the owning role.
	Role string `json:"role"`
	// Stage identifies the owning stage.
	Stage store.Stage `json:"stage"`
	// SpecificationPacket is the frozen claim packet.
	SpecificationPacket string `json:"specification_packet"`
	// PromptVersion identifies the versioned core prompt.
	PromptVersion string `json:"prompt_version"`
	// PermittedPaths lists repository-relative prefixes the coordinator will
	// accept in the completed handoff.
	PermittedPaths []string `json:"permitted_paths"`
}

const (
	// invocationPacketVersion identifies the read-only invocation packet shape.
	invocationPacketVersion = 1
	// invocationPacketFileName is the stable worker-visible packet filename.
	invocationPacketFileName = "specification.json"
)

// StartAgent prepares the frozen invocation packet, starts the pinned worker,
// creates the visible run surfaces, and launches the interactive Codex role.
func (s *Service) StartAgent(ctx context.Context, request AgentRequest) (result AgentLaunchResult, returnErr error) {
	request = normalizeAgentRequest(request)
	if err := validateAgentRequest(request); err != nil {
		return AgentLaunchResult{}, err
	}
	if err := report.ValidatePermittedPaths(request.PermittedPaths); err != nil {
		return AgentLaunchResult{}, err
	}
	registration, runStore, run, err := s.openActiveRunStore(ctx)
	if err != nil {
		return AgentLaunchResult{}, err
	}
	defer func() { _ = runStore.Close() }()
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
	var seeder worker.CredentialSeeder
	if authPath != "" {
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
		InvocationID:        invocationID,
		RunID:               run.ID,
		Role:                role,
		Stage:               string(stage),
		SpecificationPacket: run.SpecificationPacket,
		RepositoryGuidance:  packet.Issue.Body,
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
		PromptVersion:       prompt.Version,
		PermittedPaths:      append([]string(nil), request.PermittedPaths...),
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
		InvocationDirectory: packetDirectory,
		ResultDirectory:     resultDirectory,
		PermittedPaths:      append([]string(nil), request.PermittedPaths...),
		PromptVersion:       prompt.Version,
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
	credentialStoreID := ""
	if authPath != "" {
		credentialStoreID = registration.Path
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
	cleanupSurface = runWorkspace.Implementation
	invocation.WorkspaceID = string(runWorkspace.ID)
	invocation.StatusSurfaceID = string(runWorkspace.Status.ID)
	invocation.ImplementationSurfaceID = string(runWorkspace.Implementation.ID)
	invocation.ChecksSurfaceID = string(runWorkspace.Checks.ID)
	if err := invocationStore.SaveInvocation(ctx, invocation); err != nil {
		return AgentLaunchResult{}, fmt.Errorf("persist visible terminal handles: %w", err)
	}
	if err := terminalRuntime.Notify(ctx, terminal.Notification{WorkspaceID: control.ID, Title: "factory run started", Body: run.ID + " implementation agent active"}); err != nil {
		return AgentLaunchResult{}, err
	}
	session, err := harnessRuntime.Start(ctx, harness.StartRequest{
		InvocationID:    invocationID,
		RunID:           run.ID,
		Role:            role,
		Stage:           string(stage),
		WorkspaceID:     runWorkspace.ID,
		Surface:         runWorkspace.Implementation,
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
	run.Stage = store.StageImplementation
	run.Status = store.StatusActive
	run.UpdatedAt = s.deps.Now().UTC()
	if err := s.persistAgentRunState(ctx, registration, runStore, previousRun, *run); err != nil {
		return AgentLaunchResult{}, fmt.Errorf("persist implementation stage: %w", err)
	}
	return AgentLaunchResult{Invocation: invocation, Prompt: promptText}, nil
}

// AcceptAgentReport reads only the invocation report file, validates its
// identity and observed worktree state, and then lets the coordinator decide
// the resulting workflow status.
func (s *Service) AcceptAgentReport(ctx context.Context, request AgentReportRequest) (AgentResult, error) {
	if strings.TrimSpace(request.InvocationID) == "" {
		return AgentResult{}, errors.New("invocation id is required")
	}
	registration, runStore, run, err := s.openActiveRunStore(ctx)
	if err != nil {
		return AgentResult{}, err
	}
	defer func() { _ = runStore.Close() }()
	invocationStore, ok := runStore.(InvocationStore)
	if !ok {
		return AgentResult{}, errors.New("operational store does not support visible invocations")
	}
	if run == nil {
		return AgentResult{}, errors.New("no active run")
	}
	if request.RunID != "" && request.RunID != run.ID {
		return AgentResult{}, fmt.Errorf("active run is %s, not %s", run.ID, request.RunID)
	}
	invocation, err := invocationStore.Invocation(ctx, run.ID, request.InvocationID)
	if err != nil {
		return AgentResult{}, err
	}
	if invocation == nil {
		return AgentResult{}, fmt.Errorf("invocation %q does not belong to run %q", request.InvocationID, run.ID)
	}
	if invocation.Status != store.InvocationStatusActive {
		return AgentResult{}, fmt.Errorf("invocation %q is already %s", invocation.ID, invocation.Status)
	}
	if err := validateAgentRunState(*run); err != nil {
		return AgentResult{}, err
	}
	if run.Stage != invocation.Stage {
		return AgentResult{}, fmt.Errorf("invocation stage %q does not match active run stage %q", invocation.Stage, run.Stage)
	}
	if len(request.PermittedPaths) > 0 {
		if err := report.ValidatePermittedPaths(request.PermittedPaths); err != nil {
			return AgentResult{}, err
		}
		if !sameStrings(request.PermittedPaths, invocation.PermittedPaths) {
			return AgentResult{}, errors.New("agent report permitted paths do not match the invocation policy")
		}
	}
	path := filepath.Join(invocation.ResultDirectory, report.ReportFileName)
	value, err := report.Read(path)
	if err != nil {
		return AgentResult{}, err
	}
	validationContext := report.ValidationContext{
		InvocationID:   invocation.ID,
		RunID:          invocation.RunID,
		Harness:        invocation.Harness,
		Role:           invocation.Role,
		Stage:          string(invocation.Stage),
		WorktreePath:   run.Worktree,
		PermittedPaths: invocation.PermittedPaths,
	}
	inspector := s.worktreeInspector()
	if inspector == nil {
		return AgentResult{}, errors.New("worktree runtime does not support report inspection")
	}
	state, inspectErr := inspector.Inspect(ctx, run.Worktree)
	if inspectErr != nil {
		return AgentResult{}, fmt.Errorf("inspect worktree for agent report: %w", inspectErr)
	}
	validationContext.ObservedChanges = state.ChangedPaths
	validationContext.WorktreeObserved = true
	if run.CheckpointSHA != "" && state.HeadSHA != run.CheckpointSHA {
		return AgentResult{}, fmt.Errorf("agent report worktree HEAD %q does not match checkpoint %q", state.HeadSHA, run.CheckpointSHA)
	}
	if err := report.Validate(value, validationContext); err != nil {
		return AgentResult{}, err
	}
	_, harnessRuntime, err := s.ensureAgentRuntime(registration.Cmux.SocketPath)
	if err != nil {
		return AgentResult{}, fmt.Errorf("ensure agent runtime: %w", err)
	}
	nativeSessionID := invocation.NativeSessionID
	if invocation.NativeSessionID != "" {
		if value.NativeSessionID != "" && value.NativeSessionID != invocation.NativeSessionID {
			return AgentResult{}, errors.New("agent report native session identifier does not match persisted invocation")
		}
	} else {
		nativeSessionID = value.NativeSessionID
	}
	if nativeSessionID == "" {
		return AgentResult{}, errors.New("accepted agent report must retain a native session identifier")
	}
	if err := harnessRuntime.Finish(ctx, harness.Session{InvocationID: invocation.ID, NativeSessionID: nativeSessionID, Surface: terminal.Surface{ID: terminal.SurfaceID(invocation.ImplementationSurfaceID), WorkspaceID: terminal.WorkspaceID(invocation.WorkspaceID), Name: invocation.Role}}); err != nil {
		return AgentResult{}, fmt.Errorf("finish accepted harness session: %w", err)
	}
	invocation.NativeSessionID = nativeSessionID
	switch value.Outcome {
	case report.OutcomeCompleted:
		invocation.Status = store.InvocationStatusCompleted
	case report.OutcomeNeedsClarification:
		invocation.Status = store.InvocationStatusWaitingForHuman
	case report.OutcomeCannotProceed:
		invocation.Status = store.InvocationStatusCannotProceed
	}
	invocation.UpdatedAt = s.deps.Now().UTC()
	if err := invocationStore.SaveInvocation(ctx, *invocation); err != nil {
		return AgentResult{}, fmt.Errorf("persist accepted invocation: %w", err)
	}
	previousRun := *run
	run.Stage = store.StageImplementation
	switch value.Outcome {
	case report.OutcomeNeedsClarification:
		run.Status = store.StatusWaitingForHuman
	case report.OutcomeCannotProceed:
		run.Status = store.StatusFailed
	default:
		run.Status = store.StatusActive
	}
	run.UpdatedAt = s.deps.Now().UTC()
	if err := s.persistAgentRunState(ctx, registration, runStore, previousRun, *run); err != nil {
		return AgentResult{}, fmt.Errorf("persist accepted agent state: %w", err)
	}
	return AgentResult{Invocation: *invocation, Report: value}, nil
}

// RunAgent launches the visible agent and accepts a report when one is already
// present. Interactive callers normally use StartAgent and accept later.
func (s *Service) RunAgent(ctx context.Context, request AgentRequest) (AgentResult, error) {
	launch, err := s.StartAgent(ctx, request)
	if err != nil {
		return AgentResult{}, err
	}
	return s.AcceptAgentReport(ctx, AgentReportRequest{RunID: launch.Invocation.RunID, InvocationID: launch.Invocation.ID, PermittedPaths: request.PermittedPaths})
}

// normalizeAgentRequest fills the only stage/role defaults owned by issue #6.
func normalizeAgentRequest(request AgentRequest) AgentRequest {
	if request.Role == "" {
		request.Role = "implementation"
	}
	if request.Stage == "" {
		request.Stage = store.StageImplementation
	}
	if len(request.PermittedPaths) == 0 {
		request.PermittedPaths = []string{"."}
	}
	return request
}

// validateAgentRequest rejects roles and stages outside this ticket's owned
// implementation seam before external side effects occur.
func validateAgentRequest(request AgentRequest) error {
	if request.Role != "implementation" {
		return fmt.Errorf("agent role %q is not supported; issue #6 owns implementation", request.Role)
	}
	if request.Stage != store.StageImplementation {
		return fmt.Errorf("agent stage %q is not supported; issue #6 owns implementation", request.Stage)
	}
	if request.Model != "" && strings.ContainsAny(request.Model, "\x00\r\n ") {
		return errors.New("agent model contains unsafe characters")
	}
	if request.ReasoningEffort != "" && strings.ContainsAny(request.ReasoningEffort, "\x00\r\n ") {
		return errors.New("agent reasoning effort contains unsafe characters")
	}
	if request.CodexAuthPath != "" {
		if !filepath.IsAbs(request.CodexAuthPath) || strings.ContainsAny(request.CodexAuthPath, "\x00\r\n") {
			return errors.New("agent Codex auth path must be absolute and free of control characters")
		}
	}
	return nil
}

// resolveAgentPolicy applies frozen repository harness/model policy and only
// permits explicit overrides declared by that packet.
func resolveAgentPolicy(repository config.RepositoryConfig, request AgentRequest) (config.Harness, string, error) {
	harnessName := request.Harness
	if harnessName == "" {
		harnessName = repository.RoleHarnessDefaults[request.Role]
	}
	if harnessName != config.HarnessCodex && harnessName != config.HarnessClaude {
		return "", "", fmt.Errorf("no supported harness policy for role %q", request.Role)
	}
	if request.Harness != "" && !hasOverride(repository.AllowedOverrides, config.OverrideHarness) && request.Harness != repository.RoleHarnessDefaults[request.Role] {
		return "", "", errors.New("harness override is not allowed by repository policy")
	}
	model := request.Model
	options := repository.ModelOptions[request.Role]
	if model == "" && len(options) > 0 {
		model = options[0]
	}
	if model == "" {
		return "", "", fmt.Errorf("no model policy for role %q", request.Role)
	}
	if !contains(options, model) && !hasOverride(repository.AllowedOverrides, config.OverrideModel) {
		return "", "", fmt.Errorf("model %q is not allowed for role %q", model, request.Role)
	}
	if request.ReasoningEffort != "" && !hasOverride(repository.AllowedOverrides, config.OverrideReasoningEffort) {
		return "", "", errors.New("reasoning_effort override is not allowed by repository policy")
	}
	return harnessName, model, nil
}

// hasOverride reports whether a repository policy explicitly permits a setting.
func hasOverride(values []config.OverrideName, wanted config.OverrideName) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// contains reports whether a string appears in a policy list.
func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// sameStrings compares policy lists in their coordinator-preserved order.
func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// validateAgentRunState enforces the implementation role's legal predecessor
// stages before it starts or resumes a visible harness session.
func validateAgentRunState(run store.Run) error {
	if run.Status != store.StatusActive {
		return fmt.Errorf("cannot start implementation agent from run status %q", run.Status)
	}
	switch run.Stage {
	case store.StageClaim, store.StageTest, store.StageImplementation:
		return nil
	default:
		return fmt.Errorf("cannot start implementation agent from run stage %q", run.Stage)
	}
}

// persistAgentRunState uses the coordinator's GitHub label/comment transition
// when the claimed run has a status comment, retaining a direct-store fallback
// for legacy runs that predate that supervision record.
func (s *Service) persistAgentRunState(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, previous, next store.Run) error {
	if next.StatusCommentID == "" || s.deps.GitHub == nil {
		return saveRunWithRetry(ctx, runStore, next)
	}
	repository := github.Repository{Owner: registration.GitHub.Owner, Name: registration.GitHub.Repository}
	issue, err := s.deps.GitHub.Issue(ctx, repository, next.IssueNumber)
	if err != nil {
		return fmt.Errorf("read issue for agent state transition: %w", err)
	}
	if _, err := s.applyStateTransition(ctx, runStore, stateTransition{
		Repository: repository,
		Issue:      issue,
		Previous:   previous,
		Next:       next,
	}); err != nil {
		return err
	}
	return nil
}

// writeInvocationPacket atomically writes the worker-readable packet and
// leaves credentials and host paths outside its contents.
func writeInvocationPacket(directory string, packet InvocationPacket) error {
	data, err := json.MarshalIndent(packet, "", "  ")
	if err != nil {
		return fmt.Errorf("encode invocation packet: %w", err)
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(directory, ".packet-*.tmp")
	if err != nil {
		return fmt.Errorf("create invocation packet: %w", err)
	}
	path := temporary.Name()
	defer func() { _ = os.Remove(path) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect invocation packet: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write invocation packet: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync invocation packet: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close invocation packet: %w", err)
	}
	if err := os.Rename(path, filepath.Join(directory, invocationPacketFileName)); err != nil {
		return fmt.Errorf("publish invocation packet: %w", err)
	}
	return nil
}
