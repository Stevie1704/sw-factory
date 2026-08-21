package worker_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
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
	assertGroupAccess(t, worktreePath, 0o070)
	assertGroupAccess(t, filepath.Join(worktreePath, "existing.txt"), 0o060)
	assertGroupAccess(t, gitMetadataPath, 0o050)
	assertGroupAccess(t, filepath.Join(gitMetadataPath, "HEAD"), 0o040)
	assertGroupAccess(t, cachePath, 0o070)
	assertGroupAccess(t, filepath.Join(cachePath, "existing.cache"), 0o060)

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
	if groupID := os.Getgid(); groupID != 10001 && !strings.Contains(runLine, "--group-add "+strconv.Itoa(groupID)) {
		t.Fatalf("worker start omitted host group %d from supplemental groups: %q", groupID, runLine)
	}
	if strings.Contains(runLine, "docker.sock") {
		t.Fatalf("worker start mounted the Docker socket: %q", runLine)
	}
	if strings.Contains(runLine, "dst="+worker.CredentialPath) {
		t.Fatalf("worker without credentials mounted the credential volume: %q", runLine)
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

// TestDockerRuntimeRejectsAHostHarnessDirectoryAsACache verifies that a
// repository cache cannot smuggle the full host Codex or Claude state home
// into a worker.
func TestDockerRuntimeRejectsAHostHarnessDirectoryAsACache(t *testing.T) {
	stub, logPath, _ := writeDockerStub(t)
	harnessDirectory := filepath.Join(t.TempDir(), ".codex")
	if err := os.MkdirAll(harnessDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	if err := runtime.Start(context.Background(), worker.StartRequest{
		RunID:           "run-host-harness-cache",
		WorktreePath:    makeDirectory(t, "worktree"),
		GitMetadataPath: makeDirectory(t, "git-metadata"),
		Caches:          []worker.CacheMount{{Name: "codex", HostPath: harnessDirectory}},
		Image:           "ghcr.io/example/factory-worker",
		ImageDigest:     testWorkerDigest,
	}); err == nil || !strings.Contains(err.Error(), "host harness") {
		t.Fatalf("Start() error = %v, want host harness cache rejection", err)
	}
	if lines := readStubLog(t, logPath); len(lines) != 0 {
		t.Fatalf("Docker was invoked after rejecting host harness cache: %#v", lines)
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
	mountsPath := filepath.Join(root, "docker.mounts")
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
    if [ -n "${WORKER_DOCKER_INSPECT_JSON_FILE:-}" ]; then
      cat "$WORKER_DOCKER_INSPECT_JSON_FILE"
      exit 0
    fi
    if [ "$state" = "running" ]; then
      printf 'true\t%s\t' "$WORKER_DOCKER_IMAGE"
    else
      printf 'false\t%s\t' "$WORKER_DOCKER_IMAGE"
    fi
    if [ -f "$WORKER_DOCKER_MOUNTS" ]; then
      cat "$WORKER_DOCKER_MOUNTS"
    fi
    printf '\n'
    ;;
  run)
    printf 'running\n' > "$WORKER_DOCKER_STATE"
    # Extract and record mount information from run command
    printf '' > "$WORKER_DOCKER_MOUNTS"
    shift
    while [ $# -gt 0 ]; do
      if [ "$1" = "--mount" ]; then
        shift
        mount_spec="$1"
        src=$(echo "$mount_spec" | sed -n 's/.*src=\([^,]*\).*/\1/p')
        dst=$(echo "$mount_spec" | sed -n 's/.*dst=\([^,]*\).*/\1/p')
        if echo "$mount_spec" | grep -q "readonly"; then
          rw="false"
        else
          rw="true"
        fi
        printf '%s\t%s\t%s\n' "$src" "$dst" "$rw" >> "$WORKER_DOCKER_MOUNTS"
      fi
      shift
    done
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
  rm)
    rm -f "$WORKER_DOCKER_STATE"
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
	t.Setenv("WORKER_DOCKER_MOUNTS", mountsPath)
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

// assertGroupAccess verifies that the worker group can access one prepared
// mount entry without relying on permissions for unrelated host users.
func assertGroupAccess(t *testing.T, path string, required os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	got := info.Mode().Perm()
	if got&required != required {
		t.Fatalf("permissions for %q = %#o, missing worker group bits %#o", path, got, required)
	}
	if got&0o007 != 0 {
		t.Fatalf("permissions for %q = %#o, unexpectedly grant other-user access", path, got)
	}
}

// TestDockerRuntimeReusesStoppedWorkerWhenMountContractMatches verifies that a
// stopped worker is reused when its configured mounts match the new request.
func TestDockerRuntimeReusesStoppedWorkerWhenMountContractMatches(t *testing.T) {
	stub, logPath, _ := writeDockerStub(t)
	worktreePath := makeDirectory(t, "worktree")
	gitMetadataPath := makeDirectory(t, "git-metadata")
	cachePath := makeDirectory(t, "go-cache")

	runtime := &worker.DockerRuntime{DockerBinary: stub}
	request := worker.StartRequest{
		RunID:           "run-contract-mount-match",
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
	if err := runtime.Stop(context.Background(), request.RunID); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	// Start again with identical mount contract - should reuse the container
	if err := runtime.Start(context.Background(), request); err != nil {
		t.Fatalf("Start() with matching mounts error = %v", err)
	}

	lines := readStubLog(t, logPath)
	if countLogLines(lines, " run ") != 1 {
		t.Fatalf("Docker run calls = %d, want 1 (mount contract matched, container reused); calls = %#v", countLogLines(lines, " run "), lines)
	}
	if countLogLines(lines, " start ") != 1 {
		t.Fatalf("Docker start calls = %d, want 1 (reused stopped container); calls = %#v", countLogLines(lines, " start "), lines)
	}
}

// TestDockerRuntimeRefusesMountContractMismatch verifies that Start returns a
// clear error when a stopped worker's configured mounts don't match the new
// request, mirroring the existing image-mismatch error pattern.
func TestDockerRuntimeRefusesMountContractMismatch(t *testing.T) {
	stub, logPath, _ := writeDockerStub(t)
	worktreePath := makeDirectory(t, "worktree")
	gitMetadataPath := makeDirectory(t, "git-metadata")
	cachePath := makeDirectory(t, "go-cache")
	otherCachePath := makeDirectory(t, "other-cache")

	runtime := &worker.DockerRuntime{DockerBinary: stub}
	request := worker.StartRequest{
		RunID:           "run-contract-mount-mismatch",
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
	if err := runtime.Stop(context.Background(), request.RunID); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	tests := []struct {
		name    string
		request worker.StartRequest
		pattern string
	}{
		{
			name: "different worktree path",
			request: worker.StartRequest{
				RunID:           request.RunID,
				WorktreePath:    makeDirectory(t, "other-worktree"),
				GitMetadataPath: gitMetadataPath,
				Image:           request.Image,
				ImageDigest:     request.ImageDigest,
				Caches:          request.Caches,
			},
			pattern: "mount contract mismatch",
		},
		{
			name: "different git metadata path",
			request: worker.StartRequest{
				RunID:           request.RunID,
				WorktreePath:    worktreePath,
				GitMetadataPath: makeDirectory(t, "other-git-metadata"),
				Image:           request.Image,
				ImageDigest:     request.ImageDigest,
				Caches:          request.Caches,
			},
			pattern: "mount contract mismatch",
		},
		{
			name: "different cache host path",
			request: worker.StartRequest{
				RunID:           request.RunID,
				WorktreePath:    worktreePath,
				GitMetadataPath: gitMetadataPath,
				Image:           request.Image,
				ImageDigest:     request.ImageDigest,
				Caches: []worker.CacheMount{{
					Name:     "go-build",
					HostPath: otherCachePath,
					ReadOnly: false,
				}},
			},
			pattern: "mount contract mismatch",
		},
		{
			name: "different cache name",
			request: worker.StartRequest{
				RunID:           request.RunID,
				WorktreePath:    worktreePath,
				GitMetadataPath: gitMetadataPath,
				Image:           request.Image,
				ImageDigest:     request.ImageDigest,
				Caches: []worker.CacheMount{{
					Name:     "npm-cache",
					HostPath: cachePath,
					ReadOnly: false,
				}},
			},
			pattern: "mount contract mismatch",
		},
		{
			name: "different cache read-only flag",
			request: worker.StartRequest{
				RunID:           request.RunID,
				WorktreePath:    worktreePath,
				GitMetadataPath: gitMetadataPath,
				Image:           request.Image,
				ImageDigest:     request.ImageDigest,
				Caches: []worker.CacheMount{{
					Name:     "go-build",
					HostPath: cachePath,
					ReadOnly: true,
				}},
			},
			pattern: "mount contract mismatch",
		},
		{
			name: "extra cache mount",
			request: worker.StartRequest{
				RunID:           request.RunID,
				WorktreePath:    worktreePath,
				GitMetadataPath: gitMetadataPath,
				Image:           request.Image,
				ImageDigest:     request.ImageDigest,
				Caches: []worker.CacheMount{
					{
						Name:     "go-build",
						HostPath: cachePath,
						ReadOnly: false,
					},
					{
						Name:     "npm-cache",
						HostPath: otherCachePath,
						ReadOnly: false,
					},
				},
			},
			pattern: "mount contract mismatch",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := runtime.Start(context.Background(), tc.request)
			if err == nil {
				t.Fatalf("Start() succeeded with mismatched mounts, want error containing %q", tc.pattern)
			}
			if !strings.Contains(err.Error(), tc.pattern) {
				t.Fatalf("Start() error = %q, want error containing %q", err.Error(), tc.pattern)
			}
			if !strings.Contains(err.Error(), request.RunID) {
				t.Fatalf("Start() error = %q, want error mentioning run ID %q", err.Error(), request.RunID)
			}
		})
	}

	lines := readStubLog(t, logPath)
	if countLogLines(lines, " run ") != 1 {
		t.Fatalf("Docker run calls = %d, want 1 (mount mismatches should not create new containers); calls = %#v", countLogLines(lines, " run "), lines)
	}
	if countLogLines(lines, " start ") != 0 {
		t.Fatalf("Docker start calls = %d, want 0 (mount mismatches should not restart container); calls = %#v", countLogLines(lines, " start "), lines)
	}
}

var _ worker.WorkerRuntime = (*worker.DockerRuntime)(nil)
