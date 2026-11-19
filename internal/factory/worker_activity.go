package factory

import (
	"context"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// Finding codes of the worker-activity check (ADR 0032).
const (
	// FindingWorkerActive reports a live harness helper in a worker of this
	// store.
	FindingWorkerActive = "worker.active"
	// FindingDockerUnreachable reports that Docker could not list the
	// running worker containers.
	FindingDockerUnreachable = "docker.unreachable"
	// FindingWorkerUnobserved reports a running worker of this store whose
	// helper state could not be read.
	FindingWorkerUnobserved = "worker.unobserved"
)

// WorkerProbe is the read-only Docker seam of the worker-activity check.
// worker.DockerRuntime implements it.
type WorkerProbe interface {
	// RunningWorkers returns the worker IDs among workerIDs whose container
	// runs.
	RunningWorkers(ctx context.Context, workerIDs []string) ([]string, error)
	// ObserveHeadless reads the helper status of one process and writes
	// nothing, so the read-only commands stay read-only.
	ObserveHeadless(ctx context.Context, request worker.HeadlessRequest) (worker.HeadlessStatus, error)
}

// WorkerActivityFindings reports whether a harness process of this store
// still runs in a worker container. processes come from
// store.ReadWorkerProcesses; only their workers are checked, so a container
// of another registration never blocks. An idle running worker does not
// block: a terminal run can leave its worker running with no process in it,
// so the evidence is a starting or running harness helper. Unknown Docker
// evidence is a finding, never a pass. A store without processes needs no
// Docker call.
//
// The migration gate, full quiescence, and restore use this check.
func WorkerActivityFindings(ctx context.Context, processes []store.WorkerProcess, probe WorkerProbe) []store.UpgradeFinding {
	if len(processes) == 0 {
		return nil
	}
	requests := make([]worker.HeadlessRequest, 0, len(processes))
	var workerIDs []string
	seen := make(map[string]bool)
	for _, process := range processes {
		request := headlessRequestForProcess(process)
		requests = append(requests, request)
		if !seen[request.WorkerID] {
			seen[request.WorkerID] = true
			workerIDs = append(workerIDs, request.WorkerID)
		}
	}
	running, err := probe.RunningWorkers(ctx, workerIDs)
	if err != nil {
		return []store.UpgradeFinding{{Code: FindingDockerUnreachable, Detail: "Docker cannot list the running worker containers: " + err.Error()}}
	}
	runningWorker := make(map[string]bool, len(running))
	for _, workerID := range running {
		runningWorker[workerID] = true
	}
	var active, unobserved []string
	for _, request := range requests {
		if !runningWorker[request.WorkerID] {
			continue
		}
		status, err := probe.ObserveHeadless(ctx, request)
		switch {
		case err != nil:
			unobserved = append(unobserved, request.WorkerID+" ("+err.Error()+")")
		case status == worker.HeadlessStatusStarting || status == worker.HeadlessStatusRunning:
			active = append(active, request.WorkerID+" runs "+request.InvocationID)
		}
	}
	var findings []store.UpgradeFinding
	if len(active) != 0 {
		findings = append(findings, store.UpgradeFinding{Code: FindingWorkerActive, Detail: "harness processes run in workers of this store: " + strings.Join(active, ", ")})
	}
	if len(unobserved) != 0 {
		findings = append(findings, store.UpgradeFinding{Code: FindingWorkerUnobserved, Detail: "the helper state of running workers is not readable: " + strings.Join(unobserved, ", ")})
	}
	return findings
}

// headlessRequestForProcess selects the worker and the helper state of one
// process, as the coordinator selected them at launch. The clean policy only
// makes the request valid; an observation runs no harness command.
func headlessRequestForProcess(process store.WorkerProcess) worker.HeadlessRequest {
	if process.SessionID != "" {
		return worker.HeadlessRequest{RunID: sessionWorkerOwner(process.SessionID), WorkerID: process.WorkerID, InvocationID: process.InvocationID, Role: process.Role, EnvironmentPolicy: worker.EnvironmentPolicyClean}
	}
	invocation := store.Invocation{ID: process.InvocationID, RunID: process.RunID, Role: process.Role, Stage: store.Stage(process.Stage)}
	return worker.HeadlessRequest{RunID: process.RunID, WorkerID: workerIDForInvocation(invocation), InvocationID: process.InvocationID, Role: process.Role, EnvironmentPolicy: worker.EnvironmentPolicyClean}
}

// DockerRuntime is the production WorkerProbe.
var _ WorkerProbe = (*worker.DockerRuntime)(nil)
