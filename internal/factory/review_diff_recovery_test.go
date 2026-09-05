package factory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/prompt"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// TestRecoveryRejectsAChangedCurrentReviewArtifact verifies restart diagnosis
// reports an artifact identity discrepancy and does not regenerate content.
func TestRecoveryRejectsAChangedCurrentReviewArtifact(t *testing.T) {
	directory := t.TempDir()
	original := []byte("diff --git a/file b/file\n-old\n+new\n")
	digest := sha256.Sum256(original)
	if err := os.WriteFile(filepath.Join(directory, reviewDiffFileName), []byte("changed artifact\n"), 0o600); err != nil {
		t.Fatalf("write changed artifact: %v", err)
	}
	run := store.Run{ID: "run-review-artifact", CheckpointSHA: strings.Repeat("a", 64)}
	invocation := store.Invocation{
		ID: "inv-review-artifact", RunID: run.ID, Role: workflow.RoleSpecificationReview,
		Stage: store.StageReview, PromptVersion: workflow.PromptVersionSpecificationReview,
		InvocationDirectory: directory, Status: store.InvocationStatusCannotProceed,
	}
	if err := writeInvocationPacket(directory, InvocationPacket{
		SchemaVersion: invocationPacketVersion,
		InvocationID:  invocation.ID,
		RunID:         run.ID,
		Role:          invocation.Role,
		Stage:         invocation.Stage,
		PromptVersion: invocation.PromptVersion,
		ReviewContext: &prompt.ReviewContext{
			CheckpointSHA: run.CheckpointSHA,
			DiffPath:      prompt.WorkerReviewDiffPath,
			DiffBytes:     int64(len(original)),
			DiffSHA256:    hex.EncodeToString(digest[:]),
		},
	}); err != nil {
		t.Fatalf("write review packet: %v", err)
	}
	runStore := &terminalProjectionStore{repairLaunchStore: &repairLaunchStore{run: run, invocations: map[string]store.Invocation{invocation.ID: invocation}}}
	diagnosis := newRecoveryDiagnosis(run.ID)
	(&Service{}).inspectInvocationProjection(context.Background(), &diagnosis, config.RepositoryRegistration{}, runStore, run)
	if len(diagnosis.Discrepancies) != 1 || diagnosis.Discrepancies[0].Source != "review diff" || diagnosis.Discrepancies[0].Field != "artifact" {
		t.Fatalf("recovery discrepancies = %#v, want one review-artifact discrepancy", diagnosis.Discrepancies)
	}
	artifact, err := os.ReadFile(filepath.Join(directory, reviewDiffFileName))
	if err != nil {
		t.Fatalf("read changed artifact: %v", err)
	}
	if string(artifact) != "changed artifact\n" {
		t.Fatalf("recovery regenerated artifact = %q, want unchanged content", artifact)
	}
}

// TestValidatePersistedReviewDiffRejectsMissingAndNonRegularArtifacts verifies
// the materialisation seam fails closed for absent and replaced files.
func TestValidatePersistedReviewDiffRejectsMissingAndNonRegularArtifacts(t *testing.T) {
	directory := t.TempDir()
	content := []byte("review evidence")
	digest := sha256.Sum256(content)
	invocation := store.Invocation{InvocationDirectory: directory}
	if err := os.WriteFile(filepath.Join(directory, reviewDiffFileName), content, 0o600); err != nil {
		t.Fatalf("write review artifact: %v", err)
	}
	if err := writeInvocationPacket(directory, InvocationPacket{
		SchemaVersion: invocationPacketVersion,
		ReviewContext: &prompt.ReviewContext{
			DiffPath:   prompt.WorkerReviewDiffPath,
			DiffBytes:  int64(len(content)),
			DiffSHA256: hex.EncodeToString(digest[:]),
		},
	}); err != nil {
		t.Fatalf("write review packet: %v", err)
	}
	if err := validatePersistedReviewDiff(invocation); err != nil {
		t.Fatalf("validatePersistedReviewDiff() valid artifact error = %v", err)
	}
	if err := os.Remove(filepath.Join(directory, reviewDiffFileName)); err != nil {
		t.Fatalf("remove review artifact: %v", err)
	}
	if err := validatePersistedReviewDiff(invocation); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("validatePersistedReviewDiff() missing artifact error = %v, want not-exist refusal", err)
	}
	if err := os.WriteFile(filepath.Join(directory, reviewDiffFileName), content, 0o600); err != nil {
		t.Fatalf("restore review artifact: %v", err)
	}
	linkTarget := filepath.Join(directory, "target")
	if err := os.WriteFile(linkTarget, content, 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	if err := os.Remove(filepath.Join(directory, reviewDiffFileName)); err != nil {
		t.Fatalf("remove regular artifact: %v", err)
	}
	if err := os.Symlink(linkTarget, filepath.Join(directory, reviewDiffFileName)); err != nil {
		t.Fatalf("create artifact symlink: %v", err)
	}
	if err := validatePersistedReviewDiff(invocation); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("validatePersistedReviewDiff() symlink error = %v, want regular-file refusal", err)
	}
}
