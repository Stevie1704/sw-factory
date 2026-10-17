package harness_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// copilotHeadlessSession is the fixed native identity the Copilot tests
// assign, resume, and expect back as identity evidence. It is the session
// UUID of the pinned CLI fixtures.
const copilotHeadlessSession = "00000000-0000-4000-8000-000000000001"

// copilotFixture reads one sanitized probe output of the pinned Copilot CLI
// 1.0.98 compatibility record.
func copilotFixture(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("..", "..", "docs", "spikes", "copilot-cli-1.0.98", name))
	if err != nil {
		t.Fatalf("read Copilot fixture: %v", err)
	}
	return string(content)
}

// copilotIdentityEvidence is the provisional identity evidence line (pending
// RB-1): one complete JSONL object that names the session.
func copilotIdentityEvidence(identity string) string {
	return `{"sessionId":"` + identity + `"}` + "\n"
}

// copilotSessionOption returns the value of a --option=value argument.
func copilotSessionOption(command []string, option string) string {
	for _, argument := range command {
		if value, found := strings.CutPrefix(argument, option+"="); found {
			return value
		}
	}
	return ""
}

// copilotEcho replays the launched session identity as evidence from the
// current process, the way the CLI confirms an assigned session.
func copilotEcho(request worker.HeadlessRequest) worker.HeadlessInspection {
	identity := copilotSessionOption(request.Command, "--session-id")
	if identity == "" {
		identity = copilotSessionOption(request.Command, "--resume")
	}
	return worker.HeadlessInspection{Status: worker.HeadlessStatusRunning, Stdout: copilotIdentityEvidence(identity)}
}

// copilotRequest is a valid neutral launch request.
func copilotRequest() harness.HeadlessStartRequest {
	return harness.HeadlessStartRequest{
		InvocationID: "inv-copilot", RunID: "run-copilot", WorkerID: "worker-copilot",
		Role: "implementation", Stage: "implementation", Prompt: "- Implement the frozen issue.",
	}
}

// TestCopilotHeadlessLaunchesThroughTheWorkerLauncherWithTheAssignedSession
// verifies the proved fresh command: the in-worker launcher, the suppression
// options, the translated selection, and an adapter-assigned full UUID.
func TestCopilotHeadlessLaunchesThroughTheWorkerLauncherWithTheAssignedSession(t *testing.T) {
	workerRuntime := &headlessTestWorker{respond: copilotEcho}
	request := copilotRequest()
	request.Model, request.ReasoningEffort = "gpt-5.2", "xhigh"
	session, err := harness.NewCopilotHeadless(workerRuntime).StartHeadless(context.Background(), request)
	if err != nil {
		t.Fatalf("StartHeadless() error = %v", err)
	}
	start := workerRuntime.starts[0]
	if start.Command[0] != "factory-copilot" {
		t.Fatalf("command = %#v, want the in-worker launcher first", start.Command)
	}
	assigned := copilotSessionOption(start.Command, "--session-id")
	if assigned == "" || session.NativeSessionID != assigned {
		t.Fatalf("native session = %q, want the assigned identity %q", session.NativeSessionID, assigned)
	}
	for _, wanted := range []string{
		"--prompt=- Implement the frozen issue.", "--output-format=json", "--allow-all-tools", "--no-ask-user",
		"--no-auto-update", "--no-remote", "--no-remote-export", "--disable-builtin-mcps",
		"--no-custom-instructions", "--secret-env-vars=COPILOT_GITHUB_TOKEN",
		"--model=gpt-5.2", "--reasoning-effort=xhigh",
	} {
		if !containsCopilotArgument(start.Command, wanted) {
			t.Fatalf("command = %#v, want %q", start.Command, wanted)
		}
	}
	for _, forbidden := range []string{"--resume", "--continue", "--allow-all", "--yolo", "--name", "-n", "--add-dir", "--remote", "--share", "-p", "-it", "--tty"} {
		if containsCopilotOption(start.Command, forbidden) {
			t.Fatalf("fresh command = %#v, must not pass %q", start.Command, forbidden)
		}
	}
	if start.Environment["COPILOT_AUTO_UPDATE"] != "false" || start.Mode != worker.HeadlessLaunchFresh {
		t.Fatalf("start request = %#v, want the pinned CLI and fresh mode", start)
	}
	for name := range start.Environment {
		if strings.Contains(name, "TOKEN") {
			t.Fatalf("start environment names credential variable %q", name)
		}
	}
}

