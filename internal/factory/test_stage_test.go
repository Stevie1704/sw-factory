package factory_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestTestStageAcceptsVerifiedRedTestsAndLaunchesImplementation verifies the
// complete public handoff: baseline -> test invocation -> independent red
// command -> scoped test checkpoint -> protected implementation launch.
func TestTestStageAcceptsVerifiedRedTestsAndLaunchesImplementation(t *testing.T) {
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	worktreePath := filepath.Join(root, "worktree")
	if err := os.MkdirAll(filepath.Join(repositoryPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryPath, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(worktreePath, "internal", "factory"), 0o755); err != nil {
		t.Fatal(err)
	}
	protectedPath := filepath.Join(worktreePath, "internal", "factory", "agent_test.go")
	if err := os.WriteFile(protectedPath, []byte("package factory_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	policy := validRepositoryConfig()
	policy.RoleHarnessDefaults["test"] = config.HarnessCodex
	policy.ModelOptions["test"] = []string{"gpt-5"}
	storeRuntime := &agentRunStore{runs: map[string]store.Run{}, invocations: map[string]store.Invocation{}, gateResults: map[string][]store.GateResult{}}
	workerRuntime := &agentWorker{results: []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}, {ExitCode: 1, Stdout: "expected behavior assertion"}}}
	terminalRuntime := &agentTerminal{}
	harnessRuntime := &agentHarness{}
	githubRuntime := &fakeGitHub{issueValue: github.Issue{Number: 12, Title: "Test stage", State: "open", Labels: []string{github.LabelAgentReady}}}
	workspace := &testStageWorkspace{
		workspace: gitadapter.Workspace{BaseSHA: factoryGateCheckpoint, Branch: "factory/run-test-stage", Worktree: worktreePath},
		state:     gitadapter.WorktreeState{RepositoryPath: repositoryPath, Branch: "factory/run-test-stage", HeadSHA: factoryGateCheckpoint},
	}
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{
		Path:                 repositoryPath,
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		Cmux:                 config.CmuxConfig{ControlWorkspace: "factory-control"},
		OperationalDataPath:  filepath.Join(root, "state", "factory.db"),
		RepositoryConfigPath: filepath.Join(repositoryPath, config.RepositoryConfigFileName),
	}}}
	ids := []string{"run-test-stage", "test-invocation", "implementation-invocation"}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:         &fakeConfig{value: host},
		OpenStore:      func(context.Context, string) (factory.OperationalStore, error) { return storeRuntime, nil },
		LoadRepository: func(string) (config.RepositoryConfig, error) { return policy, nil },
		GitHub:         githubRuntime,
		CommitStatuses: &gateStatuses{},
		Worktree:       workspace,
		GitWorkspace:   workspace,
		Worker:         workerRuntime,
		Terminal:       terminalRuntime,
		Harness:        harnessRuntime,
		Now:            func() time.Time { return time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC) },
		NewRunID: func() (string, error) {
			if len(ids) == 0 {
				return "", errors.New("test-stage id fixture exhausted")
			}
			id := ids[0]
			ids = ids[1:]
			return id, nil
		},
		Coordinator: "coordinator-test",
	})

	claimed, err := service.ClaimIssue(context.Background(), 12)
	if err != nil {
		t.Fatalf("ClaimIssue() error = %v", err)
	}
	if _, err := service.RunBaseline(context.Background(), factory.BaselineRequest{RunID: claimed.Run.ID}); err != nil {
		t.Fatalf("RunBaseline() error = %v", err)
	}
	if storeRuntime.current.Stage != store.StageTest {
		t.Fatalf("baseline stage = %q, want test", storeRuntime.current.Stage)
	}
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() test error = %v", err)
	}
	workspace.state.ChangedPaths = []string{"internal/factory/agent_test.go"}
	value := report.Report{
		SchemaVersion: report.SchemaVersion,
		InvocationID:  launch.Invocation.ID,
		RunID:         launch.Invocation.RunID,
		Harness:       launch.Invocation.Harness,
		Role:          "test",
		Stage:         "test",
		Outcome:       report.OutcomeCompleted,
		Summary:       "focused behavior test is red on base",
		TestHandoff: &report.TestHandoff{
			AcceptanceCoverage:    []report.AcceptanceMapping{{Criterion: "behavior", Evidence: "focused test"}},
			ChangedFiles:          []string{"internal/factory/agent_test.go"},
			FocusedTestCommand:    "go test ./internal/factory -run TestBehavior",
			ExpectedFailureReason: "expected behavior assertion",
			ObservedFailureEvidence: []report.Evidence{{
				Kind:   "exit_code",
				Detail: "focused command exited with code 1",
			}},
		},
		NativeSessionID: "test-session",
		ReportedAt:      time.Now().UTC(),
	}
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, value); err != nil {
		t.Fatalf("WriteAtomicForInvocation() error = %v", err)
	}
	accepted, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: claimed.Run.ID, InvocationID: launch.Invocation.ID})
	if err != nil {
		t.Fatalf("AcceptAgentReport() error = %v", err)
	}
	if accepted.Invocation.Status != store.InvocationStatusCompleted {
		t.Fatalf("test invocation = %#v, want completed", accepted.Invocation)
	}
	if storeRuntime.current.Stage != store.StageImplementation || storeRuntime.current.TestCheckpointSHA == "" || len(storeRuntime.current.ProtectedTestPaths) != 1 {
		t.Fatalf("handoff run = %#v, want implementation with protected test checkpoint", storeRuntime.current)
	}
	if len(workerRuntime.commands) != 3 || workerRuntime.commands[2].Role != "test" || workerRuntime.commands[2].Command != value.TestHandoff.FocusedTestCommand {
		t.Fatalf("worker commands = %#v, want independently rerun focused test", workerRuntime.commands)
	}
	if len(workerRuntime.starts) < 3 || workerRuntime.starts[len(workerRuntime.starts)-1].Role != "implementation" {
		t.Fatalf("worker starts = %#v, want automatic implementation launch", workerRuntime.starts)
	}
	if launch, ok := storeRuntime.invocations["inv-implementation-invocation"]; !ok || launch.Status != store.InvocationStatusActive || launch.Role != "implementation" {
		t.Fatalf("implementation invocation = %#v, want active protected handoff consumer", launch)
	}

	// A direct implementation edit to a protected test path is rejected before
	// the implementation report can finish its invocation.
	if err := os.WriteFile(protectedPath, []byte("package factory_test\n// edited by implementation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace.state.ChangedPaths = []string{"internal/factory/agent_test.go"}
	implementation := storeRuntime.invocations["inv-implementation-invocation"]
	implementationReport := report.Report{
		SchemaVersion: report.SchemaVersion,
		InvocationID:  implementation.ID,
		RunID:         implementation.RunID,
		Harness:       implementation.Harness,
		Role:          "implementation",
		Stage:         "implementation",
		Outcome:       report.OutcomeCompleted,
		Summary:       "implementation attempted to finish",
		Handoff: &report.Handoff{
			ChangeSummary:          "implementation change",
			AcceptanceMapping:      []report.AcceptanceMapping{{Criterion: "behavior", Evidence: "focused test"}},
			ProductionFilesChanged: []string{"internal/factory/agent_test.go"},
			FocusedCommands:        []string{"go test ./internal/factory"},
		},
		NativeSessionID: "implementation-session",
		ReportedAt:      time.Now().UTC(),
	}
	if _, err := report.WriteAtomicForInvocation(implementation.ResultDirectory, implementation.ID, implementationReport); err != nil {
		t.Fatalf("WriteAtomicForInvocation() implementation error = %v", err)
	}
	if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: claimed.Run.ID, InvocationID: implementation.ID}); err == nil {
		t.Fatal("AcceptAgentReport() accepted an implementation edit to a protected test path")
	}
	if got := storeRuntime.invocations[implementation.ID].Status; got != store.InvocationStatusActive {
		t.Fatalf("protected-path invocation status = %q, want active after rejection", got)
	}
}

