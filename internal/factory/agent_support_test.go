package factory_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// inspectingWorktree adds report inspection to the shared worktree fake.
type inspectingWorktree struct {
	fakeWorktree
	state gitadapter.WorktreeState
}

// Inspect returns the configured worktree state.
func (w *inspectingWorktree) Inspect(context.Context, string) (gitadapter.WorktreeState, error) {
	return w.state, nil
}

// agentRunStore is an in-memory invocation-capable operational store.
type agentRunStore struct {
	runs        map[string]store.Run
	current     *store.Run
	invocations map[string]store.Invocation
	gateResults map[string][]store.GateResult
	github      *fakeGitHub
	worktree    *inspectingWorktree
}

// CurrentRun returns the fixture's current run.
func (s *agentRunStore) CurrentRun(context.Context) (*store.Run, error) { return s.current, nil }

// SaveRun persists the fixture's current run.
func (s *agentRunStore) SaveRun(_ context.Context, run store.Run) error {
	if s.runs == nil {
		s.runs = map[string]store.Run{}
	}
	s.runs[run.ID], s.current = run, &run
	return nil
}

// SaveInvocation persists one fixture invocation.
func (s *agentRunStore) SaveInvocation(_ context.Context, invocation store.Invocation) error {
	if s.invocations == nil {
		s.invocations = map[string]store.Invocation{}
	}
	s.invocations[invocation.ID] = invocation
	return nil
}

// HasInvocation reports whether the run has invocation history.
func (s *agentRunStore) HasInvocation(_ context.Context, runID string) (bool, error) {
	for _, value := range s.invocations {
		if value.RunID == runID {
			return true, nil
		}
	}
	return false, nil
}

// Invocation returns one fixture invocation by both identities.
func (s *agentRunStore) Invocation(_ context.Context, runID, invocationID string) (*store.Invocation, error) {
	value, ok := s.invocations[invocationID]
	if !ok || value.RunID != runID {
		return nil, nil
	}
	return &value, nil
}

// LatestInvocation returns the newest fixture invocation for a run.
func (s *agentRunStore) LatestInvocation(_ context.Context, runID string) (*store.Invocation, error) {
	var latest *store.Invocation
	for _, value := range s.invocations {
		if value.RunID != runID {
			continue
		}
		if latest == nil || value.UpdatedAt.After(latest.UpdatedAt) || value.UpdatedAt.Equal(latest.UpdatedAt) && value.ID > latest.ID {
			copy := value
			latest = &copy
		}
	}
	return latest, nil
}

// LatestInvocationByRole returns the newest fixture invocation for a role.
func (s *agentRunStore) LatestInvocationByRole(_ context.Context, runID, role string) (*store.Invocation, error) {
	var latest *store.Invocation
	for _, value := range s.invocations {
		if value.RunID != runID || value.Role != role {
			continue
		}
		if latest == nil || value.UpdatedAt.After(latest.UpdatedAt) || value.UpdatedAt.Equal(latest.UpdatedAt) && value.ID > latest.ID {
			copy := value
			latest = &copy
		}
	}
	return latest, nil
}

// ActiveInvocation returns the first active fixture invocation.
func (s *agentRunStore) ActiveInvocation(ctx context.Context, runID string) (*store.Invocation, error) {
	values, _ := s.ActiveInvocations(ctx, runID)
	if len(values) == 0 {
		return nil, nil
	}
	return &values[0], nil
}

// ActiveInvocations returns all active fixture invocations for a run.
func (s *agentRunStore) ActiveInvocations(_ context.Context, runID string) ([]store.Invocation, error) {
	values := []store.Invocation{}
	for _, value := range s.invocations {
		if value.RunID == runID && value.Status == store.InvocationStatusActive {
			values = append(values, value)
		}
	}
	return values, nil
}

// InvalidateRunResults supersedes invocations while retaining baseline gates.
func (s *agentRunStore) InvalidateRunResults(_ context.Context, runID string) error {
	for id, invocation := range s.invocations {
		if invocation.RunID == runID {
			invocation.Status = store.InvocationStatusSuperseded
			s.invocations[id] = invocation
		}
	}
	results := s.gateResults[runID][:0]
	for _, result := range s.gateResults[runID] {
		if result.Phase == store.GatePhaseBaseline {
			results = append(results, result)
		}
	}
	s.gateResults[runID] = results
	return nil
}

// InvalidateAllRunResults also removes baseline gates.
func (s *agentRunStore) InvalidateAllRunResults(ctx context.Context, runID string) error {
	if err := s.InvalidateRunResults(ctx, runID); err != nil {
		return err
	}
	delete(s.gateResults, runID)
	return nil
}

