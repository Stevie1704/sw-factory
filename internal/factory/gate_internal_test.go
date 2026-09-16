package factory

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/gate"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestPersistGateSuiteRetainsConfiguredOrdinalsAcrossSubsetRuns verifies a
// later dependency-only run cannot renumber a gate persisted by a full suite.
func TestPersistGateSuiteRetainsConfiguredOrdinalsAcrossSubsetRuns(t *testing.T) {
	t.Parallel()

	configured := []config.GateConfig{
		{Name: "format"},
		{Name: "lint"},
		{Name: "test", DependsOn: []string{"format"}},
	}
	run := store.Run{ID: "run-gate-ordinals"}
	runStore := &gateOrdinalRunStore{results: make(map[string]store.GateResult)}
	full := gate.SuiteResult{Gates: []gate.Result{
		{GateName: "format"},
		{GateName: "lint"},
		{GateName: "test"},
	}}
	if err := persistGateSuite(context.Background(), runStore, run, configured, full); err != nil {
		t.Fatalf("persistGateSuite(full) error = %v", err)
	}
	subset := gate.SuiteResult{Gates: []gate.Result{
		{GateName: "format"},
		{GateName: "test"},
	}}
	if err := persistGateSuite(context.Background(), runStore, run, configured, subset); err != nil {
		t.Fatalf("persistGateSuite(subset) error = %v", err)
	}
	if got := runStore.results["test"].Ordinal; got != 2 {
		t.Fatalf("persisted test ordinal = %d, want full-suite ordinal 2", got)
	}
}

type gateOrdinalRunStore struct {
	results map[string]store.GateResult
}

func (s *gateOrdinalRunStore) CurrentRun(context.Context) (*store.Run, error) { return nil, nil }
func (s *gateOrdinalRunStore) Close() error                                   { return nil }
func (s *gateOrdinalRunStore) SaveRun(context.Context, store.Run) error       { return nil }
func (s *gateOrdinalRunStore) SaveGateResults(_ context.Context, results []store.GateResult) error {
	for _, result := range results {
		s.results[result.GateName] = result
	}
	return nil
}
func (s *gateOrdinalRunStore) GateResults(context.Context, string, store.GatePhase, string) ([]store.GateResult, error) {
	return nil, nil
}

const (
	// baselineIdentityBaseSHA is the immutable pre-edit checkpoint.
	baselineIdentityBaseSHA = "1111111111111111111111111111111111111111"
	// baselineIdentityHeadSHA is the implementation checkpoint the worktree
	// stands on after the agent committed its edits.
	baselineIdentityHeadSHA = "2222222222222222222222222222222222222222"
)

// checkpointFiles serves one tracked file per checkpoint so a verifier can be
// exercised against a worktree whose HEAD has moved past the baseline.
type checkpointFiles map[string]map[string][]byte

// ReadFileAtCheckpoint returns the content recorded for one exact checkpoint.
func (f checkpointFiles) ReadFileAtCheckpoint(_ context.Context, _, checkpointSHA, path string) ([]byte, error) {
	content, ok := f[checkpointSHA][path]
	if !ok {
		return nil, fmt.Errorf("no %q at checkpoint %q", path, checkpointSHA)
	}
	return content, nil
}

