package factory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// recoverySessionID is a complete version-four UUID, the only native identity
// form every production adapter accepts for an exact resume.
const recoverySessionID = "6f1c2b3a-4d5e-4f60-8a1b-2c3d4e5f6a7b"

// recoveryLeakedSecret stands for a credential that a harness printed on
// stderr. It must never reach a coordinator-visible projection.
const recoveryLeakedSecret = "github_pat_11RECOVERYLEAKEDSECRET"

// TestHeadlessRecoverySettlesNativeFailuresThroughTheRecordedAdapter drives
// startup recovery through the production headless adapters and a controlled
// worker process. Each row restarts the coordinator over one durable store,
// injects the process outcome the recorded harness reports for its exact
// resume, and asserts the durable run state and the processes launched.
func TestHeadlessRecoverySettlesNativeFailuresThroughTheRecordedAdapter(t *testing.T) {
	for _, test := range []struct {
		name    string
		harness config.Harness
		// resumed is the process projection after the exact-session resume.
		resumed func(sessionID string) worker.HeadlessInspection
		status  store.Status
		reason  string
		// resumeCount is the durable automatic-resume generation afterwards:
		// zero when the outcome returns the allowance, one when it spends it.
		resumeCount int
	}{
		{
			name: "copilot missing native session fails closed", harness: config.HarnessCopilot,
			resumed: func(sessionID string) worker.HeadlessInspection {
				return exitedProcess("", "Error: No session, task, or name matched '"+sessionID+"'.\n"+recoveryLeakedSecret)
			},
			status: store.StatusWaitingForHuman, reason: LifecycleReasonAutomaticHarnessRecoveryExhausted, resumeCount: 1,
		},
		{
			name: "copilot policy or model denial is a bounded exit", harness: config.HarnessCopilot,
			resumed: func(string) worker.HeadlessInspection {
				return exitedProcess("", "Error: Copilot CLI is disabled by your organization policy.\n"+recoveryLeakedSecret)
			},
			status: store.StatusWaitingForHuman, reason: LifecycleReasonAutomaticHarnessRecoveryExhausted, resumeCount: 1,
		},
		{
			// The model-unavailable shape is unobserved (release blocker
			// RB-5); it stays a bounded exit, never a retry or a new model.
			name: "copilot unobserved model unavailable is a bounded exit", harness: config.HarnessCopilot,
			resumed: func(string) worker.HeadlessInspection {
				return exitedProcess("", "Error: Model \"gpt-5.2\" is not available. Run copilot --model to choose another model.\n"+recoveryLeakedSecret)
			},
			status: store.StatusWaitingForHuman, reason: LifecycleReasonAutomaticHarnessRecoveryExhausted, resumeCount: 1,
		},
		{
			// Copilot's capacity failure shape is unobserved (release blocker
			// RB-5), so it keeps the bounded unexpected-exit policy until a
			// classified marker exists.
			name: "copilot unobserved capacity failure is a bounded exit", harness: config.HarnessCopilot,
			resumed: func(string) worker.HeadlessInspection {
				return exitedProcess("", "Error: 429 Too Many Requests\n"+recoveryLeakedSecret)
			},
			status: store.StatusWaitingForHuman, reason: LifecycleReasonAutomaticHarnessRecoveryExhausted, resumeCount: 1,
		},
		{
			name: "copilot expired authentication pauses for refresh", harness: config.HarnessCopilot,
			resumed: func(string) worker.HeadlessInspection {
				return exitedProcess("", "Error: Authentication token found but could not be validated.\n"+recoveryLeakedSecret)
			},
			status: store.StatusWaitingForHuman, reason: LifecycleReasonHarnessAuthenticationExpired, resumeCount: 0,
		},
		{
			name: "codex capacity waits without spending the allowance", harness: config.HarnessCodex,
			resumed: func(string) worker.HeadlessInspection {
				return exitedProcess(`{"type":"error","code":"rate_limit_exceeded","message":"`+recoveryLeakedSecret+`"}`+"\n", "")
			},
			status: store.StatusWaitingForHarness, reason: LifecycleReasonHarnessCapacityUnavailable, resumeCount: 0,
		},
		{
			name: "claude expired authentication pauses for refresh", harness: config.HarnessClaude,
			resumed: func(string) worker.HeadlessInspection {
				return exitedProcess(`{"type":"system","subtype":"api_retry","error":"authentication_failed"}`+"\n"+`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"`+recoveryLeakedSecret+`"}`+"\n", "")
			},
			status: store.StatusWaitingForHuman, reason: LifecycleReasonHarnessAuthenticationExpired, resumeCount: 0,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newHeadlessRecoveryFixture(t, test.harness, recoverySessionID)
			processes := &headlessProcessRecoveryWorker{headlessRecoveryWorker: fixture.worker, harness: test.harness, onLaunch: test.resumed}

			err := fixture.restartCoordinator(processes)

			assertOnlyExactResumes(t, processes, 1)
			run, invocation := fixture.persistedRun(t), fixture.persistedInvocation(t)
			if run.Status != test.status || !strings.HasPrefix(run.LifecycleReason, test.reason) {
				t.Fatalf("run = %s (%q), want %s with reason %q; reconcile error %v", run.Status, run.LifecycleReason, test.status, test.reason, err)
			}
			if invocation.RecoveryResumeCount != test.resumeCount || invocation.NativeSessionID != recoverySessionID || invocation.Harness != string(test.harness) {
				t.Fatalf("invocation = harness %q session %q resumes %d, want the recorded %s session with %d resumes", invocation.Harness, invocation.NativeSessionID, invocation.RecoveryResumeCount, test.harness, test.resumeCount)
			}
			if run.CheckRepairAttempts != 0 || run.ReviewRepairAttempts != 0 {
				t.Fatalf("repair budgets spent: check %d review %d, want none", run.CheckRepairAttempts, run.ReviewRepairAttempts)
			}
			assertRecoveryProjectionsRedacted(t, fixture, run, err)

			// A further coordinator restart must not turn the outcome into a
			// retry loop or a fresh session.
			if test.status == store.StatusWaitingForHuman {
				_ = fixture.restartCoordinator(processes)
				assertOnlyExactResumes(t, processes, 1)
			}
		})
	}
}

