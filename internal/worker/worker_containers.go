package worker

import (
	"context"
	"fmt"
	"strings"
)

// workerContainerPrefix starts the name of every worker container.
const workerContainerPrefix = "factory-worker-"

// RunningWorkerContainers returns the names of the running worker
// containers of every installation on the Docker host. A container name does
// not name its installation, so a caller that cannot name the workers of its
// store, such as a restore over a damaged store, treats each one as
// possibly its own. It only reads; an unreachable daemon is an error.
func (r *DockerRuntime) RunningWorkerContainers(ctx context.Context) ([]string, error) {
	result, err := r.runDocker(ctx, []string{"ps", "--filter", "name=" + workerContainerPrefix, "--format", "{{.Names}}"})
	if err != nil {
		return nil, fmt.Errorf("list running worker containers: %w", dockerStderrDetail(err))
	}
	var names []string
	for _, name := range strings.Fields(result.Stdout) {
		if strings.HasPrefix(name, workerContainerPrefix) {
			names = append(names, name)
		}
	}
	return names, nil
}
