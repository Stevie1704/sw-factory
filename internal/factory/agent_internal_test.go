package factory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/terminal"

	"github.com/Stevie1704/sw-factory/internal/prompt"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// TestPromptForPersistedInvocationAcceptsSupportedSchemaVersions verifies
// recovery can rebuild prompts from the current and historical append-only
// packet shapes.
func TestPromptForPersistedInvocationAcceptsSupportedSchemaVersions(t *testing.T) {
	for _, test := range []struct {
		name       string
		packet     string
		want       string
		wantAbsent string
	}{
		{
			name: "version one",
			packet: `{
  "schema_version": 1,
  "invocation_id": "inv-prompt",
  "run_id": "run-prompt",
  "role": "implementation",
  "stage": "implementation",
  "specification_packet": "{}",
  "prompt_version": "implementation-v1",
  "permitted_paths": ["."]
}`,
			want:       "factory prompt version implementation-v1",
			wantAbsent: "Check-repair context:",
		},
		{
			name: "version two",
			packet: `{
  "schema_version": 2,
  "invocation_id": "inv-prompt",
  "run_id": "run-prompt",
  "role": "implementation",
  "stage": "implementation",
  "specification_packet": "{}",
  "prompt_version": "implementation-v1",
  "permitted_paths": ["."],
  "check_repair": {
    "version": 1,
    "run_id": "run-prompt",
    "checkpoint_sha": "checkpoint",
    "attempt": 2,
    "budget": 3,
    "setup": {"exit_code": 0},
    "gates": []
  }
}`,
			want:       "This is repair attempt 2 of 3",
			wantAbsent: "Protected test-stage handoff (coordinator-owned):",
		},
		{
			name: "version three",
			packet: `{
  "schema_version": 3,
  "invocation_id": "inv-prompt",
  "run_id": "run-prompt",
  "role": "implementation",
  "stage": "implementation",
  "specification_packet": "{}",
  "prompt_version": "implementation-v1",
  "permitted_paths": ["."],
  "test_handoff": {
    "acceptance_coverage": [{"criterion": "criterion", "evidence": "red test"}],
    "changed_files": ["internal/example_test.go"],
    "focused_test_command": "go test ./internal/example",
    "expected_failure_reason": "expected assertion",
    "observed_failure_evidence": [{"kind": "exit_code", "detail": "1"}]
  },
  "protected_test_paths": [{"path": "internal/example_test.go", "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]
}`,
			want:       "Protected test-stage handoff (coordinator-owned):",
			wantAbsent: "Check-repair context:",
		},
		{
			name: "version four",
			packet: `{
  "schema_version": 4,
  "invocation_id": "inv-prompt",
  "run_id": "run-prompt",
  "role": "implementation",
  "stage": "implementation",
  "specification_packet": "{}",
  "prompt_version": "implementation-v1",
  "permitted_paths": ["."]
}`,
			want: "factory prompt version implementation-v1",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			run := store.Run{ID: "run-prompt", SpecificationPacket: "{}"}
			invocation := store.Invocation{
				ID: "inv-prompt", RunID: run.ID, Role: "implementation", Stage: store.StageImplementation,
				InvocationDirectory: directory, PromptVersion: "implementation-v1",
			}
			if err := os.WriteFile(filepath.Join(directory, invocationPacketFileName), []byte(test.packet), 0o600); err != nil {
				t.Fatal(err)
			}
			promptText, err := promptForPersistedInvocation(run, invocation, SpecificationPacket{})
			if err != nil {
				t.Fatalf("promptForPersistedInvocation() error = %v, want supported historical packet", err)
			}
			if !strings.Contains(promptText, test.want) {
				t.Fatalf("promptForPersistedInvocation() prompt does not contain %q:\n%s", test.want, promptText)
			}
			if test.wantAbsent != "" && strings.Contains(promptText, test.wantAbsent) {
				t.Fatalf("promptForPersistedInvocation() prompt contains unexpected %q:\n%s", test.wantAbsent, promptText)
			}
		})
	}
}

