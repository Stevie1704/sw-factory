package factory

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// automaticRecoveryReasons is the allow-list of typed, self-remedying pause
// reasons the coordinator may recover without a human command. Every other
// pause keeps its human gate.
var automaticRecoveryReasons = []string{LifecycleReasonHarnessAuthenticationExpired}

// automaticRecoveryAttemptMarker records in the pause reason that one
// automatic attempt consumed the current host credential change.
const automaticRecoveryAttemptMarker = "automatic recovery attempted after host credential change"

// allowListedPauseReason returns the allow-listed reason of a human pause
// without open clarification questions, or "" when the pause is not eligible.
func allowListedPauseReason(run store.Run) string {
	if run.Status != store.StatusWaitingForHuman || len(run.PendingQuestions) > 0 {
		return ""
	}
	reason := strings.TrimSpace(run.LifecycleReason)
	for _, allowed := range automaticRecoveryReasons {
		if strings.HasPrefix(reason, allowed) {
			return allowed
		}
	}
	return ""
}

// processExitDiscrepancy reports whether a discrepancy only restates that the
// paused worker and its harness process stopped, which is the documented
// consequence of every allow-listed pause.
func processExitDiscrepancy(discrepancy RecoveryDiscrepancy) bool {
	if !discrepancy.Recoverable {
		return false
	}
	switch discrepancy.Source + "." + discrepancy.Field {
	case "worker.existence", "worker.lifecycle", "harness.lifecycle", "native session.identity":
		return true
	}
	return false
}

// typedPauseRecoveryAdmission returns the allow-listed reason that admits an
// automatic recovery of run, or "" when the human gate must stay. It admits
// only the conjunction of an allow-listed reason, agreeing sources, process-
// exit discrepancies, no pending effect, and no contradicting harness failure.
func typedPauseRecoveryAdmission(run store.Run, diagnosis RecoveryDiagnosis) string {
	reason := allowListedPauseReason(run)
	if reason == "" || diagnosis.PendingEffect != nil || !diagnosis.SourcesAgree {
		return ""
	}
	if diagnosis.headlessFailure != nil && !harness.IsAuthenticationExpired(diagnosis.headlessFailure) {
		return ""
	}
	for _, discrepancy := range diagnosis.Discrepancies {
		if !processExitDiscrepancy(discrepancy) {
			return ""
		}
	}
	return reason
}

// automaticRecoveryReason is the lifecycle reason published after an
// automatic recovery, so the admitting decision is reviewable afterwards.
func automaticRecoveryReason(admitted, harnessName string) string {
	return fmt.Sprintf("automatic recovery: %s (%s) cleared; admitted because the host credential source changed after the pause, sources agree, every discrepancy is a process exit, and no effect is pending; native session resumed", admitted, harnessName)
}

