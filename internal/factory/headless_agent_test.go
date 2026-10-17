package factory_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestStartAgentLaunchesEveryHarnessThroughTheHeadlessSeam verifies every
// production adapter cross only the detached worker protocol and persist a
// harness-native identity without any local-UI dependency.
func TestStartAgentLaunchesEveryHarnessThroughTheHeadlessSeam(t *testing.T) {
	tests := []struct {
		name, model string
		harness     config.Harness
		command     []string
	}{
		{name: "Codex", harness: config.HarnessCodex, model: "gpt-5", command: []string{"codex", "exec", "--json"}},
		{name: "Claude", harness: config.HarnessClaude, model: "claude-opus-5", command: []string{"claude", "-p", "stream-json", "--verbose", "--session-id"}},
		{name: "Copilot", harness: config.HarnessCopilot, model: "gpt-5.2", command: []string{"factory-copilot", "-p", "--output-format", "--session-id="}},
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
			copilotAuth := filepath.Join(t.TempDir(), "copilot-token")
			service := newDispatchingAgentService(t, runStore, headlessWorker, policy, config.AuthenticationConfig{
				CodexAuthPath: codexAuth, ClaudeAuthPath: claudeAuth, CopilotAuthPath: copilotAuth,
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
			if test.harness == config.HarnessCopilot && (len(runtime.copilotSeeds) != 1 || runtime.copilotSeeds[0].AuthPath != copilotAuth || len(runtime.codexSeeds)+len(runtime.claudeSeeds) != 0) {
				t.Fatalf("Copilot credential seeds = %#v, want only the registered Copilot source", runtime.copilotSeeds)
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
			if test.harness == config.HarnessCopilot && (len(runtime.copilotSeeds) != 2 || runtime.copilotSeeds[1].AuthPath != copilotAuth) {
				t.Fatalf("Copilot credential refreshes = %#v, want the registered source reseeded", runtime.copilotSeeds)
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
	paused.LifecycleReason = factory.LifecycleReasonHarnessAuthenticationExpired + " (codex); run is waiting for `factory auth refresh`"
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

// TestResumeAuthenticationFailureUsesTheRealPauseAndCancellationPath drives
// an actual native resume failure through pauseForAuthentication, proving the
// detached process is cancelled before the run waits for refreshed auth.
func TestResumeAuthenticationFailureUsesTheRealPauseAndCancellationPath(t *testing.T) {
	t.Parallel()

	_, runStore, runtime, _ := newAgentService(t)
	authPath := filepath.Join(t.TempDir(), "auth.json")
	headlessWorker := &headlessAgentWorker{
		agentWorker: runtime,
		resumeErr:   harness.NewAuthenticationExpiredError("codex"),
	}
	service := newDispatchingAgentService(t, runStore, headlessWorker, validRepositoryConfig(), config.AuthenticationConfig{CodexAuthPath: authPath})

	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() setup error = %v", err)
	}
	paused := *runStore.current
	paused.Status = store.StatusWaitingForHuman
	paused.LifecycleReason = factory.LifecycleReasonHarnessAuthenticationExpired + " (codex); waiting for auth"
	if err := runStore.SaveRun(context.Background(), paused); err != nil {
		t.Fatalf("SaveRun() pause setup error = %v", err)
	}

	_, err = service.Resume(context.Background(), factory.ResumeRequest{RunID: launch.Invocation.RunID})
	if err == nil || !strings.Contains(err.Error(), "authentication") {
		t.Fatalf("Resume() error = %v, want classified authentication failure", err)
	}
	if runStore.current.Status != store.StatusWaitingForHuman || !strings.HasPrefix(runStore.current.LifecycleReason, factory.LifecycleReasonHarnessAuthenticationExpired) {
		t.Fatalf("run after failed resume = %#v, want authentication pause", runStore.current)
	}
	if len(headlessWorker.headlessCancels) != 1 || headlessWorker.headlessCancels[0].InvocationID != launch.Invocation.ID {
		t.Fatalf("headless cancellations = %#v, want exact active invocation cancellation", headlessWorker.headlessCancels)
	}
	if runtime.stops < 2 {
		t.Fatalf("worker stops = %d, want resume cleanup and pause cleanup", runtime.stops)
	}
}

// TestRefreshAuthResumePreconditionStillProjectsCredentials verifies a
// non-resumable invocation does not turn --resume into an all-or-nothing
// credential operation.
func TestRefreshAuthResumePreconditionStillProjectsCredentials(t *testing.T) {
	t.Parallel()

	_, runStore, runtime, _ := newAgentService(t)
	headlessWorker := &headlessAgentWorker{agentWorker: runtime}
	authPath := filepath.Join(t.TempDir(), "auth.json")
	service := newDispatchingAgentService(t, runStore, headlessWorker, validRepositoryConfig(), config.AuthenticationConfig{CodexAuthPath: authPath})
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() setup error = %v", err)
	}

	invocation := runStore.invocations[launch.Invocation.ID]
	invocation.Status = store.InvocationStatusCompleted
	if err := runStore.SaveInvocation(context.Background(), invocation); err != nil {
		t.Fatalf("SaveInvocation() precondition setup error = %v", err)
	}
	before := len(runtime.codexSeeds)
	_, err = service.RefreshAuth(context.Background(), factory.AuthRefreshRequest{RunID: launch.Invocation.RunID, Resume: true})
	if err == nil || !strings.Contains(err.Error(), "authentication refreshed") {
		t.Fatalf("RefreshAuth(Resume: true) error = %v, want post-refresh resume precondition error", err)
	}
	if got := len(runtime.codexSeeds); got != before+1 {
		t.Fatalf("credential seeds = %d, want one refresh despite rejected native continuation", got)
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

// copilotRefreshService launches one Copilot implementation invocation and
// returns the service, the worker double, and the launched run identifier.
func copilotRefreshService(t *testing.T, authentication config.AuthenticationConfig) (*factory.Service, *agentWorker, *headlessAgentWorker, string) {
	t.Helper()
	_, runStore, runtime, _ := newAgentService(t)
	headlessWorker := &headlessAgentWorker{agentWorker: runtime}
	policy := validRepositoryConfig()
	policy.RoleHarnessDefaults["implementation"] = config.HarnessCopilot
	policy.ModelOptions["implementation"] = []string{"gpt-5.2"}
	service := newDispatchingAgentService(t, runStore, headlessWorker, policy, authentication)
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() setup error = %v", err)
	}
	return service, runtime, headlessWorker, launch.Invocation.RunID
}

// TestCancelledCopilotRunStopsItsProcessAndRefusesALateReport verifies that
// cancelling a run with an active Copilot invocation cancels exactly that
// invocation's process, and that a report the process writes afterwards
// cannot complete the invocation or reopen the run.
func TestCancelledCopilotRunStopsItsProcessAndRefusesALateReport(t *testing.T) {
	t.Parallel()

	_, runStore, runtime, _ := newAgentService(t)
	headlessWorker := &headlessAgentWorker{agentWorker: runtime}
	policy := validRepositoryConfig()
	policy.RoleHarnessDefaults["implementation"] = config.HarnessCopilot
	policy.ModelOptions["implementation"] = []string{"gpt-5.2"}
	service := newDispatchingAgentService(t, runStore, headlessWorker, policy, config.AuthenticationConfig{CopilotAuthPath: filepath.Join(t.TempDir(), "copilot-token")}, "internal/factory/agent.go")
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() setup error = %v", err)
	}

	runStore.github.issueValue.State = "closed"
	result, err := service.PollLifecycle(t.Context(), factory.LifecycleRequest{RunID: launch.Invocation.RunID})
	if err != nil || result.Run.Status != store.StatusCancelled {
		t.Fatalf("PollLifecycle() = %#v, %v; want a cancelled run", result, err)
	}
	if len(headlessWorker.headlessCancels) == 0 {
		t.Fatal("cancellation left the Copilot process running")
	}
	for _, cancelled := range headlessWorker.headlessCancels {
		if cancelled.InvocationID != launch.Invocation.ID || cancelled.RunID != launch.Invocation.RunID {
			t.Fatalf("cancelled process = %#v, want only invocation %s", cancelled, launch.Invocation.ID)
		}
	}

	late := report.Report{
		SchemaVersion: report.SchemaVersion, InvocationID: launch.Invocation.ID, RunID: launch.Invocation.RunID,
		Harness: launch.Invocation.Harness, Role: "implementation", Stage: "implementation", Outcome: report.OutcomeCompleted,
		Summary: "late result after cancellation",
		Handoff: &report.Handoff{
			ChangeSummary:          "late change",
			AcceptanceMapping:      []report.AcceptanceMapping{{Criterion: "behavior", Evidence: "late evidence"}},
			ProductionFilesChanged: []string{"internal/factory/agent.go"},
			FocusedCommands:        []string{"go test ./internal/factory"},
		},
		NativeSessionID: launch.Invocation.NativeSessionID, ReportedAt: time.Now().UTC(),
	}
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, late); err != nil {
		t.Fatalf("WriteAtomicForInvocation() error = %v", err)
	}
	if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: launch.Invocation.RunID, InvocationID: launch.Invocation.ID}); err == nil {
		t.Fatal("AcceptAgentReport() accepted a report of a cancelled run")
	}
	if runStore.current.Status != store.StatusCancelled {
		t.Fatalf("run after late report = %s, want cancelled", runStore.current.Status)
	}
	if invocation := runStore.invocations[launch.Invocation.ID]; invocation.Status == store.InvocationStatusCompleted {
		t.Fatalf("invocation after late report = %#v, want it not completed", invocation)
	}
}

// TestCopilotReportAcceptanceAdmitsOnlyTheExactInvocationReport verifies a
// report of a live Copilot process is accepted once and finishes the process,
// while a missing, malformed, or wrong-identity report cannot advance the run.
func TestCopilotReportAcceptanceAdmitsOnlyTheExactInvocationReport(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		write  func(t *testing.T, launch factory.AgentLaunchResult)
		accept bool
	}{
		{name: "missing report", write: func(*testing.T, factory.AgentLaunchResult) {}},
		{name: "malformed report", write: func(t *testing.T, launch factory.AgentLaunchResult) {
			if err := os.WriteFile(filepath.Join(launch.Invocation.ResultDirectory, report.ReportFileName), []byte(`{"schema_version":`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "report of another harness", write: func(t *testing.T, launch factory.AgentLaunchResult) {
			writeCopilotImplementationReport(t, launch, func(value *report.Report) { value.Harness = string(config.HarnessCodex) })
		}},
		{name: "report of another invocation", write: func(t *testing.T, launch factory.AgentLaunchResult) {
			writeCopilotImplementationReport(t, launch, func(value *report.Report) { value.InvocationID = "inv-earlier" })
		}},
		{name: "report of another run", write: func(t *testing.T, launch factory.AgentLaunchResult) {
			writeCopilotImplementationReport(t, launch, func(value *report.Report) { value.RunID = "run-other" })
		}},
		{name: "report of another role", write: func(t *testing.T, launch factory.AgentLaunchResult) {
			writeCopilotImplementationReport(t, launch, func(value *report.Report) { value.Role, value.Stage = "test", "test" })
		}},
		{name: "report while the process is alive", accept: true, write: func(t *testing.T, launch factory.AgentLaunchResult) {
			writeCopilotImplementationReport(t, launch, func(*report.Report) {})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, runStore, runtime, _ := newAgentService(t)
			headlessWorker := &headlessAgentWorker{agentWorker: runtime}
			policy := validRepositoryConfig()
			policy.RoleHarnessDefaults["implementation"] = config.HarnessCopilot
			policy.ModelOptions["implementation"] = []string{"gpt-5.2"}
			service := newDispatchingAgentService(t, runStore, headlessWorker, policy, config.AuthenticationConfig{CopilotAuthPath: filepath.Join(t.TempDir(), "copilot-token")}, "internal/factory/agent.go")
			launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
			if err != nil {
				t.Fatalf("StartAgent() setup error = %v", err)
			}
			before := *runStore.current
			test.write(t, launch)

			request := factory.AgentReportRequest{RunID: launch.Invocation.RunID, InvocationID: launch.Invocation.ID}
			_, err = service.AcceptAgentReport(context.Background(), request)

			if !test.accept {
				if err == nil {
					t.Fatal("AcceptAgentReport() accepted an inadmissible report")
				}
				if runStore.current.Stage != before.Stage || runStore.current.Status != store.StatusActive || runStore.invocations[launch.Invocation.ID].Status != store.InvocationStatusActive || headlessWorker.finishes != 0 {
					t.Fatalf("after refusal: run %s/%s invocation %s finishes %d; want the active invocation unchanged", runStore.current.Stage, runStore.current.Status, runStore.invocations[launch.Invocation.ID].Status, headlessWorker.finishes)
				}
				return
			}
			if err != nil {
				t.Fatalf("AcceptAgentReport() error = %v", err)
			}
			if runStore.invocations[launch.Invocation.ID].Status != store.InvocationStatusCompleted || headlessWorker.finishes != 1 {
				t.Fatalf("accepted invocation = %s with %d finishes, want completed and finished once", runStore.invocations[launch.Invocation.ID].Status, headlessWorker.finishes)
			}
			accepted := *runStore.current
			if _, err := service.AcceptAgentReport(context.Background(), request); err == nil && runStore.current.Revision != accepted.Revision {
				t.Fatalf("duplicate acceptance advanced the run from revision %d to %d", accepted.Revision, runStore.current.Revision)
			}
			if headlessWorker.finishes != 1 || len(headlessWorker.headlessStarts) != 1 {
				t.Fatalf("duplicate acceptance = finishes %d launches %d, want no second process effect", headlessWorker.finishes, len(headlessWorker.headlessStarts))
			}
		})
	}
}

// writeCopilotImplementationReport writes a completed implementation report
// for the launched invocation after mutate adjusts its identity.
func writeCopilotImplementationReport(t *testing.T, launch factory.AgentLaunchResult, mutate func(*report.Report)) {
	t.Helper()
	value := report.Report{
		SchemaVersion: report.SchemaVersion, InvocationID: launch.Invocation.ID, RunID: launch.Invocation.RunID,
		Harness: launch.Invocation.Harness, Role: "implementation", Stage: "implementation", Outcome: report.OutcomeCompleted,
		Summary: "implementation complete",
		Handoff: &report.Handoff{
			ChangeSummary:          "implemented the behavior",
			AcceptanceMapping:      []report.AcceptanceMapping{{Criterion: "behavior", Evidence: "focused test"}},
			ProductionFilesChanged: []string{"internal/factory/agent.go"},
			FocusedCommands:        []string{"go test ./internal/factory"},
		},
		NativeSessionID: launch.Invocation.NativeSessionID, ReportedAt: time.Now().UTC(),
	}
	mutate(&value)
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(launch.Invocation.ResultDirectory, report.ReportFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRefreshAuthServesCopilotAndKeepsTheRecordedHarness verifies an explicit
// Copilot refresh reseeds only the Copilot source, --resume continues the
// recorded Copilot session, and another harness name is refused before any
// credential effect.
func TestRefreshAuthServesCopilotAndKeepsTheRecordedHarness(t *testing.T) {
	t.Parallel()

	tokenPath := filepath.Join(t.TempDir(), "copilot-token")
	service, runtime, headlessWorker, runID := copilotRefreshService(t, config.AuthenticationConfig{CopilotAuthPath: tokenPath, ClaudeAuthPath: filepath.Join(t.TempDir(), ".credentials.json")})

	_, err := service.RefreshAuth(context.Background(), factory.AuthRefreshRequest{RunID: runID, Harness: config.HarnessClaude})
	if err == nil || !strings.Contains(err.Error(), `does not match invocation harness "copilot"`) {
		t.Fatalf("RefreshAuth(claude) error = %v, want recorded-harness mismatch", err)
	}
	if len(runtime.claudeSeeds) != 0 || len(runtime.copilotSeeds) != 1 {
		t.Fatalf("seeds after mismatch = claude %d, copilot %d; want no new projection", len(runtime.claudeSeeds), len(runtime.copilotSeeds))
	}

	refreshed, err := service.RefreshAuth(context.Background(), factory.AuthRefreshRequest{RunID: runID, Harness: config.HarnessCopilot, Resume: true})
	if err != nil {
		t.Fatalf("RefreshAuth(copilot, resume) error = %v", err)
	}
	if !refreshed.Resumed || refreshed.Harness != config.HarnessCopilot || len(runtime.copilotSeeds) != 2 {
		t.Fatalf("refresh = %#v with %d Copilot seeds, want one reseed and a Copilot resume", refreshed, len(runtime.copilotSeeds))
	}
	last := headlessWorker.headlessStarts[len(headlessWorker.headlessStarts)-1]
	if last.Mode != worker.HeadlessLaunchResume || !strings.Contains(strings.Join(last.Command, " "), "--resume="+refreshed.Invocation.NativeSessionID) {
		t.Fatalf("resume launch = %#v, want the recorded Copilot session", last)
	}
}

// TestRefreshAuthExplainsHowToRegisterAMissingCopilotSource verifies the
// remedy names the Copilot registration option.
func TestRefreshAuthExplainsHowToRegisterAMissingCopilotSource(t *testing.T) {
	t.Parallel()

	service, _, _, runID := copilotRefreshService(t, config.AuthenticationConfig{})
	_, err := service.RefreshAuth(context.Background(), factory.AuthRefreshRequest{RunID: runID})
	if err == nil || !strings.Contains(err.Error(), "factory register --update --copilot-auth <path>") {
		t.Fatalf("RefreshAuth() error = %v, want Copilot registration guidance", err)
	}
}

// TestStartAgentRefusesCopilotSelectionsBeforeAnySideEffect verifies a local
// policy refusal for Copilot starts no worker, projects no credential, and
// launches no process.
func TestStartAgentRefusesCopilotSelectionsBeforeAnySideEffect(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(*config.RepositoryConfig)
		request factory.AgentRequest
	}{
		{
			name:    "legacy harness override does not reach copilot",
			mutate:  func(c *config.RepositoryConfig) { c.AllowedOverrides = []config.OverrideName{config.OverrideHarness} },
			request: factory.AgentRequest{Harness: config.HarnessCopilot},
		},
		{
			name: "copilot effort outside the CLI set",
			mutate: func(c *config.RepositoryConfig) {
				c.RoleHarnessDefaults["implementation"] = config.HarnessCopilot
				c.AllowedOverrides = []config.OverrideName{config.OverrideReasoningEffort}
			},
			request: factory.AgentRequest{ReasoningEffort: "extreme"},
		},
		{
			name: "copilot automatic model",
			mutate: func(c *config.RepositoryConfig) {
				c.RoleHarnessDefaults["implementation"] = config.HarnessCopilot
				c.AllowedOverrides = []config.OverrideName{config.OverrideModel}
			},
			request: factory.AgentRequest{Model: "auto"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, runStore, runtime, _ := newAgentService(t)
			headlessWorker := &headlessAgentWorker{agentWorker: runtime}
			policy := validRepositoryConfig()
			test.mutate(&policy)
			service := newDispatchingAgentService(t, runStore, headlessWorker, policy, config.AuthenticationConfig{CopilotAuthPath: filepath.Join(t.TempDir(), "copilot-token")})

			_, err := service.StartAgent(context.Background(), test.request)
			var rejection *factory.PolicyRejection
			if !errors.As(err, &rejection) {
				t.Fatalf("StartAgent() error = %v, want a typed policy rejection", err)
			}
			if len(runtime.starts) != 0 || len(runtime.copilotSeeds) != 0 || len(headlessWorker.headlessStarts) != 0 || len(runStore.invocations) != 0 {
				t.Fatalf("side effects after refusal: workers %d, seeds %d, launches %d, invocations %d", len(runtime.starts), len(runtime.copilotSeeds), len(headlessWorker.headlessStarts), len(runStore.invocations))
			}
		})
	}
}

// headlessAgentWorker adds the detached process extension to the coordinator
// test worker and replays the launched session identity the way Claude Code
// confirms it in stream-json output.
type headlessAgentWorker struct {
	*agentWorker
	headlessStarts  []worker.HeadlessRequest
	headlessCancels []worker.HeadlessRequest
	resumeErr       error
	// finishes counts accepted-completion shutdowns.
	finishes int
}

// StartHeadless records one detached launch.
func (w *headlessAgentWorker) StartHeadless(_ context.Context, request worker.HeadlessRequest) (worker.HeadlessExecution, error) {
	w.headlessStarts = append(w.headlessStarts, request)
	if request.Mode == worker.HeadlessLaunchResume && w.resumeErr != nil {
		return worker.HeadlessExecution{}, w.resumeErr
	}
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
		for _, prefix := range []string{"--session-id=", "--resume="} {
			if value, found := strings.CutPrefix(argument, prefix); found {
				identity = value
			}
		}
	}
	output := `{"type":"thread.started","thread_id":"` + identity + `"}` + "\n"
	if len(command) > 0 && command[0] == "claude" {
		output = `{"type":"system","subtype":"init","session_id":"` + identity + `"}` + "\n"
	}
	if len(command) > 0 && command[0] == "factory-copilot" {
		output = `{"sessionId":"` + identity + `"}` + "\n"
	}
	return worker.HeadlessInspection{
		Status: worker.HeadlessStatusRunning,
		Stdout: output,
	}, nil
}

// CancelHeadless records detached process cancellation before worker stop.
func (w *headlessAgentWorker) CancelHeadless(_ context.Context, request worker.HeadlessRequest) error {
	w.headlessCancels = append(w.headlessCancels, request)
	return nil
}

// FinishHeadless records one accepted-completion shutdown.
func (w *headlessAgentWorker) FinishHeadless(context.Context, worker.HeadlessRequest) error {
	w.finishes++
	return nil
}

var _ worker.HeadlessProcessRuntime = (*headlessAgentWorker)(nil)
