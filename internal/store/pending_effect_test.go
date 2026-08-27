package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPendingEffectSurvivesStoreRestart verifies the single pending-effect
// journal is durable and accepts an idempotent retry of the same effect.
func TestPendingEffectSurvivesStoreRestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "factory.db")
	created := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	effect := PendingEffect{
		RunID:     "run-1",
		ID:        "effect-1",
		Kind:      PendingEffectKindLabelTransition,
		Payload:   `{"labels":["agent-running"]}`,
		CreatedAt: created,
		UpdatedAt: created,
	}

	opened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.SavePendingEffect(ctx, effect); err != nil {
		t.Fatalf("SavePendingEffect() error = %v", err)
	}
	if err := opened.SavePendingEffect(ctx, effect); err != nil {
		t.Fatalf("idempotent SavePendingEffect() error = %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.PendingEffect(ctx, effect.RunID)
	if err != nil {
		t.Fatalf("PendingEffect() error = %v", err)
	}
	if got == nil || got.ID != effect.ID || got.Kind != effect.Kind || got.Payload != effect.Payload {
		t.Fatalf("PendingEffect() = %#v, want %#v", got, effect)
	}
}

// TestPendingEffectRejectsASecondEffect verifies the one-effect invariant is
// enforced by the operational store rather than by process-local callers.
func TestPendingEffectRejectsASecondEffect(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(ctx, filepath.Join(directory, "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	first := PendingEffect{RunID: "run-1", ID: "effect-1", Kind: PendingEffectKindPush, Payload: "{}"}
	second := PendingEffect{RunID: "run-1", ID: "effect-2", Kind: PendingEffectKindPullRequest, Payload: "{}"}
	if err := opened.SavePendingEffect(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := opened.SavePendingEffect(ctx, second); !errors.Is(err, ErrPendingEffectConflict) {
		t.Fatalf("second SavePendingEffect() error = %v, want ErrPendingEffectConflict", err)
	}
	if err := opened.ClearPendingEffect(ctx, first.RunID, first.ID); err != nil {
		t.Fatalf("ClearPendingEffect() error = %v", err)
	}
	cleared, err := opened.PendingEffect(ctx, first.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if cleared != nil {
		t.Fatalf("PendingEffect() after clear = %#v, want nil", cleared)
	}
}

// TestPendingEffectRetryRefreshesTheReplayPayload verifies that re-reserving
// the same effect identity with a newer payload succeeds. Effect payloads embed
// volatile state such as the run's update time and a freshly read issue
// snapshot, so a retried attempt after a transient adapter failure never
// produces a byte-identical payload. Treating that as a conflict would block
// every later reservation for the run until the process restarted.
func TestPendingEffectRetryRefreshesTheReplayPayload(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(ctx, filepath.Join(directory, "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()

	created := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	first := PendingEffect{
		RunID: "run-1", ID: "state_transition:abc", Kind: PendingEffectKindStateTransition,
		Payload:   `{"Next":{"Revision":5,"UpdatedAt":"2026-08-27T10:00:00Z"}}`,
		CreatedAt: created, UpdatedAt: created,
	}
	if err := opened.SavePendingEffect(ctx, first); err != nil {
		t.Fatalf("first SavePendingEffect() error = %v", err)
	}
	retry := first
	retry.Payload = `{"Next":{"Revision":5,"UpdatedAt":"2026-08-27T10:00:05Z"}}`
	retry.UpdatedAt = created.Add(5 * time.Second)
	if err := opened.SavePendingEffect(ctx, retry); err != nil {
		t.Fatalf("retried SavePendingEffect() error = %v, want the reservation to be refreshed", err)
	}
	current, err := opened.PendingEffect(ctx, first.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if current == nil || current.Payload != retry.Payload {
		t.Fatalf("PendingEffect() payload = %#v, want the retried payload %q", current, retry.Payload)
	}
	if current.CreatedAt.UTC() != created {
		t.Fatalf("PendingEffect() created_at = %s, want the original reservation time %s", current.CreatedAt, created)
	}
	// A genuinely different effect must still be refused.
	other := PendingEffect{
		RunID: "run-1", ID: "push:def", Kind: PendingEffectKindPush, Payload: "{}",
		CreatedAt: created, UpdatedAt: created,
	}
	if err := opened.SavePendingEffect(ctx, other); !errors.Is(err, ErrPendingEffectConflict) {
		t.Fatalf("SavePendingEffect() for a different effect = %v, want ErrPendingEffectConflict", err)
	}
}
