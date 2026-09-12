package harness_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// claudeHeadlessSession is the fixed native session identity the Claude
// headless tests assign, resume, and expect back from a stream event.
const claudeHeadlessSession = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"

// TestClaudeHeadlessLaunchesPrintModeWithTheAssignedSession verifies the
// documented non-interactive Claude Code command, the adapter-assigned native
// identity, and the absence of any terminal allocation.
func TestClaudeHeadlessLaunchesPrintModeWithTheAssignedSession(t *testing.T) {
	workerRuntime := &headlessTestWorker{respond: claudeInitEvent}
	session, err := harness.NewClaudeHeadless(workerRuntime).StartHeadless(context.Background(), harness.HeadlessStartRequest{
		InvocationID: "inv-claude-headless", RunID: "run-claude-headless", WorkerID: "worker-claude-headless",
		Role: "implementation", Stage: "implementation", Prompt: "Implement the frozen issue.",
		Model: "claude-opus-5", ReasoningEffort: "high",
	})
	if err != nil {
		t.Fatalf("StartHeadless() error = %v", err)
	}
	if len(workerRuntime.starts) != 1 {
		t.Fatalf("headless starts = %#v, want one", workerRuntime.starts)
	}
	start := workerRuntime.starts[0]
	assigned := headlessOptionValue(start.Command, "--session-id")
	if assigned == "" || session.NativeSessionID != assigned {
		t.Fatalf("native session = %q, want the assigned identity %q", session.NativeSessionID, assigned)
	}
	for _, wanted := range []string{"claude", "-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions", "--strict-mcp-config", "--mcp-config", "--settings", "--model", "claude-opus-5", "--effort", "high"} {
		if !containsHeadlessArgument(start.Command, wanted) {
			t.Fatalf("headless command = %#v, want %q", start.Command, wanted)
		}
	}
	if containsHeadlessArgument(start.Command, "--resume") {
		t.Fatalf("fresh headless command = %#v, want no resume option", start.Command)
	}
	if last := start.Command[len(start.Command)-1]; last != "Implement the frozen issue." {
		t.Fatalf("headless command tail = %q, want the frozen prompt", last)
	}
	joined := strings.Join(start.Command, " ")
	if strings.Contains(joined, "-it") || strings.Contains(joined, "--tty") {
		t.Fatalf("headless command allocated a terminal: %q", joined)
	}
	if start.Environment["DISABLE_AUTOUPDATER"] != "1" || start.Mode != worker.HeadlessLaunchFresh {
		t.Fatalf("headless start request = %#v, want pinned harness and fresh mode", start)
	}
}

// TestClaudeHeadlessResumesTheExactNativeSession verifies exact-session resume
// and the refusal of a substituted native identity.
func TestClaudeHeadlessResumesTheExactNativeSession(t *testing.T) {
	workerRuntime := &headlessTestWorker{respond: claudeInitEvent}
	resumed, err := harness.NewClaudeHeadless(workerRuntime).ResumeHeadless(context.Background(), harness.HeadlessStartRequest{
		InvocationID: "inv-claude-resume", RunID: "run-claude-headless", WorkerID: "worker-claude-headless",
		Role: "implementation", Stage: "implementation", Prompt: "Continue.", ResumeSessionID: claudeHeadlessSession,
	})
	if err != nil {
		t.Fatalf("ResumeHeadless() error = %v", err)
	}
	start := workerRuntime.starts[0]
	if headlessOptionValue(start.Command, "--resume") != claudeHeadlessSession || containsHeadlessArgument(start.Command, "--session-id") {
		t.Fatalf("resume command = %#v, want only the exact resume option", start.Command)
	}
	if resumed.NativeSessionID != claudeHeadlessSession || start.Mode != worker.HeadlessLaunchResume {
		t.Fatalf("resumed session/request = %#v / %#v, want exact native identity and resume mode", resumed, start)
	}

	forked := &headlessTestWorker{inspection: worker.HeadlessInspection{
		Status: worker.HeadlessStatusRunning,
		Stdout: `{"type":"system","subtype":"init","session_id":"` + headlessTestSession + `"}` + "\n",
	}}
	if _, err := harness.NewClaudeHeadless(forked).ResumeHeadless(context.Background(), harness.HeadlessStartRequest{
		InvocationID: "inv-claude-forked", RunID: "run-claude-headless", Role: "implementation",
		Stage: "implementation", Prompt: "Continue.", ResumeSessionID: claudeHeadlessSession,
	}); !errors.Is(err, harness.ErrUnexpectedExit) {
		t.Fatalf("forked resume = %v, want a refused substitute identity", err)
	}
	if forked.cancelCalls != 1 {
		t.Fatalf("forked resume cancellations = %d, want one cleanup", forked.cancelCalls)
	}
}

