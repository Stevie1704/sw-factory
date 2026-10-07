package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dockerWorkerImageVariable names the pinned image@digest used by the real
// Docker command-lifetime checks. They are skipped when it is unset, so the
// ordinary test suite keeps no Docker dependency.
const dockerWorkerImageVariable = "FACTORY_DOCKER_WORKER_IMAGE"

// TestRealWorkerCommandLifetime proves, in a real pinned worker, that a
// timed-out command and its descendants stop before RunCommand returns, and
// that ordinary command results are unchanged by supervision.
func TestRealWorkerCommandLifetime(t *testing.T) {
	runtime, runID, worktree := startRealWorker(t, ResourceLimits{})
	// A short grace keeps SIGKILL escalation well before the descendants below
	// would finish on their own.
	grace := commandTerminationGrace
	commandTerminationGrace = time.Second
	t.Cleanup(func() { commandTerminationGrace = grace })

	t.Run("timed-out command cannot write after the timeout", func(t *testing.T) {
		marker := filepath.Join(worktree, "late-marker")
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := runtime.RunCommand(ctx, cleanCommand(runID, "sleep 3; touch late-marker"))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("RunCommand() error = %v, want a confirmed timeout", err)
		}
		time.Sleep(3 * time.Second)
		if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("timed-out command wrote its marker after the timeout (stat error %v)", statErr)
		}
	})

	t.Run("TERM-ignoring descendants are killed as a group", func(t *testing.T) {
		marker := filepath.Join(worktree, "descendant-marker")
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		started := time.Now()
		command := `sh -c 'trap "" TERM; sh -c "trap \"\" TERM; sleep 4; touch descendant-marker" & wait' & sleep 30`
		_, err := runtime.RunCommand(ctx, cleanCommand(runID, command))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("RunCommand() error = %v, want a confirmed timeout", err)
		}
		if elapsed := time.Since(started); elapsed < time.Second+commandTerminationGrace {
			t.Fatalf("RunCommand() returned after %s, want SIGKILL escalation after the %s grace", elapsed, commandTerminationGrace)
		}
		time.Sleep(4 * time.Second)
		if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("descendant wrote its marker after cancellation (stat error %v)", statErr)
		}
	})

	t.Run("ordinary results are preserved", func(t *testing.T) {
		result, err := runtime.RunCommand(context.Background(), cleanCommand(runID, "printf out; printf err >&2; exit 7"))
		if err != nil || result.ExitCode != 7 || result.Stdout != "out" || result.Stderr != "err" {
			t.Fatalf("RunCommand() = %#v, %v; want exit 7 with both streams", result, err)
		}
	})

	t.Run("unconfirmed termination blocks work until the worker restarts", func(t *testing.T) {
		// Fabricate the state the terminator leaves behind when a process group
		// survives SIGKILL: a cancelled record naming a live group (PID 1).
		quarantine := `records="` + commandRecordRoot + `/$(cat /proc/sys/kernel/random/boot_id)-$(cut -d ' ' -f 22 /proc/1/stat)"; printf '1\n' > "$records/stuck.pgid" && : > "$records/stuck.cancel"`
		if result, err := runtime.RunCommand(context.Background(), cleanCommand(runID, quarantine)); err != nil || result.ExitCode != 0 {
			t.Fatalf("fabricate quarantine = %#v, %v", result, err)
		}
		_, err := runtime.RunCommand(context.Background(), cleanCommand(runID, "touch overlap-marker"))
		var terminationErr *CommandTerminationError
		if !errors.As(err, &terminationErr) {
			t.Fatalf("RunCommand() error = %v, want refusal while a cancelled command is alive", err)
		}
		if _, statErr := os.Stat(filepath.Join(worktree, "overlap-marker")); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("refused command ran (stat error %v)", statErr)
		}
		if err := runtime.Stop(context.Background(), runID); err != nil {
			t.Fatalf("Stop() error = %v", err)
		}
		image, digest, _ := strings.Cut(os.Getenv(dockerWorkerImageVariable), "@")
		if err := runtime.Resume(context.Background(), ResumeRequest{RunID: runID, Image: image, ImageDigest: digest}); err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if result, err := runtime.RunCommand(context.Background(), cleanCommand(runID, "true")); err != nil || result.ExitCode != 0 {
			t.Fatalf("RunCommand() after restart = %#v, %v; want the worker usable again", result, err)
		}
	})
}

