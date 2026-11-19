package worker_test

import (
	"context"
	"slices"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestRunningWorkerContainersListsEveryWorkerContainer verifies that the
// Docker runtime lists the running worker containers of every installation
// and no other container.
func TestRunningWorkerContainersListsEveryWorkerContainer(t *testing.T) {
	t.Parallel()
	stub := runningDockerStub(t, "", testContainerName("run-2"), testContainerName("other-registration"), "unrelated")
	running, err := (&worker.DockerRuntime{DockerBinary: stub}).RunningWorkerContainers(context.Background())
	if err != nil {
		t.Fatalf("RunningWorkerContainers() error = %v", err)
	}
	if want := []string{testContainerName("run-2"), testContainerName("other-registration")}; !slices.Equal(running, want) {
		t.Fatalf("running worker containers = %v, want %v", running, want)
	}
}

// TestRunningWorkerContainersFailsWhenDockerIsUnreachable verifies that an
// unreachable Docker daemon is an error, never an empty result.
func TestRunningWorkerContainersFailsWhenDockerIsUnreachable(t *testing.T) {
	t.Parallel()
	stub := runningDockerStub(t, "Cannot connect to the Docker daemon")
	if _, err := (&worker.DockerRuntime{DockerBinary: stub}).RunningWorkerContainers(context.Background()); err == nil {
		t.Fatal("RunningWorkerContainers() with an unreachable daemon succeeded")
	}
}
