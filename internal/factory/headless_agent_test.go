package factory_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestStartAgentLaunchesAClaudeRoleThroughTheHeadlessSeam verifies a
// repository-declared Claude role reaches the detached worker protocol with no
// terminal workspace, no surface, and the factory-assigned native session
// identity persisted on the invocation.
func TestStartAgentLaunchesAClaudeRoleThroughTheHeadlessSeam(t *testing.T) {
	_, runStore, runtime, terminalRuntime, _ := newAgentService(t)
	headlessWorker := &headlessAgentWorker{agentWorker: runtime}
	policy := validRepositoryConfig()
	policy.RoleHarnessDefaults["implementation"] = config.HarnessClaude
	policy.ModelOptions["implementation"] = []string{"claude-opus-5"}
	claudeAuth := filepath.Join(t.TempDir(), ".credentials.json")
	service := newDispatchingAgentService(t, runStore, headlessWorker, terminalRuntime, policy, config.AuthenticationConfig{
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
	if len(headlessWorker.headlessStarts) != 1 {
		t.Fatalf("headless launches = %#v, want exactly one detached Claude process", headlessWorker.headlessStarts)
	}
	command := headlessWorker.headlessStarts[0].Command
	for _, wanted := range []string{"claude", "-p", "stream-json", "--verbose", "--session-id"} {
		if !strings.Contains(strings.Join(command, " "), wanted) {
			t.Fatalf("headless command = %#v, want %q", command, wanted)
		}
	}
	if len(runtime.interactive) != 0 {
		t.Fatalf("interactive worker commands = %#v, want none for a headless role", runtime.interactive)
	}
	if launch.Invocation.WorkspaceID != "" || launch.Invocation.RoleSurfaceID != "" || launch.Invocation.ImplementationSurfaceID != "" {
		t.Fatalf("invocation terminal handles = %#v, want a terminal-free headless invocation", launch.Invocation)
	}
	if len(terminalRuntime.notifications) != 0 {
		t.Fatalf("terminal notifications = %#v, want none for a headless role", terminalRuntime.notifications)
	}
	if launch.Invocation.NativeSessionID == "" {
		t.Fatal("invocation has no native session id, want the adapter-assigned Claude session")
	}
	if len(runtime.claudeSeeds) != 1 || runtime.claudeSeeds[0].AuthPath != claudeAuth {
		t.Fatalf("Claude credential seeds = %#v, want only the registered Claude source", runtime.claudeSeeds)
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
	identity := ""
	for index, argument := range command {
		if (argument == "--session-id" || argument == "--resume") && index+1 < len(command) {
			identity = command[index+1]
		}
	}
	return worker.HeadlessInspection{
		Status: worker.HeadlessStatusRunning,
		Stdout: `{"type":"system","subtype":"init","session_id":"` + identity + `"}` + "\n",
	}, nil
}

// CancelHeadless implements the detached process extension.
func (*headlessAgentWorker) CancelHeadless(context.Context, worker.HeadlessRequest) error { return nil }

// FinishHeadless implements the detached process extension.
func (*headlessAgentWorker) FinishHeadless(context.Context, worker.HeadlessRequest) error { return nil }

var _ worker.HeadlessProcessRuntime = (*headlessAgentWorker)(nil)
