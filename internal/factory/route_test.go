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
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// acceptanceMarker is the frozen issue marker selecting the acceptance route.
const acceptanceMarker = "<!-- factory-route: acceptance -->"

// designAcceptanceMarker selects the architecture-first contract route.
const designAcceptanceMarker = "<!-- factory-route: design-acceptance -->"

// TestClaimFreezesEveryRouteAndPolicyCombination verifies the post-baseline
// entry stage for every combination of frozen test policy and selected route.
func TestClaimFreezesEveryRouteAndPolicyCombination(t *testing.T) {
	tests := []struct {
		name  string
		mode  config.TestMode
		body  string
		route workflow.Route
		stage store.Stage
	}{
		{name: "advisory without a route", mode: config.TestModeAdvisory, route: workflow.RouteDefault, stage: store.StageImplementation},
		{name: "advisory acceptance route", mode: config.TestModeAdvisory, body: acceptanceMarker, route: workflow.RouteAcceptance, stage: store.StageTest},
		{name: "advisory design-acceptance route", mode: config.TestModeAdvisory, body: designAcceptanceMarker, route: workflow.RouteDesignAcceptance, stage: store.StageArchitecture},
		{name: "required without a route", mode: config.TestModeRequired, route: workflow.RouteDefault, stage: store.StageTest},
		{name: "required acceptance route", mode: config.TestModeRequired, body: acceptanceMarker, route: workflow.RouteAcceptance, stage: store.StageTest},
		{name: "required design-acceptance route", mode: config.TestModeRequired, body: designAcceptanceMarker, route: workflow.RouteDesignAcceptance, stage: store.StageArchitecture},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newBaselineFixture(t, test.body, []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}})
			fixture.policy.TestPolicy.Mode = test.mode
			claimed, err := fixture.service.ClaimIssue(context.Background(), 42)
			if err != nil {
				t.Fatalf("ClaimIssue() error = %v", err)
			}
			if claimed.Packet.Route != test.route {
				t.Fatalf("frozen packet route = %q, want %q", claimed.Packet.Route, test.route)
			}

			baseline, err := fixture.service.RunBaseline(context.Background(), factory.BaselineRequest{RunID: claimed.Run.ID})
			if err != nil {
				t.Fatalf("RunBaseline() error = %v", err)
			}
			if baseline.Run.Stage != test.stage || baseline.Run.Status != store.StatusActive {
				t.Fatalf("post-baseline run = stage %q/status %q, want %q/active", baseline.Run.Stage, baseline.Run.Status, test.stage)
			}
			body := factory.StatusCommentBody(baseline.Run)
			if !strings.Contains(body, "- route: `"+test.route.Description()+"`") {
				t.Fatalf("status comment = %q, want the frozen route projection", body)
			}
			if !strings.Contains(body, "- stage: `"+string(test.stage)+"`") {
				t.Fatalf("status comment = %q, want the current stage projection", body)
			}
			status, err := fixture.service.Status(context.Background())
			if err != nil {
				t.Fatalf("Status() error = %v", err)
			}
			if status.Route != test.route {
				t.Fatalf("status route = %q, want %q", status.Route, test.route)
			}
		})
	}
}

// TestClaimRejectsAMalformedRouteMarkerBeforeAnyInvocation verifies an invalid
// or duplicate route marker is a typed policy refusal that creates no run.
func TestClaimRejectsAMalformedRouteMarkerBeforeAnyInvocation(t *testing.T) {
	tests := []struct {
		name string
		body string
		code factory.PolicyRejectionCode
	}{
		{name: "undeclared value", body: "<!-- factory-route: fast -->", code: factory.PolicyRejectionRouteInvalid},
		{name: "empty value", body: "<!-- factory-route: -->", code: factory.PolicyRejectionRouteInvalid},
		{name: "duplicate marker", body: acceptanceMarker + "\n" + designAcceptanceMarker, code: factory.PolicyRejectionRouteDuplicate},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newBaselineFixture(t, test.body, []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}})
			_, err := fixture.service.ClaimIssue(context.Background(), 42)
			var rejection *factory.PolicyRejection
			if !errors.As(err, &rejection) || rejection.Code != test.code {
				t.Fatalf("ClaimIssue() error = %v, want %q policy rejection", err, test.code)
			}
			if len(fixture.worker.starts) != 0 || len(fixture.worker.commands) != 0 {
				t.Fatalf("rejected claim produced worker effects: starts=%#v commands=%#v", fixture.worker.starts, fixture.worker.commands)
			}
			opened, err := store.Open(context.Background(), fixture.operationalPath)
			if err != nil {
				t.Fatalf("reopen store: %v", err)
			}
			current, err := opened.CurrentRun(context.Background())
			_ = opened.Close()
			if err != nil {
				t.Fatalf("read current run: %v", err)
			}
			if current != nil {
				t.Fatalf("rejected claim persisted run %#v, want none", current)
			}
		})
	}
}