// TestCopilotHeadlessResumesOnlyTheRecordedCompleteIdentity verifies resume
// passes only --resume=<full uuid>, refuses prefixes, names, and empty
// identities before launch, and refuses a substituted identity.
func TestCopilotHeadlessResumesOnlyTheRecordedCompleteIdentity(t *testing.T) {
	workerRuntime := &headlessTestWorker{respond: copilotEcho}
	request := copilotRequest()
	request.ResumeSessionID = copilotHeadlessSession
	resumed, err := harness.NewCopilotHeadless(workerRuntime).ResumeHeadless(context.Background(), request)
	if err != nil {
		t.Fatalf("ResumeHeadless() error = %v", err)
	}
	start := workerRuntime.starts[0]
	if !containsCopilotArgument(start.Command, "--resume="+copilotHeadlessSession) || containsCopilotOption(start.Command, "--session-id") {
		t.Fatalf("resume command = %#v, want only the exact resume option", start.Command)
	}
	if resumed.NativeSessionID != copilotHeadlessSession || start.Mode != worker.HeadlessLaunchResume {
		t.Fatalf("resumed session/request = %#v / %#v, want exact identity and resume mode", resumed, start)
	}

	for _, identity := range []string{"", "0000000", "factory-missing-name", strings.ToUpper(headlessTestSession), "00000000-0000-1000-8000-000000000001"} {
		refused := &headlessTestWorker{respond: copilotEcho}
		request.ResumeSessionID = identity
		if _, err := harness.NewCopilotHeadless(refused).ResumeHeadless(context.Background(), request); err == nil || len(refused.starts) != 0 {
			t.Fatalf("ResumeHeadless(%q) = %v with %d starts, want a refusal before launch", identity, err, len(refused.starts))
		}
	}

	forked := &headlessTestWorker{inspection: worker.HeadlessInspection{Status: worker.HeadlessStatusRunning, Stdout: copilotIdentityEvidence(headlessTestSession)}}
	request.ResumeSessionID = copilotHeadlessSession
	if _, err := harness.NewCopilotHeadless(forked).ResumeHeadless(context.Background(), request); !errors.Is(err, harness.ErrUnexpectedExit) {
		t.Fatalf("forked resume = %v, want a refused substitute identity", err)
	}
	if forked.cancelCalls != 1 {
		t.Fatalf("forked resume cancellations = %d, want one cleanup", forked.cancelCalls)
	}
}

// TestCopilotHeadlessAdoptsTheProcessThatAlreadyOwnsTheInvocation verifies a
// replayed fresh launch keeps the surviving process and its identity.
func TestCopilotHeadlessAdoptsTheProcessThatAlreadyOwnsTheInvocation(t *testing.T) {
	workerRuntime := &headlessTestWorker{adopted: true, inspection: worker.HeadlessInspection{Status: worker.HeadlessStatusRunning, Stdout: copilotIdentityEvidence(copilotHeadlessSession)}}
	session, err := harness.NewCopilotHeadless(workerRuntime).StartHeadless(context.Background(), copilotRequest())
	if err != nil || session.NativeSessionID != copilotHeadlessSession || workerRuntime.cancelCalls != 0 {
		t.Fatalf("StartHeadless() = %#v, %v with %d cancellations; want the surviving process", session, err, workerRuntime.cancelCalls)
	}
}

// TestCopilotHeadlessRefusesAFreshStartThatIgnoresTheAssignedSession verifies
// that a newly started process must confirm the --session-id it was given. A
// CLI that ignores the option and reports its own identity fails closed as a
// native session mismatch, and the process is cleaned up.
func TestCopilotHeadlessRefusesAFreshStartThatIgnoresTheAssignedSession(t *testing.T) {
	workerRuntime := &headlessTestWorker{inspection: worker.HeadlessInspection{Status: worker.HeadlessStatusRunning, Stdout: copilotIdentityEvidence(headlessTestSession)}}
	runtime := harness.NewCopilotHeadless(workerRuntime)
	runtime.NewSessionID = func() (string, error) { return copilotHeadlessSession, nil }
	_, err := runtime.StartHeadless(context.Background(), copilotRequest())
	if !harness.IsNativeSessionMismatch(err) {
		t.Fatalf("StartHeadless() = %v, want a native session mismatch", err)
	}
	if workerRuntime.cancelCalls != 1 {
		t.Fatalf("cancellations = %d, want one failed-launch cleanup", workerRuntime.cancelCalls)
	}
}

