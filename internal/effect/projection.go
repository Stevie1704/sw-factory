package effect

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// WorkflowProjectionError marks a deterministic conflict between a replay
// payload and the newer persisted workflow projection. It lets reconciliation
// distinguish workflow ownership from external infrastructure uncertainty.
type WorkflowProjectionError struct {
	cause error
}

// Error returns the deterministic workflow conflict.
func (e *WorkflowProjectionError) Error() string {
	if e == nil || e.cause == nil {
		return "deterministic workflow projection conflict"
	}
	return e.cause.Error()
}

// Unwrap exposes the underlying bounded workflow conflict.
func (e *WorkflowProjectionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// workflowProjectionFailuref creates a typed deterministic replay conflict.
func workflowProjectionFailuref(format string, arguments ...any) error {
	return &WorkflowProjectionError{cause: fmt.Errorf(format, arguments...)}
}

// ValidateRunBeforeEffect rejects a run projection before its external effect
// is reserved, keeping operational-store validation ahead of the journal and
// every mutation that the journal protects.
func ValidateRunBeforeEffect(kind store.PendingEffectKind, run store.Run) error {
	if err := store.ValidateRun(run); err != nil {
		return fmt.Errorf("validate run before reserving %s effect: %w", kind, err)
	}
	return nil
}

// validateRunBeforeReplay rejects an invalid durable run projection before a
// replay can repeat any external mutation. Workflow classification preserves
// the refusal as the cause of reconciliation rather than infrastructure drift.
func validateRunBeforeReplay(kind store.PendingEffectKind, run store.Run) error {
	if err := store.ValidateRun(run); err != nil {
		return workflowProjectionFailuref("validate %s run projection before replay: %v", kind, err)
	}
	return nil
}

// reserve encodes one replay intent with the journal clock so every record
// shares the run's operational time source.
func reserve(now func() time.Time, runID string, kind store.PendingEffectKind, identity string, payload any) (store.PendingEffect, error) {
	return NewPendingEffect(now().UTC(), runID, kind, identity, payload)
}

// applier adapts a handler's effect action to the kernel's interface-valued
// application seam.
type applier func() error

// Apply runs the handler's effect action.
func (a applier) Apply() error { return a() }

// replayStore narrows the kernel's opaque store value back to the run
// projection seam every handler needs.
func replayStore(request ReplayRequest) (RunStore, error) {
	runStore, ok := request.Store.(RunStore)
	if !ok {
		return nil, errors.New("pending effect replay requires a run store")
	}
	return runStore, nil
}

// clearReplayedEffect acknowledges a completed replay when the store carries a
// journal. Stores without one never reserved the effect in the first place.
func clearReplayedEffect(ctx context.Context, runStore RunStore, pending store.PendingEffect, description string) error {
	journal, ok := runStore.(PendingEffectStore)
	if !ok {
		return nil
	}
	if err := journal.ClearPendingEffect(ctx, pending.RunID, pending.ID); err != nil {
		return fmt.Errorf("clear replayed %s: %w", description, err)
	}
	return nil
}

// readRunAfterReplay returns the durable run projection a replay leaves
// unchanged, refusing a run that disappeared mid-recovery.
func readRunAfterReplay(ctx context.Context, projector RunProjector, runStore RunStore, runID, activity string) (store.Run, error) {
	run, err := projector.Read(ctx, runStore)
	if err != nil {
		return store.Run{}, fmt.Errorf("read run after %s: %w", activity, err)
	}
	if run == nil {
		return store.Run{}, fmt.Errorf("run %q disappeared during %s", runID, activity)
	}
	return *run, nil
}

// sameStringSlice compares complete ordered projections for idempotent PUT
// suppression while preserving ordinary labels exactly as returned by GitHub.
func sameStringSlice(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// sameCommitStatus compares every semantic field that makes a status replay
// safe; GitHub may assign its own numeric status identity.
func sameCommitStatus(left, right github.CommitStatus) bool {
	return left.SHA == right.SHA && left.State == right.State && left.Context == right.Context && left.Description == right.Description && left.TargetURL == right.TargetURL
}

// samePullRequestRequest suppresses a redundant update after an interrupted
// create or update has already reached GitHub.
func samePullRequestRequest(existing github.PullRequest, request github.PullRequestRequest) bool {
	return existing.Title == request.Title && existing.Body == request.Body && existing.Draft == request.Draft &&
		existing.HeadBranch == request.HeadBranch && existing.BaseBranch == request.BaseBranch
}
