package harness_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/terminal"
)

// claudeSurface is the visible role surface used by the Claude contract tests.
func claudeSurface() terminal.Surface {
	return terminal.Surface{ID: "surface-implementation", WorkspaceID: "workspace-run", Name: "implementation"}
}

// TestClaudeStartsAnInteractiveSessionWithAnAssignedNativeSession verifies the
// coordinator assigns the native session identifier at launch instead of
// discovering it afterwards, which removes the fresh-launch discovery race.
func TestClaudeStartsAnInteractiveSessionWithAnAssignedNativeSession(t *testing.T) {
	workerRuntime := &fakeWorker{}
	terminalRuntime := &fakeTerminal{surface: claudeSurface()}
	runtime := harness.NewClaude(workerRuntime, terminalRuntime)
	runtime.NewSessionID = func() (string, error) { return "3f2504e0-4f89-41d3-9a0c-0305e82c3301", nil }
	session, err := runtime.Start(context.Background(), harness.StartRequest{
		InvocationID: "inv-1",
		RunID:        "run-1",
		Role:         "implementation",
		Stage:        "implementation",
		WorkspaceID:  "workspace-run",
		Surface:      terminalRuntime.surface,
		Prompt:       "Implement the frozen specification.",
		Model:        "claude-opus-5",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if session.NativeSessionID != "3f2504e0-4f89-41d3-9a0c-0305e82c3301" {
		t.Fatalf("NativeSessionID = %q, want the coordinator-assigned session", session.NativeSessionID)
	}
	if len(workerRuntime.requests) != 1 {
		t.Fatalf("interactive worker requests = %#v, want one", workerRuntime.requests)
	}
	request := workerRuntime.requests[0]
	wantCommand := []string{
		"claude", "--dangerously-skip-permissions",
		"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
		"--session-id", "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		"--model", "claude-opus-5",
		"Implement the frozen specification.",
	}
	if strings.Join(request.Command, "\x00") != strings.Join(wantCommand, "\x00") {
		t.Fatalf("Claude command = %#v, want isolated launch followed by the initial prompt argument", request.Command)
	}
	if request.Environment["FACTORY_HARNESS"] != "claude" || request.Environment["FACTORY_INVOCATION_ID"] != "inv-1" {
		t.Fatalf("Claude environment = %#v, want harness and invocation identity", request.Environment)
	}
	if request.Environment["DISABLE_AUTOUPDATER"] != "1" {
		t.Fatalf("Claude environment = %#v, want the pinned harness version protected from self-update", request.Environment)
	}
	if len(terminalRuntime.inputs) != 0 {
		t.Fatalf("surface inputs = %q, want startup to avoid racing terminal input", terminalRuntime.inputs)
	}
	if terminalRuntime.launched.Executable != "factory-worker-attach" {
		t.Fatalf("surface launch = %#v, want attach helper in the existing implementation surface", terminalRuntime.launched)
	}
}

// TestClaudeStartRefusesAResumeSessionIdentifier verifies a fresh launch
// cannot silently continue another invocation's session.
func TestClaudeStartRefusesAResumeSessionIdentifier(t *testing.T) {
	_, err := harness.NewClaude(&fakeWorker{}, &fakeTerminal{surface: claudeSurface()}).Start(context.Background(), harness.StartRequest{
		InvocationID:    "inv-1",
		RunID:           "run-1",
		Role:            "implementation",
		Stage:           "implementation",
		WorkspaceID:     "workspace-run",
		Prompt:          "Implement the frozen specification.",
		ResumeSessionID: "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
	})
	if err == nil || !strings.Contains(err.Error(), "resume session") {
		t.Fatalf("Start() error = %v, want a fresh-start refusal", err)
	}
}

// TestClaudePassesAValidatedReasoningEffort verifies the repository-declared
// effort reaches the harness. Claude Code warns about an unrecognized level
// and then silently uses its default, so the coordinator's declared options
// are what constrain the value.
func TestClaudePassesAValidatedReasoningEffort(t *testing.T) {
	workerRuntime := &fakeWorker{}
	terminalRuntime := &fakeTerminal{surface: claudeSurface()}
	runtime := harness.NewClaude(workerRuntime, terminalRuntime)
	runtime.NewSessionID = func() (string, error) { return "3f2504e0-4f89-41d3-9a0c-0305e82c3301", nil }
	if _, err := runtime.Start(context.Background(), harness.StartRequest{
		InvocationID:    "inv-1",
		RunID:           "run-1",
		Role:            "implementation",
		Stage:           "implementation",
		WorkspaceID:     "workspace-run",
		Surface:         terminalRuntime.surface,
		Prompt:          "Implement the frozen specification.",
		Model:           "claude-opus-5",
		ReasoningEffort: "high",
	}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	request := workerRuntime.requests[0]
	wantCommand := []string{
		"claude", "--dangerously-skip-permissions",
		"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
		"--session-id", "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		"--model", "claude-opus-5",
		"--effort", "high",
		"Implement the frozen specification.",
	}
	if strings.Join(request.Command, "\x00") != strings.Join(wantCommand, "\x00") {
		t.Fatalf("Claude command = %#v, want the validated effort level", request.Command)
	}
	if request.Environment["FACTORY_REASONING_EFFORT"] != "high" {
		t.Fatalf("Claude environment = %#v, want the reasoning-effort identity", request.Environment)
	}
}

// TestClaudeResumesTheNativeSessionWithoutAssigningANewOne verifies resume
// continues the recorded session and never mints a second identifier.
func TestClaudeResumesTheNativeSessionWithoutAssigningANewOne(t *testing.T) {
	workerRuntime := &fakeWorker{}
	terminalRuntime := &fakeTerminal{surface: claudeSurface()}
	runtime := harness.NewClaude(workerRuntime, terminalRuntime)
	runtime.NewSessionID = func() (string, error) { return "", errors.New("resume must not assign a session") }
	session, err := runtime.Resume(context.Background(), harness.StartRequest{
		InvocationID:    "inv-2",
		RunID:           "run-1",
		Role:            "implementation",
		Stage:           "implementation",
		WorkspaceID:     "workspace-run",
		Surface:         terminalRuntime.surface,
		Prompt:          "Continue the implementation.",
		ResumeSessionID: "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
	})
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if session.NativeSessionID != "3f2504e0-4f89-41d3-9a0c-0305e82c3301" {
		t.Fatalf("NativeSessionID = %q, want the continued native session", session.NativeSessionID)
	}
	wantCommand := []string{
		"claude", "--dangerously-skip-permissions",
		"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
		"--resume", "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		"Continue the implementation.",
	}
	if strings.Join(workerRuntime.requests[0].Command, "\x00") != strings.Join(wantCommand, "\x00") {
		t.Fatalf("resume command = %#v, want native resume command", workerRuntime.requests[0].Command)
	}
	if len(terminalRuntime.inputs) != 0 {
		t.Fatalf("surface inputs = %q, want resume prompt passed as an argument", terminalRuntime.inputs)
	}
}

// TestClaudeResumeRequiresANativeSessionIdentifier verifies resume cannot
// degrade into an unrelated fresh session.
func TestClaudeResumeRequiresANativeSessionIdentifier(t *testing.T) {
	_, err := harness.NewClaude(&fakeWorker{}, &fakeTerminal{surface: claudeSurface()}).Resume(context.Background(), harness.StartRequest{
		InvocationID: "inv-2",
		RunID:        "run-1",
		Role:         "implementation",
		Stage:        "implementation",
		WorkspaceID:  "workspace-run",
		Prompt:       "Continue the implementation.",
	})
	if err == nil || !strings.Contains(err.Error(), "resume session id is required") {
		t.Fatalf("Resume() error = %v, want a missing-session refusal", err)
	}
}

// TestClaudeRejectsAnUnsafeNativeSessionIdentifier verifies neither an assigned
// nor a recorded identifier can introduce a process argument.
func TestClaudeRejectsAnUnsafeNativeSessionIdentifier(t *testing.T) {
	workerRuntime := &fakeWorker{}
	runtime := harness.NewClaude(workerRuntime, &fakeTerminal{surface: claudeSurface()})
	runtime.NewSessionID = func() (string, error) { return "--dangerously-skip-permissions", nil }
	if _, err := runtime.Start(context.Background(), harness.StartRequest{
		InvocationID: "inv-1",
		RunID:        "run-1",
		Role:         "implementation",
		Stage:        "implementation",
		WorkspaceID:  "workspace-run",
		Prompt:       "Implement the frozen specification.",
	}); err == nil || !strings.Contains(err.Error(), "session id") {
		t.Fatalf("Start() error = %v, want an unsafe assigned-session refusal", err)
	}
	if _, err := runtime.Resume(context.Background(), harness.StartRequest{
		InvocationID:    "inv-2",
		RunID:           "run-1",
		Role:            "implementation",
		Stage:           "implementation",
		WorkspaceID:     "workspace-run",
		Prompt:          "Continue the implementation.",
		ResumeSessionID: "not a uuid",
	}); err == nil || !strings.Contains(err.Error(), "session id") {
		t.Fatalf("Resume() error = %v, want an unsafe recorded-session refusal", err)
	}
	if len(workerRuntime.requests) != 0 {
		t.Fatalf("interactive worker requests = %#v, want no launch after an unsafe session id", workerRuntime.requests)
	}
}

// TestClaudeFinishLeavesTheSurfaceRecoverable verifies accepted completion
// exits Claude Code without destroying its cmux surface or worker state.
func TestClaudeFinishLeavesTheSurfaceRecoverable(t *testing.T) {
	terminalRuntime := &fakeTerminal{surface: claudeSurface()}
	runtime := harness.NewClaude(&fakeWorker{}, terminalRuntime)
	if err := runtime.Finish(context.Background(), harness.Session{Surface: terminalRuntime.surface}); err != nil {
		t.Fatalf("Finish() error = %v", err)
	}
	if terminalRuntime.closed != "" || string(terminalRuntime.inputs[0]) != "/exit\n" {
		t.Fatalf("finish operations = closed=%q inputs=%q, want graceful exit with recoverable surface", terminalRuntime.closed, terminalRuntime.inputs)
	}
}

// TestClaudeAssignsAUniqueDefaultSessionIdentifier verifies the built-in
// generator produces distinct RFC 4122 version-four identifiers.
func TestClaudeAssignsAUniqueDefaultSessionIdentifier(t *testing.T) {
	terminalRuntime := &fakeTerminal{surface: claudeSurface()}
	runtime := harness.NewClaude(&fakeWorker{}, terminalRuntime)
	seen := make(map[string]struct{}, 8)
	for attempt := 0; attempt < 8; attempt++ {
		session, err := runtime.Start(context.Background(), harness.StartRequest{
			InvocationID: "inv-1",
			RunID:        "run-1",
			Role:         "implementation",
			Stage:        "implementation",
			WorkspaceID:  "workspace-run",
			Surface:      terminalRuntime.surface,
			Prompt:       "Implement the frozen specification.",
		})
		if err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		if len(session.NativeSessionID) != 36 || session.NativeSessionID[14] != '4' {
			t.Fatalf("NativeSessionID = %q, want a version-four UUID", session.NativeSessionID)
		}
		if _, exists := seen[session.NativeSessionID]; exists {
			t.Fatalf("NativeSessionID %q was assigned twice", session.NativeSessionID)
		}
		seen[session.NativeSessionID] = struct{}{}
	}
}
