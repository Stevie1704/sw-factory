package effect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// ResultAcceptance is the complete durable intent for accepting one validated
// visible report. The caller resolves its repository registration to the
// repository, socket path, and worker identity before reserving the effect.
type ResultAcceptance struct {
	// Repository is the tracked GitHub repository.
	Repository github.Repository
	// SocketPath locates the terminal a replayed harness command reattaches to.
	SocketPath string
	// WorkerID identifies the worker that owns the accepted invocation.
	WorkerID string
	// Harness finalizes the accepted native session.
	Harness harness.Runtime
	// Session is the native session being finalized.
	Session harness.Session
	// Invocation is the terminal invocation projection.
	Invocation store.Invocation
	// Previous and Next are the run revisions the acceptance moves between.
	Previous store.Run
	Next     store.Run
	// StopWorker keeps worker shutdown inside the acceptance effect.
	StopWorker bool
	// Report is the accepted report snapshot encoded for deterministic replay.
	Report report.Report
}

// resultAcceptanceHandler owns harness finalization, invocation state, and the
// resulting workflow projection as one replayable acceptance operation.
type resultAcceptanceHandler struct {
	now       func() time.Time
	issues    issueClient
	labels    issueProjection
	projector runProjector
	lifecycle lifecycle
}

// AcceptResult journals harness finalization, invocation state, and the
// resulting workflow projection as one replayable acceptance operation.
func (j *Journal) AcceptResult(ctx context.Context, runStore RunStore, invocationStore InvocationStore, request ResultAcceptance) (store.Invocation, store.Run, error) {
	handler := mustApplyHandler[resultAcceptanceHandler](j.dispatcher, store.PendingEffectKindResultAcceptance)
	return handler.accept(ctx, runStore, invocationStore, request)
}

// accept reserves and performs one report acceptance.
func (h resultAcceptanceHandler) accept(ctx context.Context, runStore RunStore, invocationStore InvocationStore, request ResultAcceptance) (store.Invocation, store.Run, error) {
	invocation := request.Invocation
	next := request.Next
	previous := request.Previous
	if err := validateRunBeforeEffect(store.PendingEffectKindResultAcceptance, next); err != nil {
		return invocation, next, err
	}
	// Encode the accepted report once for durable replay
	acceptedReportJSON, err := json.Marshal(request.Report)
	if err != nil {
		return invocation, next, fmt.Errorf("encode accepted report for effect: %w", err)
	}
	payload := resultAcceptanceEffectPayload{
		Repository:     request.Repository,
		SocketPath:     request.SocketPath,
		Issue:          github.Issue{Number: next.IssueNumber},
		Session:        request.Session,
		Invocation:     invocation,
		WorkerID:       request.WorkerID,
		Previous:       previous,
		Next:           next,
		StopWorker:     request.StopWorker,
		AcceptedReport: string(acceptedReportJSON),
	}
	// Keep the issue snapshot in the payload so replay does not have to infer
	// it from the mutable repository configuration.
	if h.issues != nil {
		if issue, err := h.issues.Issue(ctx, payload.Repository, next.IssueNumber); err == nil {
			payload.Issue = issue
		}
	}
	effect, err := reserve(h.now, next.ID, store.PendingEffectKindResultAcceptance, invocation.ID+"\x00"+string(invocation.Status)+"\x00"+fmt.Sprint(next.Revision), payload)
	if err != nil {
		return invocation, next, err
	}
	apply := func() error {
		currentInvocation, err := invocationStore.Invocation(ctx, invocation.RunID, invocation.ID)
		if err != nil {
			return fmt.Errorf("read invocation before accepting result: %w", err)
		}
		if currentInvocation == nil {
			return fmt.Errorf("invocation %q disappeared before accepting result", invocation.ID)
		}
		if currentInvocation.Status == store.InvocationStatusActive {
			// Mark the invocation terminal before Finish crosses the external
			// boundary. A restart after this reservation safely abandons Finish
			// because the native exit command has no observable idempotency key.
			if err := invocationStore.SaveInvocation(ctx, invocation); err != nil {
				return fmt.Errorf("reserve accepted invocation: %w", err)
			}
		} else if currentInvocation.Status != invocation.Status || currentInvocation.NativeSessionID != invocation.NativeSessionID {
			return fmt.Errorf("invocation %q changed while accepting result", invocation.ID)
		}
		if request.Harness == nil {
			return errors.New("harness runtime is required to accept result")
		}
		if err := request.Harness.Finish(ctx, request.Session); err != nil {
			// Fail-closed: return immediately without clearing the pending effect.
			// Worker shutdown and workflow projection will not advance while native
			// exit delivery remains unconfirmed. The pending effect remains until
			// completion is observable or explicit abandonment is requested.
			return fmt.Errorf("finish accepted harness session: %w", err)
		}
		if request.StopWorker {
			workerID := payload.WorkerID
			if workerID == "" {
				workerID = previous.ID
			}
			if err := h.lifecycle.StopWorker(ctx, workerID); err != nil {
				return err
			}
		}
		if err := h.labels.applyStateTransition(ctx, &next, StateTransition{
			Repository: payload.Repository,
			Issue:      payload.Issue,
			Previous:   previous,
			Next:       next,
		}); err != nil {
			return err
		}
		next.UpdatedAt = h.now().UTC()
		if err := h.projector.Save(ctx, runStore, next); err != nil {
			return fmt.Errorf("persist accepted result state: %w", err)
		}
		if err := h.projector.RecordTransition(ctx, runStore, previous, next, next.UpdatedAt); err != nil {
			return fmt.Errorf("record accepted result transition: %w", err)
		}
		return nil
	}
	if err := withPendingEffect(ctx, runStore, effect, applier(apply)); err != nil {
		return invocation, next, err
	}
	return invocation, next, nil
}

