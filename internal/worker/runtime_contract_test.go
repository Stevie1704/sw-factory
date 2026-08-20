package worker_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/worker"
)

const testWorkerDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestDockerRuntimeRunsAWorkerThroughThePublicRuntimeSeam verifies that the
// Docker adapter translates one run into a hardened worker without exposing
// Docker identifiers or host paths to command execution.
func TestDockerRuntimeRunsAWorkerThroughThePublicRuntimeSeam(t *testing.T) {
	stub, logPath, _ := writeDockerStub(t)
	worktreePath := filepath.Join(t.TempDir(), "worktree")
	gitMetadataPath := filepath.Join(t.TempDir(), "git-metadata")
	cachePath := filepath.Join(t.TempDir(), "go-cache")
	for _, path := range []string{worktreePath, gitMetadataPath, cachePath} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{
		filepath.Join(worktreePath, "existing.txt"),
		filepath.Join(gitMetadataPath, "HEAD"),
		filepath.Join(cachePath, "existing.cache"),
	} {
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	runtime := &worker.DockerRuntime{DockerBinary: stub}
	request := worker.StartRequest{
		RunID:           "run-contract-1",
		WorktreePath:    worktreePath,
		GitMetadataPath: gitMetadataPath,
		Image:           "ghcr.io/example/factory-worker",
		ImageDigest:     testWorkerDigest,
		Caches: []worker.CacheMount{{
			Name:     "go-build",
			HostPath: cachePath,
			ReadOnly: false,
		}},
	}
	if err := runtime.Start(context.Background(), request); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	assertMode(t, worktreePath, 0o777)
	assertMode(t, filepath.Join(worktreePath, "existing.txt"), 0o666)
	assertMode(t, gitMetadataPath, 0o755)
	assertMode(t, filepath.Join(gitMetadataPath, "HEAD"), 0o644)
	assertMode(t, cachePath, 0o777)
	assertMode(t, filepath.Join(cachePath, "existing.cache"), 0o666)

	result, err := runtime.RunCommand(context.Background(), worker.CommandRequest{
		RunID:             request.RunID,
		Command:           "printf gate-ok",
		EnvironmentPolicy: worker.EnvironmentPolicyClean,
		Role:              "gate",
		Environment:       map[string]string{"FACTORY_CHECKPOINT_SHA": "0123456789abcdef"},
	})
	if err != nil {
		t.Fatalf("RunCommand() error = %v", err)
	}
	if result.ExitCode != 0 || result.Stdout != "command-ok\n" {
		t.Fatalf("RunCommand() result = %#v, want successful command result", result)
	}
	if _, err := runtime.RunCommand(context.Background(), worker.CommandRequest{
		RunID:             request.RunID,
		Command:           "printf role-ok",
		EnvironmentPolicy: worker.EnvironmentPolicyRole,
		Role:              "gate",
	}); err != nil {
		t.Fatalf("RunCommand() with role policy error = %v", err)
	}

	inspection, err := runtime.Inspect(context.Background(), request.RunID)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if !inspection.Exists || !inspection.Running {
		t.Fatalf("Inspect() = %#v, want a running worker", inspection)
	}
	if err := runtime.Stop(context.Background(), request.RunID); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	inspection, err = runtime.Inspect(context.Background(), request.RunID)
	if err != nil {
		t.Fatalf("Inspect() after Stop() error = %v", err)
	}
	if !inspection.Exists || inspection.Running {
		t.Fatalf("Inspect() after Stop() = %#v, want an existing stopped worker", inspection)
	}

	lines := readStubLog(t, logPath)
	runLine := findLogLine(t, lines, " run ")
	assertContainsAll(t, runLine,
		"--user 10001:10001",
		"--cap-drop ALL",
		"--security-opt no-new-privileges",
		"--network bridge",
		"dst=/work",
		"dst=/git,readonly",
		"dst=/cache/go-build",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_VALUE_0=disabled://factory",
		"ghcr.io/example/factory-worker@"+testWorkerDigest,
	)
	if strings.Contains(runLine, "docker.sock") {
		t.Fatalf("worker start mounted the Docker socket: %q", runLine)
	}
	execLines := findLogLines(lines, " exec ")
	if len(execLines) != 2 {
		t.Fatalf("Docker exec calls = %d, want clean and role calls; calls = %#v", len(execLines), lines)
	}
	execLine := execLines[0]
	assertContainsAll(t, execLine, "/work", "/git", "FACTORY_CHECKPOINT_SHA=0123456789abcdef", "/bin/sh -c")
	if strings.Contains(execLine, "FACTORY_ROLE=gate") {
		t.Fatalf("clean environment included role identity: %q", execLine)
	}
	if !strings.Contains(execLines[1], "FACTORY_ROLE=gate") {
		t.Fatalf("role environment omitted role identity: %q", execLines[1])
	}
	if strings.Contains(execLine, " -lc ") {
		t.Fatalf("worker command used a login shell: %q", execLine)
	}
	if strings.Contains(execLine, worktreePath) || strings.Contains(execLine, gitMetadataPath) {
		t.Fatalf("host path leaked into worker command: %q", execLine)
	}
}

// TestDockerRuntimeResumesThePinnedWorker verifies that resume starts the
// existing run container rather than creating a new worker or changing its
// image selection.
func TestDockerRuntimeResumesThePinnedWorker(t *testing.T) {
	stub, logPath, _ := writeDockerStub(t)
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	request := worker.StartRequest{
		RunID:           "run-contract-resume",
		WorktreePath:    makeDirectory(t, "worktree"),
		GitMetadataPath: makeDirectory(t, "git-metadata"),
		Image:           "ghcr.io/example/factory-worker",
		ImageDigest:     testWorkerDigest,
	}
	if err := runtime.Start(context.Background(), request); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := runtime.Stop(context.Background(), request.RunID); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := runtime.Resume(context.Background(), worker.ResumeRequest{
		RunID:       request.RunID,
		Image:       request.Image,
		ImageDigest: request.ImageDigest,
	}); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}

	lines := readStubLog(t, logPath)
	if countLogLines(lines, " run ") != 1 {
		t.Fatalf("Docker run calls = %d, want one; calls = %#v", countLogLines(lines, " run "), lines)
	}
	startLine := findLogLine(t, lines, " start ")
	assertContainsAll(t, startLine, "factory-worker-")
	inspection, err := runtime.Inspect(context.Background(), request.RunID)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if !inspection.Running {
		t.Fatalf("resumed worker inspection = %#v, want running", inspection)
	}
}

