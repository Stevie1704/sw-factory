package worker_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/worker"
)

const interactiveWorkerDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestDockerRuntimeBuildsAnOpaqueInteractiveAttachCommand verifies that the
// terminal receives a host helper plus a run identity, never a Docker name.
func TestDockerRuntimeBuildsAnOpaqueInteractiveAttachCommand(t *testing.T) {
	runtime := &worker.DockerRuntime{AttachBinary: "factory-worker-attach"}
	command, err := runtime.InteractiveCommand(context.Background(), worker.InteractiveRequest{
		RunID:             "run-interactive",
		Command:           []string{"codex", "-s", "danger-full-access"},
		EnvironmentPolicy: worker.EnvironmentPolicyRole,
		Role:              "implementation",
		Environment:       map[string]string{"FACTORY_INVOCATION_ID": "inv-1"},
	})
	if err != nil {
		t.Fatalf("InteractiveCommand() error = %v", err)
	}
	if command.Executable != "factory-worker-attach" {
		t.Fatalf("interactive executable = %q, want host attach helper", command.Executable)
	}
	joined := strings.Join(command.Args, " ")
	if !strings.Contains(joined, "--run-id run-interactive") || !strings.Contains(joined, "--role implementation") || !strings.Contains(joined, "-- codex -s danger-full-access") || !strings.Contains(joined, "FACTORY_INVOCATION_ID=inv-1") {
		t.Fatalf("interactive args = %q, want run, environment, and command", joined)
	}
	for _, argument := range command.Args {
		if strings.HasPrefix(argument, "factory-worker-") && argument != "factory-worker-attach" {
			t.Fatalf("interactive command exposed a Docker container name: %q", joined)
		}
	}
}

// TestDockerRuntimeMountsInvocationAndResultDirectories verifies that the
// read-only packet and writable result directory are explicit worker mounts.
func TestDockerRuntimeMountsInvocationAndResultDirectories(t *testing.T) {
	stub, logPath, _ := writeDockerStub(t)
	worktree := makeDirectory(t, "worktree")
	gitMetadata := makeDirectory(t, "git-metadata")
	invocation := makeDirectory(t, "invocation")
	results := makeDirectory(t, "results")
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	if err := runtime.Start(context.Background(), worker.StartRequest{
		RunID:           "run-report-mounts",
		WorktreePath:    worktree,
		GitMetadataPath: gitMetadata,
		InvocationPath:  invocation,
		ResultPath:      results,
		Role:            "implementation",
		Image:           "ghcr.io/example/factory-worker",
		ImageDigest:     interactiveWorkerDigest,
	}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	lines := readStubLog(t, logPath)
	runLine := findLogLine(t, lines, " run ")
	if !strings.Contains(runLine, "dst=/invocation,readonly") || !strings.Contains(runLine, "dst=/results") {
		t.Fatalf("worker mounts = %q, want read-only invocation and writable results", runLine)
	}
	if !strings.Contains(runLine, "factory-role-implementation") || !strings.Contains(runLine, "dst="+worker.CredentialPath) {
		t.Fatalf("worker mounts = %q, want role session and separate credential volumes", runLine)
	}
}

// TestDockerRuntimeRecreatesAnExistingWorkerForInvocationMounts verifies that
// a gate-created worker is safely rebuilt before a visible invocation starts.
func TestDockerRuntimeRecreatesAnExistingWorkerForInvocationMounts(t *testing.T) {
	stub, logPath, _ := writeDockerStub(t)
	worktree := makeDirectory(t, "worktree")
	gitMetadata := makeDirectory(t, "git-metadata")
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	base := worker.StartRequest{
		RunID:           "run-reconfigure",
		WorktreePath:    worktree,
		GitMetadataPath: gitMetadata,
		Role:            "implementation",
		Image:           "ghcr.io/example/factory-worker",
		ImageDigest:     interactiveWorkerDigest,
	}
	if err := runtime.Start(context.Background(), base); err != nil {
		t.Fatalf("base Start() error = %v", err)
	}
	if err := runtime.Start(context.Background(), worker.StartRequest{
		RunID:           base.RunID,
		WorktreePath:    worktree,
		GitMetadataPath: gitMetadata,
		InvocationPath:  makeDirectory(t, "invocation"),
		ResultPath:      makeDirectory(t, "results"),
		Role:            base.Role,
		Image:           base.Image,
		ImageDigest:     base.ImageDigest,
	}); err != nil {
		t.Fatalf("visible Start() error = %v", err)
	}
	lines := readStubLog(t, logPath)
	if countLogLines(lines, " run ") != 2 || countLogLines(lines, " rm ") != 1 {
		t.Fatalf("Docker reconfiguration calls = %#v, want stop/rm and replacement run", lines)
	}
}

// TestDockerRuntimeSeedsOnlyTheCodexCredentialFile verifies that seeding reads
// one explicit host file and does not mount or copy the host harness directory.
func TestDockerRuntimeSeedsOnlyTheCodexCredentialFile(t *testing.T) {
	stub, logPath, _ := writeDockerStub(t)
	worktree := makeDirectory(t, "worktree")
	gitMetadata := makeDirectory(t, "git-metadata")
	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"access_token":"test-only"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	if err := runtime.Start(context.Background(), worker.StartRequest{
		RunID:           "run-auth",
		WorktreePath:    worktree,
		GitMetadataPath: gitMetadata,
		Role:            "implementation",
		Image:           "ghcr.io/example/factory-worker",
		ImageDigest:     interactiveWorkerDigest,
	}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := runtime.SeedCodexCredentials(context.Background(), worker.CredentialSeedRequest{RunID: "run-auth", AuthPath: authPath}); err != nil {
		t.Fatalf("SeedCodexCredentials() error = %v", err)
	}
	lines := readStubLog(t, logPath)
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, filepath.Dir(authPath)) || strings.Contains(joined, "docker.sock") || !strings.Contains(joined, worker.CredentialPath+"/auth.json") {
		t.Fatalf("credential seed leaked host harness path or Docker socket: %q", joined)
	}
}