// TestClaudeHeadlessAdoptsTheProcessThatAlreadyOwnsTheInvocation verifies a
// replayed fresh launch keeps the running process. The worker treats a fresh
// start as idempotent, so a second start after response loss must report the
// identity of the surviving process instead of cancelling it.
func TestClaudeHeadlessAdoptsTheProcessThatAlreadyOwnsTheInvocation(t *testing.T) {
	workerRuntime := &headlessTestWorker{inspection: worker.HeadlessInspection{
		Status: worker.HeadlessStatusRunning,
		Stdout: `{"type":"system","subtype":"init","session_id":"` + claudeHeadlessSession + `"}` + "\n",
	}}
	session, err := harness.NewClaudeHeadless(workerRuntime).StartHeadless(context.Background(), harness.HeadlessStartRequest{
		InvocationID: "inv-claude-replay", RunID: "run-claude-headless", WorkerID: "worker-claude-headless",
		Role: "implementation", Stage: "implementation", Prompt: "Implement the frozen issue.",
	})
	if err != nil {
		t.Fatalf("StartHeadless() error = %v", err)
	}
	if session.NativeSessionID != claudeHeadlessSession {
		t.Fatalf("native session = %q, want the surviving process identity %q", session.NativeSessionID, claudeHeadlessSession)
	}
	if workerRuntime.cancelCalls != 0 {
		t.Fatalf("cancellations = %d, want the running process left alone", workerRuntime.cancelCalls)
	}
}

// TestClaudeHeadlessClassifiesOnlyMachineReadableFailures verifies the typed
// outcomes come from documented stream events, that native prose never becomes
// a coordinator outcome, and that a retried request which finally succeeded is
// not reported as a capacity failure.
func TestClaudeHeadlessClassifiesOnlyMachineReadableFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		stdout string
		want   error
	}{
		{
			name:   "prose is not a failure category",
			stdout: "Claude says rate limit but emits no event\n",
			want:   harness.ErrUnexpectedExit,
		},
		{
			name:   "retry category with a failed result",
			stdout: `{"type":"system","subtype":"api_retry","error":"rate_limit"}` + "\n" + `{"type":"result","subtype":"error_during_execution","is_error":true}` + "\n",
			want:   harness.ErrRateLimited,
		},
		{
			name:   "authentication category with a failed result",
			stdout: `{"type":"system","subtype":"api_retry","error":"authentication_failed"}` + "\n" + `{"type":"result","subtype":"error_during_execution","is_error":true}` + "\n",
			want:   harness.ErrAuthenticationExpired,
		},
		{
			name:   "unrecoverable api error reported on the message",
			stdout: `{"type":"assistant","error":"authentication_failed","is_api_error_message":true}` + "\n" + `{"type":"result","subtype":"success","is_error":true}` + "\n",
			want:   harness.ErrAuthenticationExpired,
		},
		{
			name:   "retry that finally succeeded",
			stdout: `{"type":"system","subtype":"api_retry","error":"rate_limit"}` + "\n" + `{"type":"result","subtype":"success","is_error":false}` + "\n",
			want:   harness.ErrUnexpectedExit,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			workerRuntime := &headlessTestWorker{inspection: worker.HeadlessInspection{
				Status: worker.HeadlessStatusExited, ExitCode: 1, Stdout: test.stdout,
				Stderr: "token=must-not-reach-error",
			}}
			err := harness.NewClaudeHeadless(workerRuntime).HeadlessFailureFor(context.Background(), harness.HeadlessInspectionRequest{
				InvocationID: "inv-claude-classify", RunID: "run-claude-headless", WorkerID: "worker-claude-headless",
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("HeadlessFailureFor() = %v, want %v", err, test.want)
			}
			if strings.Contains(err.Error(), "token=") || strings.Contains(err.Error(), "rate limit") {
				t.Fatalf("headless error leaked native diagnostics: %v", err)
			}
		})
	}
}

// TestClaudeHeadlessRecoversTheSessionIdentityFromInvocationState verifies
// restart recovery reads the invocation's own detached process output instead
// of adopting an unrelated session from the shared role home.
func TestClaudeHeadlessRecoversTheSessionIdentityFromInvocationState(t *testing.T) {
	workerRuntime := &headlessTestWorker{inspection: worker.HeadlessInspection{
		Status: worker.HeadlessStatusRunning,
		Stdout: "older role-home session\n" + `{"type":"system","subtype":"init","session_id":"` + claudeHeadlessSession + `"}` + "\n",
	}}
	got, err := harness.NewClaudeHeadless(workerRuntime).NativeSessionID(context.Background(), harness.NativeSessionRequest{
		InvocationID: "inv-claude-recovered", RunID: "run-claude-headless", WorkerID: "worker-claude-headless", Harness: harness.NameClaude,
	})
	if err != nil {
		t.Fatalf("NativeSessionID() error = %v", err)
	}
	if got != claudeHeadlessSession {
		t.Fatalf("NativeSessionID() = %q, want the init event identity %q", got, claudeHeadlessSession)
	}
}

// claudeInitEvent replays the launched session identity through the documented
// stream-json init event, the way Claude Code confirms an assigned session.
func claudeInitEvent(request worker.HeadlessRequest) worker.HeadlessInspection {
	identity := headlessOptionValue(request.Command, "--session-id")
	if identity == "" {
		identity = headlessOptionValue(request.Command, "--resume")
	}
	return worker.HeadlessInspection{
		Status: worker.HeadlessStatusRunning,
		Stdout: `{"type":"system","subtype":"init","session_id":"` + identity + `"}` + "\n",
	}
}