// TestPromptForPersistedInvocationRejectsRepositoryCraftDigestMismatch verifies
// restart recovery refuses to rebuild a prompt when packet content no longer
// matches its recorded frozen identity.
func TestPromptForPersistedInvocationRejectsRepositoryCraftDigestMismatch(t *testing.T) {
	const sourcePath = "docs/factory/craft/implementation.md"
	packet := SpecificationPacket{
		RepositoryConfig: config.RepositoryConfig{RoleCraft: map[string]string{"implementation": sourcePath}},
		RepositoryCraft: map[string]RepositoryCraftDocument{
			"implementation": {Path: sourcePath, Content: "frozen craft bytes", SHA256: strings.Repeat("a", 64)},
		},
	}
	run := store.Run{ID: "run-craft-mismatch", SpecificationPacket: "{}"}
	invocation := store.Invocation{
		ID: "inv-craft-mismatch", RunID: run.ID, Role: workflow.RoleImplementation, Stage: store.StageImplementation,
		InvocationDirectory: t.TempDir(), PromptVersion: "implementation-v1",
		PromptCraftSourcePath: sourcePath, PromptCraftSHA256: strings.Repeat("a", 64),
	}
	if err := writeInvocationPacket(invocation.InvocationDirectory, InvocationPacket{
		SchemaVersion: invocationPacketVersion, InvocationID: invocation.ID, RunID: run.ID,
		Role: invocation.Role, Stage: invocation.Stage, SpecificationPacket: run.SpecificationPacket,
		PromptVersion: invocation.PromptVersion, PromptCraftSourcePath: sourcePath, PromptCraftSHA256: strings.Repeat("a", 64),
	}); err != nil {
		t.Fatal(err)
	}
	_, err := promptForPersistedInvocation(run, invocation, packet)
	var digestErr *CraftDigestMismatchError
	if !errors.As(err, &digestErr) {
		t.Fatalf("promptForPersistedInvocation() error = %v, want CraftDigestMismatchError", err)
	}
	if digestErr.Role != workflow.RoleImplementation || digestErr.SourcePath != sourcePath {
		t.Fatalf("digest error = %#v, want implementation/source identity", digestErr)
	}
}

// TestPromptForPersistedInvocationUsesFrozenRepositoryCraft verifies restart
// prompt construction uses packet bytes and keeps embedded authority intact.
func TestPromptForPersistedInvocationUsesFrozenRepositoryCraft(t *testing.T) {
	const sourcePath = "docs/factory/craft/implementation.md"
	content := "Frozen repository implementation recipe."
	packet := SpecificationPacket{
		RepositoryConfig: config.RepositoryConfig{RoleCraft: map[string]string{"implementation": sourcePath}},
		RepositoryCraft: map[string]RepositoryCraftDocument{
			"implementation": {Path: sourcePath, Content: content, SHA256: digestRepositoryCraft(content)},
		},
	}
	run := store.Run{ID: "run-craft-restart", SpecificationPacket: "{}"}
	invocation := store.Invocation{
		ID: "inv-craft-restart", RunID: run.ID, Role: workflow.RoleImplementation, Stage: store.StageImplementation,
		InvocationDirectory: t.TempDir(), PromptVersion: prompt.Version,
		PromptCraftSourcePath: sourcePath, PromptCraftSHA256: digestRepositoryCraft(content),
	}
	if err := writeInvocationPacket(invocation.InvocationDirectory, InvocationPacket{
		SchemaVersion: invocationPacketVersion, InvocationID: invocation.ID, RunID: run.ID,
		Role: invocation.Role, Stage: invocation.Stage, SpecificationPacket: run.SpecificationPacket,
		PromptVersion: invocation.PromptVersion, PromptCraftSourcePath: sourcePath, PromptCraftSHA256: invocation.PromptCraftSHA256,
	}); err != nil {
		t.Fatal(err)
	}
	promptText, err := promptForPersistedInvocation(run, invocation, packet)
	if err != nil {
		t.Fatalf("promptForPersistedInvocation() error = %v", err)
	}
	if !strings.Contains(promptText, content) || strings.Contains(promptText, "Work in vertical slices: implement one behavior") {
		t.Fatalf("restart prompt did not use frozen repository craft:\n%s", promptText)
	}
	if !strings.Contains(promptText, "Factory-owned precedence:") {
		t.Fatalf("restart prompt lost embedded authority:\n%s", promptText)
	}
}