// pilotDecisionComments supplies the authorized measured-pilot decision used
// by the objection-cycle fixture without contacting GitHub.
type pilotDecisionComments struct {
	comments []github.Comment
}

// IssueComments returns the fixture's immutable pilot-decision comments.
func (r *pilotDecisionComments) IssueComments(context.Context, github.Repository, int) ([]github.Comment, error) {
	return append([]github.Comment(nil), r.comments...), nil
}

// TestImplementationObjectionResumesTheOriginalTestSession verifies the
// bounded objection loop: implementation submits evidence without touching a
// protected test, the original native test session resumes, and a revised red
// test unlocks implementation again.
func TestImplementationObjectionResumesTheOriginalTestSession(t *testing.T) {
	service, storeRuntime, workerRuntime, harnessRuntime, implementation, workspace := newObjectionCycleFixture(t)
	run := *storeRuntime.current
	workspace.state.HeadSHA = run.CheckpointSHA
	workspace.state.ChangedPaths = []string{"internal/factory/agent.go"}
	implementationReport := report.Report{
		SchemaVersion: report.SchemaVersion,
		InvocationID:  implementation.ID,
		RunID:         implementation.RunID,
		Harness:       implementation.Harness,
		Role:          "implementation",
		Stage:         "implementation",
		Outcome:       report.OutcomeCompleted,
		Summary:       "implementation found the protected assertion too narrow",
		Handoff: &report.Handoff{
			ChangeSummary:          "implementation change with a test objection",
			AcceptanceMapping:      []report.AcceptanceMapping{{Criterion: "behavior", Evidence: "implementation evidence"}},
			ProductionFilesChanged: []string{"internal/factory/agent.go"},
			FocusedCommands:        []string{"go test ./internal/factory"},
			TestObjections: []report.TestObjection{{
				Test:     "internal/factory/agent_test.go:TestBehavior",
				Claim:    "the test requires an obsolete internal detail",
				Evidence: "equivalent public behavior passes with the implementation change",
			}},
		},
		NativeSessionID: "implementation-session",
		ReportedAt:      time.Now().UTC(),
	}
	if _, err := report.WriteAtomicForInvocation(implementation.ResultDirectory, implementation.ID, implementationReport); err != nil {
		t.Fatalf("WriteAtomicForInvocation() objection error = %v", err)
	}
	if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: run.ID, InvocationID: implementation.ID}); err != nil {
		t.Fatalf("AcceptAgentReport() objection error = %v", err)
	}
	if storeRuntime.current.Stage != store.StageTest || storeRuntime.current.Status != store.StatusActive || storeRuntime.current.TestRevisionAttempts != 1 || storeRuntime.current.TestObjection == nil {
		t.Fatalf("objection projection = %#v, want active test revision 1", storeRuntime.current)
	}
	if len(storeRuntime.github.editedComments) == 0 {
		t.Fatal("objection transition did not update the status comment")
	}
	statusComment := storeRuntime.github.editedComments[len(storeRuntime.github.editedComments)-1].body
	for _, expected := range []string{"- attempts: 1/2", "- remaining: 1", "- outcomes: 1=pending", "- current test: `internal/factory/agent_test.go:TestBehavior`"} {
		if !strings.Contains(statusComment, expected) {
			t.Fatalf("objection status comment = %q, want %q", statusComment, expected)
		}
	}
	if len(harnessRuntime.resumes) != 1 || harnessRuntime.resumes[0].ResumeSessionID != "test-session" {
		t.Fatalf("native resumes = %#v, want original test session", harnessRuntime.resumes)
	}
	if !strings.Contains(harnessRuntime.resumes[0].Prompt, "the test requires an obsolete internal detail") || !strings.Contains(harnessRuntime.resumes[0].Prompt, "revision attempt 1 of 2") {
		t.Fatalf("resumed test prompt = %q, want objection and budget context", harnessRuntime.resumes[0].Prompt)
	}
	revision := storeRuntime.invocations["inv-revision-invocation"]
	if revision.Status != store.InvocationStatusActive || revision.Role != "test" {
		t.Fatalf("revision invocation = %#v, want active test invocation", revision)
	}
	workspace.state.ChangedPaths = []string{"internal/factory/agent.go", "internal/factory/agent_test.go"}
	workerRuntime.results = append(workerRuntime.results, worker.CommandResult{ExitCode: 1, Stdout: "expected revised behavior assertion"})
	revisionReport := report.Report{
		SchemaVersion: report.SchemaVersion,
		InvocationID:  revision.ID,
		RunID:         revision.RunID,
		Harness:       revision.Harness,
		Role:          "test",
		Stage:         "test",
		Outcome:       report.OutcomeCompleted,
		Summary:       "test accepted the objection and revised the assertion",
		TestObjectionResponse: &report.TestObjectionResponse{
			Decision: report.TestObjectionAccepted,
			Reason:   "the public behavior is the stable contract",
		},
		TestHandoff: &report.TestHandoff{
			AcceptanceCoverage:    []report.AcceptanceMapping{{Criterion: "behavior", Evidence: "revised focused test"}},
			ChangedFiles:          []string{"internal/factory/agent_test.go"},
			FocusedTestCommand:    "go test ./internal/factory -run TestBehavior",
			ExpectedFailureReason: "expected revised behavior assertion",
			ObservedFailureEvidence: []report.Evidence{{
				Kind: "exit_code", Detail: "focused command exited with code 1",
			}},
		},
		NativeSessionID: "session-repaired",
		ReportedAt:      time.Now().UTC(),
	}
	if _, err := report.WriteAtomicForInvocation(revision.ResultDirectory, revision.ID, revisionReport); err != nil {
		t.Fatalf("WriteAtomicForInvocation() revision error = %v", err)
	}
	if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: run.ID, InvocationID: revision.ID}); err != nil {
		t.Fatalf("AcceptAgentReport() revision error = %v", err)
	}
	if storeRuntime.current.Stage != store.StageImplementation || storeRuntime.current.Status != store.StatusActive || storeRuntime.current.TestObjection != nil || storeRuntime.current.TestRevisionAttempts != 1 || len(storeRuntime.current.TestRevisionHistory) != 1 || storeRuntime.current.TestRevisionHistory[0].Outcome != store.TestRevisionAccepted {
		t.Fatalf("revised handoff projection = %#v, want active implementation with accepted cycle", storeRuntime.current)
	}
	if len(workerRuntime.starts) < 4 || workerRuntime.starts[len(workerRuntime.starts)-1].Role != "implementation" {
		t.Fatalf("worker starts = %#v, want implementation after revised handoff", workerRuntime.starts)
	}
}