// TestDockerRuntimeRejectsMutableImagesAndCredentialEnvironment verifies the
// worker cannot start from a mutable image or execute with a host credential.
func TestDockerRuntimeRejectsMutableImagesAndCredentialEnvironment(t *testing.T) {
	stub, logPath, _ := writeDockerStub(t)
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	request := worker.StartRequest{
		RunID:           "run-contract-security",
		WorktreePath:    makeDirectory(t, "worktree"),
		GitMetadataPath: makeDirectory(t, "git-metadata"),
		Image:           "ghcr.io/example/factory-worker:latest",
		ImageDigest:     "sha256:not-a-digest",
	}
	if err := runtime.Start(context.Background(), request); err == nil {
		t.Fatal("Start() accepted a malformed image digest")
	}
	request.ImageDigest = testWorkerDigest
	if err := runtime.Start(context.Background(), request); err == nil {
		t.Fatal("Start() accepted a mutable image tag with a valid digest")
	}
	if _, err := runtime.RunCommand(context.Background(), worker.CommandRequest{
		RunID:             request.RunID,
		Command:           "printf nope",
		EnvironmentPolicy: worker.EnvironmentPolicyClean,
		Role:              "gate",
		Environment:       map[string]string{"GH_TOKEN": "secret-value"},
	}); err == nil {
		t.Fatal("RunCommand() accepted a GitHub credential")
	}
	if lines := readStubLog(t, logPath); len(lines) != 0 {
		t.Fatalf("Docker was invoked after rejecting unsafe input: %#v", lines)
	}
}

// TestDockerRuntimeReturnsACommandExitResult verifies a deterministic command
// failure remains a command result and is not confused with runtime failure.
func TestDockerRuntimeReturnsACommandExitResult(t *testing.T) {
	stub, _, _ := writeDockerStub(t)
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	request := worker.StartRequest{
		RunID:           "run-contract-exit",
		WorktreePath:    makeDirectory(t, "worktree"),
		GitMetadataPath: makeDirectory(t, "git-metadata"),
		Image:           "ghcr.io/example/factory-worker",
		ImageDigest:     testWorkerDigest,
	}
	if err := runtime.Start(context.Background(), request); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	for _, test := range []struct {
		command  string
		exitCode int
		stderr   string
	}{
		{command: "fail-command", exitCode: 7, stderr: "command-failed\n"},
		{command: "fail-126", exitCode: 126, stderr: "command-failed-126\n"},
		{command: "fail-127", exitCode: 127, stderr: "command-failed-127\n"},
		{command: "fail-137", exitCode: 137, stderr: "command-failed-137\n"},
	} {
		result, err := runtime.RunCommand(context.Background(), worker.CommandRequest{
			RunID:             request.RunID,
			Command:           test.command,
			EnvironmentPolicy: worker.EnvironmentPolicyClean,
			Role:              "gate",
		})
		if err != nil {
			t.Fatalf("RunCommand(%q) error = %v, want a command result", test.command, err)
		}
		if result.ExitCode != test.exitCode || result.Stderr != test.stderr {
			t.Fatalf("RunCommand(%q) result = %#v, want exit code %d", test.command, result, test.exitCode)
		}
	}
}

