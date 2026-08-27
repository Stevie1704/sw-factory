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
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// InteractiveCommand records the harness argv one adapter asked the worker for.
func (w *agentWorker) InteractiveCommand(_ context.Context, request worker.InteractiveRequest) (worker.InteractiveCommand, error) {
	w.interactive = append(w.interactive, request)
	return worker.InteractiveCommand{Executable: "factory-worker-attach", Args: []string{"--run-id", request.RunID}}, nil
}

// SeedCodexCredentials records one narrowly scoped Codex credential seed.
func (w *agentWorker) SeedCodexCredentials(_ context.Context, request worker.CredentialSeedRequest) error {
	w.codexSeeds = append(w.codexSeeds, request)
	return nil
}

// SeedClaudeCredentials records one narrowly scoped Claude credential seed.
func (w *agentWorker) SeedClaudeCredentials(_ context.Context, request worker.CredentialSeedRequest) error {
	w.claudeSeeds = append(w.claudeSeeds, request)
	return nil
}

// TestStartAgentLaunchesTheHarnessDeclaredForTheRole verifies a role declared
// as Claude reaches the Claude adapter, records the Claude harness identity on
// the invocation, and seeds only the Claude credential source.
func TestStartAgentLaunchesTheHarnessDeclaredForTheRole(t *testing.T) {
	_, runStore, runtime, terminalRuntime, _ := newAgentService(t)
	policy := validRepositoryConfig()
	policy.RoleHarnessDefaults["implementation"] = config.HarnessClaude
	policy.ModelOptions["implementation"] = []string{"claude-opus-5"}
	claudeAuth := filepath.Join(t.TempDir(), ".credentials.json")
	service := newDispatchingAgentService(t, runStore, runtime, terminalRuntime, policy, config.AuthenticationConfig{
		CodexAuthPath:  filepath.Join(t.TempDir(), "auth.json"),
		ClaudeAuthPath: claudeAuth,
	})

	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	if launch.Invocation.Harness != string(config.HarnessClaude) {
		t.Fatalf("invocation harness = %q, want the role's declared Claude harness", launch.Invocation.Harness)
	}
	if launch.Invocation.Model != "claude-opus-5" {
		t.Fatalf("invocation model = %q, want the role's declared Claude model", launch.Invocation.Model)
	}
	if len(runtime.interactive) != 1 || runtime.interactive[0].Command[0] != "claude" {
		t.Fatalf("interactive worker commands = %#v, want one Claude launch", runtime.interactive)
	}
	if runtime.interactive[0].Environment["FACTORY_HARNESS"] != string(config.HarnessClaude) {
		t.Fatalf("interactive environment = %#v, want the Claude harness identity", runtime.interactive[0].Environment)
	}
	if len(runtime.claudeSeeds) != 1 || runtime.claudeSeeds[0].AuthPath != claudeAuth {
		t.Fatalf("Claude credential seeds = %#v, want only the registered Claude source", runtime.claudeSeeds)
	}
	if len(runtime.codexSeeds) != 0 {
		t.Fatalf("Codex credential seeds = %#v, want no seeding for a Claude invocation", runtime.codexSeeds)
	}
	if launch.Invocation.NativeSessionID == "" {
		t.Fatal("invocation has no native session id, want the adapter-assigned Claude session")
	}
}

// TestRefreshAuthReseedsTheRegisteredSourceWithoutChangingIt verifies an
// explicit auth refresh updates only the worker-facing credential projection.
func TestRefreshAuthReseedsTheRegisteredSourceWithoutChangingIt(t *testing.T) {
	_, runStore, runtime, terminalRuntime, _ := newAgentService(t)
	root := t.TempDir()
	authPath := filepath.Join(root, "auth.json")
	source := []byte("token=super-secret\n")
	if err := os.WriteFile(authPath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	service := newDispatchingAgentService(t, runStore, runtime, terminalRuntime, validRepositoryConfig(), config.AuthenticationConfig{CodexAuthPath: authPath})
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}

	refreshed, err := service.RefreshAuth(context.Background(), factory.AuthRefreshRequest{RunID: launch.Invocation.RunID})
	if err != nil {
		t.Fatalf("RefreshAuth() error = %v", err)
	}
	if refreshed.Harness != config.HarnessCodex || refreshed.Invocation.CredentialStoreID == "" {
		t.Fatalf("RefreshAuth() result = %#v, want Codex and a factory credential store", refreshed)
	}
	if len(runtime.codexSeeds) != 2 || runtime.codexSeeds[1].AuthPath != authPath {
		t.Fatalf("Codex credential seeds = %#v, want launch and refresh from the registered source", runtime.codexSeeds)
	}
	unchanged, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(unchanged) != string(source) {
		t.Fatalf("registered auth source changed to %q", unchanged)
	}
}