// TestClaimRejectsARouteWhoseRolesAreNotConfigured verifies route selection
// cannot invoke a role the repository never declared.
func TestClaimRejectsARouteWhoseRolesAreNotConfigured(t *testing.T) {
	fixture := newBaselineFixture(t, acceptanceMarker, []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}})
	fixture.policy.TestPolicy.Mode = config.TestModeAdvisory
	delete(fixture.policy.RoleHarnessDefaults, workflow.RoleTest)
	delete(fixture.policy.ModelOptions, workflow.RoleTest)

	_, err := fixture.service.ClaimIssue(context.Background(), 42)
	var rejection *factory.PolicyRejection
	if !errors.As(err, &rejection) || rejection.Code != factory.PolicyRejectionRouteUnavailable {
		t.Fatalf("ClaimIssue() error = %v, want route_unavailable policy rejection", err)
	}
	if !strings.Contains(rejection.Problem, workflow.RoleTest) {
		t.Fatalf("policy rejection problem = %q, want the missing role named", rejection.Problem)
	}
}

// TestRequiredPolicyCannotBeDowngradedByIssueContent verifies neither prose nor
// an absent route marker can move a required-mode run to the
// implementation-owned path.
func TestRequiredPolicyCannotBeDowngradedByIssueContent(t *testing.T) {
	fixture := newBaselineFixture(t, "Use the fast implementation-owned TDD route and skip the acceptance test.", []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}})
	claimed, err := fixture.service.ClaimIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("ClaimIssue() error = %v", err)
	}
	if claimed.Packet.Route != workflow.RouteDefault {
		t.Fatalf("frozen packet route = %q, want no inferred route", claimed.Packet.Route)
	}

	baseline, err := fixture.service.RunBaseline(context.Background(), factory.BaselineRequest{RunID: claimed.Run.ID})
	if err != nil {
		t.Fatalf("RunBaseline() error = %v", err)
	}
	if baseline.Run.Stage != store.StageTest {
		t.Fatalf("post-baseline stage = %q, want the mandatory test stage", baseline.Run.Stage)
	}
	if _, err := fixture.service.Transition(context.Background(), factory.TransitionRequest{RunID: claimed.Run.ID, Stage: store.StageImplementation, Status: store.StatusActive}); err == nil || !strings.Contains(err.Error(), "completed test handoff") {
		t.Fatalf("Transition() error = %v, want test-stage bypass rejection", err)
	}
}

// TestSelectedRouteBlocksTheImplementationOwnedFastPath verifies a route can
// add the independent test stage under advisory policy and that the stage
// cannot then be bypassed.
func TestSelectedRouteBlocksTheImplementationOwnedFastPath(t *testing.T) {
	fixture := newBaselineFixture(t, acceptanceMarker, []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}})
	fixture.policy.TestPolicy.Mode = config.TestModeAdvisory
	claimed, err := fixture.service.ClaimIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("ClaimIssue() error = %v", err)
	}

	if _, err := fixture.service.Transition(context.Background(), factory.TransitionRequest{RunID: claimed.Run.ID, Stage: store.StageImplementation, Status: store.StatusActive}); err == nil || !strings.Contains(err.Error(), "completed test handoff") {
		t.Fatalf("Transition() error = %v, want test-stage bypass rejection", err)
	}
}

