package factory_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestStartAgentLaunchesEveryHarnessThroughTheHeadlessSeam verifies both
// production adapters cross only the detached worker protocol and persist a
// harness-native identity without any local-UI dependency.
func TestStartAgentLaunchesEveryHarnessThroughTheHeadlessSeam(t *testing.T) {
	tests := []struct {
		name, model string
		harness     config.Harness
		command     []string
	}{
		{name: "Codex", harness: config.HarnessCodex, model: "gpt-5", command: []string{"codex", "exec", "--json"}},
		{name: "Claude", harness: config.HarnessClaude, model: "claude-opus-5", command: []string{"claude", "-p", "stream-json", "--verbose", "--session-id"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			for _, removedBinary := range []string{"cmux", "tmux"} {
				if path, err := exec.LookPath(removedBinary); err == nil {
					t.Fatalf("terminal-free test PATH unexpectedly resolves %s at %q", removedBinary, path)
				}
			}
			_, runStore, runtime, _ := newAgentService(t)
			headlessWorker := &headlessAgentWorker{agentWorker: runtime}
			policy := validRepositoryConfig()
			policy.RoleHarnessDefaults["implementation"] = test.harness
			policy.ModelOptions["implementation"] = []string{test.model}
			codexAuth := filepath.Join(t.TempDir(), "auth.json")
			claudeAuth := filepath.Join(t.TempDir(), ".credentials.json")
			service := newDispatchingAgentService(t, runStore, headlessWorker, policy, config.AuthenticationConfig{
				CodexAuthPath: codexAuth, ClaudeAuthPath: claudeAuth,
			})

			launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
			if err != nil {
				t.Fatalf("StartAgent() error = %v", err)
			}
			if launch.Invocation.Harness != string(test.harness) {
				t.Fatalf("invocation harness = %q, want %q", launch.Invocation.Harness, test.harness)
			}
			if len(headlessWorker.headlessStarts) != 1 {
				t.Fatalf("headless launches = %#v, want exactly one detached process", headlessWorker.headlessStarts)
			}
			command := strings.Join(headlessWorker.headlessStarts[0].Command, " ")
			for _, wanted := range test.command {
				if !strings.Contains(command, wanted) {
					t.Fatalf("headless command = %q, want %q", command, wanted)
				}
			}
			if launch.Invocation.NativeSessionID == "" {
				t.Fatal("invocation has no harness-native session id")
			}
			for key := range headlessWorker.headlessStarts[0].Environment {
				if key == "TERM" {
					t.Fatalf("headless environment unexpectedly exposes %q", key)
				}
			}
			for _, removed := range []string{"attach", "cmux", "tmux"} {
				if strings.Contains(command, removed) {
					t.Fatalf("headless command = %q, unexpectedly contains removed local-UI token %q", command, removed)
				}
			}
			if test.harness == config.HarnessCodex && (len(runtime.codexSeeds) != 1 || runtime.codexSeeds[0].AuthPath != codexAuth) {
				t.Fatalf("Codex credential seeds = %#v, want registered source", runtime.codexSeeds)
			}
			if test.harness == config.HarnessClaude && (len(runtime.claudeSeeds) != 1 || runtime.claudeSeeds[0].AuthPath != claudeAuth) {
				t.Fatalf("Claude credential seeds = %#v, want registered source", runtime.claudeSeeds)
			}

			refreshed, err := service.RefreshAuth(context.Background(), factory.AuthRefreshRequest{RunID: launch.Invocation.RunID})
			if err != nil {
				t.Fatalf("RefreshAuth() error = %v", err)
			}
			if refreshed.Harness != test.harness || refreshed.Invocation.CredentialStoreID == "" {
				t.Fatalf("RefreshAuth() result = %#v, want %s managed credentials", refreshed, test.harness)
			}
			if test.harness == config.HarnessCodex && (len(runtime.codexSeeds) != 2 || runtime.codexSeeds[1].AuthPath != codexAuth) {
				t.Fatalf("Codex credential refreshes = %#v, want the registered source reseeded", runtime.codexSeeds)
			}
			if test.harness == config.HarnessClaude && (len(runtime.claudeSeeds) != 2 || runtime.claudeSeeds[1].AuthPath != claudeAuth) {
				t.Fatalf("Claude credential refreshes = %#v, want the registered source reseeded", runtime.claudeSeeds)
			}
			resumed, err := service.Resume(context.Background(), factory.ResumeRequest{RunID: launch.Invocation.RunID})
			if err != nil {
				t.Fatalf("Resume() error = %v", err)
			}
			if resumed.Invocation.NativeSessionID != launch.Invocation.NativeSessionID || resumed.Invocation.ManualResumeCount != 1 {
				t.Fatalf("resumed invocation = %#v, want exact native identity and manual generation 1", resumed.Invocation)
			}
			if len(headlessWorker.headlessStarts) != 2 || headlessWorker.headlessStarts[1].Mode != worker.HeadlessLaunchResume {
				t.Fatalf("headless launches after recovery = %#v, want one exact-session resume", headlessWorker.headlessStarts)
			}
		})
	}
}

