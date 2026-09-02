package effect_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/effect"
	"github.com/Stevie1704/sw-factory/internal/store"
)

type payloadForTest struct {
	Message string `json:"message"`
	Count   int    `json:"count"`
}

type journalForTest struct {
	pending  *store.PendingEffect
	steps    *[]string
	saveErr  error
	clearErr error
}

type abandonerForTest struct {
	runID    string
	effectID string
	reason   string
}

func (a *abandonerForTest) AbandonPendingEffect(_ context.Context, runID, effectID, reason string) error {
	a.runID = runID
	a.effectID = effectID
	a.reason = reason
	return nil
}

type replayHandlerForTest struct {
	calls int
}

func (h *replayHandlerForTest) Replay(_ context.Context, request effect.ReplayRequest) (store.Run, error) {
	h.calls++
	return store.Run{ID: request.Effect.RunID}, nil
}

func (j *journalForTest) PendingEffect(_ context.Context, _ string) (*store.PendingEffect, error) {
	if j.pending == nil {
		return nil, nil
	}
	copy := *j.pending
	return &copy, nil
}

func (j *journalForTest) SavePendingEffect(_ context.Context, pending store.PendingEffect) error {
	*j.steps = append(*j.steps, "reserve")
	if j.saveErr != nil {
		return j.saveErr
	}
	copy := pending
	j.pending = &copy
	return nil
}

func (j *journalForTest) ClearPendingEffect(_ context.Context, runID, effectID string) error {
	*j.steps = append(*j.steps, "complete")
	if j.clearErr != nil {
		return j.clearErr
	}
	if j.pending != nil && j.pending.RunID == runID && j.pending.ID == effectID {
		j.pending = nil
	}
	return nil
}

// TestPendingEffectIDPreservesLegacyIdentityEncoding protects the durable
// identity bytes used by journal entries written before the package split.
func TestPendingEffectIDPreservesLegacyIdentityEncoding(t *testing.T) {
	got := effect.PendingEffectID("run-1", store.PendingEffectKindPush, "worktree\x00main\x00abc")
	want := "push:0a1e00552cdf127f48d3dc6cbd133268ec4529ed66dea3faa86243aa63f7b2ec"
	if got != want {
		t.Fatalf("PendingEffectID() = %q, want %q", got, want)
	}
}

// TestPendingEffectRoundTripsPayloadAndMetadata verifies that a replay intent
// keeps its legacy identity, JSON payload, and coordinator timestamp together.
func TestPendingEffectRoundTripsPayloadAndMetadata(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 34, 56, 789, time.FixedZone("test", 3600))
	wantPayload := `{"message":"resume","count":7}`

	got, err := effect.NewPendingEffect(now, "run-1", store.PendingEffectKindHarnessResume, "invocation-7\x003", payloadForTest{Message: "resume", Count: 7})
	if err != nil {
		t.Fatalf("NewPendingEffect() error = %v", err)
	}
	if got.ID != effect.PendingEffectID("run-1", store.PendingEffectKindHarnessResume, "invocation-7\x003") {
		t.Fatalf("NewPendingEffect() ID = %q, want the protocol identity", got.ID)
	}
	if got.Payload != wantPayload {
		t.Fatalf("NewPendingEffect() payload = %q, want %q", got.Payload, wantPayload)
	}
	if !got.CreatedAt.Equal(now.UTC()) || !got.UpdatedAt.Equal(now.UTC()) {
		t.Fatalf("NewPendingEffect() timestamps = %s/%s, want %s", got.CreatedAt, got.UpdatedAt, now.UTC())
	}

	var decoded payloadForTest
	if err := effect.DecodePendingEffect(got, &decoded); err != nil {
		t.Fatalf("DecodePendingEffect() error = %v", err)
	}
	if decoded != (payloadForTest{Message: "resume", Count: 7}) {
		t.Fatalf("DecodePendingEffect() = %#v, want %#v", decoded, payloadForTest{Message: "resume", Count: 7})
	}
}

