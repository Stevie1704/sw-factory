package factory_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/bugreport"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestBugReportEvidenceWithoutConfigurationIsAPartialDraftSource verifies
// that a missing or corrupt host configuration produces partial evidence and
// a stated limitation instead of an error.
func TestBugReportEvidenceWithoutConfigurationIsAPartialDraftSource(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	corrupt := filepath.Join(root, "corrupt.yaml")
	if err := os.WriteFile(corrupt, []byte("token: ghp_secretvalue\n\t: : not yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"missing": filepath.Join(root, "missing.yaml"), "corrupt": corrupt} {
		t.Run(name, func(t *testing.T) {
			evidence, err := factory.NewWithDependencies(path, factory.Dependencies{}).BugReportEvidence(context.Background(), "run-1")
			if err != nil {
				t.Fatalf("BugReportEvidence() error = %v, want partial evidence", err)
			}
			if evidence.Configuration != bugreport.AvailabilityUnavailable || evidence.Store != bugreport.AvailabilityUnavailable || evidence.Run != nil {
				t.Fatalf("BugReportEvidence() = %#v, want unavailable configuration and store", evidence)
			}
			if len(evidence.Limitations) == 0 {
				t.Fatal("BugReportEvidence() has no limitation for the unavailable configuration")
			}
			if dump := fmt.Sprintf("%#v", evidence); strings.Contains(dump, "ghp_secretvalue") || strings.Contains(dump, root) {
				t.Fatalf("BugReportEvidence() = %s, want no configuration content or path", dump)
			}
		})
	}
}

// TestBugReportEvidenceCollectsAnAllowlistedRun verifies the Run projection:
// fixed vocabulary and Factory identifiers, valid worker defect proposals,
// and limitations for unavailable or rejected retained reports. Private
// values in the store never reach the evidence.
func TestBugReportEvidenceCollectsAnAllowlistedRun(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath, storePath := registerBugReportRepository(t, root)
	opened, err := store.Open(t.Context(), storePath)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	checkpoint := "0123456789abcdef0123456789abcdef01234567"
	if err := opened.SaveRun(t.Context(), store.Run{
		ID: "run-1", RepositoryPath: filepath.Join(root, "acme-private-repo"), IssueNumber: 7,
		Stage: store.StageImplementation, Status: store.StatusWaitingForHuman,
		Branch: "factory/acme-secret-branch", Worktree: filepath.Join(root, "worktrees", "acme-private-repo"),
		CheckpointSHA: checkpoint, LifecycleReason: "token ghp_lifecycle leaked in a message",
		CreatedAt: base, UpdatedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	if err := opened.SaveGateResults(t.Context(), []store.GateResult{
		{RunID: "run-1", CheckpointSHA: checkpoint, Phase: store.GatePhaseCheckpoint, Ordinal: 0, GateName: "acme-secret-gate", Outcome: store.GateOutcomeFailed, Status: "failure", Blocking: true},
	}); err != nil {
		t.Fatal(err)
	}
	proposal := report.FactoryDefectProposal{InvocationID: "inv-defect", Code: "report.handoff_rejected", Expected: "accepted", Observed: "rejected", Reason: "no repository file is involved"}
	invocations := []struct {
		id       string
		proposal *report.FactoryDefectProposal
		write    bool
	}{
		{id: "inv-defect", proposal: &proposal, write: true},
		{id: "inv-missing", write: false},
		{id: "inv-forged", proposal: &report.FactoryDefectProposal{InvocationID: "inv-defect", Code: "x", Expected: "a", Observed: "b", Reason: "c"}, write: true},
	}
	for index, entry := range invocations {
		resultDirectory := filepath.Join(root, "results", entry.id)
		if err := opened.SaveInvocation(t.Context(), store.Invocation{
			ID: entry.id, RunID: "run-1", Harness: "codex", Role: "implementation",
			Stage: store.StageImplementation, Status: store.InvocationStatusCannotProceed,
			ResultDirectory: resultDirectory, PromptVersion: "implementation-v3",
			CreatedAt: base, UpdatedAt: base.Add(time.Duration(index) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
		if !entry.write {
			continue
		}
		value := report.Report{
			SchemaVersion: report.SchemaVersion, InvocationID: entry.id, RunID: "run-1",
			Harness: "codex", Role: "implementation", Stage: "implementation",
			Outcome: report.OutcomeCannotProceed, Summary: "blocked",
			Evidence:   []report.Evidence{{Kind: "command", Detail: "detail"}},
			ReportedAt: base,
		}
		if err := os.MkdirAll(resultDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
		writeRawReport(t, resultDirectory, value, entry.proposal)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	evidence, err := factory.NewWithDependencies(configPath, factory.Dependencies{}).BugReportEvidence(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("BugReportEvidence() error = %v", err)
	}
	if evidence.Configuration != bugreport.AvailabilityAvailable || evidence.Store != bugreport.AvailabilityAvailable || evidence.Provider != "github" {
		t.Fatalf("BugReportEvidence() sources = %#v, want available github sources", evidence)
	}
	run := evidence.Run
	if run == nil || run.ID != "run-1" || run.Stage != "implementation" || run.Status != string(store.StatusWaitingForHuman) {
		t.Fatalf("BugReportEvidence().Run = %#v, want run-1 implementation cannot_proceed", run)
	}
	if len(run.FailedGates) != 1 || run.FailedGates[0] != (bugreport.GateEvidence{Phase: "checkpoint", Outcome: "failed"}) {
		t.Fatalf("FailedGates = %#v, want one nameless failed checkpoint gate", run.FailedGates)
	}
	if len(run.Invocations) != 3 || run.Invocations[0] != (bugreport.InvocationEvidence{ID: "inv-defect", Role: "implementation", Stage: "implementation", Harness: "codex", Status: "cannot_proceed", PromptVersion: "implementation-v3"}) {
		t.Fatalf("Invocations = %#v, want three allowlisted invocations", run.Invocations)
	}
	if len(run.DefectProposals) != 1 || run.DefectProposals[0] != proposal {
		t.Fatalf("DefectProposals = %#v, want only the bound proposal", run.DefectProposals)
	}
	limitations := strings.Join(evidence.Limitations, "\n")
	if !strings.Contains(limitations, "inv-missing") || !strings.Contains(limitations, "inv-forged") {
		t.Fatalf("Limitations = %q, want the missing and the rejected report named", limitations)
	}
	dump := fmt.Sprintf("%#v", evidence)
	for _, private := range []string{"acme", "ghp_", root, "factory/"} {
		if strings.Contains(dump, private) {
			t.Fatalf("BugReportEvidence() contains private value %q: %s", private, dump)
		}
	}
}

// TestBugReportEvidenceNamesAnUnknownRun verifies that an unknown Run is a
// stated limitation, not an error.
func TestBugReportEvidenceNamesAnUnknownRun(t *testing.T) {
	t.Parallel()

	configPath, storePath := registerBugReportRepository(t, t.TempDir())
	opened, err := store.Open(t.Context(), storePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	evidence, err := factory.NewWithDependencies(configPath, factory.Dependencies{}).BugReportEvidence(context.Background(), "run-unknown")
	if err != nil {
		t.Fatalf("BugReportEvidence() error = %v", err)
	}
	if evidence.Run != nil || !strings.Contains(strings.Join(evidence.Limitations, "\n"), "run-unknown") {
		t.Fatalf("BugReportEvidence() = %#v, want a limitation that names the unknown run", evidence)
	}
}

// registerBugReportRepository saves a host configuration with one GitHub
// registration and returns its path and the operational store path.
func registerBugReportRepository(t *testing.T, root string) (string, string) {
	t.Helper()
	repositoryPath := filepath.Join(root, "acme-private-repo")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "host", "config.yaml")
	storePath := filepath.Join(root, "state", "factory.db")
	registration := config.RepositoryRegistration{
		Path:                 repositoryPath,
		GitHub:               config.GitHubConfig{Owner: "acme", Repository: "acme-private-repo"},
		AuthorizedUsers:      []string{"alice"},
		Polling:              config.PollingConfig{Interval: "30s", Backoff: "5m"},
		OperationalDataPath:  storePath,
		RepositoryConfigPath: filepath.Join(repositoryPath, config.RepositoryConfigFileName),
	}
	if err := config.SaveHost(configPath, config.HostConfig{SchemaVersion: config.CurrentHostSchemaVersion, Repositories: []config.RepositoryRegistration{registration}}); err != nil {
		t.Fatal(err)
	}
	return configPath, storePath
}

// writeRawReport writes a report envelope without worker-side validation, so
// a test can retain a proposal that names another invocation.
func writeRawReport(t *testing.T, resultDirectory string, value report.Report, proposal *report.FactoryDefectProposal) {
	t.Helper()
	value.SuspectedFactoryDefect = proposal
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resultDirectory, report.ReportFileName), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}