// baselineVerifierFixture persists one baseline suite in a real store and
// returns everything the read-only verifier consumes.
func baselineVerifierFixture(t *testing.T, files checkpointFiles, baselineCheckpoint, headCheckpoint string) (RunStore, store.Run, SpecificationPacket) {
	t.Helper()
	opened, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state", "factory.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	packet := SpecificationPacket{RepositoryConfig: config.RepositoryConfig{
		SetupFiles: []string{"python/pyproject.toml"},
		Gates:      []config.GateConfig{{Name: "build", Command: "build", Timeout: "1m", Blocking: true}},
	}}
	run := store.Run{ID: "run-baseline-identity", Worktree: t.TempDir(), BaseCheckpointSHA: baselineCheckpoint, CheckpointSHA: headCheckpoint}
	fingerprint, err := setupInputFingerprint(context.Background(), files, run.Worktree, baselineCheckpoint, packet.RepositoryConfig.SetupFiles)
	if err != nil {
		t.Fatalf("baseline fingerprint: %v", err)
	}
	if err := opened.SaveGateResults(context.Background(), []store.GateResult{{
		RunID:            run.ID,
		CheckpointSHA:    baselineCheckpoint,
		Phase:            store.GatePhaseBaseline,
		Ordinal:          0,
		GateName:         "build",
		Outcome:          store.GateOutcomePassed,
		Status:           "success",
		Blocking:         true,
		SetupFingerprint: fingerprint,
	}}); err != nil {
		t.Fatalf("save baseline gate results: %v", err)
	}
	return opened, run, packet
}

// TestBaselineReadinessAcceptsASetupFileEditedAfterTheBaseline verifies the
// verifier reads the configured setup files at the baseline checkpoint. An
// agent may legitimately add a dependency, and the working tree then holds a
// manifest that never belonged to the recorded baseline.
func TestBaselineReadinessAcceptsASetupFileEditedAfterTheBaseline(t *testing.T) {
	files := checkpointFiles{
		baselineIdentityBaseSHA: {"python/pyproject.toml": []byte("[project]\nname = \"sil\"\n")},
		baselineIdentityHeadSHA: {"python/pyproject.toml": []byte("[project]\nname = \"sil\"\n\n[project.scripts]\nsil-acc = \"sil.examples.acc:main\"\n")},
	}
	runStore, run, packet := baselineVerifierFixture(t, files, baselineIdentityBaseSHA, baselineIdentityHeadSHA)

	if err := ensureBaselineReadyForLaunch(context.Background(), files, runStore, run, packet); err != nil {
		t.Fatalf("ensureBaselineReadyForLaunch() error = %v, want an edited setup file to keep the baseline verifiable", err)
	}
}

// TestBaselineReadinessRejectsAChangedBaselineCheckpoint verifies the identity
// check still bites when the recorded fingerprint does not describe the
// dependency graph committed at the baseline checkpoint itself.
func TestBaselineReadinessRejectsAChangedBaselineCheckpoint(t *testing.T) {
	files := checkpointFiles{baselineIdentityBaseSHA: {"python/pyproject.toml": []byte("[project]\nname = \"sil\"\n")}}
	runStore, run, packet := baselineVerifierFixture(t, files, baselineIdentityBaseSHA, baselineIdentityHeadSHA)
	files[baselineIdentityBaseSHA]["python/pyproject.toml"] = []byte("[project]\nname = \"rewritten\"\n")

	err := ensureBaselineReadyForLaunch(context.Background(), files, runStore, run, packet)
	if err == nil || !strings.Contains(err.Error(), "does not match the frozen run identity") {
		t.Fatalf("ensureBaselineReadyForLaunch() error = %v, want a frozen-identity refusal", err)
	}
}

// TestBaselineReadinessRefusesWithoutACheckpointReader verifies a missing
// exact-checkpoint seam refuses instead of falling back to the working tree.
func TestBaselineReadinessRefusesWithoutACheckpointReader(t *testing.T) {
	files := checkpointFiles{baselineIdentityBaseSHA: {"python/pyproject.toml": []byte("[project]\nname = \"sil\"\n")}}
	runStore, run, packet := baselineVerifierFixture(t, files, baselineIdentityBaseSHA, baselineIdentityHeadSHA)

	err := ensureBaselineReadyForLaunch(context.Background(), nil, runStore, run, packet)
	if err == nil || !strings.Contains(err.Error(), "exact-checkpoint setup file reads") {
		t.Fatalf("ensureBaselineReadyForLaunch() error = %v, want a missing-seam refusal", err)
	}
}
