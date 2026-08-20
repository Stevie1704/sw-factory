package factory_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
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

// TestAcceptAgentReportUsesOnlyTheStructuredReportAndClosesTheSession verifies
// that a valid completion changes state only after report validation.
func TestAcceptAgentReportUsesOnlyTheStructuredReportAndClosesTheSession(t *testing.T) {
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
		ReportedAt: time.Now().UTC(),
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
		t.Fatalf("run state = %#v, finished = %#v, want active run and closed surface", runStore.runs[launch.Invocation.RunID], harnessRuntime.finished)
	}
	if runtime.starts[0].WorktreePath == "" {
		t.Fatal("worker start did not receive the run worktree")
	}
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
	packetData, err := json.Marshal(factory.SpecificationPacket{Version: 1, Issue: githubIssueFixture(), RepositoryConfig: policy})
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 8, 21, 8, 0, 0, 0, time.UTC)
	run := store.Run{ID: "run-agent", RepositoryPath: repositoryPath, IssueNumber: 6, Stage: store.StageClaim, Status: store.StatusActive, Worktree: worktreePath, CheckpointSHA: "base", ImageDigest: policy.WorkerBuild.Digest, SpecificationPacket: string(packetData), CreatedAt: created, UpdatedAt: created}
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{Path: repositoryPath, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"}, AuthorizedUsers: []string{"alice"}, Polling: config.PollingConfig{Interval: "30s", Backoff: "5m"}, Cmux: config.CmuxConfig{ControlWorkspace: "factory-control"}, OperationalDataPath: filepath.Join(root, "state", "factory.db"), RepositoryConfigPath: filepath.Join(repositoryPath, "factory.yaml")}}}
	runStore := &agentRunStore{runs: map[string]store.Run{run.ID: run}, current: &run, invocations: map[string]store.Invocation{}}
	runtime := &agentWorker{}
	terminalRuntime := &agentTerminal{}
	harnessRuntime := &agentHarness{}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:    &fakeConfig{value: host},
		OpenStore: func(context.Context, string) (factory.OperationalStore, error) { return runStore, nil },
		Worker:    runtime,
		Terminal:  terminalRuntime,
		Harness:   harnessRuntime,
		Now:       func() time.Time { return created },
		NewRunID:  func() (string, error) { return "generated", nil },
		Worktree:  &fakeWorktree{},
	})
	return service, runStore, runtime, terminalRuntime, harnessRuntime
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

// Close implements the operational-store seam.
func (*agentRunStore) Close() error { return nil }

// agentWorker records the worker start request without running Docker.
type agentWorker struct {
	starts []worker.StartRequest
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
func (*agentWorker) Stop(context.Context, string) error { return nil }

// Inspect implements WorkerRuntime.
func (*agentWorker) Inspect(context.Context, string) (worker.Inspection, error) {
	return worker.Inspection{Exists: true, Running: true}, nil
}

// agentTerminal records terminal handles and notifications.
type agentTerminal struct {
	notifications []terminal.Notification
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
func (*agentTerminal) CloseSurface(context.Context, terminal.SurfaceID) error { return nil }

// CloseWorkspace implements TerminalRuntime.
func (*agentTerminal) CloseWorkspace(context.Context, terminal.WorkspaceID) error { return nil }

// agentHarness records visible session lifecycle operations.
type agentHarness struct {
	starts   []harness.StartRequest
	finished []harness.Session
}

// Start records the coordinator-owned prompt request.
func (h *agentHarness) Start(_ context.Context, request harness.StartRequest) (harness.Session, error) {
	h.starts = append(h.starts, request)
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
