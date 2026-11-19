package worker_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/worker"
)

// runningDockerStub writes a Docker stub whose ps lists the given container
// names, or fails when failure is set.
func runningDockerStub(t *testing.T, failure string, names ...string) string {
	t.Helper()
	stub := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\n[ \"$1\" = ps ] || exit 2\n"
	if failure != "" {
		script += "echo '" + failure + "' >&2\nexit 1\n"
	}
	for _, name := range names {
		script += "echo " + name + "\n"
	}
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return stub
}

// testContainerName mirrors the private container name of one worker.
func testContainerName(workerID string) string {
	digest := sha256.Sum256([]byte(workerID))
	return "factory-worker-" + hex.EncodeToString(digest[:])[:24]
}

// TestRunningWorkersNamesOnlyTheRunningWorkersAsked verifies that the
// Docker runtime reports a running worker by its worker ID and ignores the
// running containers of other workers.
func TestRunningWorkersNamesOnlyTheRunningWorkersAsked(t *testing.T) {
	t.Parallel()
	stub := runningDockerStub(t, "", testContainerName("run-2"), testContainerName("other-registration"), "unrelated")
	running, err := (&worker.DockerRuntime{DockerBinary: stub}).RunningWorkers(context.Background(), []string{"run-1", "run-2"})
	if err != nil {
		t.Fatalf("RunningWorkers() error = %v", err)
	}
	if !slices.Equal(running, []string{"run-2"}) {
		t.Fatalf("running workers = %v, want run-2", running)
	}
}

// TestRunningWorkersFailsWhenDockerIsUnreachable verifies that an
// unreachable Docker daemon is an error, never an empty result.
func TestRunningWorkersFailsWhenDockerIsUnreachable(t *testing.T) {
	t.Parallel()
	stub := runningDockerStub(t, "Cannot connect to the Docker daemon")
	if _, err := (&worker.DockerRuntime{DockerBinary: stub}).RunningWorkers(context.Background(), []string{"run-1"}); err == nil {
		t.Fatal("RunningWorkers() with an unreachable daemon succeeded")
	}
}
