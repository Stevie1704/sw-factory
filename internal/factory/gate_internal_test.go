package factory

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/gate"
	"github.com/Stevie1704/sw-factory/internal/github"
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

// finalGateRunStore serves one fixed final-checkpoint projection, including
// rows a real store would refuse, so every identity rule can be exercised.
type finalGateRunStore struct {
	gateOrdinalRunStore
	checkpoint []store.GateResult
}

// GateResults returns the configured projection for every query.
func (s *finalGateRunStore) GateResults(context.Context, string, store.GatePhase, string) ([]store.GateResult, error) {
	return s.checkpoint, nil
}

// finalGateFixture returns a frozen required-plus-advisory suite whose
// checkpoint projection is fully valid: the required gate passed and the
// independent advisory gate failed.
func finalGateFixture(t *testing.T) (checkpointFiles, store.Run, SpecificationPacket, []store.GateResult) {
	t.Helper()
	files := checkpointFiles{baselineIdentityHeadSHA: {"go.mod": []byte("module example\n")}}
	packet := SpecificationPacket{RepositoryConfig: config.RepositoryConfig{
		SetupFiles: []string{"go.mod"},
		Gates: []config.GateConfig{
			{Name: "required", Command: "required", Timeout: "1m", Blocking: true},
			{Name: "advisory", Command: "advisory", Timeout: "1m", Blocking: false},
		},
	}}
	run := store.Run{ID: "run-final-gates", Worktree: t.TempDir(), CheckpointSHA: baselineIdentityHeadSHA}
	fingerprint, err := setupInputFingerprint(context.Background(), files, run.Worktree, run.CheckpointSHA, packet.RepositoryConfig.SetupFiles)
	if err != nil {
		t.Fatalf("final fingerprint: %v", err)
	}
	results := []store.GateResult{
		{RunID: run.ID, CheckpointSHA: run.CheckpointSHA, Phase: store.GatePhaseCheckpoint, Ordinal: 0, GateName: "required", Outcome: store.GateOutcomePassed, Status: string(github.CommitStatusSuccess), Blocking: true, SetupFingerprint: fingerprint},
		{RunID: run.ID, CheckpointSHA: run.CheckpointSHA, Phase: store.GatePhaseCheckpoint, Ordinal: 1, GateName: "advisory", Outcome: store.GateOutcomeFailed, Status: string(github.CommitStatusFailure), Blocking: false, SetupFingerprint: fingerprint},
	}
	return files, run, packet, results
}

// TestFinalReadinessAcceptsAnIndependentAdvisoryCommandFailure verifies a
// gate the repository declared non-blocking cannot refuse readiness only
// because its command failed, while its failure evidence stays unchanged.
func TestFinalReadinessAcceptsAnIndependentAdvisoryCommandFailure(t *testing.T) {
	files, run, packet, results := finalGateFixture(t)

	if err := ensureFinalCheckpointGatesAccepted(context.Background(), files, &finalGateRunStore{checkpoint: results}, run, packet); err != nil {
		t.Fatalf("ensureFinalCheckpointGatesAccepted() error = %v, want an advisory failure to permit readiness", err)
	}
}

// TestFinalReadinessRejectsUnacceptableGateResults verifies required gates
// must pass, advisory gates accept only a declared command failure, and every
// identity rule applies to advisory results as strictly as to required ones.
func TestFinalReadinessRejectsUnacceptableGateResults(t *testing.T) {
	cases := []struct {
		name   string
		mutate func([]store.GateResult) []store.GateResult
		want   string
	}{
		{"failed required gate", func(r []store.GateResult) []store.GateResult {
			r[0].Outcome, r[0].Status = store.GateOutcomeFailed, string(github.CommitStatusFailure)
			return r
		}, `"required" has outcome "failed"`},
		{"required gate skipped after an advisory prerequisite failed", func(r []store.GateResult) []store.GateResult {
			r[0].Outcome, r[0].Status = store.GateOutcomeSkipped, string(github.CommitStatusPending)
			return r
		}, `"required" has outcome "skipped"`},
		{"advisory runtime error", func(r []store.GateResult) []store.GateResult {
			r[1].Outcome, r[1].Status = store.GateOutcomeError, string(github.CommitStatusError)
			return r
		}, `"advisory" has outcome "error"`},
		{"advisory setup failure", func(r []store.GateResult) []store.GateResult {
			r[1].Outcome, r[1].Status = store.GateOutcomeSetupFailed, string(github.CommitStatusError)
			return r
		}, `"advisory" has outcome "setup_failed"`},
		{"advisory skip", func(r []store.GateResult) []store.GateResult {
			r[1].Outcome, r[1].Status = store.GateOutcomeSkipped, string(github.CommitStatusError)
			return r
		}, `"advisory" has outcome "skipped"`},
		{"advisory failure with a fabricated success status", func(r []store.GateResult) []store.GateResult {
			r[1].Status = string(github.CommitStatusSuccess)
			return r
		}, `"advisory" has status "success"`},
		{"missing advisory result", func(r []store.GateResult) []store.GateResult {
			return r[:1]
		}, "incomplete"},
		{"duplicate advisory result", func(r []store.GateResult) []store.GateResult {
			return []store.GateResult{r[1], r[1]}
		}, "duplicated"},
		{"stale advisory checkpoint", func(r []store.GateResult) []store.GateResult {
			r[1].CheckpointSHA = baselineIdentityBaseSHA
			return r
		}, "frozen run identity"},
		{"wrong advisory phase", func(r []store.GateResult) []store.GateResult {
			r[1].Phase = store.GatePhaseBaseline
			return r
		}, "frozen run identity"},
		{"wrong advisory blocking policy", func(r []store.GateResult) []store.GateResult {
			r[1].Blocking = true
			return r
		}, "frozen run identity"},
		{"wrong advisory ordinal", func(r []store.GateResult) []store.GateResult {
			r[1].Ordinal = 0
			return r
		}, "frozen run identity"},
		{"wrong advisory setup fingerprint", func(r []store.GateResult) []store.GateResult {
			r[1].SetupFingerprint = "stale"
			return r
		}, "frozen run identity"},
		{"advisory result of another run", func(r []store.GateResult) []store.GateResult {
			r[1].RunID = "run-other"
			return r
		}, "frozen run identity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files, run, packet, results := finalGateFixture(t)
			runStore := &finalGateRunStore{checkpoint: tc.mutate(results)}

			err := ensureFinalCheckpointGatesAccepted(context.Background(), files, runStore, run, packet)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ensureFinalCheckpointGatesAccepted() error = %v, want refusal containing %q", err, tc.want)
			}
		})
	}
}