// TestSelectedRouteBlocksTheImplementationAgentAtClaim verifies the agent seam
// refuses an implementation launch that would skip a route's test stage, even
// when the repository test policy alone would allow it.
func TestSelectedRouteBlocksTheImplementationAgentAtClaim(t *testing.T) {
	service, runStore, runtime, _, _ := newAgentService(t)
	run := routedRun(t, runStore, workflow.RouteAcceptance)
	run.Stage = store.StageClaim
	run.TestStageSkipped = false
	run.TestExemption = nil
	if err := runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("persist routed claim fixture: %v", err)
	}

	_, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err == nil || !strings.Contains(err.Error(), "cannot bypass the configured test stage") {
		t.Fatalf("StartAgent() error = %v, want test-stage bypass rejection", err)
	}
	if len(runtime.starts) != 0 {
		t.Fatalf("rejected launch started workers: %#v", runtime.starts)
	}
}

// TestSelectedRouteOverridesAPreAuthorizedTestSkip verifies a deliberate route
// selection is not silently discarded by a human test-exemption marker.
func TestSelectedRouteOverridesAPreAuthorizedTestSkip(t *testing.T) {
	fixture := newBaselineFixture(t, acceptanceMarker+"\n<!-- factory-test-exemption: human | documentation-only change -->", []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}})
	fixture.policy.TestPolicy.AllowHumanExemption = true
	claimed, err := fixture.service.ClaimIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("ClaimIssue() error = %v", err)
	}

	baseline, err := fixture.service.RunBaseline(context.Background(), factory.BaselineRequest{RunID: claimed.Run.ID})
	if err != nil {
		t.Fatalf("RunBaseline() error = %v", err)
	}
	if baseline.Run.Stage != store.StageTest || baseline.Run.TestStageSkipped || baseline.Run.TestExemption != nil {
		t.Fatalf("post-baseline run = %#v, want the selected acceptance stage without a skip", baseline.Run)
	}
}

// TestDesignAcceptanceRouteRequiresArchitectureFirst verifies neither test nor
// implementation can start before the accepted design on the design route.
func TestDesignAcceptanceRouteRequiresArchitectureFirst(t *testing.T) {
	for _, stage := range []store.Stage{store.StageTest, store.StageImplementation} {
		t.Run(string(stage), func(t *testing.T) {
			fixture := newBaselineFixture(t, designAcceptanceMarker, []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}})
			claimed, err := fixture.service.ClaimIssue(context.Background(), 42)
			if err != nil {
				t.Fatalf("ClaimIssue() error = %v", err)
			}
			_, err = fixture.service.Transition(context.Background(), factory.TransitionRequest{RunID: claimed.Run.ID, Stage: stage, Status: store.StatusActive})
			if err == nil || !strings.Contains(err.Error(), "requires the architecture stage") {
				t.Fatalf("Transition(%q) error = %v, want architecture-first rejection", stage, err)
			}
		})
	}
}