// TestCopilotHeadlessFailsClosedOnUnusableIdentityEvidence verifies absent,
// partial, prose, malformed, nested, oversized, and conflicting evidence never
// becomes a native identity, and the failed launch is cleaned up.
func TestCopilotHeadlessFailsClosedOnUnusableIdentityEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		stdout string
	}{
		{name: "absent", stdout: ""},
		{name: "partial JSON line", stdout: `{"sessionId":"` + copilotHeadlessSession},
		{name: "prose", stdout: "Resume with copilot --resume=" + copilotHeadlessSession + "\n"},
		{name: "malformed identity", stdout: `{"sessionId":"0000000"}` + "\n"},
		{name: "non-string identity", stdout: `{"sessionId":7}` + "\n" + copilotIdentityEvidence(copilotHeadlessSession)},
		{name: "conflicting identities", stdout: copilotIdentityEvidence(copilotHeadlessSession) + `{"session_id":"` + headlessTestSession + `"}` + "\n"},
		{name: "nested identity only", stdout: `{"data":{"sessionId":"` + copilotHeadlessSession + `"}}` + "\n"},
		{name: "conflicting identity after an oversized line", stdout: copilotIdentityEvidence(copilotHeadlessSession) + `{"text":"` + strings.Repeat("x", 2<<20) + `"}` + "\n" + `{"session_id":"` + headlessTestSession + `"}` + "\n"},
		{name: "oversized line", stdout: `{"text":"` + strings.Repeat("x", 2<<20) + `"}` + "\n" + copilotIdentityEvidence(copilotHeadlessSession)},
	} {
		t.Run(test.name, func(t *testing.T) {
			workerRuntime := &headlessTestWorker{inspection: worker.HeadlessInspection{Status: worker.HeadlessStatusExited, Stdout: test.stdout}}
			runtime := harness.NewCopilotHeadless(workerRuntime)
			if _, err := runtime.StartHeadless(context.Background(), copilotRequest()); !errors.Is(err, harness.ErrUnexpectedExit) {
				t.Fatalf("StartHeadless() = %v, want a fail-closed unexpected exit", err)
			}
			if workerRuntime.cancelCalls != 1 {
				t.Fatalf("cancellations = %d, want one failed-launch cleanup", workerRuntime.cancelCalls)
			}
			if got, err := runtime.NativeSessionID(context.Background(), harness.NativeSessionRequest{
				InvocationID: "inv-copilot", RunID: "run-copilot", Harness: harness.NameCopilot,
			}); err == nil {
				t.Fatalf("NativeSessionID() = %q, want no identity", got)
			}
		})
	}
}

// TestCopilotHeadlessMapsObservedFailuresToTypedOutcomes verifies the stderr
// evidence of the pinned CLI, during identity discovery and after process
// settlement. Session, argument, and unknown failures (such as a policy or
// model denial) fail closed as an unexpected exit and never become a
// capacity retry.
func TestCopilotHeadlessMapsObservedFailuresToTypedOutcomes(t *testing.T) {
	for _, test := range []struct {
		name   string
		stderr string
		want   error
	}{
		{name: "no credential", stderr: copilotFixture(t, "fresh-missing-auth.stderr"), want: harness.ErrAuthenticationExpired},
		{name: "token not validated", stderr: copilotFixture(t, "fake-gh-token.stderr"), want: harness.ErrAuthenticationExpired},
		{name: "launcher credential unavailable", stderr: "factory-copilot: credential unavailable\n", want: harness.ErrAuthenticationExpired},
		{name: "unknown session", stderr: copilotFixture(t, "resume-unknown.stderr"), want: harness.ErrUnexpectedExit},
		{name: "corrupt session", stderr: copilotFixture(t, "resume-corrupt.stderr"), want: harness.ErrUnexpectedExit},
		{name: "non-UUID session", stderr: copilotFixture(t, "session-id-unknown-name.stderr"), want: harness.ErrUnexpectedExit},
		{name: "argument error", stderr: copilotFixture(t, "effort-unsupported.stderr"), want: harness.ErrUnexpectedExit},
		{name: "unobserved model unavailable", stderr: "Error: Model \"gpt-5.2\" is not available. Run copilot --model to choose another model.\n", want: harness.ErrUnexpectedExit},
		{name: "unobserved policy denial", stderr: "Error: Copilot CLI is disabled by your organization's policy. Rate limit does not apply.\n", want: harness.ErrUnexpectedExit},
	} {
		t.Run(test.name, func(t *testing.T) {
			settled := worker.HeadlessInspection{Status: worker.HeadlessStatusExited, ExitCode: 1, Stderr: test.stderr}
			discovery := &headlessTestWorker{inspection: settled}
			_, launchErr := harness.NewCopilotHeadless(discovery).StartHeadless(context.Background(), copilotRequest())

			settled.Stdout = copilotIdentityEvidence(copilotHeadlessSession)
			settledErr := harness.NewCopilotHeadless(&headlessTestWorker{inspection: settled}).HeadlessFailureFor(context.Background(), harness.HeadlessInspectionRequest{
				InvocationID: "inv-copilot", RunID: "run-copilot", WorkerID: "worker-copilot",
			})
			for phase, err := range map[string]error{"discovery": launchErr, "settlement": settledErr} {
				if !errors.Is(err, test.want) || errors.Is(err, harness.ErrRateLimited) {
					t.Fatalf("%s outcome = %v, want %v", phase, err, test.want)
				}
				if strings.Contains(err.Error(), "Error:") || len(harness.HeadlessDiagnostics(err)) > 16<<10 {
					t.Fatalf("%s error leaked or overflowed native output: %v", phase, err)
				}
			}
		})
	}
}

