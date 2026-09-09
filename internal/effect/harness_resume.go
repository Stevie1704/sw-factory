package effect

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// harnessResumeHandler owns the native-session continuation reserved before a
// harness command crosses into a visible terminal.
type harnessResumeHandler struct {
	now       func() time.Time
	lifecycle lifecycle
	projector runProjector
}

// ResumeHarness journals a native-session continuation before the harness
// command crosses into a visible terminal. The invocation counter is reserved
// before the native command, so an ambiguous post-launch failure is never
// replayed as a second visible session.
func (j *Journal) ResumeHarness(ctx context.Context, runStore RunStore, invocationStore InvocationStore, socketPath string, runtime harness.Runtime, invocation store.Invocation, request harness.StartRequest) (store.Invocation, error) {
	handler := mustApplyHandler[harnessResumeHandler](j.dispatcher, store.PendingEffectKindHarnessResume)
	return handler.resume(ctx, runStore, invocationStore, socketPath, runtime, invocation, request)
}

// ResumeHarnessManually performs an explicit operator resume. The native
// command is journaled, but the automatic recovery counter is left unchanged
// because manual intervention is outside that bounded policy.
func (j *Journal) ResumeHarnessManually(ctx context.Context, runStore RunStore, invocationStore InvocationStore, socketPath string, runtime harness.Runtime, invocation store.Invocation, request harness.StartRequest) (store.Invocation, error) {
	handler := mustApplyHandler[harnessResumeHandler](j.dispatcher, store.PendingEffectKindHarnessResume)
	return handler.resumeManually(ctx, runStore, invocationStore, socketPath, runtime, invocation, request)
}

// resume reserves and performs one automatic native-session continuation.
func (h harnessResumeHandler) resume(ctx context.Context, runStore RunStore, invocationStore InvocationStore, socketPath string, runtime harness.Runtime, invocation store.Invocation, request harness.StartRequest) (store.Invocation, error) {
	if runtime == nil {
		return invocation, errors.New("harness runtime is required for native resume")
	}
	targetCount := invocation.RecoveryResumeCount + 1
	reserved := invocation
	reserved.RecoveryResumeCount = targetCount
	reserved.UpdatedAt = h.now().UTC()
	if _, journaled := runStore.(PendingEffectStore); !journaled {
		// Keep the compatibility behavior of older embedding stores: they do
		// not expose a restart journal, so their historical resume boundary is
		// the native command followed by invocation persistence.
		session, err := runtime.Resume(ctx, request)
		if err != nil {
			return invocation, fmt.Errorf("resume native harness session: %w", classifyHarnessRuntimeError(runtime, err))
		}
		updated := reserved
		if session.NativeSessionID != "" {
			updated.NativeSessionID = session.NativeSessionID
		}
		updated.UpdatedAt = h.now().UTC()
		if err := invocationStore.SaveInvocation(ctx, updated); err != nil {
			return updated, fmt.Errorf("persist resumed native session: %w", err)
		}
		return updated, nil
	}
	payload := harnessResumeEffectPayload{
		SocketPath: socketPath, Request: request, Invocation: invocation,
		TargetResumeCount: targetCount,
	}
	effect, err := reserve(h.now, invocation.RunID, store.PendingEffectKindHarnessResume, invocation.ID+"\x00"+fmt.Sprint(targetCount), payload)
	if err != nil {
		return invocation, err
	}
	updated := invocation
	var waitingFailure error
	apply := func() error {
		if err := invocationStore.SaveInvocation(ctx, reserved); err != nil {
			return fmt.Errorf("reserve native session resume: %w", err)
		}
		updated = reserved
		session, err := runtime.Resume(ctx, request)
		if err != nil {
			classified := classifyHarnessRuntimeError(runtime, err)
			if harness.IsRateLimited(classified) || harness.IsAuthenticationExpired(classified) {
				// Capacity and expired-auth failures happen before a native
				// continuation boundary. Roll the reservation back and clear the
				// effect so the same invocation remains retryable.
				if saveErr := invocationStore.SaveInvocation(ctx, invocation); saveErr != nil {
					return fmt.Errorf("rollback native session resume after %s: %w", classified, saveErr)
				}
				updated = invocation
				waitingFailure = classified
				return nil
			}
			return fmt.Errorf("resume native harness session: %w", classified)
		}
		if session.NativeSessionID != "" {
			updated.NativeSessionID = session.NativeSessionID
		}
		updated.UpdatedAt = h.now().UTC()
		if err := invocationStore.SaveInvocation(ctx, updated); err != nil {
			return fmt.Errorf("persist resumed native session: %w", err)
		}
		return nil
	}
	if err := withPendingEffect(ctx, runStore, effect, applier(apply)); err != nil {
		// A journaled resume is never rolled back after reservation is
		// attempted. Returning the reserved projection makes callers retain
		// the one-resume ceiling even when the journal or native adapter fails.
		return reserved, err
	}
	if waitingFailure != nil {
		return invocation, waitingFailure
	}
	return updated, nil
}

