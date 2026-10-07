package worker_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestDockerRuntimeStartsEveryWorkerWithResourceLimits verifies that the
// docker run arguments carry the host-owned resource limits, and the documented
// defaults when the host configuration omits them.
func TestDockerRuntimeStartsEveryWorkerWithResourceLimits(t *testing.T) {
	for _, test := range []struct {
		name   string
		limits worker.ResourceLimits
		want   []string
	}{
		{
			name:   "defaults when the host omits limits",
			limits: worker.ResourceLimits{},
			want: []string{
				"--memory 8g", "--memory-swap 8g", "--cpus 4", "--pids-limit 4096",
				"--log-driver json-file", "--log-opt max-size=10m", "--log-opt max-file=3",
				"--init",
			},
		},
		{
			name:   "configured host limits",
			limits: worker.ResourceLimits{Memory: "512m", CPUs: "1.5", PIDs: "256", LogMaxSize: "1m", LogMaxFiles: "2"},
			want: []string{
				"--memory 512m", "--memory-swap 512m", "--cpus 1.5", "--pids-limit 256",
				"--log-driver json-file", "--log-opt max-size=1m", "--log-opt max-file=2",
			},
		},
		{
			name:   "defaults fill only the omitted limits",
			limits: worker.ResourceLimits{Memory: "2g"},
			want:   []string{"--memory 2g", "--memory-swap 2g", "--cpus 4", "--pids-limit 4096", "--log-opt max-size=10m"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			stub, logPath, _ := writeDockerStub(t)
			runtime := &worker.DockerRuntime{DockerBinary: stub}
			err := runtime.Start(context.Background(), worker.StartRequest{
				RunID:           "run-contract-limits",
				WorktreePath:    makeDirectory(t, "worktree"),
				GitMetadataPath: makeDirectory(t, "git-metadata"),
				Image:           "ghcr.io/example/factory-worker",
				ImageDigest:     testWorkerDigest,
				Limits:          test.limits,
			})
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			assertContainsAll(t, findLogLine(t, readStubLog(t, logPath), " run "), test.want...)
		})
	}
}

// TestDockerRuntimeRefusesInvalidResourceLimits verifies that the adapter never
// passes an invalid limit to Docker and names the rejected field.
func TestDockerRuntimeRefusesInvalidResourceLimits(t *testing.T) {
	stub, logPath, _ := writeDockerStub(t)
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	err := runtime.Start(context.Background(), worker.StartRequest{
		RunID:           "run-contract-bad-limits",
		WorktreePath:    makeDirectory(t, "worktree"),
		GitMetadataPath: makeDirectory(t, "git-metadata"),
		Image:           "ghcr.io/example/factory-worker",
		ImageDigest:     testWorkerDigest,
		Limits:          worker.ResourceLimits{PIDs: "--privileged"},
	})
	var limitErr *worker.ResourceLimitError
	if !errors.As(err, &limitErr) || limitErr.Field != "pids" {
		t.Fatalf("Start() error = %v, want a pids resource-limit error", err)
	}
	if lines := readStubLog(t, logPath); len(lines) != 0 {
		t.Fatalf("Docker calls = %#v, want none for invalid limits", lines)
	}
}