// TestImplementationObjectionCanBeRejectedByTheTestRole verifies a test role
// may reject a dispute with reasoning while preserving the implementation
// changes for explicit human disposition.
func TestImplementationObjectionCanBeRejectedByTheTestRole(t *testing.T) {
	service, storeRuntime, workerRuntime, harnessRuntime, implementation, workspace := newObjectionCycleFixture(t)
	run := *storeRuntime.current
	workspace.state.HeadSHA = run.CheckpointSHA
	workspace.state.ChangedPaths = []string{"internal/factory/agent.go"}
	implementationReport := report.Report{
		SchemaVersion: report.SchemaVersion,
		InvocationID:  implementation.ID,
		RunID:         implementation.RunID,
		Harness:       implementation.Harness,
		Role:          "implementation",
		Stage:         "implementation",
		Outcome:       report.OutcomeCompleted,
		Summary:       "implementation challenged the protected assertion",
		Handoff: &report.Handoff{
			ChangeSummary:          "implementation change with a test objection",
			AcceptanceMapping:      []report.AcceptanceMapping{{Criterion: "behavior", Evidence: "implementation evidence"}},
			ProductionFilesChanged: []string{"internal/factory/agent.go"},
			FocusedCommands:        []string{"go test ./internal/factory"},
			TestObjections: []report.TestObjection{{
				Test:     "internal/factory/agent_test.go:TestBehavior",
				Claim:    "the assertion is too narrow",
				Evidence: "the frozen behavior remains required",
			}},
		},
		NativeSessionID: "implementation-session",
		ReportedAt:      time.Now().UTC(),
	}
	if _, err := report.WriteAtomicForInvocation(implementation.ResultDirectory, implementation.ID, implementationReport); err != nil {
		t.Fatalf("WriteAtomicForInvocation() objection error = %v", err)
	}
	if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: run.ID, InvocationID: implementation.ID}); err != nil {
		t.Fatalf("AcceptAgentReport() objection error = %v", err)
	}
	revision := storeRuntime.invocations["inv-revision-invocation"]
	workspace.state.ChangedPaths = []string{"internal/factory/agent.go"}
	startsBeforeRejection := len(workerRuntime.starts)
	revisionReport := report.Report{
		SchemaVersion: report.SchemaVersion,
		InvocationID:  revision.ID,
		RunID:         revision.RunID,
		Harness:       revision.Harness,
		Role:          "test",
		Stage:         "test",
		Outcome:       report.OutcomeCompleted,
		Summary:       "test role confirms the protected assertion",
		TestObjectionResponse: &report.TestObjectionResponse{
			Decision: report.TestObjectionRejected,
			Reason:   "the frozen behavior is still the required contract",
		},
		NativeSessionID: revision.NativeSessionID,
		ReportedAt:      time.Now().UTC(),
	}
	if _, err := report.WriteAtomicForInvocation(revision.ResultDirectory, revision.ID, revisionReport); err != nil {
		t.Fatalf("WriteAtomicForInvocation() rejection error = %v", err)
	}
	accepted, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: run.ID, InvocationID: revision.ID})
	if err != nil {
		t.Fatalf("AcceptAgentReport() rejection error = %v", err)
	}
	if accepted.Invocation.Status != store.InvocationStatusCompleted || storeRuntime.current.Status != store.StatusWaitingForHuman || storeRuntime.current.Stage != store.StageTest {
		t.Fatalf("rejected objection projection = invocation %#v run %#v, want completed invocation and waiting human/test", accepted.Invocation, storeRuntime.current)
	}
	if storeRuntime.current.TestObjection == nil || storeRuntime.current.TestRevisionAttempts != 1 || len(storeRuntime.current.TestRevisionHistory) != 1 || storeRuntime.current.TestRevisionHistory[0].Outcome != store.TestRevisionRejected {
		t.Fatalf("rejected objection history = %#v, want one rejected attempt with preserved objection", storeRuntime.current)
	}
	if !strings.Contains(storeRuntime.current.LifecycleReason, "the frozen behavior is still the required contract") {
		t.Fatalf("rejected objection reason = %q, want test reasoning", storeRuntime.current.LifecycleReason)
	}
	if len(harnessRuntime.resumes) != 1 || len(workerRuntime.starts) != startsBeforeRejection {
		t.Fatalf("rejected objection effects = resumes=%d starts=%d (before=%d), want one native resume and no implementation relaunch", len(harnessRuntime.resumes), len(workerRuntime.starts), startsBeforeRejection)
	}
	if len(storeRuntime.github.editedComments) == 0 || !strings.Contains(storeRuntime.github.editedComments[len(storeRuntime.github.editedComments)-1].body, "1=rejected") {
		t.Fatalf("rejected objection status = %#v, want recorded outcome", storeRuntime.github.editedComments)
	}
}