// TestDesignAcceptanceHandsTheAcceptedDesignToTheTestRole verifies the
// architecture-to-test transition and the design handoff the test invocation
// receives alongside the frozen specification packet.
func TestDesignAcceptanceHandsTheAcceptedDesignToTheTestRole(t *testing.T) {
	service, runStore, runtime, _, _ := newAgentService(t)
	run := routedRun(t, runStore, workflow.RouteDesignAcceptance)
	run.Stage = store.StageArchitecture
	run.TestStageSkipped = false
	run.TestExemption = nil
	runStore.worktree.state.ChangedPaths = []string{"docs/architecture/issue-6.md"}
	if err := runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("persist design-acceptance fixture: %v", err)
	}

	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent(architecture) error = %v", err)
	}
	if launch.Invocation.Role != workflow.RoleArchitecture || launch.Route != workflow.RouteDesignAcceptance {
		t.Fatalf("routed launch = %#v, want the architecture role on the design-acceptance route", launch)
	}
	design := report.Handoff{
		ChangeSummary:          "recorded the publishing boundary",
		AcceptanceMapping:      []report.AcceptanceMapping{{Criterion: "boundary", Evidence: "docs/architecture/issue-6.md"}},
		ProductionFilesChanged: []string{"docs/architecture/issue-6.md"},
		FocusedCommands:        []string{"test -f docs/architecture/issue-6.md"},
	}
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, report.Report{
		SchemaVersion:   report.SchemaVersion,
		InvocationID:    launch.Invocation.ID,
		RunID:           launch.Invocation.RunID,
		Harness:         launch.Invocation.Harness,
		Role:            workflow.RoleArchitecture,
		Stage:           string(workflow.StageArchitecture),
		Outcome:         report.OutcomeCompleted,
		Summary:         "architecture design recorded",
		Handoff:         &design,
		NativeSessionID: "session-design",
		ReportedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("write architecture report: %v", err)
	}
	if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{InvocationID: launch.Invocation.ID}); err != nil {
		t.Fatalf("AcceptAgentReport(architecture) error = %v", err)
	}
	if runStore.current.Stage != store.StageTest || runStore.current.Status != store.StatusActive {
		t.Fatalf("run after design handoff = stage %q/status %q, want test/active", runStore.current.Stage, runStore.current.Status)
	}

	testLaunch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent(test) error = %v", err)
	}
	if testLaunch.Invocation.Role != workflow.RoleTest {
		t.Fatalf("second launch role = %q, want the test role", testLaunch.Invocation.Role)
	}
	packet := readInvocationPacket(t, testLaunch.Invocation.InvocationDirectory)
	if packet.Route != workflow.RouteDesignAcceptance || packet.DesignHandoff == nil || packet.DesignHandoff.ChangeSummary != design.ChangeSummary {
		t.Fatalf("test invocation packet = %#v, want the frozen route and accepted design", packet)
	}
	if packet.SpecificationPacket == "" {
		t.Fatal("test invocation packet lost the frozen specification packet")
	}
	for _, marker := range []string{"Accepted architecture design (coordinator-owned):", "docs/architecture/issue-6.md", "highest practical observable interface"} {
		if !strings.Contains(testLaunch.Prompt, marker) {
			t.Fatalf("test prompt missing %q:\n%s", marker, testLaunch.Prompt)
		}
	}
	if len(runtime.starts) == 0 {
		t.Fatal("routed test invocation started no worker")
	}
}

// TestRouteStaysFrozenAcrossASpecificationRefresh verifies a later issue edit
// cannot change the route selected before claim.
func TestRouteStaysFrozenAcrossASpecificationRefresh(t *testing.T) {
	service, runStore, runtime, _, _ := newAgentService(t)
	run := routedRun(t, runStore, workflow.RouteAcceptance)
	run.Stage = store.StageTest
	run.TestStageSkipped = false
	run.TestExemption = nil
	if err := runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("persist acceptance-route fixture: %v", err)
	}
	runStore.github.issueValue.Body = designAcceptanceMarker + "\nrewritten product intent"

	result, err := service.HandleCommand(context.Background(), factory.CommandRequest{
		IssueNumber: run.IssueNumber,
		Comment:     github.Comment{ID: "refresh-route", Author: "alice", Body: "/factory refresh"},
	})
	if err != nil {
		t.Fatalf("HandleCommand() error = %v", err)
	}
	if result.Outcome != factory.CommandAccepted {
		t.Fatalf("refresh outcome = %q, want accepted", result.Outcome)
	}
	if result.Run.Stage != store.StageTest {
		t.Fatalf("refreshed stage = %q, want the frozen acceptance route stage", result.Run.Stage)
	}
	var refreshed factory.SpecificationPacket
	if err := json.Unmarshal([]byte(result.Run.SpecificationPacket), &refreshed); err != nil {
		t.Fatalf("decode refreshed specification packet: %v", err)
	}
	if refreshed.Route != workflow.RouteAcceptance {
		t.Fatalf("refreshed packet route = %q, want the frozen acceptance route", refreshed.Route)
	}
	if !strings.Contains(refreshed.Issue.Body, designAcceptanceMarker) {
		t.Fatal("refresh did not re-read the live issue body")
	}
	packet := readInvocationPacket(t, runtime.starts[len(runtime.starts)-1].InvocationPath)
	if packet.Route != workflow.RouteAcceptance {
		t.Fatalf("resumed invocation packet route = %q, want the frozen acceptance route", packet.Route)
	}
}