// resumeManually reserves and performs one operator-requested resume.
func (h harnessResumeHandler) resumeManually(ctx context.Context, runStore RunStore, invocationStore InvocationStore, socketPath string, runtime harness.Runtime, invocation store.Invocation, request harness.StartRequest) (store.Invocation, error) {
	if runtime == nil {
		return invocation, errors.New("harness runtime is required for manual native resume")
	}
	if _, journaled := runStore.(PendingEffectStore); !journaled {
		return h.resumeManuallyWithoutJournal(ctx, invocationStore, runtime, invocation, request)
	}
	payload := harnessResumeEffectPayload{
		SocketPath: socketPath, Request: request, Invocation: invocation,
		TargetResumeCount: invocation.RecoveryResumeCount, Manual: true,
	}
	effect, err := reserve(h.now, invocation.RunID, store.PendingEffectKindHarnessResume, invocation.ID+"\x00manual", payload)
	if err != nil {
		return invocation, err
	}
	updated := invocation
	reserved := invocation
	reserved.AttachRequired = !runtime.Capabilities().Headless
	reserved.UpdatedAt = h.now().UTC()
	var waitingFailure error
	apply := func() error {
		if err := invocationStore.SaveInvocation(ctx, reserved); err != nil {
			return fmt.Errorf("reserve manual native session resume: %w", err)
		}
		updated = reserved
		session, resumeErr := runtime.Resume(ctx, request)
		if resumeErr != nil {
			classified := classifyHarnessRuntimeError(runtime, resumeErr)
			if harness.IsRateLimited(classified) || harness.IsAuthenticationExpired(classified) {
				// A capacity or credential rejection happens before the
				// operator can attach a resumed session. Restore the original
				// invocation so the explicit command remains retryable.
				if saveErr := invocationStore.SaveInvocation(ctx, invocation); saveErr != nil {
					return fmt.Errorf("rollback manual native session resume after %s: %w", classified, saveErr)
				}
				updated = invocation
				waitingFailure = classified
				return nil
			}
			return fmt.Errorf("resume native harness session manually: %w", classified)
		}
		if session.NativeSessionID != "" {
			updated.NativeSessionID = session.NativeSessionID
		}
		updated.UpdatedAt = h.now().UTC()
		if err := invocationStore.SaveInvocation(ctx, updated); err != nil {
			return fmt.Errorf("persist manually resumed native session: %w", err)
		}
		return nil
	}
	if err := withPendingEffect(ctx, runStore, effect, applier(apply)); err != nil {
		return updated, err
	}
	if waitingFailure != nil {
		return invocation, waitingFailure
	}
	return updated, nil
}