// headlessOptionValue returns the value following one command option, or an
// empty string when the option is absent.
func headlessOptionValue(command []string, option string) string {
	for index, argument := range command {
		if argument == option && index+1 < len(command) {
			return command[index+1]
		}
	}
	return ""
}

// TestClaudeHeadlessClassifiesRealExpiredCredentialOutput verifies the
// classifier against output captured verbatim from Claude Code 2.1.232 running
// in the pinned worker image with no usable credential. The captured events
// are the reason the category is read from the message's own `error` field:
// the human-readable reason lives in `message.content`, which must never
// become a coordinator outcome, and the run ends with `subtype` "success"
// despite reporting `is_error`.
func TestClaudeHeadlessClassifiesRealExpiredCredentialOutput(t *testing.T) {
	captured := `{"type":"system","subtype":"init","cwd":"/work","session_id":"` + claudeHeadlessSession + `","tools":["Task","Bash"],"mcp_servers":[],"model":"claude-opus-5","permissionMode":"bypassPermissions","apiKeySource":"none","claude_code_version":"2.1.232"}
{"type":"assistant","message":{"id":"1f46f0c3-3869-4161-9eb3-5c9023ab702f","model":"<synthetic>","role":"assistant","type":"message","content":[{"type":"text","text":"Not logged in · Please run /login"}]},"parent_tool_use_id":null,"session_id":"` + claudeHeadlessSession + `","error":"authentication_failed","is_api_error_message":true}
{"is_error":true,"num_turns":1,"session_id":"` + claudeHeadlessSession + `","total_cost_usd":0,"permission_denials":[],"terminal_reason":"api_error","subtype":"success","result":"Not logged in · Please run /login","type":"result"}
`
	workerRuntime := &headlessTestWorker{inspection: worker.HeadlessInspection{
		Status: worker.HeadlessStatusExited, Stdout: captured,
	}}
	runtime := harness.NewClaudeHeadless(workerRuntime)
	err := runtime.HeadlessFailureFor(context.Background(), harness.HeadlessInspectionRequest{
		InvocationID: "inv-claude-expired", RunID: "run-claude-headless", WorkerID: "worker-claude-headless",
	})
	if !errors.Is(err, harness.ErrAuthenticationExpired) {
		t.Fatalf("HeadlessFailureFor() = %v, want the authentication-expired outcome", err)
	}
	if strings.Contains(err.Error(), "Not logged in") {
		t.Fatalf("headless error leaked the harness result text: %v", err)
	}
	got, err := runtime.NativeSessionID(context.Background(), harness.NativeSessionRequest{
		InvocationID: "inv-claude-expired", RunID: "run-claude-headless", Harness: harness.NameClaude,
	})
	if err != nil || got != claudeHeadlessSession {
		t.Fatalf("NativeSessionID() = %q, %v; want the captured init identity", got, err)
	}
}

// TestClaudeHeadlessRefusesAResumeIdentityItCouldNotHaveAssigned verifies the
// headless adapter applies the same pre-launch session-shape refusal as the
// interactive one. Claude Code also accepts a session name or a transcript
// path after --resume, so an identifier the adapter never assigned must not
// reach the command.
func TestClaudeHeadlessRefusesAResumeIdentityItCouldNotHaveAssigned(t *testing.T) {
	workerRuntime := &headlessTestWorker{respond: claudeInitEvent}
	if _, err := harness.NewClaudeHeadless(workerRuntime).ResumeHeadless(context.Background(), harness.HeadlessStartRequest{
		InvocationID: "inv-claude-unsafe", RunID: "run-claude-headless", Role: "implementation",
		Stage: "implementation", Prompt: "Continue.", ResumeSessionID: "auth-refactor",
	}); err == nil {
		t.Fatal("ResumeHeadless() accepted a non-session identifier, want a refused launch")
	}
	if len(workerRuntime.starts) != 0 {
		t.Fatalf("headless launches = %#v, want the refusal before any worker call", workerRuntime.starts)
	}
}

// TestClaudeHeadlessWithholdsWorktreeDeclaredHooks verifies the launch passes
// the settings layer that keeps a repository's own hooks out of the session.
// A non-interactive run otherwise executes them without a trust prompt, which
// would turn mutable worktree content into commands.
func TestClaudeHeadlessWithholdsWorktreeDeclaredHooks(t *testing.T) {
	workerRuntime := &headlessTestWorker{respond: claudeInitEvent}
	if _, err := harness.NewClaudeHeadless(workerRuntime).StartHeadless(context.Background(), harness.HeadlessStartRequest{
		InvocationID: "inv-claude-hooks", RunID: "run-claude-headless", Role: "implementation",
		Stage: "implementation", Prompt: "Implement the frozen issue.",
	}); err != nil {
		t.Fatalf("StartHeadless() error = %v", err)
	}
	command := workerRuntime.starts[0].Command
	if headlessOptionValue(command, "--settings") != `{"disableAllHooks":true}` {
		t.Fatalf("headless command = %#v, want the hook-withholding settings layer", command)
	}
}