// TestRoutedRunsRestartCleanlyFromTheirSelectedStage verifies a coordinator
// restart resumes each route's entry stage without a recovery pause.
func TestRoutedRunsRestartCleanlyFromTheirSelectedStage(t *testing.T) {
	tests := []struct {
		name  string
		route workflow.Route
		stage store.Stage
		role  string
	}{
		{name: "acceptance route test stage", route: workflow.RouteAcceptance, stage: store.StageTest, role: workflow.RoleTest},
		{name: "design-acceptance route architecture stage", route: workflow.RouteDesignAcceptance, stage: store.StageArchitecture, role: workflow.RoleArchitecture},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, runStore, runtime, terminalRuntime, harnessRuntime := newAgentService(t)
			run := routedRun(t, runStore, test.route)
			run.Stage = test.stage
			run.TestStageSkipped = false
			run.TestExemption = nil
			if err := runStore.SaveRun(context.Background(), run); err != nil {
				t.Fatalf("persist routed restart fixture: %v", err)
			}
			worktree := &inspectingWorktree{
				fakeWorktree: fakeWorktree{workspace: gitadapter.Workspace{Worktree: run.Worktree}},
				state:        gitadapter.WorktreeState{RepositoryPath: run.RepositoryPath, Branch: run.Branch, HeadSHA: run.CheckpointSHA},
			}
			fresh := newFreshAgentService(t, runStore, worktree, runtime, terminalRuntime, harnessRuntime)

			launch, err := fresh.StartAgent(context.Background(), factory.AgentRequest{})
			if err != nil {
				t.Fatalf("fresh StartAgent() error = %v, want a clean routed restart", err)
			}
			if launch.Invocation.Role != test.role || launch.Route != test.route {
				t.Fatalf("restarted launch = role %q route %q, want %q/%q", launch.Invocation.Role, launch.Route, test.role, test.route)
			}
		})
	}
}

// routedRun rewrites the persisted run's frozen packet with one selected route
// so route behavior can be exercised at the agent seam.
func routedRun(t *testing.T, runStore *agentRunStore, route workflow.Route) store.Run {
	t.Helper()
	run := *runStore.current
	var packet factory.SpecificationPacket
	if err := json.Unmarshal([]byte(run.SpecificationPacket), &packet); err != nil {
		t.Fatalf("decode specification packet: %v", err)
	}
	packet.Route = route
	packet.Issue.Body = string(route) + " route fixture"
	encoded, err := json.Marshal(packet)
	if err != nil {
		t.Fatalf("encode routed specification packet: %v", err)
	}
	run.SpecificationPacket = string(encoded)
	return run
}

// readInvocationPacket decodes the read-only packet written for one invocation.
func readInvocationPacket(t *testing.T, directory string) factory.InvocationPacket {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(directory, "specification.json"))
	if err != nil {
		t.Fatalf("read invocation packet: %v", err)
	}
	var packet factory.InvocationPacket
	if err := json.Unmarshal(data, &packet); err != nil {
		t.Fatalf("decode invocation packet: %v", err)
	}
	return packet
}