// TestStartAgentRefusesAnIssueHarnessOverrideOutsidePolicy verifies an
// authorized issue-level override is still validated against the frozen
// repository policy and refused as a typed policy rejection.
func TestStartAgentRefusesAnIssueHarnessOverrideOutsidePolicy(t *testing.T) {
	_, runStore, runtime, terminalRuntime, _ := newAgentService(t)
	policy := validRepositoryConfig()
	service := newDispatchingAgentService(t, runStore, runtime, terminalRuntime, policy, config.AuthenticationConfig{})

	_, err := service.StartAgent(context.Background(), factory.AgentRequest{Harness: config.HarnessClaude})
	var rejection *factory.PolicyRejection
	if !errors.As(err, &rejection) || rejection.Code != factory.PolicyRejectionHarnessOverride {
		t.Fatalf("StartAgent() error = %v, want a typed harness-override rejection", err)
	}
	if len(runtime.interactive) != 0 || len(runtime.starts) != 0 {
		t.Fatal("a refused override started a worker or a harness")
	}
}

// TestStartAgentAcceptsAnIssueHarnessOverridePermittedByPolicy verifies the
// recorded override from a `/factory config harness=claude` comment selects
// the Claude adapter once the repository permits harness overrides.
func TestStartAgentAcceptsAnIssueHarnessOverridePermittedByPolicy(t *testing.T) {
	_, runStore, runtime, terminalRuntime, _ := newAgentService(t)
	policy := validRepositoryConfig()
	policy.AllowedOverrides = append(policy.AllowedOverrides, config.OverrideHarness)
	policy.ModelOptions["implementation"] = []string{"claude-opus-5"}

	run := *runStore.current
	run.HarnessOverride = string(config.HarnessClaude)
	if err := runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	service := newDispatchingAgentService(t, runStore, runtime, terminalRuntime, policy, config.AuthenticationConfig{})
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	if launch.Invocation.Harness != string(config.HarnessClaude) {
		t.Fatalf("invocation harness = %q, want the authorized override", launch.Invocation.Harness)
	}
}

// TestStartAgentPassesTheDeclaredReasoningEffortToClaude verifies reasoning
// effort is selected per role from the repository-declared options and reaches
// the Claude adapter, which is what constrains the value: Claude Code warns
// about an unrecognized level and then silently uses its default.
func TestStartAgentPassesTheDeclaredReasoningEffortToClaude(t *testing.T) {
	_, runStore, runtime, terminalRuntime, _ := newAgentService(t)
	policy := validRepositoryConfig()
	policy.RoleHarnessDefaults["implementation"] = config.HarnessClaude
	policy.ModelOptions["implementation"] = []string{"claude-opus-5"}
	policy.ReasoningEffortOptions = map[string][]string{"implementation": {"high", "max"}}
	service := newDispatchingAgentService(t, runStore, runtime, terminalRuntime, policy, config.AuthenticationConfig{})

	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	if launch.Invocation.ReasoningEffort != "high" {
		t.Fatalf("invocation reasoning effort = %q, want the role's first declared option", launch.Invocation.ReasoningEffort)
	}
	command := runtime.interactive[0].Command
	if !containsAdjacentArguments(command, "--effort", "high") {
		t.Fatalf("Claude command = %#v, want the declared effort level", command)
	}
}