// TestResourceLimitsValidation verifies the accepted value grammar for every
// host-owned limit.
func TestResourceLimitsValidation(t *testing.T) {
	for _, test := range []struct {
		limits worker.ResourceLimits
		field  string
	}{
		{limits: worker.ResourceLimits{}},
		{limits: worker.ResourceLimits{Memory: "8g", CPUs: "4", PIDs: "4096", LogMaxSize: "10m", LogMaxFiles: "3"}},
		{limits: worker.ResourceLimits{Memory: "6m", CPUs: "0.5", LogMaxSize: "512k"}},
		{limits: worker.ResourceLimits{Memory: "1073741824"}},
		{limits: worker.ResourceLimits{Memory: "0"}, field: "memory"},
		{limits: worker.ResourceLimits{Memory: "-1g"}, field: "memory"},
		{limits: worker.ResourceLimits{Memory: "lots"}, field: "memory"},
		{limits: worker.ResourceLimits{Memory: "5m"}, field: "memory"},
		{limits: worker.ResourceLimits{CPUs: "0"}, field: "cpus"},
		{limits: worker.ResourceLimits{CPUs: "-2"}, field: "cpus"},
		{limits: worker.ResourceLimits{CPUs: "four"}, field: "cpus"},
		{limits: worker.ResourceLimits{PIDs: "0"}, field: "pids"},
		{limits: worker.ResourceLimits{PIDs: "-1"}, field: "pids"},
		{limits: worker.ResourceLimits{PIDs: "1.5"}, field: "pids"},
		{limits: worker.ResourceLimits{LogMaxSize: "0m"}, field: "log_max_size"},
		{limits: worker.ResourceLimits{LogMaxSize: "10 MB"}, field: "log_max_size"},
		{limits: worker.ResourceLimits{LogMaxFiles: "0"}, field: "log_max_files"},
		{limits: worker.ResourceLimits{LogMaxFiles: "x"}, field: "log_max_files"},
	} {
		err := test.limits.Validate()
		if test.field == "" {
			if err != nil {
				t.Errorf("Validate(%#v) error = %v, want valid", test.limits, err)
			}
			continue
		}
		var limitErr *worker.ResourceLimitError
		if !errors.As(err, &limitErr) || limitErr.Field != test.field {
			t.Errorf("Validate(%#v) error = %v, want a %s error", test.limits, err, test.field)
		}
	}
}

// TestResourceLimitsEffectiveValues verifies the documented defaults and that
// the effective limits render as one operator-readable summary.
func TestResourceLimitsEffectiveValues(t *testing.T) {
	effective := worker.ResourceLimits{CPUs: "2"}.Effective()
	want := worker.ResourceLimits{Memory: "8g", CPUs: "2", PIDs: "4096", LogMaxSize: "10m", LogMaxFiles: "3"}
	if effective != want {
		t.Fatalf("Effective() = %#v, want %#v", effective, want)
	}
	summary := effective.String()
	for _, fragment := range []string{"memory 8g", "swap 8g", "cpus 2", "pids 4096", "log 3 x 10m"} {
		if !strings.Contains(summary, fragment) {
			t.Errorf("String() = %q, want %q", summary, fragment)
		}
	}
}

// TestDockerRuntimeClassifiesAnOutOfMemoryKill verifies that a command the
// worker's memory limit killed is a typed infrastructure failure, not an
// ordinary non-zero exit. The kernel's kill count is the only evidence, so
// command output that imitates a marker, even with the private command
// identity, stays an ordinary exit.
func TestDockerRuntimeClassifiesAnOutOfMemoryKill(t *testing.T) {
	stub, _, _ := writeDockerStub(t)
	oomFile := filepath.Join(t.TempDir(), "oom-kills")
	if err := os.WriteFile(oomFile, []byte("2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORKER_DOCKER_OOM_FILE", oomFile)
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	request := worker.StartRequest{
		RunID:           "run-contract-oom",
		WorktreePath:    makeDirectory(t, "worktree"),
		GitMetadataPath: makeDirectory(t, "git-metadata"),
		Image:           "ghcr.io/example/factory-worker",
		ImageDigest:     testWorkerDigest,
	}
	if err := runtime.Start(context.Background(), request); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	command := func(text string) worker.CommandRequest {
		return worker.CommandRequest{RunID: request.RunID, Command: text, EnvironmentPolicy: worker.EnvironmentPolicyClean, Role: "gate"}
	}

	result, err := runtime.RunCommand(context.Background(), command("oom-command"))
	var oomErr *worker.OutOfMemoryError
	if !errors.As(err, &oomErr) {
		t.Fatalf("RunCommand() = %#v, %v; want a typed out-of-memory error", result, err)
	}
	if result != (worker.CommandResult{}) {
		t.Fatalf("RunCommand() returned result %#v with the out-of-memory error, want none", result)
	}
	if message := err.Error(); !strings.Contains(message, "memory limit") || strings.Contains(message, "partial output") {
		t.Fatalf("out-of-memory error = %q, want the cause without command output", message)
	}

	result, err = runtime.RunCommand(context.Background(), command("forged-oom"))
	if err != nil || result.ExitCode != 137 {
		t.Fatalf("RunCommand() = %#v, %v; want an ordinary exit 137 for a forged marker", result, err)
	}
}