// Replay completes a report acceptance without repeating harness finalization
// after its terminal invocation reservation. The external exit command has no
// observable idempotency key, so an ambiguous prior attempt is safely
// abandoned while the durable projections converge.
func (h resultAcceptanceHandler) Replay(ctx context.Context, request replayRequest) (store.Run, error) {
	runStore, err := replayStore(request)
	if err != nil {
		return store.Run{}, err
	}
	effect := request.Effect
	var payload resultAcceptanceEffectPayload
	if err := decodePendingEffect(effect, &payload); err != nil {
		return store.Run{}, err
	}
	if err := validateRunBeforeReplay(store.PendingEffectKindResultAcceptance, payload.Next); err != nil {
		return store.Run{}, err
	}
	currentRun, err := h.projector.Read(ctx, runStore)
	if err != nil {
		return store.Run{}, fmt.Errorf("read run during result acceptance replay: %w", err)
	}
	next := payload.Next
	if currentRun != nil && currentRun.ID != effect.RunID {
		return store.Run{}, workflowProjectionFailuref("result acceptance replay belongs to run %q, current run is %q", effect.RunID, currentRun.ID)
	}
	if currentRun != nil && currentRun.Revision > next.Revision {
		return store.Run{}, workflowProjectionFailuref("result acceptance revision %d is older than current revision %d", next.Revision, currentRun.Revision)
	}
	if currentRun != nil && currentRun.Revision < next.Revision && currentRun.Revision != payload.Previous.Revision {
		return store.Run{}, workflowProjectionFailuref("result acceptance replay expected revision %d, found %d", payload.Previous.Revision, currentRun.Revision)
	}
	invocationStore, ok := runStore.(InvocationStore)
	if !ok {
		return store.Run{}, errors.New("operational store does not support result acceptance replay")
	}
	currentInvocation, err := invocationStore.Invocation(ctx, effect.RunID, payload.Invocation.ID)
	if err != nil {
		return store.Run{}, fmt.Errorf("read invocation during result acceptance replay: %w", err)
	}
	if currentInvocation == nil {
		return store.Run{}, fmt.Errorf("invocation %q disappeared during result acceptance replay", payload.Invocation.ID)
	}
	finishNeeded := currentInvocation.Status == store.InvocationStatusActive
	if finishNeeded {
		// Reserve the terminal invocation projection before replaying Finish.
		if err := invocationStore.SaveInvocation(ctx, payload.Invocation); err != nil {
			return store.Run{}, fmt.Errorf("reserve replayed accepted invocation: %w", err)
		}
	} else if currentInvocation.Status != payload.Invocation.Status || currentInvocation.NativeSessionID != payload.Invocation.NativeSessionID {
		return store.Run{}, workflowProjectionFailuref("invocation %q changed during result acceptance replay", payload.Invocation.ID)
	}
	if finishNeeded {
		harnessRuntime, runtimeErr := h.lifecycle.HarnessRuntime(payload.SocketPath, payload.Invocation.Harness)
		if runtimeErr != nil {
			return store.Run{}, fmt.Errorf("ensure harness for result acceptance replay: %w", runtimeErr)
		}
		if err := harnessRuntime.Finish(ctx, payload.Session); err != nil {
			// Fail-closed: do not advance worker shutdown or workflow projection
			// while native exit delivery remains unconfirmed. Retain the pending
			// effect so a subsequent reconciliation can retry or require explicit
			// abandonment before allowing projection advancement.
			return store.Run{}, fmt.Errorf("finish replayed harness session: %w", err)
		}
	}
	if payload.StopWorker {
		// Stopping is idempotent for the run-scoped worker. Repeat it even when
		// the invocation was already marked terminal before an earlier process
		// crossed this boundary; that state is the durable evidence that the
		// acceptance reservation was already in flight.
		workerID := payload.WorkerID
		if workerID == "" {
			workerID = effect.RunID
		}
		if err := h.lifecycle.StopWorker(ctx, workerID); err != nil {
			return store.Run{}, err
		}
	}
	if currentRun != nil && currentRun.Revision == next.Revision && currentRun.StatusCommentID != "" {
		next.StatusCommentID = currentRun.StatusCommentID
	}
	if err := h.labels.applyStateTransition(ctx, &next, StateTransition{
		Repository: payload.Repository,
		Issue:      payload.Issue,
		Previous:   payload.Previous,
		Next:       next,
	}); err != nil {
		return store.Run{}, fmt.Errorf("replay accepted result projection: %w", err)
	}
	next.UpdatedAt = h.now().UTC()
	if currentRun == nil || currentRun.Revision < next.Revision {
		if err := h.projector.Save(ctx, runStore, next); err != nil {
			return store.Run{}, fmt.Errorf("persist replayed accepted result: %w", err)
		}
	}
	if err := clearReplayedEffect(ctx, runStore, effect, "result acceptance"); err != nil {
		return store.Run{}, err
	}
	return next, nil
}

// resultAcceptanceRecord is the durable intent recorded for one accepted
// report. Recovery reads it to continue the coordinator projection after the
// journaled harness boundary has completed.
type resultAcceptanceRecord struct {
	// Invocation is the terminal invocation the acceptance reserved.
	Invocation store.Invocation
	// AcceptedReport is the JSON-encoded report snapshot. A journal entry
	// written before the snapshot existed leaves it empty.
	AcceptedReport string
}

// ReadResultAcceptance decodes one result-acceptance journal entry.
func ReadResultAcceptance(pending store.PendingEffect) (resultAcceptanceRecord, error) {
	if pending.Kind != store.PendingEffectKindResultAcceptance {
		return resultAcceptanceRecord{}, fmt.Errorf("expected result_acceptance effect, got %s", pending.Kind)
	}
	var payload resultAcceptanceEffectPayload
	if err := decodePendingEffect(pending, &payload); err != nil {
		return resultAcceptanceRecord{}, fmt.Errorf("decode result acceptance payload: %w", err)
	}
	return resultAcceptanceRecord{Invocation: payload.Invocation, AcceptedReport: payload.AcceptedReport}, nil
}
