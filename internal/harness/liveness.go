package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/worker"
)

// nativeSessionRunning checks the worker process table without reading the
// visible terminal. The bracketed executable pattern prevents the ps/awk
// inspection command from matching itself.
func nativeSessionRunning(ctx context.Context, runtime worker.WorkerRuntime, request NativeSessionRequest, processPattern string) (bool, error) {
	if runtime == nil {
		return false, fmt.Errorf("%s worker runtime is required", request.Harness)
	}
	if strings.TrimSpace(processPattern) == "" {
		return false, errors.New("native harness process pattern is required")
	}
	result, err := runtime.RunCommand(ctx, worker.CommandRequest{
		RunID:             request.RunID,
		Command:           fmt.Sprintf(`ps -eo pid=,args= | awk -v me="$$" '$1 != me && $0 ~ /%s/ { found = 1 } END { exit found ? 0 : 1 }'`, processPattern),
		EnvironmentPolicy: worker.EnvironmentPolicyClean,
		Role:              "coordinator",
	})
	if err != nil {
		return false, fmt.Errorf("run native session liveness command: %w", err)
	}
	switch result.ExitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("native harness liveness command exited with code %d", result.ExitCode)
	}
}