// TestDecodePendingEffectRejectsMalformedPayload verifies corrupt journal data
// is reported as a decode failure rather than silently accepted.
func TestDecodePendingEffectRejectsMalformedPayload(t *testing.T) {
	pending := store.PendingEffect{Kind: store.PendingEffectKindPush, Payload: `{"payload":`}
	var decoded payloadForTest
	err := effect.DecodePendingEffect(pending, &decoded)
	if err == nil || !strings.Contains(err.Error(), "decode push effect") {
		t.Fatalf("DecodePendingEffect() error = %v, want a typed-context decode error", err)
	}
}

// TestWithPendingEffectReservesBeforeApplying verifies that the journal entry
// is durable before the external mutation and is cleared only after success.
func TestWithPendingEffectReservesBeforeApplying(t *testing.T) {
	steps := []string{}
	journal := &journalForTest{steps: &steps}
	pending := store.PendingEffect{RunID: "run-1", ID: "effect-1", Kind: store.PendingEffectKindPush, Payload: `{}`}

	if err := effect.WithPendingEffect(context.Background(), journal, pending, func() error {
		steps = append(steps, "apply")
		return nil
	}); err != nil {
		t.Fatalf("WithPendingEffect() error = %v", err)
	}
	if got, want := steps, []string{"reserve", "apply", "complete"}; !equalStrings(got, want) {
		t.Fatalf("protocol steps = %#v, want %#v", got, want)
	}
	if journal.pending != nil {
		t.Fatalf("pending effect after success = %#v, want nil", journal.pending)
	}
}

// TestWithPendingEffectRetainsReservationAfterApplyFailure verifies that a
// lost external response remains available for later reconciliation.
func TestWithPendingEffectRetainsReservationAfterApplyFailure(t *testing.T) {
	steps := []string{}
	journal := &journalForTest{steps: &steps}
	pending := store.PendingEffect{RunID: "run-1", ID: "effect-1", Kind: store.PendingEffectKindPush, Payload: `{}`}
	applyErr := errors.New("response lost after external mutation")

	err := effect.WithPendingEffect(context.Background(), journal, pending, func() error {
		steps = append(steps, "apply")
		return applyErr
	})
	if !errors.Is(err, applyErr) {
		t.Fatalf("WithPendingEffect() error = %v, want %v", err, applyErr)
	}
	if got, want := steps, []string{"reserve", "apply"}; !equalStrings(got, want) {
		t.Fatalf("protocol steps = %#v, want %#v", got, want)
	}
	if journal.pending == nil || journal.pending.ID != pending.ID {
		t.Fatalf("pending effect after apply failure = %#v, want %q", journal.pending, pending.ID)
	}
}

// TestWithPendingEffectDoesNotApplyWhenReservationFails verifies the fail
// closed boundary before an external mutation can begin.
func TestWithPendingEffectDoesNotApplyWhenReservationFails(t *testing.T) {
	steps := []string{}
	reserveErr := errors.New("reservation unavailable")
	journal := &journalForTest{steps: &steps, saveErr: reserveErr}
	pending := store.PendingEffect{RunID: "run-1", ID: "effect-1", Kind: store.PendingEffectKindPush, Payload: `{}`}

	err := effect.WithPendingEffect(context.Background(), journal, pending, func() error {
		steps = append(steps, "apply")
		return nil
	})
	if !errors.Is(err, reserveErr) {
		t.Fatalf("WithPendingEffect() error = %v, want %v", err, reserveErr)
	}
	if got, want := steps, []string{"reserve"}; !equalStrings(got, want) {
		t.Fatalf("protocol steps = %#v, want %#v", got, want)
	}
	if journal.pending != nil {
		t.Fatalf("pending effect after reservation failure = %#v, want nil", journal.pending)
	}
}

