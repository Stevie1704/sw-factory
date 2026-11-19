package factory_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// workerProbeForTest is a Docker view: running lists the worker IDs whose
// container runs, and helpers gives the helper state per invocation.
type workerProbeForTest struct {
	running    []string
	helpers    map[string]worker.HeadlessStatus
	listErr    error
	inspectErr error
	asked      []string
	inspected  []worker.HeadlessRequest
}

// RunningWorkers returns the asked workers whose container runs.
func (p *workerProbeForTest) RunningWorkers(_ context.Context, workerIDs []string) ([]string, error) {
	p.asked = append(p.asked, workerIDs...)
	if p.listErr != nil {
		return nil, p.listErr
	}
	var found []string
	for _, id := range workerIDs {
		if slices.Contains(p.running, id) {
			found = append(found, id)
		}
	}
	return found, nil
}

// ObserveHeadless returns the helper status of one invocation.
func (p *workerProbeForTest) ObserveHeadless(_ context.Context, request worker.HeadlessRequest) (worker.HeadlessStatus, error) {
	p.inspected = append(p.inspected, request)
	if p.inspectErr != nil {
		return "", p.inspectErr
	}
	status, found := p.helpers[request.InvocationID]
	if !found {
		status = worker.HeadlessStatusMissing
	}
	return status, nil
}

// storeProcessesForTest are the processes of one store: an implementation
// and a review of run-1 and one session role.
var storeProcessesForTest = []store.WorkerProcess{
	{RunID: "run-1", InvocationID: "inv-impl", Role: workflow.RoleImplementation, Stage: string(store.StageImplementation)},
	{RunID: "run-1", InvocationID: "inv-review", Role: workflow.RoleSpecificationReview, Stage: string(store.StageReview)},
	{SessionID: "s1", InvocationID: "sinv-1", Role: workflow.RoleIdeation, WorkerID: "session-s1-ideation"},
}

// findingCodes returns the codes of findings.
func findingCodes(findings []store.UpgradeFinding) []string {
	codes := []string{}
	for _, finding := range findings {
		codes = append(codes, finding.Code)
	}
	return codes
}

// TestWorkerActivityAsksDockerOnlyForTheWorkersOfThisStore verifies that
// the check derives the worker of each process as the coordinator does, so
// the containers of another registration are never asked for.
func TestWorkerActivityAsksDockerOnlyForTheWorkersOfThisStore(t *testing.T) {
	t.Parallel()
	probe := &workerProbeForTest{running: []string{"run-other"}}
	findings := factory.WorkerActivityFindings(context.Background(), storeProcessesForTest, probe)
	if len(findings) != 0 {
		t.Fatalf("findings = %+v, want none", findings)
	}
	if want := []string{"run-1", "run-1-inv-review", "session-s1-ideation"}; !slices.Equal(probe.asked, want) {
		t.Fatalf("asked workers = %v, want %v", probe.asked, want)
	}
}

// TestWorkerActivityIgnoresAnIdleRunningWorker verifies that a running
// worker container whose harness helper ended does not block: a terminal run
// can leave its idle worker running (open risk of #323).
func TestWorkerActivityIgnoresAnIdleRunningWorker(t *testing.T) {
	t.Parallel()
	probe := &workerProbeForTest{
		running: []string{"run-1", "session-s1-ideation"},
		helpers: map[string]worker.HeadlessStatus{"inv-impl": worker.HeadlessStatusExited, "sinv-1": worker.HeadlessStatusCancelled},
	}
	if findings := factory.WorkerActivityFindings(context.Background(), storeProcessesForTest, probe); len(findings) != 0 {
		t.Fatalf("findings = %+v, want none for idle workers", findings)
	}
	if len(probe.inspected) != 2 {
		t.Fatalf("inspected = %+v, want only the processes of running workers", probe.inspected)
	}
}

// TestWorkerActivityBlocksOnALiveHelper verifies that a starting or running
// harness helper in a worker of this store is a worker.active finding.
func TestWorkerActivityBlocksOnALiveHelper(t *testing.T) {
	t.Parallel()
	for _, status := range []worker.HeadlessStatus{worker.HeadlessStatusStarting, worker.HeadlessStatusRunning} {
		probe := &workerProbeForTest{running: []string{"run-1-inv-review"}, helpers: map[string]worker.HeadlessStatus{"inv-review": status}}
		findings := factory.WorkerActivityFindings(context.Background(), storeProcessesForTest, probe)
		if !slices.Equal(findingCodes(findings), []string{factory.FindingWorkerActive}) {
			t.Fatalf("%s findings = %+v, want worker.active", status, findings)
		}
		request := probe.inspected[0]
		if request.RunID != "run-1" || request.WorkerID != "run-1-inv-review" || request.InvocationID != "inv-review" {
			t.Fatalf("inspection request = %+v, want the review worker of run-1", request)
		}
	}
}

// TestWorkerActivityFailsClosed verifies that unknown Docker evidence is a
// finding, never a pass.
func TestWorkerActivityFailsClosed(t *testing.T) {
	t.Parallel()
	unreachable := &workerProbeForTest{listErr: errors.New("Cannot connect to the Docker daemon")}
	if codes := findingCodes(factory.WorkerActivityFindings(context.Background(), storeProcessesForTest, unreachable)); !slices.Equal(codes, []string{factory.FindingDockerUnreachable}) {
		t.Fatalf("unreachable Docker findings = %v, want docker.unreachable", codes)
	}
	unreadable := &workerProbeForTest{running: []string{"run-1"}, inspectErr: errors.New("exec failed")}
	if codes := findingCodes(factory.WorkerActivityFindings(context.Background(), storeProcessesForTest, unreadable)); !slices.Equal(codes, []string{factory.FindingWorkerUnobserved}) {
		t.Fatalf("unreadable helper findings = %v, want worker.unobserved", codes)
	}
}

// TestWorkerActivityNeedsNoDockerForAStoreWithoutWorkers verifies that a
// store that never launched a worker passes without a Docker call.
func TestWorkerActivityNeedsNoDockerForAStoreWithoutWorkers(t *testing.T) {
	t.Parallel()
	probe := &workerProbeForTest{listErr: errors.New("unreachable")}
	if findings := factory.WorkerActivityFindings(context.Background(), nil, probe); len(findings) != 0 || len(probe.asked) != 0 {
		t.Fatalf("findings = %+v, asked = %v; want none without workers", findings, probe.asked)
	}
}
