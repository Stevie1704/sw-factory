package worker_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestDockerRuntimeStartsHeadlessWithoutTTY verifies the detached Docker
// contract and the logical identity boundary used by the Codex adapter.
func TestDockerRuntimeStartsHeadlessWithoutTTY(t *testing.T) {
	root := t.TempDir()
	logPath := root + "/docker.log"
	dockerStub := root + "/docker-stub"
	dockerScript := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$WORKER_HEADLESS_LOG"
case "${1:-}" in
  container)
    printf 'true\t%s\t\n' "ghcr.io/example/factory-worker@` + testWorkerDigest + `"
    ;;
  exec)
    case "$*" in
      *factory-worker-headless*inspect*)
        printf '{"status":"%s","exit_code":0,"stdout":"","stderr":"","stdout_truncated":false,"stderr_truncated":false}\n' "${WORKER_HEADLESS_STATUS:-missing}"
        ;;
      *)
        ;;
    esac
    ;;
esac
`
	if err := os.WriteFile(dockerStub, []byte(dockerScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORKER_HEADLESS_LOG", logPath)
	runtime := &worker.DockerRuntime{DockerBinary: dockerStub}
	request := worker.HeadlessRequest{
		RunID: "run-headless-contract", WorkerID: "worker-headless-contract", InvocationID: "inv-headless-contract",
		Command: []string{"codex", "exec", "--json", "prompt"}, EnvironmentPolicy: worker.EnvironmentPolicyRole,
		Role: "implementation", Environment: map[string]string{"FACTORY_INVOCATION_ID": "inv-headless-contract"},
		Mode: worker.HeadlessLaunchFresh,
	}
	if _, err := runtime.StartHeadless(context.Background(), request); err != nil {
		t.Fatalf("StartHeadless() error = %v", err)
	}
	if err := runtime.CancelHeadless(context.Background(), request); err != nil {
		t.Fatalf("CancelHeadless() error = %v", err)
	}
	t.Setenv("WORKER_HEADLESS_STATUS", "exited")
	resume := request
	resume.Mode = worker.HeadlessLaunchResume
	if _, err := runtime.StartHeadless(context.Background(), resume); err != nil {
		t.Fatalf("StartHeadless(resume) error = %v", err)
	}
	lines := readStubLog(t, logPath)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "exec -d") {
		t.Fatalf("headless Docker calls = %q, want detached exec", joined)
	}
	if strings.Contains(joined, "exec -it") || strings.Contains(joined, "exec -i ") || strings.Contains(joined, "exec -t ") {
		t.Fatalf("headless Docker calls allocated a TTY or stdin: %q", joined)
	}
	if !strings.Contains(joined, "/usr/local/bin/factory-worker-headless run") || !strings.Contains(joined, "/usr/local/bin/factory-worker-headless cancel") {
		t.Fatalf("headless Docker calls = %q, want worker helper lifecycle", joined)
	}
	if !strings.Contains(joined, "/usr/local/bin/factory-worker-headless run --state-dir /home/factory/.factory-headless/inv-headless-contract --replace") {
		t.Fatalf("headless Docker calls = %q, want exact resume replacement", joined)
	}
	if !strings.Contains(joined, "--env FACTORY_INVOCATION_ID=inv-headless-contract") {
		t.Fatalf("headless Docker calls omitted explicit invocation identity: %q", joined)
	}
}

var _ worker.HeadlessProcessRuntime = (*worker.DockerRuntime)(nil)

// TestDockerRuntimeReportsAHeadlessOutOfMemoryKill verifies that a detached
// process records a root-owned out-of-memory baseline at launch, and that only
// a failed exit whose kernel kill count grew is reported as out of memory.
func TestDockerRuntimeReportsAHeadlessOutOfMemoryKill(t *testing.T) {
	root := t.TempDir()
	logPath := root + "/docker.log"
	dockerStub := root + "/docker-stub"
	dockerScript := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$WORKER_HEADLESS_LOG"
case "${1:-}" in
  container)
    printf 'true\t%s\t\n' "ghcr.io/example/factory-worker@` + testWorkerDigest + `"
    ;;
  exec)
    case "$*" in
      *factory-oom-check*)
        if [ "${WORKER_HEADLESS_OOM:-}" = 1 ]; then echo oom; fi
        ;;
      *factory-worker-headless*inspect*)
        printf '{"status":"%s","exit_code":%s,"stdout":"","stderr":"","stdout_truncated":false,"stderr_truncated":false}\n' "${WORKER_HEADLESS_STATUS:-missing}" "${WORKER_HEADLESS_EXIT:-0}"
        ;;
    esac
    ;;
esac
`
	if err := os.WriteFile(dockerStub, []byte(dockerScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORKER_HEADLESS_LOG", logPath)
	runtime := &worker.DockerRuntime{DockerBinary: dockerStub}
	request := worker.HeadlessRequest{
		RunID: "run-headless-oom", WorkerID: "worker-headless-oom", InvocationID: "inv-headless-oom",
		Command: []string{"codex", "exec", "--json", "prompt"}, EnvironmentPolicy: worker.EnvironmentPolicyRole,
		Role: "implementation", Mode: worker.HeadlessLaunchFresh,
	}
	if _, err := runtime.StartHeadless(context.Background(), request); err != nil {
		t.Fatalf("StartHeadless() error = %v", err)
	}
	// The stub log splits multi-line script arguments, so it is read whole.
	calls := strings.Join(readStubLog(t, logPath), "\n")
	if !strings.Contains(calls, "exec --user 0:0 ") || !strings.Contains(calls, "factory-oom-baseline inv-headless-oom") {
		t.Fatalf("Docker calls = %q, want a root-owned baseline record for the invocation", calls)
	}
	if strings.Index(calls, "factory-oom-baseline") > strings.Index(calls, "exec -d") {
		t.Fatalf("Docker calls = %q, want the baseline recorded before the process starts", calls)
	}

	t.Setenv("WORKER_HEADLESS_STATUS", "exited")
	for _, test := range []struct {
		name     string
		exitCode string
		oom      string
		want     bool
	}{
		{name: "failed exit with a grown kill count", exitCode: "137", oom: "1", want: true},
		{name: "failed exit with an unchanged kill count", exitCode: "137", want: false},
		{name: "successful exit", exitCode: "0", oom: "1", want: false},
	} {
		t.Setenv("WORKER_HEADLESS_EXIT", test.exitCode)
		t.Setenv("WORKER_HEADLESS_OOM", test.oom)
		inspection, err := runtime.InspectHeadless(context.Background(), request)
		if err != nil {
			t.Fatalf("%s: InspectHeadless() error = %v", test.name, err)
		}
		if inspection.OutOfMemory != test.want {
			t.Fatalf("%s: OutOfMemory = %t, want %t", test.name, inspection.OutOfMemory, test.want)
		}
	}
}