// TestHeadlessRecoveryBoundsAnUnexpectedCopilotExit verifies a Copilot
// session that exits after its one automatic resume reaches the manual-resume
// boundary on the next coordinator start, and that a report written before
// the exit is left to report acceptance instead of a resume.
func TestHeadlessRecoveryBoundsAnUnexpectedCopilotExit(t *testing.T) {
	for _, test := range []struct {
		name       string
		reported   bool
		wantStatus store.Status
	}{
		{name: "exit without a report", wantStatus: store.StatusWaitingForHuman},
		{name: "exit after the report", reported: true, wantStatus: store.StatusActive},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newHeadlessRecoveryFixture(t, config.HarnessCopilot, recoverySessionID)
			processes := &headlessProcessRecoveryWorker{headlessRecoveryWorker: fixture.worker, harness: config.HarnessCopilot, onLaunch: runningProcess(config.HarnessCopilot)}
			if err := fixture.restartCoordinator(processes); err != nil {
				t.Fatalf("first startup reconciliation: %v", err)
			}
			assertOnlyExactResumes(t, processes, 1)
			if run := fixture.persistedRun(t); run.Status != store.StatusActive {
				t.Fatalf("run after the automatic resume = %s (%q), want active", run.Status, run.LifecycleReason)
			}

			// Launch materialisation creates the invocation root that keeps
			// the local diagnostic.
			if err := os.MkdirAll(invocationRoot(fixture.persistedRun(t), fixture.invocation.ID), 0o700); err != nil {
				t.Fatal(err)
			}
			processes.process = exitedProcess(identityEvent(config.HarnessCopilot, recoverySessionID), "unrecognised failure\n"+recoveryLeakedSecret+"\n"+strings.Repeat("x", 4*maxHarnessFailureDiagnosticBytes))
			if test.reported {
				if err := os.WriteFile(filepath.Join(fixture.invocation.ResultDirectory, report.ReportFileName), []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for restart := 0; restart < 2; restart++ {
				err := fixture.restartCoordinator(processes)
				run := fixture.persistedRun(t)
				if run.Status != test.wantStatus {
					t.Fatalf("restart %d: run = %s (%q), want %s; reconcile error %v", restart, run.Status, run.LifecycleReason, test.wantStatus, err)
				}
				if test.wantStatus == store.StatusWaitingForHuman && !strings.HasPrefix(run.LifecycleReason, LifecycleReasonAutomaticHarnessRecoveryExhausted) {
					t.Fatalf("restart %d: reason = %q, want the manual-resume boundary", restart, run.LifecycleReason)
				}
				assertRecoveryProjectionsRedacted(t, fixture, run, err)
			}
			assertOnlyExactResumes(t, processes, 1)
			if invocation := fixture.persistedInvocation(t); invocation.RecoveryResumeCount != 1 {
				t.Fatalf("recovery resumes = %d, want the single automatic allowance", invocation.RecoveryResumeCount)
			}
			if !test.reported {
				diagnostic, err := os.ReadFile(filepath.Join(invocationRoot(fixture.persistedRun(t), fixture.invocation.ID), harnessFailureDiagnosticName))
				if err != nil || !strings.Contains(string(diagnostic), "unrecognised failure") || len(diagnostic) > maxHarnessFailureDiagnosticBytes+1024 {
					t.Fatalf("local diagnostic = %d bytes, %v; want the bounded exit output kept beside the invocation", len(diagnostic), err)
				}
				if strings.Contains(string(diagnostic), recoveryLeakedSecret) {
					t.Fatal("local diagnostic keeps the token the harness printed")
				}
			}
		})
	}
}

