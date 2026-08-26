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
