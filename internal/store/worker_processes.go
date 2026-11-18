package store

import (
	"context"
	"fmt"
)

// WorkerProcess is one harness process that this store launched in a
// worker. Its fields select the worker and the process state inside it. A
// run process has RunID, Role, and Stage; a session-role process has
// SessionID, Role, and the recorded WorkerID.
type WorkerProcess struct {
	RunID        string
	SessionID    string
	InvocationID string
	Role         string
	Stage        string
	WorkerID     string
}

// ReadWorkerProcesses lists every run and session-role process of the store,
// whatever its status: an ended process can leave its worker running. The
// workers of this store are exactly the workers of these processes, so the
// list never names a worker of another registration.
func ReadWorkerProcesses(ctx context.Context, database Querier) ([]WorkerProcess, error) {
	var processes []WorkerProcess
	runs, err := database.QueryContext(ctx, "SELECT run_id, id, role, stage FROM invocations ORDER BY run_id, id")
	if err != nil {
		return nil, fmt.Errorf("read run worker processes: %w", err)
	}
	defer func() { _ = runs.Close() }()
	for runs.Next() {
		var process WorkerProcess
		if err := runs.Scan(&process.RunID, &process.InvocationID, &process.Role, &process.Stage); err != nil {
			return nil, fmt.Errorf("read run worker processes: %w", err)
		}
		processes = append(processes, process)
	}
	if err := runs.Err(); err != nil {
		return nil, fmt.Errorf("read run worker processes: %w", err)
	}
	sessions, err := database.QueryContext(ctx, "SELECT session_id, id, role, worker_id FROM ideation_invocations ORDER BY session_id, id")
	if err != nil {
		return nil, fmt.Errorf("read session worker processes: %w", err)
	}
	defer func() { _ = sessions.Close() }()
	for sessions.Next() {
		var process WorkerProcess
		if err := sessions.Scan(&process.SessionID, &process.InvocationID, &process.Role, &process.WorkerID); err != nil {
			return nil, fmt.Errorf("read session worker processes: %w", err)
		}
		processes = append(processes, process)
	}
	if err := sessions.Err(); err != nil {
		return nil, fmt.Errorf("read session worker processes: %w", err)
	}
	return processes, nil
}