// TestImplementationObjectionEscalatesAfterTwoRevisionAttempts verifies the
// coordinator never starts a third test revision after the bounded budget is
// exhausted.
func TestImplementationObjectionEscalatesAfterTwoRevisionAttempts(t *testing.T) {
	service, storeRuntime, workerRuntime, harnessRuntime, implementation, workspace := newObjectionCycleFixture(t)
	run := *storeRuntime.current
	writeReport := func(invocation store.Invocation, value report.Report) {
		t.Helper()
		if _, err := report.WriteAtomicForInvocation(invocation.ResultDirectory, invocation.ID, value); err != nil {
			t.Fatalf("WriteAtomicForInvocation(%s) error = %v", invocation.ID, err)
		}
	}
	implementationObjection := func(invocation store.Invocation, attempt int) report.Report {
		return report.Report{
			SchemaVersion: report.SchemaVersion,
			InvocationID:  invocation.ID,
			RunID:         invocation.RunID,
			Harness:       invocation.Harness,
			Role:          "implementation",
			Stage:         "implementation",
			Outcome:       report.OutcomeCompleted,
			Summary:       "implementation challenged the protected assertion",
			Handoff: &report.Handoff{
				ChangeSummary:          "implementation change with a test objection",
				AcceptanceMapping:      []report.AcceptanceMapping{{Criterion: "behavior", Evidence: "implementation evidence"}},
				ProductionFilesChanged: []string{"internal/factory/agent.go"},
				FocusedCommands:        []string{"go test ./internal/factory"},
				TestObjections: []report.TestObjection{{
					Test:     "internal/factory/agent_test.go:TestBehavior",
					Claim:    "the assertion is too narrow",
					Evidence: "the frozen behavior remains required",
				}},
			},
			NativeSessionID: fmt.Sprintf("implementation-session-%d", attempt),
			ReportedAt:      time.Now().UTC(),
		}
	}
	revisedTest := func(invocation store.Invocation, attempt int) report.Report {
		reason := fmt.Sprintf("expected revised behavior assertion %d", attempt)
		return report.Report{
			SchemaVersion: report.SchemaVersion,
			InvocationID:  invocation.ID,
			RunID:         invocation.RunID,
			Harness:       invocation.Harness,
			Role:          "test",
			Stage:         "test",
			Outcome:       report.OutcomeCompleted,
			Summary:       "test accepted the objection and revised the assertion",
			TestObjectionResponse: &report.TestObjectionResponse{
				Decision: report.TestObjectionAccepted,
				Reason:   "the public behavior is the stable contract",
			},
			TestHandoff: &report.TestHandoff{
				AcceptanceCoverage:    []report.AcceptanceMapping{{Criterion: "behavior", Evidence: "revised focused test"}},
				ChangedFiles:          []string{"internal/factory/agent_test.go"},
				FocusedTestCommand:    "go test ./internal/factory -run TestBehavior",
				ExpectedFailureReason: reason,
				ObservedFailureEvidence: []report.Evidence{{
					Kind: "exit_code", Detail: "focused command exited with code 1",
				}},
			},
			NativeSessionID: "session-repaired",
			ReportedAt:      time.Now().UTC(),
		}
	}

	currentImplementation := implementation
	for attempt := 1; attempt <= 2; attempt++ {
		run = *storeRuntime.current
		workspace.state.HeadSHA = run.CheckpointSHA
		workspace.state.ChangedPaths = []string{"internal/factory/agent.go"}
		writeReport(currentImplementation, implementationObjection(currentImplementation, attempt))
		if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: run.ID, InvocationID: currentImplementation.ID}); err != nil {
			t.Fatalf("AcceptAgentReport() objection %d error = %v", attempt, err)
		}
		if storeRuntime.current.Stage != store.StageTest || storeRuntime.current.Status != store.StatusActive || storeRuntime.current.TestRevisionAttempts != attempt || storeRuntime.current.TestObjection == nil {
			t.Fatalf("objection %d projection = %#v, want active test revision", attempt, storeRuntime.current)
		}
		revisionID := "inv-revision-invocation"
		if attempt == 2 {
			revisionID = "inv-revision-invocation-2"
		}
		revision := storeRuntime.invocations[revisionID]
		workspace.state.ChangedPaths = []string{"internal/factory/agent.go", "internal/factory/agent_test.go"}
		workerRuntime.results = append(workerRuntime.results, worker.CommandResult{ExitCode: 1, Stdout: fmt.Sprintf("expected revised behavior assertion %d", attempt)})
		writeReport(revision, revisedTest(revision, attempt))
		if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: run.ID, InvocationID: revision.ID}); err != nil {
			t.Fatalf("AcceptAgentReport() revision %d error = %v", attempt, err)
		}
		if storeRuntime.current.Stage != store.StageImplementation || storeRuntime.current.Status != store.StatusActive || len(storeRuntime.current.TestRevisionHistory) != attempt || storeRuntime.current.TestRevisionHistory[attempt-1].Outcome != store.TestRevisionAccepted {
			t.Fatalf("revision %d projection = %#v, want accepted implementation handoff", attempt, storeRuntime.current)
		}
		if attempt == 1 {
			currentImplementation = storeRuntime.invocations["inv-implementation-after-revision"]
		} else {
			currentImplementation = storeRuntime.invocations["inv-implementation-after-revision-2"]
		}
	}

	run = *storeRuntime.current
	workspace.state.HeadSHA = run.CheckpointSHA
	workspace.state.ChangedPaths = []string{"internal/factory/agent.go"}
	startsBeforeEscalation := len(workerRuntime.starts)
	thirdObjection := implementationObjection(currentImplementation, 3)
	writeReport(currentImplementation, thirdObjection)
	if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: run.ID, InvocationID: currentImplementation.ID}); err != nil {
		t.Fatalf("AcceptAgentReport() third objection error = %v", err)
	}
	if storeRuntime.current.Stage != store.StageTest || storeRuntime.current.Status != store.StatusWaitingForHuman || storeRuntime.current.TestRevisionAttempts != 2 || storeRuntime.current.TestRevisionBudget != 2 || storeRuntime.current.TestObjection == nil {
		t.Fatalf("third objection projection = %#v, want waiting human after budget exhaustion", storeRuntime.current)
	}
	if len(storeRuntime.current.TestRevisionHistory) != 2 || storeRuntime.current.TestRevisionHistory[0].Outcome != store.TestRevisionAccepted || storeRuntime.current.TestRevisionHistory[1].Outcome != store.TestRevisionAccepted {
		t.Fatalf("third objection history = %#v, want two accepted attempts", storeRuntime.current.TestRevisionHistory)
	}
	if !strings.Contains(storeRuntime.current.LifecycleReason, "after 2 revision attempts") {
		t.Fatalf("third objection reason = %q, want bounded escalation", storeRuntime.current.LifecycleReason)
	}
	if len(harnessRuntime.resumes) != 2 || len(workerRuntime.starts) != startsBeforeEscalation {
		t.Fatalf("third objection effects = resumes=%d starts=%d (before=%d), want no third revision", len(harnessRuntime.resumes), len(workerRuntime.starts), startsBeforeEscalation)
	}
}

