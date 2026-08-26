package harness_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// contractImage is the pinned worker image reference used by the stub adapter.
const contractImage = "ghcr.io/example/factory-worker@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// stubCodexSession is the native session the Codex stub persists at launch.
const stubCodexSession = "7c9e6679-7425-40de-944b-e07fc1f90ae7"

// TestHarnessAdaptersSatisfyTheSameContract drives both adapters through the
// complete lifecycle against controlled stub executables, before any live run.
// Each adapter builds its command through the real Docker worker adapter, its
// surface actually launches the stub attach executable, and the report the
// stub writes is validated against the invocation identity the adapter set.
func TestHarnessAdaptersSatisfyTheSameContract(t *testing.T) {
	for _, test := range []struct {
		name            string
		harness         string
		wantExecutable  string
		wantLaunchFlag  string
		wantResumeFlag  string
		wantAssignedID  bool
		wantResumeFirst bool
	}{
		{name: "codex", harness: harness.NameCodex, wantExecutable: "codex", wantLaunchFlag: "-s", wantResumeFlag: "resume"},
		{name: "claude", harness: harness.NameClaude, wantExecutable: "claude", wantLaunchFlag: "--session-id", wantResumeFlag: "--resume", wantAssignedID: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newHarnessFixture(t)
			runtime, err := harness.New(test.harness, fixture.worker, fixture.terminal)
			if err != nil {
				t.Fatalf("New(%q) error = %v", test.harness, err)
			}

			// Capability discovery.
			capabilities := runtime.Capabilities()
			if capabilities.Name != test.harness || !capabilities.NativeResume {
				t.Fatalf("Capabilities() = %#v, want %q with native resume", capabilities, test.harness)
			}

			// Launch.
			request := harness.StartRequest{
				InvocationID: "inv-contract-1",
				RunID:        "run-contract",
				Role:         "implementation",
				Stage:        "implementation",
				WorkspaceID:  fixture.terminal.surface.WorkspaceID,
				Surface:      fixture.terminal.surface,
				Prompt:       "Implement the frozen specification.",
			}
			session, err := runtime.Start(context.Background(), request)
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			launched := fixture.launchedArguments(t)
			command := harnessCommand(launched)
			if len(command) == 0 || command[0] != test.wantExecutable {
				t.Fatalf("launched harness command = %#v, want %q", command, test.wantExecutable)
			}
			if !containsArgument(command, test.wantLaunchFlag) {
				t.Fatalf("launched harness command = %#v, want the %q launch flag", command, test.wantLaunchFlag)
			}
			if command[len(command)-1] != request.Prompt {
				t.Fatalf("last harness argument = %q, want the immutable role prompt", command[len(command)-1])
			}
			if containsArgument(launched, "FACTORY_HARNESS="+test.harness) == false {
				t.Fatalf("attach arguments = %#v, want the harness identity in the invocation environment", launched)
			}

			// Native session identification.
			if session.NativeSessionID == "" {
				t.Fatal("NativeSessionID is empty, want a recoverable native session")
			}
			if test.wantAssignedID && !containsArgument(command, session.NativeSessionID) {
				t.Fatalf("harness command = %#v, want the assigned session %q", command, session.NativeSessionID)
			}
			if !test.wantAssignedID && session.NativeSessionID != stubCodexSession {
				t.Fatalf("NativeSessionID = %q, want the session the stub persisted", session.NativeSessionID)
			}

			// Result validation against the identity this adapter established.
			value, err := report.Read(filepath.Join(fixture.resultDirectory, report.ReportFileName))
			if err != nil {
				t.Fatalf("report.Read() error = %v", err)
			}
			validation := report.ValidationContext{
				InvocationID:   request.InvocationID,
				RunID:          request.RunID,
				Harness:        test.harness,
				Role:           request.Role,
				Stage:          request.Stage,
				WorktreePath:   fixture.worktree,
				PermittedPaths: []string{"."},
			}
			if err := report.Validate(value, validation); err != nil {
				t.Fatalf("report.Validate() error = %v, want the stub report accepted", err)
			}
			foreign := validation
			foreign.Harness = "other-harness"
			if err := report.Validate(value, foreign); err == nil {
				t.Fatal("report.Validate() accepted a report from a different harness")
			}

			// Resume.
			resumeRequest := request
			resumeRequest.InvocationID = "inv-contract-2"
			resumeRequest.ResumeSessionID = session.NativeSessionID
			resumed, err := runtime.Resume(context.Background(), resumeRequest)
			if err != nil {
				t.Fatalf("Resume() error = %v", err)
			}
			if resumed.NativeSessionID != session.NativeSessionID {
				t.Fatalf("resumed NativeSessionID = %q, want the continued session %q", resumed.NativeSessionID, session.NativeSessionID)
			}
			resumeCommand := harnessCommand(fixture.launchedArguments(t))
			if !containsArgument(resumeCommand, test.wantResumeFlag) || !containsArgument(resumeCommand, session.NativeSessionID) {
				t.Fatalf("resume command = %#v, want %q with the recorded session", resumeCommand, test.wantResumeFlag)
			}

			// Graceful stop.
			if err := runtime.Finish(context.Background(), resumed); err != nil {
				t.Fatalf("Finish() error = %v", err)
			}
			if fixture.terminal.closed != "" {
				t.Fatalf("closed surface = %q, want the surface left recoverable", fixture.terminal.closed)
			}
			if len(fixture.terminal.inputs) != 1 || string(fixture.terminal.inputs[0]) != "/exit\n" {
				t.Fatalf("surface inputs = %q, want one graceful exit request", fixture.terminal.inputs)
			}
		})
	}
}