// TestAcceptanceRouteRunsTheVerifiedRedHandoffUnderAdvisoryPolicy verifies the
// acceptance route gives an advisory repository the complete independent
// test stage: coordinator-rerun red evidence, a scoped test checkpoint,
// protected test paths, and an automatic implementation launch.
func TestAcceptanceRouteRunsTheVerifiedRedHandoffUnderAdvisoryPolicy(t *testing.T) {
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
	policy.TestPolicy.Mode = config.TestModeAdvisory
	storeRuntime := &agentRunStore{runs: map[string]store.Run{}, invocations: map[string]store.Invocation{}, gateResults: map[string][]store.GateResult{}}
	workerRuntime := &agentWorker{results: []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}, {ExitCode: 1, Stdout: "expected behavior assertion"}}}
	githubRuntime := &fakeGitHub{issueValue: github.Issue{Number: 12, Title: "Routed", Body: acceptanceMarker, State: "open", Labels: []string{github.LabelAgentReady}}}
	workspace := &testStageWorkspace{
		workspace: gitadapter.Workspace{BaseSHA: factoryGateCheckpoint, Branch: "factory/run-routed", Worktree: worktreePath},
		state:     gitadapter.WorktreeState{RepositoryPath: repositoryPath, Branch: "factory/run-routed", HeadSHA: factoryGateCheckpoint},
	}
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{
		Path:                 repositoryPath,
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		Cmux:                 config.CmuxConfig{ControlWorkspace: "factory-control"},
		OperationalDataPath:  filepath.Join(root, "state", "factory.db"),
		RepositoryConfigPath: filepath.Join(repositoryPath, config.RepositoryConfigFileName),
	}}}
	ids := []string{"run-routed", "test-invocation", "implementation-invocation"}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:         &fakeConfig{value: host},
		OpenStore:      func(context.Context, string) (factory.OperationalStore, error) { return storeRuntime, nil },
		LoadRepository: func(string) (config.RepositoryConfig, error) { return policy, nil },
		GitHub:         githubRuntime,
		CommitStatuses: &gateStatuses{},
		Worktree:       workspace,
		GitWorkspace:   workspace,
		Worker:         workerRuntime,
		Terminal:       &agentTerminal{},
		Harness:        &agentHarness{},
		Now:            func() time.Time { return time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC) },
		NewRunID: func() (string, error) {
			if len(ids) == 0 {
				return "", errors.New("routed id fixture exhausted")
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
		t.Fatalf("routed advisory baseline stage = %q, want test", storeRuntime.current.Stage)
	}
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent(test) error = %v", err)
	}
	if launch.Invocation.Role != workflow.RoleTest || launch.TestPolicyMode != config.TestModeAdvisory || launch.Route != workflow.RouteAcceptance {
		t.Fatalf("routed advisory launch = %#v, want the test role on the acceptance route", launch)
	}
	workspace.state.ChangedPaths = []string{"internal/factory/agent_test.go"}
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, report.Report{
		SchemaVersion: report.SchemaVersion,
		InvocationID:  launch.Invocation.ID,
		RunID:         launch.Invocation.RunID,
		Harness:       launch.Invocation.Harness,
		Role:          workflow.RoleTest,
		Stage:         string(store.StageTest),
		Outcome:       report.OutcomeCompleted,
		Summary:       "focused behavior test is red on base",
		TestHandoff: &report.TestHandoff{
			AcceptanceCoverage:      []report.AcceptanceMapping{{Criterion: "behavior", Evidence: "focused test"}},
			ChangedFiles:            []string{"internal/factory/agent_test.go"},
			FocusedTestCommand:      "go test ./internal/factory -run TestBehavior",
			ExpectedFailureReason:   "expected behavior assertion",
			ObservedFailureEvidence: []report.Evidence{{Kind: "exit_code", Detail: "focused command exited with code 1"}},
		},
		NativeSessionID: "routed-test-session",
		ReportedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("WriteAtomicForInvocation() error = %v", err)
	}
	if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: claimed.Run.ID, InvocationID: launch.Invocation.ID}); err != nil {
		t.Fatalf("AcceptAgentReport(test) error = %v", err)
	}
	if storeRuntime.current.Stage != store.StageImplementation || storeRuntime.current.TestCheckpointSHA == "" || len(storeRuntime.current.ProtectedTestPaths) != 1 {
		t.Fatalf("routed advisory handoff run = %#v, want implementation with a protected test checkpoint", storeRuntime.current)
	}
	if len(workerRuntime.commands) != 3 || workerRuntime.commands[2].Command != "go test ./internal/factory -run TestBehavior" {
		t.Fatalf("worker commands = %#v, want the independently rerun focused test", workerRuntime.commands)
	}
	implementation, ok := storeRuntime.invocations["inv-implementation-invocation"]
	if !ok || implementation.Role != workflow.RoleImplementation || implementation.Status != store.InvocationStatusActive {
		t.Fatalf("implementation invocation = %#v, want an active protected handoff consumer", implementation)
	}
	packet := readInvocationPacket(t, implementation.InvocationDirectory)
	if packet.Route != workflow.RouteAcceptance || packet.TestHandoff == nil || len(packet.ProtectedTestPaths) != 1 {
		t.Fatalf("routed implementation packet = %#v, want the frozen route and protected test handoff", packet)
	}
}
