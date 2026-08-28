package factory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