// resumeManuallyWithoutJournal executes the compatibility path for stores
// without a pending-effect journal.
func (h harnessResumeHandler) resumeManuallyWithoutJournal(ctx context.Context, invocationStore InvocationStore, runtime harness.Runtime, invocation store.Invocation, request harness.StartRequest) (store.Invocation, error) {
	session, err := runtime.Resume(ctx, request)
	if err != nil {
		return invocation, fmt.Errorf("resume native harness session manually: %w", classifyHarnessRuntimeError(runtime, err))
	}
	updated := invocation
	updated.AttachRequired = !runtime.Capabilities().Headless
	if session.NativeSessionID != "" {
		updated.NativeSessionID = session.NativeSessionID
	}
	updated.UpdatedAt = h.now().UTC()
	if err := invocationStore.SaveInvocation(ctx, updated); err != nil {
		return updated, fmt.Errorf("persist manually resumed native session: %w", err)
	}
	return updated, nil
}

// classifyHarnessRuntimeError turns adapter status text into a redacted typed
// failure while preserving unrelated infrastructure errors.
func classifyHarnessRuntimeError(runtime harness.Runtime, err error) error {
	name := ""
	if runtime != nil {
		name = runtime.Capabilities().Name
	}
	return harness.ClassifyError(err, name)
}

// Replay completes a native-resume reservation. A durable reservation is
// treated as an already-crossed boundary; a missing reservation is recorded
// before replay so a second restart cannot launch a duplicate session after an
// ambiguous native command.
func (h harnessResumeHandler) Replay(ctx context.Context, request replayRequest) (store.Run, error) {
	runStore, err := replayStore(request)
	if err != nil {
		return store.Run{}, err
	}
	effect := request.Effect
	var payload harnessResumeEffectPayload
	if err := decodePendingEffect(effect, &payload); err != nil {
		return store.Run{}, err
	}
	invocationStore, ok := runStore.(InvocationStore)
	if !ok {
		return store.Run{}, errors.New("operational store does not support harness resume replay")
	}
	invocation, err := invocationStore.Invocation(ctx, effect.RunID, payload.Invocation.ID)
	if err != nil {
		return store.Run{}, fmt.Errorf("read invocation during harness resume replay: %w", err)
	}
	if invocation == nil {
		return store.Run{}, fmt.Errorf("invocation %q disappeared during harness resume replay", payload.Invocation.ID)
	}
	if payload.Manual {
		if !invocation.AttachRequired {
			harnessRuntime, runtimeErr := h.lifecycle.HarnessRuntime(payload.SocketPath, payload.Invocation.Harness)
			if runtimeErr != nil {
				return store.Run{}, fmt.Errorf("ensure harness for manual native resume replay: %w", runtimeErr)
			}
			reserved := *invocation
			reserved.AttachRequired = !harnessRuntime.Capabilities().Headless
			reserved.UpdatedAt = h.now().UTC()
			if err := invocationStore.SaveInvocation(ctx, reserved); err != nil {
				return store.Run{}, fmt.Errorf("reserve replayed manual native session resume: %w", err)
			}
			session, resumeErr := harnessRuntime.Resume(ctx, payload.Request)
			if resumeErr != nil {
				classified := classifyHarnessRuntimeError(harnessRuntime, resumeErr)
				if harness.IsRateLimited(classified) || harness.IsAuthenticationExpired(classified) {
					return store.Run{}, rollbackReplayedHarnessResume(ctx, runStore, effect, *invocation, classified, "manual")
				}
				return store.Run{}, fmt.Errorf("resume native harness session during manual replay: %w", classified)
			}
			if session.NativeSessionID != "" {
				reserved.NativeSessionID = session.NativeSessionID
			}
			reserved.UpdatedAt = h.now().UTC()
			if err := invocationStore.SaveInvocation(ctx, reserved); err != nil {
				return store.Run{}, fmt.Errorf("persist replayed manual native session: %w", err)
			}
		}
	} else if invocation.RecoveryResumeCount < payload.TargetResumeCount {
		reserved := *invocation
		reserved.RecoveryResumeCount = payload.TargetResumeCount
		reserved.UpdatedAt = h.now().UTC()
		if err := invocationStore.SaveInvocation(ctx, reserved); err != nil {
			return store.Run{}, fmt.Errorf("reserve replayed native session resume: %w", err)
		}
		harnessRuntime, runtimeErr := h.lifecycle.HarnessRuntime(payload.SocketPath, payload.Invocation.Harness)
		if runtimeErr != nil {
			return store.Run{}, fmt.Errorf("ensure harness for native resume replay: %w", runtimeErr)
		}
		session, resumeErr := harnessRuntime.Resume(ctx, payload.Request)
		if resumeErr != nil {
			classified := classifyHarnessRuntimeError(harnessRuntime, resumeErr)
			if harness.IsRateLimited(classified) || harness.IsAuthenticationExpired(classified) {
				return store.Run{}, rollbackReplayedHarnessResume(ctx, runStore, effect, *invocation, classified, "automatic")
			}
			return store.Run{}, fmt.Errorf("resume native harness session during replay: %w", classified)
		}
		if session.NativeSessionID != "" {
			reserved.NativeSessionID = session.NativeSessionID
		}
		reserved.UpdatedAt = h.now().UTC()
		if err := invocationStore.SaveInvocation(ctx, reserved); err != nil {
			return store.Run{}, fmt.Errorf("persist replayed native session: %w", err)
		}
	}
	if err := clearReplayedEffect(ctx, runStore, effect, "harness resume"); err != nil {
		return store.Run{}, err
	}
	return readRunAfterReplay(ctx, h.projector, runStore, effect.RunID, "harness resume replay")
}