// SaveGateResults replaces matching fixture gate projections.
func (s *agentRunStore) SaveGateResults(_ context.Context, results []store.GateResult) error {
	if len(results) == 0 {
		return errors.New("at least one gate result is required")
	}
	if s.gateResults == nil {
		s.gateResults = map[string][]store.GateResult{}
	}
	for _, incoming := range results {
		values, replaced := s.gateResults[incoming.RunID], false
		for i, existing := range values {
			if existing.CheckpointSHA == incoming.CheckpointSHA && existing.Phase == incoming.Phase && existing.GateName == incoming.GateName {
				values[i], replaced = incoming, true
			}
		}
		if !replaced {
			values = append(values, incoming)
		}
		s.gateResults[incoming.RunID] = values
	}
	return nil
}

// GateResults returns matching fixture gate projections.
func (s *agentRunStore) GateResults(_ context.Context, runID string, phase store.GatePhase, checkpoint string) ([]store.GateResult, error) {
	values := []store.GateResult{}
	for _, result := range s.gateResults[runID] {
		if result.Phase == phase && result.CheckpointSHA == checkpoint {
			values = append(values, result)
		}
	}
	return values, nil
}

// Close is a no-op for the in-memory fixture.
func (*agentRunStore) Close() error { return nil }

// agentWorker records worker operations without Docker.
type agentWorker struct {
	starts                  []worker.StartRequest
	stops                   int
	commands                []worker.CommandRequest
	results                 []worker.CommandResult
	codexSeeds, claudeSeeds []worker.CredentialSeedRequest
	inspectResult           *worker.Inspection
	inspectErr, seedErr     error
}

// Start records one worker launch.
func (w *agentWorker) Start(_ context.Context, request worker.StartRequest) error {
	w.starts = append(w.starts, request)
	return nil
}

// Resume accepts worker continuation for fixture tests.
func (*agentWorker) Resume(context.Context, worker.ResumeRequest) error { return nil }

// RunCommand returns the next configured command result.
func (w *agentWorker) RunCommand(_ context.Context, request worker.CommandRequest) (worker.CommandResult, error) {
	w.commands = append(w.commands, request)
	if len(w.results) == 0 {
		return worker.CommandResult{}, nil
	}
	result := w.results[0]
	w.results = w.results[1:]
	return result, nil
}

// Stop records one worker stop.
func (w *agentWorker) Stop(context.Context, string) error { w.stops++; return nil }

// Inspect returns the configured worker state.
func (w *agentWorker) Inspect(context.Context, string) (worker.Inspection, error) {
	if w.inspectErr != nil {
		return worker.Inspection{}, w.inspectErr
	}
	if w.inspectResult != nil {
		return *w.inspectResult, nil
	}
	return worker.Inspection{Exists: true, Running: true}, nil
}

// SeedCodexCredentials records one Codex credential projection.
func (w *agentWorker) SeedCodexCredentials(_ context.Context, request worker.CredentialSeedRequest) error {
	w.codexSeeds = append(w.codexSeeds, request)
	return w.seedErr
}

// SeedClaudeCredentials records one Claude credential projection.
func (w *agentWorker) SeedClaudeCredentials(_ context.Context, request worker.CredentialSeedRequest) error {
	w.claudeSeeds = append(w.claudeSeeds, request)
	return w.seedErr
}

// agentHarness records detached harness operations.
type agentHarness struct {
	starts, resumes     []harness.StartRequest
	finished            []harness.Session
	startErr, resumeErr error
}

// Capabilities identifies the headless fixture adapter.
func (*agentHarness) Capabilities() harness.Capabilities {
	return harness.Capabilities{Name: harness.NameCodex, NativeResume: true, Headless: true}
}

// StartHeadless records one fixture harness launch.
func (h *agentHarness) StartHeadless(_ context.Context, request harness.HeadlessStartRequest) (harness.HeadlessSession, error) {
	h.starts = append(h.starts, request)
	if h.startErr != nil {
		return harness.HeadlessSession{}, h.startErr
	}
	return harness.HeadlessSession{InvocationID: request.InvocationID, RunID: request.RunID, WorkerID: request.WorkerID}, nil
}

// ResumeHeadless records one fixture native-session continuation.
func (h *agentHarness) ResumeHeadless(_ context.Context, request harness.HeadlessStartRequest) (harness.HeadlessSession, error) {
	h.resumes = append(h.resumes, request)
	if h.resumeErr != nil {
		return harness.HeadlessSession{}, h.resumeErr
	}
	return harness.HeadlessSession{InvocationID: request.InvocationID, RunID: request.RunID, WorkerID: request.WorkerID, NativeSessionID: "session-repaired"}, nil
}