// TestWithPendingEffectReportsCompletionFailure verifies that a failed clear
// remains visible to reconciliation instead of being reported as complete.
func TestWithPendingEffectReportsCompletionFailure(t *testing.T) {
	steps := []string{}
	completeErr := errors.New("completion unavailable")
	journal := &journalForTest{steps: &steps, clearErr: completeErr}
	pending := store.PendingEffect{RunID: "run-1", ID: "effect-1", Kind: store.PendingEffectKindPush, Payload: `{}`}

	err := effect.WithPendingEffect(context.Background(), journal, pending, func() error {
		steps = append(steps, "apply")
		return nil
	})
	if !errors.Is(err, completeErr) {
		t.Fatalf("WithPendingEffect() error = %v, want %v", err, completeErr)
	}
	if got, want := steps, []string{"reserve", "apply", "complete"}; !equalStrings(got, want) {
		t.Fatalf("protocol steps = %#v, want %#v", got, want)
	}
	if journal.pending == nil || journal.pending.ID != pending.ID {
		t.Fatalf("pending effect after completion failure = %#v, want %q", journal.pending, pending.ID)
	}
}

// TestWithPendingEffectKeepsLegacyDirectExecutionFallback verifies that an
// older operational-store adapter still executes the mutation directly.
func TestWithPendingEffectKeepsLegacyDirectExecutionFallback(t *testing.T) {
	called := false
	pending := store.PendingEffect{RunID: "run-1", ID: "effect-1", Kind: store.PendingEffectKindPush, Payload: `{}`}
	if err := effect.WithPendingEffect(context.Background(), struct{}{}, pending, func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("WithPendingEffect() error = %v", err)
	}
	if !called {
		t.Fatal("legacy direct execution did not run the mutation")
	}
}

// TestPendingEffectAbandonerExposesOnlyStoreCapability verifies the kernel's
// abandonment contract carries the exact bounded arguments to the store.
func TestPendingEffectAbandonerExposesOnlyStoreCapability(t *testing.T) {
	abandoner := &abandonerForTest{}
	var capability effect.PendingEffectAbandoner = abandoner
	if err := capability.AbandonPendingEffect(context.Background(), "run-1", "effect-1", "verified remotely"); err != nil {
		t.Fatalf("AbandonPendingEffect() error = %v", err)
	}
	if abandoner.runID != "run-1" || abandoner.effectID != "effect-1" || abandoner.reason != "verified remotely" {
		t.Fatalf("abandonment arguments = %#v, want run/effect/reason", abandoner)
	}
}

// TestReplayDispatcherUsesRegisteredHandler verifies that a kind is routed
// through the handler registered at the kernel seam.
func TestReplayDispatcherUsesRegisteredHandler(t *testing.T) {
	dispatcher := effect.NewDispatcher()
	handler := &replayHandlerForTest{}
	if err := dispatcher.Register(store.PendingEffectKindPush, handler); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	pending := store.PendingEffect{RunID: "run-1", ID: "effect-1", Kind: store.PendingEffectKindPush, Payload: `{}`}
	got, err := dispatcher.Replay(context.Background(), struct{ Name string }{Name: "store"}, pending)
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if got.ID != pending.RunID || handler.calls != 1 {
		t.Fatalf("Replay() = %#v with %d handler calls, want run and one call", got, handler.calls)
	}
}

// TestReplayDispatcherRejectsUnknownKindWithTypedError verifies that an
// unregistered journal kind is fail-closed and inspectable by callers.
func TestReplayDispatcherRejectsUnknownKindWithTypedError(t *testing.T) {
	dispatcher := effect.NewDispatcher()
	pending := store.PendingEffect{RunID: "run-1", ID: "effect-1", Kind: store.PendingEffectKind("future_kind"), Payload: `{}`}
	_, err := dispatcher.Replay(context.Background(), struct{}{}, pending)
	var unknown *effect.UnknownKindError
	if !errors.As(err, &unknown) {
		t.Fatalf("Replay() error = %v, want *UnknownKindError", err)
	}
	if unknown.Kind != pending.Kind {
		t.Fatalf("UnknownKindError.Kind = %q, want %q", unknown.Kind, pending.Kind)
	}
}

func equalStrings(left, right []string) bool {
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