// TestImplementationObjectionWaitsForMeasuredPilotAuthorization verifies the
// pre-pilot fail-closed path preserves the objection for human disposition and
// never starts a second test session.
func TestImplementationObjectionWaitsForMeasuredPilotAuthorization(t *testing.T) {
	service, storeRuntime, workerRuntime, harnessRuntime, implementation, workspace := newObjectionCycleFixtureWith(t, false)
	run := *storeRuntime.current
	workspace.state.HeadSHA = run.CheckpointSHA
	workspace.state.ChangedPaths = []string{"internal/factory/agent.go"}
	value := report.Report{
		SchemaVersion: report.SchemaVersion, InvocationID: implementation.ID, RunID: implementation.RunID,
		Harness: implementation.Harness, Role: "implementation", Stage: "implementation", Outcome: report.OutcomeCompleted,
		Summary: "implementation found a test dispute",
		Handoff: &report.Handoff{
			ChangeSummary: "implementation change", AcceptanceMapping: []report.AcceptanceMapping{{Criterion: "behavior", Evidence: "focused test"}},
			ProductionFilesChanged: []string{"internal/factory/agent.go"}, FocusedCommands: []string{"go test ./internal/factory"},
			TestObjections: []report.TestObjection{{Test: "internal/factory/agent_test.go:TestBehavior", Claim: "the assertion is too narrow", Evidence: "public behavior is correct"}},
		},
		NativeSessionID: "implementation-session", ReportedAt: time.Now().UTC(),
	}
	if _, err := report.WriteAtomicForInvocation(implementation.ResultDirectory, implementation.ID, value); err != nil {
		t.Fatal(err)
	}
	startsBeforeObjection := len(workerRuntime.starts)
	if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: run.ID, InvocationID: implementation.ID}); err != nil {
		t.Fatalf("AcceptAgentReport() error = %v", err)
	}
	if storeRuntime.current.Status != store.StatusWaitingForHuman || storeRuntime.current.TestObjection == nil || storeRuntime.current.TestRevisionAttempts != 0 {
		t.Fatalf("run after pre-pilot objection = %#v, want waiting with preserved objection", storeRuntime.current)
	}
	if len(storeRuntime.github.editedComments) == 0 || !strings.Contains(storeRuntime.github.editedComments[len(storeRuntime.github.editedComments)-1].body, "automated revision is disabled pending measured-pilot authorization") {
		t.Fatalf("pre-pilot status comment = %#v, want measured-pilot pause reason", storeRuntime.github.editedComments)
	}
	if len(harnessRuntime.resumes) != 0 || len(workerRuntime.starts) != startsBeforeObjection {
		t.Fatalf("pre-pilot effects = resumes=%d worker starts=%d, want no resume and no new worker", len(harnessRuntime.resumes), len(workerRuntime.starts)-startsBeforeObjection)
	}
}