// TestStartAgentRefusesAnUndeclaredReasoningEffort verifies an effort outside
// the role's declared options is refused as a typed policy rejection before
// the launch creates a worker, a credential copy, or a visible surface.
func TestStartAgentRefusesAnUndeclaredReasoningEffort(t *testing.T) {
	_, runStore, runtime, terminalRuntime, _ := newAgentService(t)
	policy := validRepositoryConfig()
	policy.AllowedOverrides = nil
	policy.RoleHarnessDefaults["implementation"] = config.HarnessClaude
	policy.ModelOptions["implementation"] = []string{"claude-opus-5"}
	policy.ReasoningEffortOptions = map[string][]string{"implementation": {"high"}}
	service := newDispatchingAgentService(t, runStore, runtime, terminalRuntime, policy, config.AuthenticationConfig{
		ClaudeAuthPath: filepath.Join(t.TempDir(), ".credentials.json"),
	})

	_, err := service.StartAgent(context.Background(), factory.AgentRequest{ReasoningEffort: "bogus"})
	var rejection *factory.PolicyRejection
	if !errors.As(err, &rejection) || rejection.Code != factory.PolicyRejectionReasoningEffortOverride {
		t.Fatalf("StartAgent() error = %v, want a typed reasoning-effort rejection", err)
	}
	if len(runtime.starts) != 0 || len(runtime.claudeSeeds) != 0 || len(runtime.interactive) != 0 {
		t.Fatal("a refused setting started a worker, seeded a credential, or launched a harness")
	}
	if len(runStore.invocations) != 0 {
		t.Fatalf("persisted invocations = %#v, want none after a refused setting", runStore.invocations)
	}
}

// containsAdjacentArguments reports whether a flag is immediately followed by
// its expected value.
func containsAdjacentArguments(arguments []string, flag, value string) bool {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == flag && arguments[index+1] == value {
			return true
		}
	}
	return false
}

// newDispatchingAgentService recreates the coordinator around a persisted claim
// without an injected harness runtime, so the service resolves the adapter for
// the frozen repository policy itself.
func newDispatchingAgentService(t *testing.T, runStore *agentRunStore, runtime *agentWorker, terminalRuntime *agentTerminal, policy config.RepositoryConfig, authentication config.AuthenticationConfig) *factory.Service {
	t.Helper()
	if runStore.current == nil {
		t.Fatal("dispatching coordinator fixture requires a persisted run")
	}
	repackageSpecification(t, runStore, policy)
	run := *runStore.current
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{
		Path:                 run.RepositoryPath,
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		Polling:              config.PollingConfig{Interval: "30s", Backoff: "5m"},
		Cmux:                 config.CmuxConfig{ControlWorkspace: "factory-control"},
		Authentication:       authentication,
		OperationalDataPath:  filepath.Join(filepath.Dir(run.RepositoryPath), "state", "factory.db"),
		RepositoryConfigPath: filepath.Join(run.RepositoryPath, "factory.yaml"),
	}}}
	githubRuntime := &fakeGitHub{
		issueValue:    github.Issue{Number: run.IssueNumber, State: "open", Labels: []string{github.LabelAgentRunning}},
		statusComment: github.Comment{ID: run.StatusCommentID, Body: factory.StatusCommentBody(run)},
	}
	worktree := &inspectingWorktree{
		fakeWorktree: fakeWorktree{workspace: gitadapter.Workspace{Worktree: run.Worktree}},
		state:        gitadapter.WorktreeState{RepositoryPath: run.RepositoryPath, Branch: run.Branch, HeadSHA: run.CheckpointSHA},
	}
	return factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:         &fakeConfig{value: host},
		OpenStore:      func(context.Context, string) (factory.OperationalStore, error) { return runStore, nil },
		LoadRepository: func(string) (config.RepositoryConfig, error) { return policy, nil },
		Worker:         runtime,
		Terminal:       terminalRuntime,
		GitHub:         githubRuntime,
		Worktree:       worktree,
		Now:            func() time.Time { return time.Date(2026, 8, 21, 8, 0, 0, 0, time.UTC) },
		NewRunID:       func() (string, error) { return "generated-dispatch", nil },
	})
}

// repackageSpecification refreezes the claimed run's specification packet with
// the repository policy under test, because the coordinator reads harness and
// model policy from the frozen packet rather than the live configuration.
func repackageSpecification(t *testing.T, runStore *agentRunStore, policy config.RepositoryConfig) {
	t.Helper()
	run := *runStore.current
	var packet factory.SpecificationPacket
	if err := json.Unmarshal([]byte(run.SpecificationPacket), &packet); err != nil {
		t.Fatalf("decode frozen specification packet: %v", err)
	}
	packet.RepositoryConfig = policy
	refrozen, err := json.Marshal(packet)
	if err != nil {
		t.Fatalf("refreeze specification packet: %v", err)
	}
	run.SpecificationPacket = string(refrozen)
	if err := runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
}

var _ worker.InteractiveRuntime = (*agentWorker)(nil)
var _ worker.CredentialSeeder = (*agentWorker)(nil)
var _ worker.ClaudeCredentialSeeder = (*agentWorker)(nil)