// TestCopilotHeadlessNeverTreatsCLISuccessAsCompletion verifies that a clean
// exit with identity evidence still settles as an unexpected exit, so only an
// accepted factory report completes an invocation, and that retry events the
// run recovered from before it succeeded never become a capacity outcome.
// The retry event shape is unobserved (release blocker RB-5).
func TestCopilotHeadlessNeverTreatsCLISuccessAsCompletion(t *testing.T) {
	success := `{"type":"done","success":true}` + "\n"
	for _, test := range []struct {
		name, stdout, stderr string
	}{
		{name: "success", stdout: copilotIdentityEvidence(copilotHeadlessSession) + success},
		{
			name:   "retry events then success",
			stdout: copilotIdentityEvidence(copilotHeadlessSession) + `{"type":"retry","error":"rate_limit","status":429}` + "\n" + success,
			stderr: "Error: 429 Too Many Requests; retrying\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			workerRuntime := &headlessTestWorker{inspection: worker.HeadlessInspection{Status: worker.HeadlessStatusExited, Stdout: test.stdout, Stderr: test.stderr}}
			err := harness.NewCopilotHeadless(workerRuntime).HeadlessFailureFor(context.Background(), harness.HeadlessInspectionRequest{InvocationID: "inv-copilot", RunID: "run-copilot"})
			if !errors.Is(err, harness.ErrUnexpectedExit) || errors.Is(err, harness.ErrRateLimited) {
				t.Fatalf("HeadlessFailureFor() = %v, want an unexpected exit without a report", err)
			}
		})
	}
}

// TestCopilotHeadlessRefusesSelectionsThePinnedCLICannotHonor verifies an
// unsupported effort and the non-exact "auto" model fail before any worker
// launch, while every effort the pinned CLI accepts is passed on.
func TestCopilotHeadlessRefusesSelectionsThePinnedCLICannotHonor(t *testing.T) {
	for _, test := range []struct{ model, effort string }{
		{effort: "extreme"}, {effort: "HIGH"}, {model: "auto"}, {model: "Auto"},
	} {
		workerRuntime := &headlessTestWorker{respond: copilotEcho}
		request := copilotRequest()
		request.Model, request.ReasoningEffort = test.model, test.effort
		if _, err := harness.NewCopilotHeadless(workerRuntime).StartHeadless(context.Background(), request); err == nil || len(workerRuntime.starts) != 0 {
			t.Fatalf("StartHeadless(%+v) = %v with %d starts, want a refusal before launch", test, err, len(workerRuntime.starts))
		}
	}
	for _, effort := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"} {
		request := copilotRequest()
		request.ReasoningEffort = effort
		if _, err := harness.NewCopilotHeadless(&headlessTestWorker{respond: copilotEcho}).StartHeadless(context.Background(), request); err != nil {
			t.Fatalf("StartHeadless(effort %q) error = %v", effort, err)
		}
	}
}

// containsCopilotArgument reports whether command has exactly argument.
func containsCopilotArgument(command []string, argument string) bool {
	for _, candidate := range command {
		if candidate == argument {
			return true
		}
	}
	return false
}

// containsCopilotOption reports whether command passes option bare or in the
// --option=value form.
func containsCopilotOption(command []string, option string) bool {
	for _, candidate := range command {
		if candidate == option || strings.HasPrefix(candidate, option+"=") {
			return true
		}
	}
	return false
}