// TestHeadlessRecoveryResumesTheSameCopilotSessionAfterAuthRefresh verifies
// the expired-authentication pause keeps the recorded Copilot session, and an
// explicit refresh with resume continues that exact session.
func TestHeadlessRecoveryResumesTheSameCopilotSessionAfterAuthRefresh(t *testing.T) {
	fixture := newHeadlessRecoveryFixture(t, config.HarnessCopilot, recoverySessionID)
	processes := &headlessProcessRecoveryWorker{headlessRecoveryWorker: fixture.worker, harness: config.HarnessCopilot, onLaunch: func(string) worker.HeadlessInspection {
		return exitedProcess("", "Error: No authentication information found.")
	}}
	_ = fixture.restartCoordinator(processes)
	if run := fixture.persistedRun(t); !strings.HasPrefix(run.LifecycleReason, LifecycleReasonHarnessAuthenticationExpired) {
		t.Fatalf("run = %s (%q), want the authentication pause", run.Status, run.LifecycleReason)
	}
	seeds := len(fixture.worker.seeds)

	processes.onLaunch = runningProcess(config.HarnessCopilot)
	service := NewWithDependencies(filepath.Join(fixture.root, "host.yaml"), fixture.dependencies(harness.NewHeadlessAdapters(processes)))
	service.deps.Worker = processes
	refreshed, err := service.RefreshAuth(t.Context(), AuthRefreshRequest{RunID: fixture.run.ID, Resume: true})
	if err != nil {
		t.Fatalf("RefreshAuth(resume) error = %v", err)
	}
	if !refreshed.Resumed || refreshed.Invocation.NativeSessionID != recoverySessionID || refreshed.Harness != config.HarnessCopilot {
		t.Fatalf("refresh = %#v, want the recorded Copilot session resumed", refreshed)
	}
	if len(fixture.worker.seeds) != seeds+1 || fixture.worker.seededHarnesses[len(fixture.worker.seededHarnesses)-1] != config.HarnessCopilot {
		t.Fatalf("credential seeds = %v, want one more Copilot projection", fixture.worker.seededHarnesses)
	}
	assertOnlyExactResumes(t, processes, 2)
	if run := fixture.persistedRun(t); run.Status != store.StatusActive {
		t.Fatalf("run after refresh = %s (%q), want active", run.Status, run.LifecycleReason)
	}
}

