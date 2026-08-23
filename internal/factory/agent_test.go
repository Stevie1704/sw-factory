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
	if packet.InvocationID != launch.Invocation.ID || packet.SpecificationPacket == "" || packet.PromptVersion != "implementation-v1" {
		t.Fatalf("invocation packet = %#v, want frozen identity and prompt version", packet)
	}
	if len(terminalRuntime.notifications) != 1 || len(harnessRuntime.starts) != 1 {
		t.Fatalf("terminal notifications = %#v, harness starts = %#v", terminalRuntime.notifications, harnessRuntime.starts)
	}
	if len(runStore.invocations) != 1 || runStore.invocations[launch.Invocation.ID].Status != store.InvocationStatusActive {
		t.Fatalf("stored invocations = %#v, want active invocation", runStore.invocations)
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
	created := time.Date(2026, 8, 21, 8, 0, 0, 0, time.UTC)
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{Path: repositoryPath, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"}, AuthorizedUsers: []string{"alice"}, Polling: config.PollingConfig{Interval: "30s", Backoff: "5m"}, Cmux: config.CmuxConfig{ControlWorkspace: "factory-control"}, OperationalDataPath: filepath.Join(root, "state", "factory.db"), RepositoryConfigPath: filepath.Join(repositoryPath, "factory.yaml")}}}
	runStore := &agentRunStore{runs: map[string]store.Run{}, invocations: map[string]store.Invocation{}}
	runtime := &agentWorker{}
	terminalRuntime := &agentTerminal{}
	harnessRuntime := &agentHarness{}
	issue := githubIssueFixture()
	issue.Labels = []string{github.LabelAgentReady}
	githubRuntime := &fakeGitHub{issueValue: issue}
	worktree := &inspectingWorktree{
		fakeWorktree: fakeWorktree{workspace: gitadapter.Workspace{BaseSHA: "base", Branch: "factory/run-agent", Worktree: worktreePath}},
		state:        gitadapter.WorktreeState{Branch: "factory/run-agent", HeadSHA: "base", ChangedPaths: []string{"internal/factory/agent.go"}},
	}
	ids := []string{"run-agent", "generated"}
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
	return service, runStore, runtime, terminalRuntime, harnessRuntime
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
	runs        map[string]store.Run
	current     *store.Run
	invocations map[string]store.Invocation
}

// CurrentRun returns the configured active run.
func (s *agentRunStore) CurrentRun(context.Context) (*store.Run, error) { return s.current, nil }

// SaveRun stores one updated run state.
func (s *agentRunStore) SaveRun(_ context.Context, run store.Run) error {
	s.runs[run.ID] = run
	s.current = &run
	return nil
}

// SaveInvocation stores one updated invocation state.
func (s *agentRunStore) SaveInvocation(_ context.Context, invocation store.Invocation) error {
	s.invocations[invocation.ID] = invocation
	return nil
}

// Invocation loads one invocation only for its owning run.
func (s *agentRunStore) Invocation(_ context.Context, runID, invocationID string) (*store.Invocation, error) {
	value, ok := s.invocations[invocationID]
	if !ok || value.RunID != runID {
		return nil, nil
	}
	return &value, nil
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

// Close implements the operational-store seam.
func (*agentRunStore) Close() error { return nil }

// agentWorker records the worker start request without running Docker.
type agentWorker struct {
	starts []worker.StartRequest
	stops  int
}

// Start records one worker start.
func (w *agentWorker) Start(_ context.Context, request worker.StartRequest) error {
	w.starts = append(w.starts, request)
	return nil
}

// Resume implements WorkerRuntime.
func (*agentWorker) Resume(context.Context, worker.ResumeRequest) error { return nil }

// RunCommand implements WorkerRuntime.
func (*agentWorker) RunCommand(context.Context, worker.CommandRequest) (worker.CommandResult, error) {
	return worker.CommandResult{}, nil
}

// Stop implements WorkerRuntime.
func (w *agentWorker) Stop(context.Context, string) error {
	w.stops++
	return nil
}

// Inspect implements WorkerRuntime.
func (*agentWorker) Inspect(context.Context, string) (worker.Inspection, error) {
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
	starts   []harness.StartRequest
	finished []harness.Session
	startErr error
}

// Start records the coordinator-owned prompt request.
func (h *agentHarness) Start(_ context.Context, request harness.StartRequest) (harness.Session, error) {
	h.starts = append(h.starts, request)
	if h.startErr != nil {
		return harness.Session{}, h.startErr
	}
	return harness.Session{InvocationID: request.InvocationID, Surface: request.Surface}, nil
}

// Resume implements harness.Runtime.
func (*agentHarness) Resume(context.Context, harness.StartRequest) (harness.Session, error) {
	return harness.Session{}, nil
}

// Finish records the accepted session close.
func (h *agentHarness) Finish(_ context.Context, session harness.Session) error {
	h.finished = append(h.finished, session)
	return nil
}

var _ factory.InvocationStore = (*agentRunStore)(nil)
var _ worker.WorkerRuntime = (*agentWorker)(nil)
var _ terminal.TerminalRuntime = (*agentTerminal)(nil)
var _ harness.Runtime = (*agentHarness)(nil)