// rollbackReplayedHarnessResume restores the invocation and clears a replay
// reservation when the adapter rejects the native resume before its boundary.
// Rate and authentication failures are retryable classifications, so replay
// must leave the automatic ceiling untouched and let the coordinator publish
// the appropriate waiting state.
func rollbackReplayedHarnessResume(ctx context.Context, runStore RunStore, effect store.PendingEffect, original store.Invocation, classified error, mode string) error {
	invocationStore, ok := runStore.(InvocationStore)
	if !ok {
		return errors.Join(classified, fmt.Errorf("rollback %s harness resume: invocation store unavailable", mode))
	}
	if err := invocationStore.SaveInvocation(ctx, original); err != nil {
		return errors.Join(classified, fmt.Errorf("rollback %s harness resume: %w", mode, err))
	}
	journal, ok := runStore.(PendingEffectStore)
	if !ok {
		return classified
	}
	if err := journal.ClearPendingEffect(ctx, effect.RunID, effect.ID); err != nil {
		return errors.Join(classified, fmt.Errorf("clear %s harness resume after rollback: %w", mode, err))
	}
	return classified
}

// harnessResumeRecord is the durable intent recorded for one native-session
// resume. Recovery reads it to decide whether the one-resume ceiling was
// already consumed before a restart.
type harnessResumeRecord struct {
	// InvocationID identifies the invocation the resume belongs to.
	InvocationID string
	// Harness is the non-secret adapter identity for a bounded wait message.
	Harness string
	// TargetResumeCount is the automatic ceiling the reservation claimed.
	TargetResumeCount int
	// Manual marks an operator-requested resume.
	Manual bool
}

// ReadHarnessResume decodes one harness-resume journal entry.
func ReadHarnessResume(pending store.PendingEffect) (harnessResumeRecord, error) {
	if pending.Kind != store.PendingEffectKindHarnessResume {
		return harnessResumeRecord{}, fmt.Errorf("expected harness_resume effect, got %s", pending.Kind)
	}
	var payload harnessResumeEffectPayload
	if err := decodePendingEffect(pending, &payload); err != nil {
		return harnessResumeRecord{}, err
	}
	return harnessResumeRecord{
		InvocationID:      payload.Invocation.ID,
		Harness:           payload.Invocation.Harness,
		TargetResumeCount: payload.TargetResumeCount,
		Manual:            payload.Manual,
	}, nil
}
