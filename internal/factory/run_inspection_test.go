package factory_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/store"
	"golang.org/x/sys/unix"
)

// inspectionFixture registers a real local store with one historical run and
// returns the public service, store and private diagnostic location.
func inspectionFixture(t *testing.T) (*factory.Service, *store.Store, string) {
	t.Helper()
	root := t.TempDir()
	configPath, dbPath := registerBugReportRepository(t, root)
	db, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := db.SaveRun(t.Context(), store.Run{ID: "historical", RepositoryPath: filepath.Join(root, "acme-private-repo"), Worktree: filepath.Join(root, "worktrees", "historical"), Stage: store.StageCheck, Status: store.StatusComplete, CheckpointSHA: strings.Repeat("b", 40), CreatedAt: at, UpdatedAt: at}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "worktrees", ".factory-agents", "historical", "gate-failure", "checkpoint.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	return factory.NewWithDependencies(configPath, factory.Dependencies{OpenStore: func(context.Context, string) (factory.OperationalStore, error) {
		t.Fatal("inspection opened the mutation store")
		return nil, errors.New("mutation forbidden")
	}}), db, path
}

// TestRunInspectionChronologyUsesRetainedObservations verifies that failed
// gates at different checkpoints and separate invocations remain ordered facts.
func TestRunInspectionChronologyUsesRetainedObservations(t *testing.T) {
	t.Parallel()
	svc, db, _ := inspectionFixture(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, inv := range []store.Invocation{
		{ID: "attempt-2", CreatedAt: at.Add(3 * time.Minute), UpdatedAt: at.Add(4 * time.Minute)},
		{ID: "attempt-1", CreatedAt: at, UpdatedAt: at.Add(time.Minute)},
	} {
		inv.RunID = "historical"
		inv.Harness = "codex"
		inv.Role = "implementation"
		inv.Stage = store.StageImplementation
		inv.Status = store.InvocationStatusCompleted
		if err := db.SaveInvocation(t.Context(), inv); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SaveGateResults(t.Context(), []store.GateResult{
		{RunID: "historical", Phase: store.GatePhaseCheckpoint, CheckpointSHA: strings.Repeat("a", 40), GateName: "test", Outcome: store.GateOutcomeFailed, Status: "failure", Blocking: true, CreatedAt: at.Add(2 * time.Minute), UpdatedAt: at.Add(2 * time.Minute)},
		{RunID: "historical", Phase: store.GatePhaseCheckpoint, CheckpointSHA: strings.Repeat("b", 40), GateName: "test", Outcome: store.GateOutcomeFailed, Status: "failure", Blocking: true, CreatedAt: at.Add(5 * time.Minute), UpdatedAt: at.Add(5 * time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.RunInspect(t.Context(), "historical", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Timeline) != 6 || len(got.Gates) != 2 || len(got.Invocations) != 2 {
		t.Fatalf("retained history: %+v", got)
	}
	want := []string{"invocation.created", "invocation.observed", "gate.observed", "invocation.created", "invocation.observed", "gate.observed"}
	for i, event := range got.Timeline {
		if event.Kind != want[i] || (i > 0 && event.At.Before(got.Timeline[i-1].At)) {
			t.Fatalf("chronology: %+v", got.Timeline)
		}
	}
	if got.Timeline[2].CheckpointSHA != strings.Repeat("a", 40) || got.Timeline[2].Outcome != "failed" {
		t.Fatalf("gate observation: %+v", got.Timeline[2])
	}
	for offset := 0; offset < 6; offset += 2 {
		page, err := svc.RunInspect(t.Context(), "historical", offset, 2)
		if err != nil || len(page.Timeline) != 2 || page.Coverage.TimelineTotal != 6 {
			t.Fatalf("chronology page %d: %+v %v", offset, page, err)
		}
		if offset < 4 && (page.Coverage.NextOffset == nil || *page.Coverage.NextOffset != offset+2) {
			t.Fatalf("chronology cursor %d: %+v", offset, page.Coverage)
		}
		if offset == 4 && page.Coverage.NextOffset != nil {
			t.Fatalf("last chronology cursor: %+v", page.Coverage)
		}
	}
}

// TestRunEvidenceIsExplicitBoundedAndCheckpointHonest verifies metadata never
// includes raw output and a selected stale diagnostic is paged in bytes.
func TestRunEvidenceIsExplicitBoundedAndCheckpointHonest(t *testing.T) {
	t.Parallel()
	svc, _, path := inspectionFixture(t)
	const private = "PRIVATE-REPOSITORY-OUTPUT"
	content := "observed at: 2026-10-01T12:00:00Z\nrun: historical\nphase: checkpoint\ncheckpoint: " + strings.Repeat("a", 40) + "\n" + private + strings.Repeat("x", 300<<10)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	inspection, err := svc.RunInspect(t.Context(), "historical", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var ref factory.EvidenceReference
	for _, item := range inspection.Evidence {
		if item.Selector == "gate-failure.checkpoint" {
			ref = item
		}
	}
	if ref.State != "stale_checkpoint" || ref.CheckpointSHA != strings.Repeat("a", 40) || !ref.SourceTruncated || ref.ID == "" {
		t.Fatalf("evidence: %+v", ref)
	}
	encoded, _ := json.Marshal(inspection)
	if strings.Contains(string(encoded), private) {
		t.Fatal("metadata exposed output")
	}
	page, err := svc.RunEvidence(t.Context(), "historical", ref.ID, 0, 16)
	if err != nil {
		t.Fatal(err)
	}
	if page.Content != "observed at: 202" || page.NextOffset == nil || *page.NextOffset != 16 || !page.Truncated || page.Units != "bytes" {
		t.Fatalf("page: %+v", page)
	}
	last, err := svc.RunEvidence(t.Context(), "historical", ref.ID, factory.MaxRetainedEvidenceBytes-4, 16)
	if err != nil || last.Content != "xxxx" || last.NextOffset != nil || !last.Truncated || !last.Evidence.SourceTruncated {
		t.Fatalf("retained end: %+v %v", last, err)
	}
}

// TestRunEvidenceRejectsUnsafeOrUnownedFiles exercises public retrieval with
// traversal selectors, symlinks, replacement, FIFOs and forged run headers.
func TestRunEvidenceRejectsUnsafeOrUnownedFiles(t *testing.T) {
	t.Parallel()
	svc, _, path := inspectionFixture(t)
	content := "observed at: now\nrun: historical\nphase: checkpoint\ncheckpoint: " + strings.Repeat("b", 40) + "\nfailure: gate\n\nprivate output"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	inspection, err := svc.RunInspect(t.Context(), "historical", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var original factory.EvidenceReference
	for _, ref := range inspection.Evidence {
		if ref.Selector == "gate-failure.checkpoint" {
			original = ref
		}
	}
	for _, id := range []string{"../../etc/passwd", path, "gate-output." + strings.Repeat("c", 40), "unknown"} {
		if _, err := svc.RunEvidence(t.Context(), "historical", id, 0, 64); !errors.Is(err, factory.ErrEvidenceNotFound) {
			t.Fatalf("selector %q: %v", id, err)
		}
	}
	// Replacing identical bytes, with identical timestamps, still changes inode
	// identity and invalidates the previously selected evidence.
	replacement := path + ".replacement"
	if err := os.WriteFile(replacement, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, original.ModifiedAt, original.ModifiedAt); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunEvidence(t.Context(), "historical", original.ID, 0, 64); !errors.Is(err, factory.ErrEvidenceChanged) {
		t.Fatalf("replacement: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.log")
	if err := os.WriteFile(outside, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{outside, "other.log"} {
		if target == "other.log" {
			if err := os.WriteFile(filepath.Join(filepath.Dir(path), target), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.RunEvidence(t.Context(), "historical", original.ID, 0, 64); !errors.Is(err, factory.ErrEvidenceUnavailable) {
			t.Fatalf("symlink %s: %v", target, err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunEvidence(t.Context(), "historical", original.ID, 0, 64); !errors.Is(err, factory.ErrEvidenceUnavailable) {
		t.Fatalf("FIFO: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(content, "run: historical", "run: another-run", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunEvidence(t.Context(), "historical", original.ID, 0, 64); !errors.Is(err, factory.ErrEvidenceUnavailable) {
		t.Fatalf("ownership: %v", err)
	}
	inspection, err = svc.RunInspect(t.Context(), "historical", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range inspection.Evidence {
		if ref.Selector == "gate-failure.checkpoint" && ref.State != "unavailable" {
			t.Fatalf("unsafe metadata: %+v", ref)
		}
	}
}

// TestRunInspectionDistinguishesMissingCleanedAndUnknown checks absent local
// artifacts, retained cleanup summaries, unknown IDs and bounded empty pages.
func TestRunInspectionDistinguishesMissingCleanedAndUnknown(t *testing.T) {
	t.Parallel()
	svc, db, path := inspectionFixture(t)
	got, err := svc.RunInspect(t.Context(), "historical", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range got.Evidence {
		if ref.State != "absent" {
			t.Fatalf("missing artifact: %+v", ref)
		}
	}
	if err := db.SaveEvaluationSummary(t.Context(), store.EvaluationSummary{RunID: "cleaned", Outcome: store.EvaluationOutcomeComplete, StartedAt: time.Now().Add(-time.Hour), CompletedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	got, err = svc.RunInspect(t.Context(), "cleaned", 0, 100)
	if err != nil || got.State != "cleaned_up" {
		t.Fatalf("cleaned inspection: %+v %v", got, err)
	}
	page, err := svc.RunEvidence(t.Context(), "cleaned", "gate-failure.checkpoint", 0, 100)
	if err != nil || page.Evidence.State != "cleaned_up" || page.Content != "" {
		t.Fatalf("cleaned evidence: %+v %v", page, err)
	}
	if _, err := svc.RunInspect(t.Context(), "unknown", 0, 100); !errors.Is(err, factory.ErrRunNotFound) {
		t.Fatalf("unknown run: %v", err)
	}
	got, err = svc.RunInspect(t.Context(), "historical", int(^uint(0)>>1), 100)
	if err != nil || len(got.Evidence) != 0 || got.Coverage.NextOffset != nil {
		t.Fatalf("empty page: %+v %v", got, err)
	}
	// A symlink in the artifact directory chain must be refused as well.
	if err := os.Rename(filepath.Dir(path), filepath.Dir(path)+"-real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(filepath.Dir(path))+"-real", filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunEvidence(t.Context(), "historical", "gate-failure.checkpoint", 0, 100); !errors.Is(err, factory.ErrEvidenceUnavailable) {
		t.Fatalf("symlink directory: %v", err)
	}
}