// TestPromptForPersistedInvocationRejectsUnsupportedSchemaVersion verifies
// recovery refuses missing and future packet versions without building a
// degraded role prompt.
func TestPromptForPersistedInvocationRejectsUnsupportedSchemaVersion(t *testing.T) {
	for _, test := range []struct {
		name    string
		version int
	}{
		{name: "missing", version: 0},
		{name: "future", version: invocationPacketVersion + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			run := store.Run{ID: "run-prompt", SpecificationPacket: "{}"}
			invocation := store.Invocation{
				ID: "inv-prompt", RunID: run.ID, Role: "implementation", Stage: store.StageImplementation,
				InvocationDirectory: directory,
			}
			if err := writeInvocationPacket(directory, InvocationPacket{
				SchemaVersion: test.version,
				InvocationID:  invocation.ID,
				RunID:         run.ID,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := promptForPersistedInvocation(run, invocation, SpecificationPacket{}); err == nil || !strings.Contains(err.Error(), "unsupported persisted invocation packet schema version") {
				t.Fatalf("promptForPersistedInvocation() error = %v, want schema-version refusal", err)
			}
		})
	}
}

// TestPromptForPersistedInvocationRejectsInvalidReviewContext verifies recovery
// cannot resume a reviewer without a context tied to the run's checkpoint.
func TestPromptForPersistedInvocationRejectsInvalidReviewContext(t *testing.T) {
	for _, test := range []struct {
		name    string
		context *prompt.ReviewContext
		want    string
	}{
		{name: "missing context", want: "has no review context"},
		{name: "empty checkpoint", context: &prompt.ReviewContext{}, want: "has no review checkpoint"},
		{name: "mismatched checkpoint", context: &prompt.ReviewContext{CheckpointSHA: "different-checkpoint"}, want: "review checkpoint does not match the run"},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			run := store.Run{ID: "run-review", CheckpointSHA: "review-checkpoint", SpecificationPacket: "{}"}
			invocation := store.Invocation{
				ID: "inv-review", RunID: run.ID, Role: workflow.RoleSpecificationReview, Stage: store.StageReview,
				InvocationDirectory: directory, PromptVersion: workflow.PromptVersionSpecificationReview,
			}
			if err := writeInvocationPacket(directory, InvocationPacket{
				SchemaVersion:       invocationPacketVersion,
				InvocationID:        invocation.ID,
				RunID:               run.ID,
				Role:                invocation.Role,
				Stage:               invocation.Stage,
				SpecificationPacket: run.SpecificationPacket,
				PromptVersion:       invocation.PromptVersion,
				PermittedPaths:      []string{"."},
				ReviewContext:       test.context,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := promptForPersistedInvocation(run, invocation, SpecificationPacket{}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("promptForPersistedInvocation() error = %v, want %q refusal", err, test.want)
			}
		})
	}
}

// TestValidatePersistedInvocationPacketRequiresReviewRepairContext verifies
// recovery cannot resume implementation while silently dropping a durable
// blocking-review packet.
func TestValidatePersistedInvocationPacketRequiresReviewRepairContext(t *testing.T) {
	packet := &store.ReviewRepairPacket{RunID: "run-repair", CheckpointSHA: "checkpoint", Attempt: 1, Budget: 2}
	run := store.Run{ID: "run-repair", SpecificationPacket: "specification", ReviewRepairPacket: packet}
	invocation := store.Invocation{ID: "inv-repair", RunID: run.ID, Role: workflow.RoleImplementation, Stage: store.StageImplementation, PromptVersion: "implementation-v1"}
	persisted := InvocationPacket{
		SchemaVersion:       invocationPacketVersion,
		InvocationID:        invocation.ID,
		RunID:               run.ID,
		Role:                invocation.Role,
		Stage:               invocation.Stage,
		SpecificationPacket: run.SpecificationPacket,
		PromptVersion:       invocation.PromptVersion,
	}
	if err := validatePersistedInvocationPacket(run, invocation, persisted); err == nil || !strings.Contains(err.Error(), "review-repair context") {
		t.Fatalf("validatePersistedInvocationPacket() error = %v, want missing review-repair context refusal", err)
	}
}

// TestWriteHarnessFailureDiagnosticRecordsTheTranscript verifies that a failed
// launch leaves a readable local diagnostic beside its invocation, since the
// surface is closed and the worker stopped immediately afterwards.
func TestWriteHarnessFailureDiagnosticRecordsTheTranscript(t *testing.T) {
	root := t.TempDir()
	observed := time.Date(2026, 8, 31, 8, 32, 55, 0, time.UTC)
	cause := errors.New("discover Codex native session: deadline exceeded")
	launchErr := &harness.LaunchFailure{Cause: cause, Transcript: "codex: stream error: 429 Too Many Requests"}

	path := writeHarnessFailureDiagnostic(root, "launch", launchErr, harness.LaunchTranscript(launchErr), observed)
	if path != filepath.Join(root, harnessFailureDiagnosticName) {
		t.Fatalf("writeHarnessFailureDiagnostic() = %q, want the invocation-local diagnostic path", path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v, want the diagnostic written", err)
	}
	for _, want := range []string{"2026-08-31T08:32:55Z", "occasion: launch", cause.Error(), "codex: stream error: 429 Too Many Requests"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("diagnostic = %q, want it to contain %q", string(body), want)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	// The transcript holds harness output, so it stays owner-readable only.
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("diagnostic mode = %v, want 0600", info.Mode().Perm())
	}
}

// TestWriteHarnessFailureDiagnosticNamesTheOccasion verifies that a session
// that exited after a successful launch is distinguishable from a launch that
// never started. Claude Code assigns its own session identifier, so its failed
// launches only ever reach this occasion.
func TestWriteHarnessFailureDiagnosticNamesTheOccasion(t *testing.T) {
	root := t.TempDir()
	cause := errors.New("claude native session \"session-1\" exited before reporting")

	path := writeHarnessFailureDiagnostic(root, "session exit", cause, "claude: invalid API key", time.Now().UTC())
	if path == "" {
		t.Fatal("writeHarnessFailureDiagnostic() = \"\", want a diagnostic path")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	for _, want := range []string{"occasion: session exit", "exited before reporting", "claude: invalid API key"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("diagnostic = %q, want it to contain %q", string(body), want)
		}
	}
}

// TestWriteHarnessFailureDiagnosticSkipsAnEmptyTranscript verifies that a
// failure carrying no captured output writes no file and reports no path, so
// nothing points an operator at a diagnostic that does not exist.
func TestWriteHarnessFailureDiagnosticSkipsAnEmptyTranscript(t *testing.T) {
	root := t.TempDir()
	launchErr := &harness.LaunchFailure{Cause: errors.New("discover Codex native session: deadline exceeded")}

	if path := writeHarnessFailureDiagnostic(root, "launch", launchErr, harness.LaunchTranscript(launchErr), time.Now().UTC()); path != "" {
		t.Fatalf("writeHarnessFailureDiagnostic() = %q, want no path without a transcript", path)
	}
	if _, err := os.Stat(filepath.Join(root, harnessFailureDiagnosticName)); !os.IsNotExist(err) {
		t.Fatalf("Stat() error = %v, want no diagnostic file", err)
	}
}

// readableRecoveryTerminal adds the optional surface-read capability to the
// recovery terminal fake.
type readableRecoveryTerminal struct {
	*journalRecoveryTerminal
	screen  string
	readErr error
	lines   int
}

// ReadSurface returns the fake surface output and records the requested bound.
func (t *readableRecoveryTerminal) ReadSurface(_ context.Context, _ terminal.SurfaceID, lines int) (string, error) {
	t.lines = lines
	if t.readErr != nil {
		return "", t.readErr
	}
	return t.screen, nil
}

// TestCaptureSurfaceTranscriptReadsABoundedSurface verifies the coordinator
// captures a bounded transcript when the terminal adapter can read surfaces.
func TestCaptureSurfaceTranscriptReadsABoundedSurface(t *testing.T) {
	runtime := &readableRecoveryTerminal{journalRecoveryTerminal: &journalRecoveryTerminal{}, screen: "  claude: invalid API key  "}

	if got := captureSurfaceTranscript(context.Background(), runtime, "surface-1"); got != "claude: invalid API key" {
		t.Fatalf("captureSurfaceTranscript() = %q, want the trimmed surface output", got)
	}
	if runtime.lines != harnessTranscriptLines {
		t.Fatalf("requested lines = %d, want the bounded transcript length %d", runtime.lines, harnessTranscriptLines)
	}
}

// TestCaptureSurfaceTranscriptDegradesQuietly verifies that a capture failure
// never propagates. The caller is already reporting a harness failure, and a
// missing diagnostic must not replace it.
func TestCaptureSurfaceTranscriptDegradesQuietly(t *testing.T) {
	cases := map[string]struct {
		runtime   terminal.TerminalRuntime
		surfaceID terminal.SurfaceID
	}{
		"adapter cannot read surfaces": {runtime: &journalRecoveryTerminal{}, surfaceID: "surface-1"},
		"invocation has no surface":    {runtime: &readableRecoveryTerminal{journalRecoveryTerminal: &journalRecoveryTerminal{}, screen: "output"}, surfaceID: ""},
		"read fails": {runtime: &readableRecoveryTerminal{
			journalRecoveryTerminal: &journalRecoveryTerminal{}, readErr: errors.New("surface is gone"),
		}, surfaceID: "surface-1"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			if got := captureSurfaceTranscript(context.Background(), test.runtime, test.surfaceID); got != "" {
				t.Fatalf("captureSurfaceTranscript() = %q, want empty", got)
			}
		})
	}
}