// FinishHeadless records accepted harness completion.
func (h *agentHarness) FinishHeadless(_ context.Context, session harness.HeadlessSession) error {
	h.finished = append(h.finished, session)
	return nil
}

// InspectHeadless reports a running fixture process.
func (*agentHarness) InspectHeadless(context.Context, harness.HeadlessInspectionRequest) (harness.HeadlessInspection, error) {
	return harness.HeadlessInspection{Status: worker.HeadlessStatusRunning}, nil
}

// CancelHeadless accepts fixture cancellation.
func (*agentHarness) CancelHeadless(context.Context, harness.HeadlessSession) error { return nil }

// testHeadlessHarnesses maps both supported names to one recording adapter.
func testHeadlessHarnesses(runtime *agentHarness) map[config.Harness]harness.HeadlessRuntime {
	return map[config.Harness]harness.HeadlessRuntime{
		config.HarnessCodex: runtime, config.HarnessClaude: runtime,
	}
}

// githubIssueFixture returns the claimed issue used by agent tests.
func githubIssueFixture() github.Issue {
	return github.Issue{Number: 6, Title: "Implementation", Body: "Repository guidance", State: "open"}
}

// newAgentService creates a claimed implementation fixture with detached
// harness adapters.
func newAgentService(t *testing.T) (*factory.Service, *agentRunStore, *agentWorker, *agentHarness) {
	t.Helper()
	root, repositoryPath, worktreePath := t.TempDir(), "", ""
	repositoryPath = filepath.Join(root, "repository")
	worktreePath = filepath.Join(root, "worktree")
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
	policy.TestPolicy.AllowTechnicalExemption = true
	host := config.HostConfig{SchemaVersion: config.CurrentHostSchemaVersion, Repositories: []config.RepositoryRegistration{{Path: repositoryPath, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"}, AuthorizedUsers: []string{"alice"}, Polling: config.PollingConfig{Interval: "30s", Backoff: "5m"}, OperationalDataPath: filepath.Join(root, "state", "factory.db"), RepositoryConfigPath: filepath.Join(repositoryPath, "factory.yaml")}}}
	runStore := &agentRunStore{runs: map[string]store.Run{}, invocations: map[string]store.Invocation{}, gateResults: map[string][]store.GateResult{}}
	runtime, harnessRuntime := &agentWorker{}, &agentHarness{}
	issue := githubIssueFixture()
	issue.Labels = []string{github.LabelAgentReady}
	githubRuntime := &fakeGitHub{issueValue: issue}
	runStore.github = githubRuntime
	worktree := &inspectingWorktree{fakeWorktree: fakeWorktree{workspace: gitadapter.Workspace{BaseSHA: "base", Branch: "factory/run-agent", Worktree: worktreePath}}, state: gitadapter.WorktreeState{Branch: "factory/run-agent", HeadSHA: "base", ChangedPaths: []string{"internal/factory/agent.go"}}}
	runStore.worktree = worktree
	ids := []string{"run-agent", "generated", "generated-2", "generated-3"}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{Config: &fakeConfig{value: host}, OpenStore: func(context.Context, string) (factory.OperationalStore, error) { return runStore, nil }, LoadRepository: func(string) (config.RepositoryConfig, error) { return policy, nil }, Worker: runtime, HeadlessHarnesses: map[config.Harness]harness.HeadlessRuntime{config.HarnessCodex: harnessRuntime, config.HarnessClaude: harnessRuntime}, GitHub: githubRuntime, Now: func() time.Time { return time.Date(2026, 8, 21, 8, 0, 0, 0, time.UTC) }, NewRunID: func() (string, error) {
		if len(ids) == 0 {
			return "", errors.New("run id fixture exhausted")
		}
		id := ids[0]
		ids = ids[1:]
		return id, nil
	}, Worktree: worktree})
	if _, err := service.ClaimIssue(context.Background(), 6); err != nil {
		t.Fatalf("ClaimIssue() fixture setup error = %v", err)
	}
	run := *runStore.current
	run.TestStageSkipped = true
	run.TestExemption = &store.TestExemption{Kind: "human", Justification: "implementation seam fixture"}
	_ = runStore.SaveRun(context.Background(), run)
	runStore.gateResults[run.ID] = []store.GateResult{{RunID: run.ID, CheckpointSHA: run.CheckpointSHA, Phase: store.GatePhaseBaseline, GateName: policy.Gates[0].Name, Outcome: store.GateOutcomePassed, Status: string(github.CommitStatusSuccess), Blocking: policy.Gates[0].Blocking}}
	return service, runStore, runtime, harnessRuntime
}

// newDispatchingAgentService recreates a coordinator that derives real
// headless adapters from the supplied worker process runtime.
func newDispatchingAgentService(t *testing.T, runStore *agentRunStore, runtime worker.WorkerRuntime, policy config.RepositoryConfig, authentication config.AuthenticationConfig) *factory.Service {
	t.Helper()
	repackageSpecification(t, runStore, policy)
	run := *runStore.current
	host := config.HostConfig{SchemaVersion: config.CurrentHostSchemaVersion, Repositories: []config.RepositoryRegistration{{Path: run.RepositoryPath, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"}, Polling: config.PollingConfig{Interval: "30s", Backoff: "5m"}, Authentication: authentication, OperationalDataPath: filepath.Join(filepath.Dir(run.RepositoryPath), "state", "factory.db"), RepositoryConfigPath: filepath.Join(run.RepositoryPath, "factory.yaml")}}}
	githubRuntime := &fakeGitHub{issueValue: github.Issue{Number: run.IssueNumber, State: "open", Labels: []string{github.LabelAgentRunning}}, statusComment: github.Comment{ID: run.StatusCommentID, Body: factory.StatusCommentBody(run)}}
	runStore.github = githubRuntime
	worktree := &inspectingWorktree{fakeWorktree: fakeWorktree{workspace: gitadapter.Workspace{Worktree: run.Worktree}}, state: gitadapter.WorktreeState{RepositoryPath: run.RepositoryPath, Branch: run.Branch, HeadSHA: run.CheckpointSHA}}
	return factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{Config: &fakeConfig{value: host}, OpenStore: func(context.Context, string) (factory.OperationalStore, error) { return runStore, nil }, LoadRepository: func(string) (config.RepositoryConfig, error) { return policy, nil }, Worker: runtime, GitHub: githubRuntime, Worktree: worktree, Now: func() time.Time { return time.Date(2026, 8, 21, 8, 0, 0, 0, time.UTC) }, NewRunID: func() (string, error) { return "generated-dispatch", nil }})
}

// newFreshAgentService rebuilds a coordinator around persisted headless state.
func newFreshAgentService(t *testing.T, runStore *agentRunStore, worktree *inspectingWorktree, runtime *agentWorker, harnessRuntime *agentHarness) *factory.Service {
	t.Helper()
	run := *runStore.current
	host := config.HostConfig{SchemaVersion: config.CurrentHostSchemaVersion, Repositories: []config.RepositoryRegistration{{Path: run.RepositoryPath, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"}, Polling: config.PollingConfig{Interval: "30s", Backoff: "5m"}, OperationalDataPath: filepath.Join(filepath.Dir(run.RepositoryPath), "state", "factory.db"), RepositoryConfigPath: filepath.Join(run.RepositoryPath, "factory.yaml")}}}
	githubRuntime := &fakeGitHub{issueValue: github.Issue{Number: run.IssueNumber, State: "open", Labels: []string{github.LabelAgentRunning}}, statusComment: github.Comment{ID: run.StatusCommentID, Body: factory.StatusCommentBody(run)}}
	return factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{Config: &fakeConfig{value: host}, OpenStore: func(context.Context, string) (factory.OperationalStore, error) { return runStore, nil }, LoadRepository: func(string) (config.RepositoryConfig, error) { return validRepositoryConfig(), nil }, Worker: runtime, HeadlessHarnesses: testHeadlessHarnesses(harnessRuntime), GitHub: githubRuntime, Worktree: worktree, Now: func() time.Time { return time.Date(2026, 8, 21, 8, 0, 0, 0, time.UTC) }, NewRunID: func() (string, error) { return "generated", nil }})
}

// repackageSpecification updates the frozen packet with supplied policy.
func repackageSpecification(t *testing.T, runStore *agentRunStore, policy config.RepositoryConfig) {
	t.Helper()
	run := *runStore.current
	var packet factory.SpecificationPacket
	if err := json.Unmarshal([]byte(run.SpecificationPacket), &packet); err != nil {
		t.Fatal(err)
	}
	packet.RepositoryConfig = policy
	data, err := json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	run.SpecificationPacket = string(data)
	if err := runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
}

var _ factory.InvocationStore = (*agentRunStore)(nil)
var _ worker.WorkerRuntime = (*agentWorker)(nil)
var _ harness.HeadlessRuntime = (*agentHarness)(nil)