// TestHeadlessRecoveryAdoptsAnIdentityLaunchedBeforeItWasPersisted verifies
// a coordinator that stopped after the launch but before it persisted the
// native identity recovers that identity from the process output on restart
// and continues exactly that session. Unusable Copilot identity evidence is
// never adopted, so the run waits for a person instead of starting fresh.
func TestHeadlessRecoveryAdoptsAnIdentityLaunchedBeforeItWasPersisted(t *testing.T) {
	for _, test := range []struct {
		name    string
		harness config.Harness
		stdout  string
		adopted bool
	}{
		{name: "codex", harness: config.HarnessCodex, stdout: identityEvent(config.HarnessCodex, recoverySessionID), adopted: true},
		{name: "claude", harness: config.HarnessClaude, stdout: identityEvent(config.HarnessClaude, recoverySessionID), adopted: true},
		{name: "copilot", harness: config.HarnessCopilot, stdout: identityEvent(config.HarnessCopilot, recoverySessionID), adopted: true},
		{name: "copilot malformed identity", harness: config.HarnessCopilot, stdout: `{"sessionId":"not-a-session"}` + "\n"},
		{name: "copilot conflicting identities", harness: config.HarnessCopilot, stdout: identityEvent(config.HarnessCopilot, recoverySessionID) + `{"sessionId":"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"}` + "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newHeadlessRecoveryFixture(t, test.harness, "")
			processes := &headlessProcessRecoveryWorker{
				headlessRecoveryWorker: fixture.worker, harness: test.harness, onLaunch: runningProcess(test.harness),
				process: worker.HeadlessInspection{Status: worker.HeadlessStatusRunning, Stdout: test.stdout},
			}

			err := fixture.restartCoordinator(processes)

			invocation := fixture.persistedInvocation(t)
			if !test.adopted {
				if invocation.NativeSessionID != "" || len(processes.launches) != 0 {
					t.Fatalf("invocation session %q with %d launches, want no adopted identity and no launch", invocation.NativeSessionID, len(processes.launches))
				}
				if run := fixture.persistedRun(t); run.Status != store.StatusWaitingForHuman {
					t.Fatalf("run = %s (%q), want a human pause; reconcile error %v", run.Status, run.LifecycleReason, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("startup reconciliation: %v", err)
			}
			if invocation.NativeSessionID != recoverySessionID {
				t.Fatalf("persisted native session = %q, want the launched %s", invocation.NativeSessionID, recoverySessionID)
			}
			assertOnlyExactResumes(t, processes, 1)
		})
	}
}

// restartCoordinator builds a new coordinator over the fixture's durable state
// with production adapters bound to the controlled process worker, and runs
// its startup reconciliation once.
func (f headlessRecoveryFixture) restartCoordinator(processes *headlessProcessRecoveryWorker) error {
	dependencies := f.dependencies(harness.NewHeadlessAdapters(processes))
	dependencies.Worker = processes
	return NewWithDependencies(filepath.Join(f.root, "host.yaml"), dependencies).reconcileRegisteredRun(context.Background(), f.registration)
}

// assertOnlyExactResumes verifies every detached launch continued the
// recorded native session; none started a fresh one.
func assertOnlyExactResumes(t *testing.T, processes *headlessProcessRecoveryWorker, want int) {
	t.Helper()
	if len(processes.launches) != want {
		t.Fatalf("detached launches = %d, want %d", len(processes.launches), want)
	}
	for _, launch := range processes.launches {
		if launch.Mode != worker.HeadlessLaunchResume || sessionFromCommand(launch.Command) != recoverySessionID {
			t.Fatalf("launch = mode %q command %q, want an exact resume of %s", launch.Mode, launch.Command, recoverySessionID)
		}
	}
}

// assertRecoveryProjectionsRedacted verifies harness output stays out of every
// coordinator-visible projection, and that the local diagnostic stays private
// and bounded.
func assertRecoveryProjectionsRedacted(t *testing.T, fixture headlessRecoveryFixture, run store.Run, reconcileErr error) {
	t.Helper()
	visible := run.LifecycleReason + "\n" + fixture.github.comment.Body
	if reconcileErr != nil {
		visible += "\n" + reconcileErr.Error()
	}
	if strings.Contains(visible, recoveryLeakedSecret) {
		t.Fatalf("coordinator-visible projection leaks harness output: %q", visible)
	}
	logs, err := filepath.Glob(filepath.Join(fixture.root, ".factory-agents", "*", "*", harnessFailureDiagnosticName))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range logs {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if info.Mode().Perm() != 0o600 || info.Size() > maxHarnessFailureDiagnosticBytes+1024 {
			t.Fatalf("diagnostic %s = mode %v size %d, want private and bounded", path, info.Mode().Perm(), info.Size())
		}
	}
}

// exitedProcess is a settled detached process with the given retained output.
func exitedProcess(stdout, stderr string) worker.HeadlessInspection {
	return worker.HeadlessInspection{Status: worker.HeadlessStatusExited, ExitCode: 1, Stdout: stdout, Stderr: stderr}
}

// runningProcess returns a launch outcome in which the harness confirms the
// session it was asked to continue.
func runningProcess(harnessName config.Harness) func(string) worker.HeadlessInspection {
	return func(sessionID string) worker.HeadlessInspection {
		return worker.HeadlessInspection{Status: worker.HeadlessStatusRunning, Stdout: identityEvent(harnessName, sessionID)}
	}
}

// identityEvent is the machine event each harness prints to confirm its
// native session. Copilot's is provisional pending release blocker RB-1.
func identityEvent(harnessName config.Harness, sessionID string) string {
	switch harnessName {
	case config.HarnessClaude:
		return `{"type":"system","subtype":"init","session_id":"` + sessionID + `"}` + "\n"
	case config.HarnessCopilot:
		return `{"sessionId":"` + sessionID + `"}` + "\n"
	default:
		return `{"type":"thread.started","thread_id":"` + sessionID + `"}` + "\n"
	}
}

// sessionFromCommand returns the native session a launch command names,
// whichever option form its harness uses.
func sessionFromCommand(command []string) string {
	for index, argument := range command {
		for _, prefix := range []string{"--resume=", "--session-id="} {
			if value, found := strings.CutPrefix(argument, prefix); found {
				return value
			}
		}
		if (argument == "--resume" || argument == "--session-id" || argument == "resume") && index+1 < len(command) {
			return command[index+1]
		}
	}
	return ""
}

// headlessProcessRecoveryWorker adds a controlled detached-process extension
// to the recovery worker. A launch replaces the process projection with the
// outcome the test injects for the session the command names.
type headlessProcessRecoveryWorker struct {
	*headlessRecoveryWorker
	harness           config.Harness
	process           worker.HeadlessInspection
	onLaunch          func(sessionID string) worker.HeadlessInspection
	launches          []worker.HeadlessRequest
	cancels, finishes int
}

// StartHeadless records one detached launch and applies its injected outcome.
func (w *headlessProcessRecoveryWorker) StartHeadless(_ context.Context, request worker.HeadlessRequest) (worker.HeadlessExecution, error) {
	w.launches = append(w.launches, request)
	w.process = w.onLaunch(sessionFromCommand(request.Command))
	return worker.HeadlessExecution{RunID: request.RunID, WorkerID: request.WorkerID, InvocationID: request.InvocationID}, nil
}

// InspectHeadless returns the current process projection.
func (w *headlessProcessRecoveryWorker) InspectHeadless(context.Context, worker.HeadlessRequest) (worker.HeadlessInspection, error) {
	if w.process.Status == "" {
		return worker.HeadlessInspection{Status: worker.HeadlessStatusMissing}, nil
	}
	return w.process, nil
}

// CancelHeadless records a cancellation of the owned process.
func (w *headlessProcessRecoveryWorker) CancelHeadless(context.Context, worker.HeadlessRequest) error {
	w.cancels++
	return nil
}

// FinishHeadless records an accepted-completion shutdown.
func (w *headlessProcessRecoveryWorker) FinishHeadless(context.Context, worker.HeadlessRequest) error {
	w.finishes++
	return nil
}

var _ worker.HeadlessProcessRuntime = (*headlessProcessRecoveryWorker)(nil)

// TestAutomaticResumeIsBoundedToOnePerInvocation pins the automatic-resume
// allowance at the resume seam itself: an invocation that already used its
// one automatic resume is returned unchanged, with no worker or store effect,
// whatever a caller's own pre-check allows.
func TestAutomaticResumeIsBoundedToOnePerInvocation(t *testing.T) {
	invocation := store.Invocation{ID: "inv-resumed", RunID: "run-resumed", Harness: string(config.HarnessCopilot), NativeSessionID: recoverySessionID, Status: store.InvocationStatusActive, RecoveryResumeCount: 1}
	module := newInvocationLifecycle(nil, nil, nil, nil, nil, nil, invocationLifecycleHooks{}, nil)
	got, err := module.resumePersistedInvocation(context.Background(), config.RepositoryRegistration{}, nil, store.Run{ID: "run-resumed"}, invocation)
	if err != nil || got.RecoveryResumeCount != 1 || got.ID != invocation.ID {
		t.Fatalf("second automatic resume = %#v, %v; want the invocation unchanged and no resume", got, err)
	}
}

// TestCoordinatorRestartAfterACopilotReportAcceptanceRepeatsNoEffect verifies
// a coordinator that restarts after AcceptAgentReport completed a Copilot
// invocation relaunches, resumes, cancels, and finishes no Copilot process,
// the invocation stays completed, and the run stays active.
func TestCoordinatorRestartAfterACopilotReportAcceptanceRepeatsNoEffect(t *testing.T) {
	assertRestartAfterReportAcceptanceRepeatsNoEffect(t, config.HarnessCopilot)
}

// TestCoordinatorRestartAfterAnAcceptedImplementationReportKeepsTheRunActive
// verifies the clean-implementation startup rule is harness-neutral: a run
// without an independent test stage whose implementation report was accepted
// is a healthy run awaiting its checkpoint, not an interrupted one.
func TestCoordinatorRestartAfterAnAcceptedImplementationReportKeepsTheRunActive(t *testing.T) {
	for _, harnessName := range []config.Harness{config.HarnessCodex, config.HarnessClaude} {
		t.Run(string(harnessName), func(t *testing.T) {
			assertRestartAfterReportAcceptanceRepeatsNoEffect(t, harnessName)
		})
	}
}

// assertRestartAfterReportAcceptanceRepeatsNoEffect accepts a completed
// implementation report for the given harness, restarts the coordinator, and
// verifies the restart touches no process and leaves the run active at the
// implementation stage. The fixture declares no independent test stage, so
// implementation is a clean start boundary for it.
func assertRestartAfterReportAcceptanceRepeatsNoEffect(t *testing.T, harnessName config.Harness) {
	t.Helper()
	fixture := newHeadlessRecoveryFixture(t, harnessName, recoverySessionID)
	processes := &headlessProcessRecoveryWorker{headlessRecoveryWorker: fixture.worker, harness: harnessName, onLaunch: runningProcess(harnessName)}
	processes.process = runningProcess(harnessName)(recoverySessionID)
	if _, err := report.WriteAtomicForInvocation(fixture.invocation.ResultDirectory, fixture.invocation.ID, report.Report{
		SchemaVersion: report.SchemaVersion, InvocationID: fixture.invocation.ID, RunID: fixture.run.ID,
		Harness: string(harnessName), Role: "implementation", Stage: "implementation",
		Outcome: report.OutcomeCompleted, Summary: "implementation complete",
		Handoff: &report.Handoff{
			ChangeSummary:     "implemented the behavior",
			AcceptanceMapping: []report.AcceptanceMapping{{Criterion: "behavior", Evidence: "focused test"}},
			FocusedCommands:   []string{"go test ./..."},
		},
		NativeSessionID: recoverySessionID, ReportedAt: fixture.now,
	}); err != nil {
		t.Fatal(err)
	}
	dependencies := fixture.dependencies(harness.NewHeadlessAdapters(processes))
	dependencies.Worker = processes
	accepting := NewWithDependencies(filepath.Join(fixture.root, "host.yaml"), dependencies)
	if _, err := accepting.AcceptAgentReport(t.Context(), AgentReportRequest{RunID: fixture.run.ID, InvocationID: fixture.invocation.ID}); err != nil {
		t.Fatalf("AcceptAgentReport() error = %v", err)
	}
	if invocation := fixture.persistedInvocation(t); invocation.Status != store.InvocationStatusCompleted {
		t.Fatalf("accepted invocation = %s, want completed", invocation.Status)
	}
	// The accepting coordinator is itself a fresh start, so it may already
	// have used the one automatic resume; only the restart must add nothing.
	launches, cancels, finishes := len(processes.launches), processes.cancels, processes.finishes
	resumes := fixture.persistedInvocation(t).RecoveryResumeCount

	if err := fixture.restartCoordinator(processes); err != nil {
		t.Fatalf("restart after accepted report: %v", err)
	}
	if run := fixture.persistedRun(t); run.Stage != store.StageImplementation || run.Status != store.StatusActive {
		t.Fatalf("run after restart = %s/%s (%q), want implementation/active", run.Stage, run.Status, run.LifecycleReason)
	}
	if len(processes.launches) != launches || processes.cancels != cancels || processes.finishes != finishes {
		t.Fatalf("restart effects = launches %d->%d cancels %d->%d finishes %d->%d, want none", launches, len(processes.launches), cancels, processes.cancels, finishes, processes.finishes)
	}
	if invocation := fixture.persistedInvocation(t); invocation.Status != store.InvocationStatusCompleted || invocation.RecoveryResumeCount != resumes {
		t.Fatalf("invocation after restart = %#v, want completed and not resumed again", invocation)
	}
}
