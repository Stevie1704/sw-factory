package factory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// authPausedRecoveryFixture persists a headless run that a restart paused on
// an expired credential, with the recorded native session kept active.
func authPausedRecoveryFixture(t *testing.T) (headlessRecoveryFixture, *headlessProcessRecoveryWorker) {
	t.Helper()
	fixture := newHeadlessRecoveryFixture(t, config.HarnessCodex, recoverySessionID)
	processes := &headlessProcessRecoveryWorker{headlessRecoveryWorker: fixture.worker, harness: config.HarnessCodex, onLaunch: expiredCodexCredential}
	_ = fixture.restartCoordinator(processes)
	if run := fixture.persistedRun(t); run.Status != store.StatusWaitingForHuman || !strings.HasPrefix(run.LifecycleReason, LifecycleReasonHarnessAuthenticationExpired) {
		t.Fatalf("run = %s (%q), want the authentication pause", run.Status, run.LifecycleReason)
	}
	// The pause stopped the worker; the shared fake ignores Stop.
	fixture.worker.inspection.Running = false
	return fixture, processes
}

// expiredCodexCredential is the detached process outcome of a Codex session
// whose projected credential has expired.
func expiredCodexCredential(string) worker.HeadlessInspection {
	return exitedProcess(`{"type":"error","message":"401 Unauthorized: token expired"}`+"\n", "")
}

// setCredentialModified moves the host credential source's modification time.
func setCredentialModified(t *testing.T, fixture headlessRecoveryFixture, modified time.Time) {
	t.Helper()
	if err := os.Chtimes(fixture.authPath, modified, modified); err != nil {
		t.Fatal(err)
	}
}

// pollTick runs one coordinator poll-loop recovery tick over the fixture.
func pollTick(t *testing.T, fixture headlessRecoveryFixture, processes *headlessProcessRecoveryWorker) error {
	t.Helper()
	service := NewWithDependencies(filepath.Join(fixture.root, "host.yaml"), fixture.dependencies(harness.NewHeadlessAdapters(processes)))
	service.deps.Worker = processes
	return service.retryWaitingForHarness(t.Context(), fixture.registration)
}

// TestPollAutoRecoversAnAuthenticationPauseAfterTheHostCredentialChanges
// verifies the coordinator resumes the exact recorded session, without a
// human command and without spending the retry budget, once the host
// credential source is newer than the pause.
func TestPollAutoRecoversAnAuthenticationPauseAfterTheHostCredentialChanges(t *testing.T) {
	fixture, processes := authPausedRecoveryFixture(t)
	launches, seeds := len(processes.launches), len(fixture.worker.seeds)
	setCredentialModified(t, fixture, fixture.now.Add(time.Minute))
	processes.onLaunch = runningProcess(config.HarnessCodex)

	if err := pollTick(t, fixture, processes); err != nil {
		t.Fatalf("poll tick error = %v", err)
	}

	run, invocation := fixture.persistedRun(t), fixture.persistedInvocation(t)
	if run.Status != store.StatusActive {
		t.Fatalf("run = %s (%q), want active after automatic recovery", run.Status, run.LifecycleReason)
	}
	if !strings.HasPrefix(run.LifecycleReason, "automatic recovery: ") || !strings.Contains(run.LifecycleReason, LifecycleReasonHarnessAuthenticationExpired) || !strings.Contains(run.LifecycleReason, "host credential source changed") {
		t.Fatalf("lifecycle reason = %q, want the admitted reason recorded", run.LifecycleReason)
	}
	if !strings.Contains(fixture.github.comment.Body, "automatic recovery: ") {
		t.Fatalf("status comment does not record the automatic recovery:\n%s", fixture.github.comment.Body)
	}
	if len(processes.launches) != launches+1 || len(fixture.worker.seeds) != seeds+1 {
		t.Fatalf("launches %d seeds %d, want one more of each", len(processes.launches)-launches, len(fixture.worker.seeds)-seeds)
	}
	assertOnlyExactResumes(t, processes, len(processes.launches))
	if invocation.RecoveryResumeCount != 0 {
		t.Fatalf("recovery resumes = %d, want the retry budget untouched", invocation.RecoveryResumeCount)
	}
}

// TestPollKeepsAnAuthenticationPauseWhileTheHostCredentialIsUnchanged
// verifies the same expired credential is never retried by the poll loop.
func TestPollKeepsAnAuthenticationPauseWhileTheHostCredentialIsUnchanged(t *testing.T) {
	fixture, processes := authPausedRecoveryFixture(t)
	launches := len(processes.launches)
	setCredentialModified(t, fixture, fixture.now.Add(-time.Hour))

	if err := pollTick(t, fixture, processes); err != nil {
		t.Fatalf("poll tick error = %v", err)
	}

	if len(processes.launches) != launches {
		t.Fatalf("launches = %d, want none while the credential is unchanged", len(processes.launches)-launches)
	}
	if run := fixture.persistedRun(t); run.Status != store.StatusWaitingForHuman || !strings.HasPrefix(run.LifecycleReason, LifecycleReasonHarnessAuthenticationExpired) {
		t.Fatalf("run = %s (%q), want the authentication pause kept", run.Status, run.LifecycleReason)
	}
}

