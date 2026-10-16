package harness_test

import (
	"context"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestHeadlessAdaptersExportSessionOwnership verifies that both adapters give
// a session-role process its explicit session identity and pinned revision,
// so factory-report can name the session instead of a run.
func TestHeadlessAdaptersExportSessionOwnership(t *testing.T) {
	for name, build := range map[string]func(worker.HeadlessProcessRuntime) harness.HeadlessRuntime{
		harness.NameCodex: func(runtime worker.HeadlessProcessRuntime) harness.HeadlessRuntime {
			return harness.NewCodexHeadless(runtime)
		},
		harness.NameClaude: func(runtime worker.HeadlessProcessRuntime) harness.HeadlessRuntime {
			return harness.NewClaudeHeadless(runtime)
		},
	} {
		workerRuntime := &headlessTestWorker{inspection: worker.HeadlessInspection{
			Status: worker.HeadlessStatusRunning,
			Stdout: "{\"type\":\"thread.started\",\"thread_id\":\"" + headlessTestSession + "\"}\n" +
				"{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"" + headlessTestSession + "\"}\n",
		}}
		_, err := build(workerRuntime).StartHeadless(context.Background(), harness.HeadlessStartRequest{
			InvocationID: "sinv-1", RunID: "session-owner", WorkerID: "session-owner-ideation",
			SessionID: "session-1", SessionRevision: 9,
			Role: "ideation", Stage: "ideation", Prompt: "Ask the next question.",
		})
		if err != nil {
			t.Fatalf("%s StartHeadless() error = %v", name, err)
		}
		environment := workerRuntime.starts[0].Environment
		if environment["FACTORY_SESSION_ID"] != "session-1" || environment["FACTORY_SESSION_REVISION"] != "9" {
			t.Fatalf("%s environment = %#v, want session identity and revision", name, environment)
		}
	}
}

// TestNativeSessionMismatchIsTypedAndKeepsTheUnexpectedExitPolicy verifies
// that a resume which reveals a different native identity is classified as a
// native-session mismatch while run recovery still sees an unexpected exit.
func TestNativeSessionMismatchIsTypedAndKeepsTheUnexpectedExitPolicy(t *testing.T) {
	const other = "9b2f6c1e-3d4a-4f5b-8c6d-7e8f9a0b1c2d"
	workerRuntime := &headlessTestWorker{inspection: worker.HeadlessInspection{
		Status: worker.HeadlessStatusRunning,
		Stdout: "{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"" + other + "\"}\n",
	}}
	_, err := harness.NewClaudeHeadless(workerRuntime).ResumeHeadless(context.Background(), harness.HeadlessStartRequest{
		InvocationID: "sinv-2", RunID: "session-owner", Role: "ideation", Stage: "ideation",
		Prompt: "Continue.", ResumeSessionID: headlessTestSession,
	})
	classified := harness.ClassifyError(err, harness.NameClaude)
	if !harness.IsNativeSessionMismatch(classified) || !harness.IsUnexpectedExit(classified) {
		t.Fatalf("ResumeHeadless() error = %v, want a typed native-session mismatch that is also an unexpected exit", err)
	}
	if harness.IsNativeSessionMismatch(harness.NewUnexpectedExitError(harness.NameClaude)) {
		t.Fatal("an ordinary unexpected exit must not be a native-session mismatch")
	}
	if workerRuntime.cancelCalls != 1 {
		t.Fatalf("cancel calls = %d, want the forked process cancelled", workerRuntime.cancelCalls)
	}
}