// newObjectionCycleFixture creates a required independent test run through a
// verified red handoff and returns its active implementation invocation.
func newObjectionCycleFixture(t *testing.T) (*factory.Service, *agentRunStore, *agentWorker, *agentHarness, store.Invocation, *testStageWorkspace) {
	return newObjectionCycleFixtureWith(t, true)
}

// newObjectionCycleFixtureWith creates the objection-cycle fixture with an
// explicit evidence-gate setting for testing both enabled and pre-pilot paths.
func newObjectionCycleFixtureWith(t *testing.T, allowAutomatedObjections bool) (*factory.Service, *agentRunStore, *agentWorker, *agentHarness, store.Invocation, *testStageWorkspace) {
	t.Helper()
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	worktreePath := filepath.Join(root, "worktree")
	if err := os.MkdirAll(filepath.Join(repositoryPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryPath, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(worktreePath, "internal", "factory"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, "internal", "factory", "agent_test.go"), []byte("package factory_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := validRepositoryConfig()
	policy.TestPolicy.AllowAutomatedObjections = allowAutomatedObjections
	policy.RoleHarnessDefaults["test"] = config.HarnessCodex
	policy.ModelOptions["test"] = []string{"gpt-5"}
	storeRuntime := &agentRunStore{runs: map[string]store.Run{}, invocations: map[string]store.Invocation{}, gateResults: map[string][]store.GateResult{}}
	workerRuntime := &agentWorker{results: []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}, {ExitCode: 1, Stdout: "expected behavior assertion"}}}
	terminalRuntime := &agentTerminal{}
	harnessRuntime := &agentHarness{}
	githubRuntime := &fakeGitHub{issueValue: github.Issue{Number: 13, Title: "Test objection", State: "open", Labels: []string{github.LabelAgentReady}}}
	workspace := &testStageWorkspace{
		workspace: gitadapter.Workspace{BaseSHA: factoryGateCheckpoint, Branch: "factory/run-objection", Worktree: worktreePath},
		state:     gitadapter.WorktreeState{RepositoryPath: repositoryPath, Branch: "factory/run-objection", HeadSHA: factoryGateCheckpoint},
	}
	storeRuntime.github = githubRuntime
	storeRuntime.worktree = &inspectingWorktree{fakeWorktree: fakeWorktree{workspace: workspace.workspace}, state: workspace.state}
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{
		Path:                 repositoryPath,
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		AuthorizedUsers:      []string{"alice"},
		Cmux:                 config.CmuxConfig{ControlWorkspace: "factory-control"},
		OperationalDataPath:  filepath.Join(root, "state", "factory.db"),
		RepositoryConfigPath: filepath.Join(repositoryPath, config.RepositoryConfigFileName),
	}}}
	ids := []string{"run-objection", "test-invocation", "implementation-invocation", "revision-invocation", "implementation-after-revision", "revision-invocation-2", "implementation-after-revision-2"}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:         &fakeConfig{value: host},
		OpenStore:      func(context.Context, string) (factory.OperationalStore, error) { return storeRuntime, nil },
		LoadRepository: func(string) (config.RepositoryConfig, error) { return policy, nil },
		GitHub:         githubRuntime,
		Comments:       &pilotDecisionComments{comments: []github.Comment{{Author: "alice", Body: "Decision: proceed"}}},
		CommitStatuses: &gateStatuses{},
		Worktree:       workspace,
		GitWorkspace:   workspace,
		Worker:         workerRuntime,
		Terminal:       terminalRuntime,
		Harness:        harnessRuntime,
		Now:            func() time.Time { return time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC) },
		NewRunID: func() (string, error) {
			if len(ids) == 0 {
				return "", errors.New("objection-cycle id fixture exhausted")
			}
			id := ids[0]
			ids = ids[1:]
			return id, nil
		},
		Coordinator: "coordinator-test",
	})
	claimed, err := service.ClaimIssue(context.Background(), 13)
	if err != nil {
		t.Fatalf("ClaimIssue() error = %v", err)
	}
	if _, err := service.RunBaseline(context.Background(), factory.BaselineRequest{RunID: claimed.Run.ID}); err != nil {
		t.Fatalf("RunBaseline() error = %v", err)
	}
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() test error = %v", err)
	}
	workspace.state.ChangedPaths = []string{"internal/factory/agent_test.go"}
	value := report.Report{
		SchemaVersion: report.SchemaVersion, InvocationID: launch.Invocation.ID, RunID: launch.Invocation.RunID,
		Harness: launch.Invocation.Harness, Role: "test", Stage: "test", Outcome: report.OutcomeCompleted,
		Summary: "focused behavior test is red on base",
		TestHandoff: &report.TestHandoff{
			AcceptanceCoverage: []report.AcceptanceMapping{{Criterion: "behavior", Evidence: "focused test"}},
			ChangedFiles:       []string{"internal/factory/agent_test.go"}, FocusedTestCommand: "go test ./internal/factory -run TestBehavior",
			ExpectedFailureReason: "expected behavior assertion", ObservedFailureEvidence: []report.Evidence{{Kind: "exit_code", Detail: "focused command exited with code 1"}},
		}, NativeSessionID: "test-session", ReportedAt: time.Now().UTC(),
	}
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, value); err != nil {
		t.Fatalf("WriteAtomicForInvocation() initial test error = %v", err)
	}
	if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: claimed.Run.ID, InvocationID: launch.Invocation.ID}); err != nil {
		t.Fatalf("AcceptAgentReport() initial test error = %v", err)
	}
	return service, storeRuntime, workerRuntime, harnessRuntime, storeRuntime.invocations["inv-implementation-invocation"], workspace
}