// TestPollAttemptsAutomaticRecoveryOncePerCredentialChange verifies a
// credential change that does not clear the expiry admits one attempt, not a
// resume on every poll tick.
func TestPollAttemptsAutomaticRecoveryOncePerCredentialChange(t *testing.T) {
	fixture, processes := authPausedRecoveryFixture(t)
	launches := len(processes.launches)
	// The change is newer than the coordinator clock, so only the recorded
	// observation, not the clock, can stop a second attempt.
	setCredentialModified(t, fixture, fixture.now.Add(time.Minute))

	for tick := 0; tick < 3; tick++ {
		if err := pollTick(t, fixture, processes); err != nil {
			t.Fatalf("poll tick %d error = %v", tick, err)
		}
	}

	if len(processes.launches) != launches+1 {
		t.Fatalf("launches = %d, want exactly one automatic attempt", len(processes.launches)-launches)
	}
	run := fixture.persistedRun(t)
	if run.Status != store.StatusWaitingForHuman || !strings.HasPrefix(run.LifecycleReason, LifecycleReasonHarnessAuthenticationExpired) || !strings.Contains(run.LifecycleReason, "automatic recovery attempted") {
		t.Fatalf("run = %s (%q), want the authentication pause with the attempt recorded", run.Status, run.LifecycleReason)
	}
}

// TestTypedPauseRecoveryAdmissionKeepsTheHumanGateOutsideTheConjunction
// verifies each condition of the automatic-recovery policy independently: a
// pause is admitted only when all of them hold.
func TestTypedPauseRecoveryAdmissionKeepsTheHumanGateOutsideTheConjunction(t *testing.T) {
	authPause := store.Run{Status: store.StatusWaitingForHuman, LifecycleReason: LifecycleReasonHarnessAuthenticationExpired + " (codex); run is waiting for `factory auth refresh`"}
	processExit := []RecoveryDiscrepancy{
		{Source: "worker", Field: "lifecycle", Expected: "running", Observed: "stopped", Recoverable: true},
		{Source: "harness", Field: "lifecycle", Expected: "persisted native session process running", Observed: "exited", Recoverable: true},
		{Source: "native session", Field: "identity", Expected: recoverySessionID, Observed: "codex headless process has no native session event", Recoverable: true},
	}
	for _, test := range []struct {
		name      string
		run       func(store.Run) store.Run
		diagnosis func(RecoveryDiagnosis) RecoveryDiagnosis
		admitted  bool
	}{
		{name: "credential expiry with process-exit discrepancies", admitted: true},
		{name: "credential expiry with the classified expiry still observed", admitted: true, diagnosis: func(d RecoveryDiagnosis) RecoveryDiagnosis {
			d.headlessFailure = harness.ClassifyError(harness.ErrAuthenticationExpired, "codex")
			return d
		}},
		{name: "reason outside the allow-list", run: func(r store.Run) store.Run {
			r.LifecycleReason = LifecycleReasonAutomaticHarnessRecoveryExhausted + " (codex); manual native resume required"
			return r
		}},
		{name: "restart reconciliation pause", run: func(r store.Run) store.Run {
			r.LifecycleReason = restartReconciliationPausePrefix + " worker.lifecycle expected=\"running\" observed=\"stopped\""
			return r
		}},
		{name: "run not waiting for a human", run: func(r store.Run) store.Run {
			r.Status = store.StatusActive
			return r
		}},
		{name: "pending clarification questions", run: func(r store.Run) store.Run {
			r.PendingQuestions = []store.PendingQuestion{{ID: "q1", Prompt: "which?"}}
			return r
		}},
		{name: "sources disagree", diagnosis: func(d RecoveryDiagnosis) RecoveryDiagnosis {
			d.SourcesAgree = false
			return d
		}},
		{name: "discrepancy outside the process-exit family", diagnosis: func(d RecoveryDiagnosis) RecoveryDiagnosis {
			d.Discrepancies = append(d.Discrepancies, RecoveryDiscrepancy{Source: "worker", Field: "image", Expected: "image@digest", Observed: "other", Recoverable: true})
			return d
		}},
		{name: "unrecoverable process-exit discrepancy", diagnosis: func(d RecoveryDiagnosis) RecoveryDiagnosis {
			d.Discrepancies[2].Recoverable = false
			return d
		}},
		{name: "pending effect journaled", diagnosis: func(d RecoveryDiagnosis) RecoveryDiagnosis {
			d.PendingEffect = &store.PendingEffect{ID: "effect-1", Kind: store.PendingEffectKindHarnessResume}
			return d
		}},
		{name: "different harness failure observed", diagnosis: func(d RecoveryDiagnosis) RecoveryDiagnosis {
			d.headlessFailure = &harness.FailureError{Kind: harness.FailureUnexpectedExit, Harness: "codex", OutOfMemory: true}
			return d
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := authPause
			if test.run != nil {
				run = test.run(run)
			}
			diagnosis := RecoveryDiagnosis{SourcesAgree: true, Discrepancies: append([]RecoveryDiscrepancy(nil), processExit...)}
			if test.diagnosis != nil {
				diagnosis = test.diagnosis(diagnosis)
			}
			got := typedPauseRecoveryAdmission(run, diagnosis)
			if test.admitted && got != LifecycleReasonHarnessAuthenticationExpired {
				t.Fatalf("admission = %q, want the credential-expiry reason", got)
			}
			if !test.admitted && got != "" {
				t.Fatalf("admission = %q, want the human gate kept", got)
			}
		})
	}
}