// hostCredentialModified returns the modification time of the registered
// host credential source of harnessName. It reports false when no source is
// registered or the source cannot be read, so the pause stays closed.
func hostCredentialModified(registration config.RepositoryRegistration, harnessName string) (time.Time, bool) {
	path := registeredCredentialSource(registration.Authentication, config.Harness(harnessName))
	if strings.TrimSpace(path) == "" {
		return time.Time{}, false
	}
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

// autoRecoverTypedPause continues an allow-listed pause once its blocker has
// cleared outside the coordinator: the host credential source changed after
// the pause. One credential change admits at most one attempt. A failed
// attempt leaves the run paused with the attempt recorded, as a manual
// resume would.
func (s *Service) autoRecoverTypedPause(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run) error {
	activeStore, ok := runStore.(ActiveInvocationStore)
	if !ok {
		return nil
	}
	active, err := activeStore.ActiveInvocation(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("read active invocation for automatic recovery: %w", err)
	}
	if active == nil || strings.TrimSpace(active.NativeSessionID) == "" {
		return nil
	}
	modified, ok := hostCredentialModified(registration, active.Harness)
	if !ok || !modified.After(run.UpdatedAt) {
		return nil
	}
	diagnosis := s.diagnoseInterruptedRunWithStore(ctx, registration, runStore, run)
	if journal, journaled := runStore.(PendingEffectStore); journaled {
		pending, pendingErr := journal.PendingEffect(ctx, run.ID)
		if pendingErr != nil {
			return fmt.Errorf("inspect pending effect for automatic recovery: %w", pendingErr)
		}
		diagnosis.PendingEffect = pending
	}
	admitted := typedPauseRecoveryAdmission(run, diagnosis)
	if admitted == "" {
		return nil
	}
	attempted, err := s.recordAutomaticRecoveryAttempt(ctx, runStore, run, modified)
	if err != nil {
		return err
	}
	result, resumeErr := s.lifecycleModule().refreshAuth(ctx, InvocationRecoveryRequest{
		Registration: registration, RunStore: runStore, Run: &attempted,
		ResumeReason: automaticRecoveryReason(admitted, active.Harness),
	}, AuthRefreshRequest{Resume: true, ExpectedInvocationID: active.ID})
	if resumeErr != nil {
		return s.publishFailedAutomaticRecovery(ctx, registration, runStore, run.ID, resumeErr)
	}
	if result.Resumed && result.Invocation.ID != "" {
		s.markInvocationStarted(result.Invocation.ID)
	}
	return nil
}

// publishFailedAutomaticRecovery keeps the human gate after a failed attempt
// and makes the failure visible in the status comment. A failure that already
// moved the run to another pause has published that pause, so only a run that
// still carries the attempt marker is updated. The poll loop continues; the
// manual commands remain the way forward.
func (s *Service) publishFailedAutomaticRecovery(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, runID string, cause error) error {
	current, err := runStore.CurrentRun(ctx)
	if err != nil {
		return fmt.Errorf("read run after failed automatic recovery: %w", err)
	}
	if current == nil || current.ID != runID || current.Status != store.StatusWaitingForHuman {
		return nil
	}
	marker := strings.Index(current.LifecycleReason, automaticRecoveryAttemptMarker)
	if marker < 0 {
		return nil
	}
	if err := s.lifecycleModule().stopActiveRunWorkers(ctx, runStore, *current); err != nil {
		return err
	}
	next := *current
	attempt := current.LifecycleReason[:marker+len(automaticRecoveryAttemptMarker)]
	next.LifecycleReason = boundedText(attempt+" failed: "+recoveryReasonValue(observedFailure(cause)), maxRecoveryReasonBytes)
	next.Revision = current.Revision + 1
	// The attempt time stays the lower bound, so the same credential change
	// cannot admit a second attempt.
	if now := s.deps.Now().UTC(); now.After(next.UpdatedAt) {
		next.UpdatedAt = now
	}
	if err := s.lifecycleModule().persistLifecycleRun(ctx, registration, runStore, *current, next); err != nil {
		return fmt.Errorf("publish failed automatic recovery: %w", err)
	}
	return nil
}

// recordAutomaticRecoveryAttempt persists the attempt before it crosses the
// native boundary. The recorded time covers the observed credential change,
// so neither a crash nor a clock behind the file lets the same change admit a
// second attempt.
func (s *Service) recordAutomaticRecoveryAttempt(ctx context.Context, runStore RunStore, run store.Run, credentialModified time.Time) (store.Run, error) {
	next := run
	if !strings.Contains(next.LifecycleReason, automaticRecoveryAttemptMarker) {
		next.LifecycleReason = boundedText(run.LifecycleReason+"; "+automaticRecoveryAttemptMarker, maxRecoveryReasonBytes)
	}
	next.UpdatedAt = s.deps.Now().UTC()
	if !next.UpdatedAt.After(credentialModified) {
		next.UpdatedAt = credentialModified.UTC()
	}
	if err := saveRunWithRetry(ctx, runStore, next); err != nil {
		return run, fmt.Errorf("record automatic recovery attempt: %w", err)
	}
	return next, nil
}