// TestNewRefusesAnUnknownHarness verifies an unconfigured harness name cannot
// silently fall back to another adapter.
func TestNewRefusesAnUnknownHarness(t *testing.T) {
	if _, err := harness.New("gemini", &fakeWorker{}, &fakeTerminal{}); err == nil || !strings.Contains(err.Error(), "gemini") {
		t.Fatalf("New() error = %v, want an unknown-harness refusal", err)
	}
}

// harnessFixture wires the real Docker worker adapter and a launching terminal
// onto controlled stub executables.
type harnessFixture struct {
	worker          *worker.DockerRuntime
	terminal        *launchingTerminal
	resultDirectory string
	worktree        string
	attachLog       string
}

// newHarnessFixture writes the stub docker and attach executables and returns
// the seams an adapter needs.
func newHarnessFixture(t *testing.T) *harnessFixture {
	t.Helper()
	root := t.TempDir()
	fixture := &harnessFixture{
		resultDirectory: filepath.Join(root, "results"),
		worktree:        filepath.Join(root, "worktree"),
		attachLog:       filepath.Join(root, "attach.log"),
	}
	sessionFile := filepath.Join(root, "session")
	for _, directory := range []string{fixture.resultDirectory, fixture.worktree} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	dockerStub := writeStub(t, filepath.Join(root, "docker-stub"), `#!/bin/sh
set -eu
case "${1:-}" in
  container)
    printf 'true\t%s\t\n' "`+contractImage+`"
    ;;
  exec)
    if [ -f "$HARNESS_SESSION_FILE" ]; then cat "$HARNESS_SESSION_FILE"; fi
    ;;
esac
`)
	attachStub := writeStub(t, filepath.Join(root, "attach-stub"), `#!/bin/sh
set -eu
: > "$HARNESS_ATTACH_LOG"
run_id=""
role=""
harness_name=""
invocation=""
stage=""
previous=""
separator=0
for argument in "$@"; do
  printf '%s\n' "$argument" >> "$HARNESS_ATTACH_LOG"
  if [ "$separator" = "1" ] && [ "$argument" = "codex" ]; then
    printf '%s\n' "$HARNESS_CODEX_SESSION" > "$HARNESS_SESSION_FILE"
  fi
  case "$previous" in
    --run-id) run_id="$argument" ;;
    --role) role="$argument" ;;
    --env)
      name="${argument%%=*}"
      value="${argument#*=}"
      case "$name" in
        FACTORY_HARNESS) harness_name="$value" ;;
        FACTORY_INVOCATION_ID) invocation="$value" ;;
        FACTORY_STAGE) stage="$value" ;;
      esac
      ;;
  esac
  if [ "$argument" = "--" ]; then separator=1; fi
  previous="$argument"
done
cat > "$HARNESS_RESULT_DIR/report.json" <<JSON
{
  "schema_version": 1,
  "invocation_id": "$invocation",
  "run_id": "$run_id",
  "harness": "$harness_name",
  "role": "$role",
  "stage": "$stage",
  "reported_at": "2026-01-01T00:00:00Z",
  "outcome": "cannot_proceed",
  "summary": "stub harness reported no progress",
  "evidence": [{"kind": "command", "detail": "stub executable produced no change"}]
}
JSON
`)
	// The stub executables read their fixture paths from the process
	// environment, exactly as the repository's other stub-driven adapters do.
	t.Setenv("HARNESS_ATTACH_LOG", fixture.attachLog)
	t.Setenv("HARNESS_RESULT_DIR", fixture.resultDirectory)
	t.Setenv("HARNESS_SESSION_FILE", sessionFile)
	t.Setenv("HARNESS_CODEX_SESSION", stubCodexSession)
	fixture.worker = &worker.DockerRuntime{DockerBinary: dockerStub, AttachBinary: attachStub}
	fixture.terminal = &launchingTerminal{
		surface: terminal.Surface{ID: "surface-implementation", WorkspaceID: "workspace-run", Name: "implementation"},
	}
	return fixture
}