// TestRefreshAuthCanExplicitlyResumeTheNativeSession verifies credential
// refresh and manual native recovery are one opt-in operation, with exactly
// one credential projection and one detached resume after the initial launch.
func TestRefreshAuthCanExplicitlyResumeTheNativeSession(t *testing.T) {
	t.Parallel()

	_, runStore, runtime, _ := newAgentService(t)
	headlessWorker := &headlessAgentWorker{agentWorker: runtime}
	policy := validRepositoryConfig()
	authPath := filepath.Join(t.TempDir(), "auth.json")
	service := newDispatchingAgentService(t, runStore, headlessWorker, policy, config.AuthenticationConfig{CodexAuthPath: authPath})

	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	paused := *runStore.current
	paused.Status = store.StatusWaitingForHuman
	paused.LifecycleReason = "harness authentication expired (codex); run is waiting for `factory auth refresh`"
	if err := runStore.SaveRun(context.Background(), paused); err != nil {
		t.Fatalf("SaveRun() pause setup error = %v", err)
	}
	refreshed, err := service.RefreshAuth(context.Background(), factory.AuthRefreshRequest{RunID: launch.Invocation.RunID, Resume: true})
	if err != nil {
		t.Fatalf("RefreshAuth(Resume: true) error = %v", err)
	}
	if !refreshed.Resumed || refreshed.Invocation.NativeSessionID != launch.Invocation.NativeSessionID || refreshed.Invocation.ManualResumeCount != 1 {
		t.Fatalf("refreshed result = %#v, want explicit native resume", refreshed)
	}
	if len(runtime.codexSeeds) != 2 {
		t.Fatalf("credential seeds = %#v, want initial projection plus one refresh", runtime.codexSeeds)
	}
	if len(headlessWorker.headlessStarts) != 2 || headlessWorker.headlessStarts[1].Mode != worker.HeadlessLaunchResume {
		t.Fatalf("headless launches = %#v, want one exact-session resume", headlessWorker.headlessStarts)
	}
}

// TestRefreshAuthExplainsHowToRegisterAMissingCredentialSource verifies an
// operator receives the registration remedy when no host source is configured.
func TestRefreshAuthExplainsHowToRegisterAMissingCredentialSource(t *testing.T) {
	t.Parallel()

	_, runStore, runtime, _ := newAgentService(t)
	headlessWorker := &headlessAgentWorker{agentWorker: runtime}
	service := newDispatchingAgentService(t, runStore, headlessWorker, validRepositoryConfig(), config.AuthenticationConfig{})
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() setup error = %v", err)
	}

	_, err = service.RefreshAuth(context.Background(), factory.AuthRefreshRequest{RunID: launch.Invocation.RunID})
	if err == nil {
		t.Fatal("RefreshAuth() error = nil, want missing-source guidance")
	}
	for _, want := range []string{
		"no factory-managed codex credential source is registered",
		"factory register --update --codex-auth <path>",
		"factory auth refresh",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("RefreshAuth() error = %q, want it to contain %q", err, want)
		}
	}
}

// headlessAgentWorker adds the detached process extension to the coordinator
// test worker and replays the launched session identity the way Claude Code
// confirms it in stream-json output.
type headlessAgentWorker struct {
	*agentWorker
	headlessStarts []worker.HeadlessRequest
}

// StartHeadless records one detached launch.
func (w *headlessAgentWorker) StartHeadless(_ context.Context, request worker.HeadlessRequest) (worker.HeadlessExecution, error) {
	w.headlessStarts = append(w.headlessStarts, request)
	return worker.HeadlessExecution{RunID: request.RunID, WorkerID: request.WorkerID, InvocationID: request.InvocationID}, nil
}

// InspectHeadless returns the init event carrying the assigned identity.
func (w *headlessAgentWorker) InspectHeadless(context.Context, worker.HeadlessRequest) (worker.HeadlessInspection, error) {
	if len(w.headlessStarts) == 0 {
		return worker.HeadlessInspection{Status: worker.HeadlessStatusMissing}, nil
	}
	command := w.headlessStarts[len(w.headlessStarts)-1].Command
	identity := "11111111-1111-4111-8111-111111111111"
	for index, argument := range command {
		if (argument == "--session-id" || argument == "--resume" || argument == "resume") && index+1 < len(command) {
			identity = command[index+1]
		}
	}
	output := `{"type":"thread.started","thread_id":"` + identity + `"}` + "\n"
	if len(command) > 0 && command[0] == "claude" {
		output = `{"type":"system","subtype":"init","session_id":"` + identity + `"}` + "\n"
	}
	return worker.HeadlessInspection{
		Status: worker.HeadlessStatusRunning,
		Stdout: output,
	}, nil
}

// CancelHeadless implements the detached process extension.
func (*headlessAgentWorker) CancelHeadless(context.Context, worker.HeadlessRequest) error { return nil }

// FinishHeadless implements the detached process extension.
func (*headlessAgentWorker) FinishHeadless(context.Context, worker.HeadlessRequest) error { return nil }

var _ worker.HeadlessProcessRuntime = (*headlessAgentWorker)(nil)
