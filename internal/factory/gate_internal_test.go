package factory

import (
	"context"
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
func (s *gateOrdinalRunStore) ClaimLifecycleNotification(context.Context, string, store.Status) (bool, error) {
	return true, nil
}
func (s *gateOrdinalRunStore) ReleaseLifecycleNotification(context.Context, string, store.Status) error {
	return nil
}
func (s *gateOrdinalRunStore) SaveGateResults(_ context.Context, results []store.GateResult) error {
	for _, result := range results {
		s.results[result.GateName] = result
	}
	return nil
}
func (s *gateOrdinalRunStore) GateResults(context.Context, string, store.GatePhase, string) ([]store.GateResult, error) {
	return nil, nil
}