// launchedArguments returns the arguments the stub attach executable received
// for the most recent launch.
func (f *harnessFixture) launchedArguments(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(f.attachLog)
	if err != nil {
		t.Fatalf("read stub attach log: %v", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// harnessCommand returns the harness argv that follows the attach separator.
func harnessCommand(arguments []string) []string {
	for index, argument := range arguments {
		if argument == "--" {
			return arguments[index+1:]
		}
	}
	return nil
}

// containsArgument reports whether an exact argument was passed.
func containsArgument(arguments []string, wanted string) bool {
	for _, argument := range arguments {
		if argument == wanted {
			return true
		}
	}
	return false
}

// writeStub writes one executable stub script and returns its path.
func writeStub(t *testing.T, path, script string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// launchingTerminal runs the adapter's attach command as a real process so the
// contract test observes the exact argv a live surface would receive.
type launchingTerminal struct {
	surface terminal.Surface
	inputs  [][]byte
	closed  string
}

// EnsureControlWorkspace implements TerminalRuntime for the contract test.
func (*launchingTerminal) EnsureControlWorkspace(context.Context, terminal.WorkspaceRequest) (terminal.Workspace, error) {
	return terminal.Workspace{ID: "workspace-control"}, nil
}

// EnsureRunWorkspace implements TerminalRuntime for the contract test.
func (*launchingTerminal) EnsureRunWorkspace(context.Context, terminal.RunWorkspaceRequest) (terminal.RunWorkspace, error) {
	return terminal.RunWorkspace{}, nil
}

// CreateSurface runs the surface command in a new role-named surface.
func (t *launchingTerminal) CreateSurface(ctx context.Context, request terminal.SurfaceRequest) (terminal.Surface, error) {
	if err := t.run(ctx, request.Command); err != nil {
		return terminal.Surface{}, err
	}
	return t.surface, nil
}

// LaunchSurface runs the surface command in the existing role surface.
func (t *launchingTerminal) LaunchSurface(ctx context.Context, _ terminal.SurfaceID, command terminal.Command) error {
	return t.run(ctx, command)
}

// run executes one surface command against the controlled stub executable.
func (t *launchingTerminal) run(ctx context.Context, command terminal.Command) error {
	return exec.CommandContext(ctx, command.Executable, command.Args...).Run()
}

// SendInput records exact bytes sent to the surface.
func (t *launchingTerminal) SendInput(_ context.Context, _ terminal.SurfaceID, input []byte) error {
	t.inputs = append(t.inputs, input)
	return nil
}

// Notify implements TerminalRuntime for the contract test.
func (*launchingTerminal) Notify(context.Context, terminal.Notification) error { return nil }

// CloseSurface records the detached surface.
func (t *launchingTerminal) CloseSurface(_ context.Context, surfaceID terminal.SurfaceID) error {
	t.closed = string(surfaceID)
	return nil
}

// CloseWorkspace implements TerminalRuntime for the contract test.
func (*launchingTerminal) CloseWorkspace(context.Context, terminal.WorkspaceID) error { return nil }

var _ terminal.TerminalRuntime = (*launchingTerminal)(nil)