// TestTestStagePausesAnUnverifiableCompletedReportForHumanDisposition verifies
// malformed red-test evidence cannot leave the test run active for an
// implementation bypass or an automated revision loop.
func TestTestStagePausesAnUnverifiableCompletedReportForHumanDisposition(t *testing.T) {
	service, runStore, workerRuntime, _, _ := newAgentService(t)
	run := *runStore.current
	run.Stage = store.StageTest
	run.Status = store.StatusActive
	run.TestStageSkipped = false
	if err := runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("persist test-stage fixture: %v", err)
	}
	worktree := serviceTestWorktree(runStore)
	worktree.state.ChangedPaths = []string{"internal/factory/agent_test.go"}
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() test error = %v", err)
	}
	malformed := report.Report{
		SchemaVersion: report.SchemaVersion,
		InvocationID:  launch.Invocation.ID,
		RunID:         launch.Invocation.RunID,
		Harness:       launch.Invocation.Harness,
		Role:          "test",
		Stage:         "test",
		Outcome:       report.OutcomeCompleted,
		Summary:       "test report omitted required red evidence",
		TestHandoff: &report.TestHandoff{
			AcceptanceCoverage: []report.AcceptanceMapping{{Criterion: "behavior", Evidence: "focused test"}},
			ChangedFiles:       []string{"internal/factory/agent_test.go"},
			FocusedTestCommand: "go test ./internal/factory -run TestBehavior",
		},
		ReportedAt: time.Now().UTC(),
	}
	data, err := json.Marshal(malformed)
	if err != nil {
		t.Fatalf("marshal malformed test report: %v", err)
	}
	if err := os.WriteFile(filepath.Join(launch.Invocation.ResultDirectory, report.ReportFileName), data, 0o600); err != nil {
		t.Fatalf("write malformed test report: %v", err)
	}
	accepted, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: run.ID, InvocationID: launch.Invocation.ID})
	if err != nil {
		t.Fatalf("AcceptAgentReport() error = %v", err)
	}
	if accepted.Invocation.Status != store.InvocationStatusWaitingForHuman || runStore.current.Status != store.StatusWaitingForHuman || runStore.current.Stage != store.StageTest {
		t.Fatalf("disposition = invocation %#v run %#v, want waiting_for_human/test", accepted.Invocation, runStore.current)
	}
	if runStore.current.LifecycleReason != "focused red-test report is unverifiable" {
		t.Fatalf("lifecycle reason = %q, want content-free unverifiable disposition", runStore.current.LifecycleReason)
	}
	if workerRuntime.stops == 0 {
		t.Fatal("worker was not stopped for human disposition")
	}
}