// TestDockerRuntimeKeepsUnrelatedInspectFailuresAsRuntimeErrors verifies that
// only the Docker inspect missing-container response is treated as absence.
func TestDockerRuntimeKeepsUnrelatedInspectFailuresAsRuntimeErrors(t *testing.T) {
	stub, _, _ := writeDockerStub(t)
	t.Setenv("WORKER_DOCKER_INSPECT_ERROR", "resource not found")
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	if _, err := runtime.Inspect(context.Background(), "run-contract-inspect-error"); err == nil {
		t.Fatal("Inspect() converted an unrelated 'not found' error into absence")
	}
}

// writeDockerStub creates a controlled executable that models the Docker
// lifecycle while recording the adapter's arguments for contract assertions.
func writeDockerStub(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	stub := filepath.Join(root, "docker-stub")
	logPath := filepath.Join(root, "docker.log")
	statePath := filepath.Join(root, "docker.state")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$WORKER_DOCKER_LOG"
command_name="${1:-}"
if [ "$command_name" = "container" ]; then
  command_name="${2:-}"
fi
case "$command_name" in
  inspect)
    if [ -n "${WORKER_DOCKER_INSPECT_ERROR:-}" ]; then
      printf '%s\n' "$WORKER_DOCKER_INSPECT_ERROR" >&2
      exit "${WORKER_DOCKER_INSPECT_EXIT:-1}"
    fi
    if [ ! -f "$WORKER_DOCKER_STATE" ]; then
      echo "No such object" >&2
      exit 1
    fi
    state=$(cat "$WORKER_DOCKER_STATE")
    if [ "$state" = "running" ]; then
      printf 'true\t%s\n' "$WORKER_DOCKER_IMAGE"
    else
      printf 'false\t%s\n' "$WORKER_DOCKER_IMAGE"
    fi
    ;;
  run)
    printf 'running\n' > "$WORKER_DOCKER_STATE"
    printf 'stub-container\n'
    ;;
  start)
    printf 'running\n' > "$WORKER_DOCKER_STATE"
    printf 'stub-container\n'
    ;;
  stop)
    printf 'stopped\n' > "$WORKER_DOCKER_STATE"
    printf 'stub-container\n'
    ;;
  exec)
    case "$*" in
      *fail-126*)
        printf 'command-failed-126\n' >&2
        exit 126
        ;;
      *fail-127*)
        printf 'command-failed-127\n' >&2
        exit 127
        ;;
      *fail-137*)
        printf 'command-failed-137\n' >&2
        exit 137
        ;;
      *fail-command*)
        printf 'command-failed\n' >&2
        exit 7
        ;;
      *)
        printf 'command-ok\n'
        ;;
    esac
    ;;
  *)
    echo "unsupported docker command: $*" >&2
    exit 2
    ;;
esac
`
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORKER_DOCKER_LOG", logPath)
	t.Setenv("WORKER_DOCKER_STATE", statePath)
	t.Setenv("WORKER_DOCKER_IMAGE", "ghcr.io/example/factory-worker@"+testWorkerDigest)
	return stub, logPath, statePath
}

// makeDirectory creates one host path used by a worker start request.
func makeDirectory(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// readStubLog returns recorded Docker invocations in their original order.
func readStubLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	contents := strings.TrimSpace(string(data))
	if contents == "" {
		return nil
	}
	return strings.Split(contents, "\n")
}

// findLogLine returns the first invocation containing the requested marker.
func findLogLine(t *testing.T, lines []string, marker string) string {
	t.Helper()
	for _, line := range lines {
		if strings.Contains(" "+line+" ", marker) {
			return line
		}
	}
	t.Fatalf("no Docker invocation contains %q: %#v", marker, lines)
	return ""
}

// findLogLines returns every invocation containing the requested marker.
func findLogLines(lines []string, marker string) []string {
	result := make([]string, 0)
	for _, line := range lines {
		if strings.Contains(" "+line+" ", marker) {
			result = append(result, line)
		}
	}
	return result
}

// countLogLines counts invocations containing the requested marker.
func countLogLines(lines []string, marker string) int {
	count := 0
	for _, line := range lines {
		if strings.Contains(" "+line+" ", marker) {
			count++
		}
	}
	return count
}

// assertContainsAll checks an independent set of required command fragments.
func assertContainsAll(t *testing.T, value string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(value, fragment) {
			t.Errorf("value %q does not contain %q", value, fragment)
		}
	}
}

// assertMode verifies the owner-independent permissions applied to one worker
// mount entry before Docker starts the fixed non-root worker.
func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("permissions for %q = %#o, want %#o", path, got, want)
	}
}

var _ worker.WorkerRuntime = (*worker.DockerRuntime)(nil)