// TestRealWorkerResourceLimits proves, in a real pinned worker, that a command
// which allocates past the memory limit produces the typed out-of-memory
// failure, and that a fork loop stops at the PID limit.
func TestRealWorkerResourceLimits(t *testing.T) {
	runtime, runID, _ := startRealWorker(t, ResourceLimits{Memory: "64m", PIDs: "64"})

	t.Run("allocation past the memory limit is an out-of-memory failure", func(t *testing.T) {
		// awk doubles one string until the memory limit kills it.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		result, err := runtime.RunCommand(ctx, cleanCommand(runID, `awk 'BEGIN { s = "x"; while (1) s = s s }'`))
		var oomErr *OutOfMemoryError
		if !errors.As(err, &oomErr) {
			t.Fatalf("RunCommand() = %#v, %v; want a typed out-of-memory error", result, err)
		}
	})

	t.Run("a fork loop stops at the PID limit", func(t *testing.T) {
		// Each background sleep holds one PID. The loop asks for far more
		// PIDs than the limit, so the kernel refuses a fork and the shell
		// stops instead of exhausting the host.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		result, err := runtime.RunCommand(ctx, cleanCommand(runID, `i=0; while [ $i -lt 200 ]; do sleep 3 & i=$((i + 1)); done; wait`))
		if err != nil {
			t.Fatalf("RunCommand() error = %v", err)
		}
		if result.ExitCode == 0 || !strings.Contains(strings.ToLower(result.Stderr), "fork") {
			t.Fatalf("RunCommand() = %#v, want a refused fork at the PID limit", result)
		}
		// The worker recovers when the sleeps that hold the PIDs end.
		deadline := time.Now().Add(15 * time.Second)
		for {
			result, err := runtime.RunCommand(context.Background(), cleanCommand(runID, "cat /sys/fs/cgroup/pids.max"))
			if err == nil && result.ExitCode == 0 {
				if limit := strings.TrimSpace(result.Stdout); limit != "64" {
					t.Fatalf("worker pids.max = %q, want 64", limit)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("worker after the fork loop = %#v, %v; want it usable again", result, err)
			}
			time.Sleep(500 * time.Millisecond)
		}
	})
}

// startRealWorker starts a disposable credential-free worker from the pinned
// image with the given limits and removes it with its directories when the
// test ends.
func startRealWorker(t *testing.T, limits ResourceLimits) (*DockerRuntime, string, string) {
	t.Helper()
	reference := os.Getenv(dockerWorkerImageVariable)
	if reference == "" {
		t.Skipf("set %s to a pinned image@digest to run real Docker checks", dockerWorkerImageVariable)
	}
	image, digest, found := strings.Cut(reference, "@")
	if !found {
		t.Fatalf("%s = %q, want image@digest", dockerWorkerImageVariable, reference)
	}
	// Docker Desktop and colima share only the user's home with their VM, so
	// the bind-mount sources stay below the working directory.
	root, err := os.MkdirTemp(".", ".docker-verify.")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(root, "work")
	gitMetadata := filepath.Join(root, "git")
	for _, path := range []string{worktree, gitMetadata} {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runtime := NewDockerRuntime()
	runID := "command-lifetime-" + filepath.Base(root)[len(".docker-verify."):]
	t.Cleanup(func() {
		_ = runtime.Cleanup(context.Background(), CleanupRequest{RunID: runID, Roles: []string{"implementation"}})
		_ = os.RemoveAll(root)
	})
	if err := runtime.Start(context.Background(), StartRequest{RunID: runID, WorktreePath: worktree, GitMetadataPath: gitMetadata, Image: image, ImageDigest: digest, Limits: limits}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	return runtime, runID, worktree
}

// cleanCommand builds a clean-policy command request for the real worker.
func cleanCommand(runID, command string) CommandRequest {
	return CommandRequest{RunID: runID, Command: command, EnvironmentPolicy: EnvironmentPolicyClean, Role: "gate"}
}