// TestTestStageRejectsProductionChangesBeforeTechnicalExemption verifies a
// technical exemption cannot turn an out-of-scope test edit into an
// implementation handoff.
func TestTestStageRejectsProductionChangesBeforeTechnicalExemption(t *testing.T) {
	service, runStore, workerRuntime, _, _ := newAgentService(t)
	run := *runStore.current
	run.Stage = store.StageTest
	run.Status = store.StatusActive
	run.TestStageSkipped = false
	run.TestExemption = nil
	if err := runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("persist test-stage fixture: %v", err)
	}
	worktree := serviceTestWorktree(runStore)
	worktree.state.ChangedPaths = []string{"internal/factory/agent.go"}
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() test error = %v", err)
	}
	value := report.Report{
		SchemaVersion:   report.SchemaVersion,
		InvocationID:    launch.Invocation.ID,
		RunID:           launch.Invocation.RunID,
		Harness:         launch.Invocation.Harness,
		Role:            "test",
		Stage:           "test",
		Outcome:         report.OutcomeCompleted,
		Summary:         "test infrastructure is unavailable",
		Exemptions:      []report.Exemption{report.ExemptionTechnical},
		NativeSessionID: "test-session",
		ReportedAt:      time.Now().UTC(),
	}
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, value); err != nil {
		t.Fatalf("WriteAtomicForInvocation() error = %v", err)
	}
	accepted, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: run.ID, InvocationID: launch.Invocation.ID})
	if err != nil {
		t.Fatalf("AcceptAgentReport() error = %v", err)
	}
	if accepted.Invocation.Status != store.InvocationStatusWaitingForHuman || runStore.current.Status != store.StatusWaitingForHuman {
		t.Fatalf("disposition = invocation %#v run %#v, want waiting_for_human", accepted.Invocation, runStore.current)
	}
	if runStore.current.Stage != store.StageTest || len(runStore.invocations) != 1 {
		t.Fatalf("technical exemption run = %#v invocations=%d, want paused test without implementation", runStore.current, len(runStore.invocations))
	}
	if workerRuntime.stops == 0 {
		t.Fatal("worker was not stopped after the production-path dispute")
	}
}

// serviceTestWorktree returns the inspecting worktree owned by the agent
// fixture so a test-stage report can expose a test-owned observed path.
func serviceTestWorktree(runStore *agentRunStore) *inspectingWorktree {
	return runStore.worktree
}

// testStageWorkspace is a deterministic host Git workspace for the public
// coordinator test; it records the scoped test checkpoint without shelling out.
type testStageWorkspace struct {
	workspace  gitadapter.Workspace
	state      gitadapter.WorktreeState
	checkpoint gitadapter.CheckpointRequest
}

// Create returns the isolated test-stage workspace.
func (w *testStageWorkspace) Create(context.Context, string, string, string) (gitadapter.Workspace, error) {
	return w.workspace, nil
}

// Remove releases the deterministic test workspace.
func (*testStageWorkspace) Remove(context.Context, string, gitadapter.Workspace) error { return nil }

// Inspect returns the current checkpoint and changed-path projection.
func (w *testStageWorkspace) Inspect(context.Context, string) (gitadapter.WorktreeState, error) {
	return w.state, nil
}

// CreateCheckpoint records a scoped test checkpoint and clears uncommitted test
// paths as a real Git checkpoint would.
func (w *testStageWorkspace) CreateCheckpoint(_ context.Context, request gitadapter.CheckpointRequest) (gitadapter.CheckpointResult, error) {
	w.checkpoint = request
	w.state.HeadSHA = testStageCheckpoint
	w.state.ChangedPaths = nil
	return gitadapter.CheckpointResult{SHA: testStageCheckpoint, Created: true}, nil
}

// Push is unused by the test-stage handoff.
func (*testStageWorkspace) Push(context.Context, gitadapter.PushRequest) error { return nil }

// SynchronizeBase is unused by the test-stage handoff.
func (*testStageWorkspace) SynchronizeBase(context.Context, gitadapter.BaseSyncRequest) error {
	return nil
}

// testStageCheckpoint is the deterministic full commit identity returned by
// the scoped test-checkpoint fake.
const testStageCheckpoint = "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"

var _ gitadapter.GitWorkspace = (*testStageWorkspace)(nil)
