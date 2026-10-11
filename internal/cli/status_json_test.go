package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/cli"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// statusDocument is the consumer view of `factory status --json`.
type statusDocument struct {
	SchemaVersion int    `json:"schema_version"`
	Command       string `json:"command"`
	Outcome       string `json:"outcome"`
	Registration  struct {
		State      string `json:"state"`
		Provider   string `json:"provider"`
		Repository string `json:"repository"`
	} `json:"registration"`
	Supervisor struct {
		State string `json:"state"`
	} `json:"supervisor"`
	Run struct {
		State               string   `json:"state"`
		ID                  string   `json:"id"`
		Stage               string   `json:"stage"`
		Status              string   `json:"status"`
		Activity            string   `json:"activity"`
		CheckpointSHA       string   `json:"checkpoint_sha"`
		LifecycleReason     string   `json:"lifecycle_reason"`
		ActiveInvocationIDs []string `json:"active_invocation_ids"`
		DefectProposals     []struct {
			InvocationID string `json:"invocation_id"`
			Code         string `json:"code"`
		} `json:"defect_proposals"`
	} `json:"run"`
	Recovery *struct {
		SourcesAgree  bool `json:"sources_agree"`
		Discrepancies []struct {
			Kind   string `json:"kind"`
			Source string `json:"source"`
			Field  string `json:"field"`
		} `json:"discrepancies"`
		SafeActions []string `json:"safe_actions"`
	} `json:"recovery"`
	Blockers []struct {
		Code         string `json:"code"`
		LikelyOrigin string `json:"likely_origin"`
		Action       *struct {
			Summary string `json:"summary"`
			Effect  string `json:"effect"`
		} `json:"action"`
	} `json:"blockers"`
	Error *struct {
		Code         string `json:"code"`
		LikelyOrigin string `json:"likely_origin"`
	} `json:"error"`
}

// blocker returns the origin of the blocker with code, or fails.
func (d statusDocument) blocker(t *testing.T, code string) string {
	t.Helper()
	for _, blocker := range d.Blockers {
		if blocker.Code == code {
			return blocker.LikelyOrigin
		}
	}
	t.Fatalf("blockers = %+v, want code %s", d.Blockers, code)
	return ""
}

// runStatusJSON runs `factory status --json` and decodes its one document.
func runStatusJSON(t *testing.T, configPath string) (statusDocument, int, string) {
	t.Helper()
	var output, errorsOutput bytes.Buffer
	code := cli.Run(context.Background(), []string{"status", "--json", "--config", configPath}, &output, &errorsOutput)
	var document statusDocument
	decodeSingleDocument(t, output.String(), &document)
	return document, code, errorsOutput.String()
}

// TestRunStatusJSONReportsExplicitAbsence verifies an empty installation
// reports no registration and no run instead of fabricated values.
func TestRunStatusJSONReportsExplicitAbsence(t *testing.T) {
	t.Parallel()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	var output bytes.Buffer
	if code := cli.Run(context.Background(), []string{"init", "--config", configPath}, &output, &output); code != 0 {
		t.Fatalf("init exit code = %d, output = %s", code, output.String())
	}

	document, code, stderr := runStatusJSON(t, configPath)
	if code != 0 || stderr != "" {
		t.Fatalf("status --json exit = %d stderr = %q, want 0 and no stderr", code, stderr)
	}
	if document.SchemaVersion != 1 || document.Command != "status" || document.Outcome != "ok" {
		t.Fatalf("document = %+v, want a schema 1 ok status", document)
	}
	if document.Registration.State != "none" || document.Supervisor.State != "not_configured" || document.Run.State != "none" || document.Run.ID != "" {
		t.Fatalf("document = %+v, want explicit none states", document)
	}
}

// TestRunStatusJSONReportsAPausedRunWithAFailedGate verifies a paused run,
// its failed repository gate, a stale supervisor, and recovery discrepancies
// are explicit typed fields, and that inspection still exits 0.
func TestRunStatusJSONReportsAPausedRunWithAFailedGate(t *testing.T) {
	t.Parallel()
	configPath, operationalPath, repositoryPath := registeredStatusFixture(t)
	const checkpoint = "0123456789abcdef0123456789abcdef01234567"
	started := time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC)
	opened, err := store.Open(context.Background(), operationalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	if err := opened.SaveRun(context.Background(), store.Run{
		ID: "run-json", RepositoryPath: repositoryPath, IssueNumber: 21,
		Stage: store.StageCheck, Status: store.StatusWaitingForHuman, CheckpointSHA: checkpoint,
		LifecycleReason: "check-repair budget exhausted", CreatedAt: started, UpdatedAt: started,
	}); err != nil {
		t.Fatal(err)
	}
	if err := opened.SaveGateResults(context.Background(), []store.GateResult{
		{RunID: "run-json", CheckpointSHA: checkpoint, Phase: store.GatePhaseCheckpoint, Ordinal: 0, GateName: "test", Outcome: store.GateOutcomeFailed, Status: "failure", Blocking: true},
		{RunID: "run-json", CheckpointSHA: checkpoint, Phase: store.GatePhaseCheckpoint, Ordinal: 1, GateName: "lint", Outcome: store.GateOutcomeSetupFailed, Status: "failure", Blocking: true},
		{RunID: "run-json", CheckpointSHA: checkpoint, Phase: store.GatePhaseCheckpoint, Ordinal: 2, GateName: "build", Outcome: store.GateOutcomeError, Status: "error", Blocking: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := opened.SaveSupervisorHeartbeat(context.Background(), store.SupervisorHeartbeat{
		Coordinator: "host-a", PID: 42, StartedAt: started, RenewedAt: started, ExpiresAt: started.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	document, code, stderr := runStatusJSON(t, configPath)
	if code != 0 || stderr != "" {
		t.Fatalf("status --json exit = %d stderr = %q, want 0 and no stderr", code, stderr)
	}
	if document.Registration.State != "registered" || document.Registration.Provider != "github" || document.Registration.Repository != "example/project" {
		t.Fatalf("registration = %+v, want the registered GitHub repository", document.Registration)
	}
	if document.Supervisor.State != "stale" {
		t.Fatalf("supervisor = %+v, want stale", document.Supervisor)
	}
	run := document.Run
	if run.State != "active" || run.ID != "run-json" || run.Stage != "check" || run.Status != "waiting_for_human" || run.Activity != "waiting-for-human" || run.CheckpointSHA != checkpoint {
		t.Fatalf("run = %+v, want the paused check run", run)
	}
	for code, want := range map[string]string{"run.gate.failed": "repository", "run.gate.setup_failed": "unknown", "run.gate.error": "environment"} {
		if origin := document.blocker(t, code); origin != want {
			t.Fatalf("%s origin = %q, want %s", code, origin, want)
		}
	}
	if origin := document.blocker(t, "run.supervisor.stale"); origin != "environment" {
		t.Fatalf("supervisor blocker origin = %q, want environment", origin)
	}
	for _, blocker := range document.Blockers {
		if blocker.Code == "run.supervisor.stale" && (blocker.Action == nil || blocker.Action.Effect != "external_mutation") {
			t.Fatalf("supervisor blocker = %+v, want factory start classified as an external mutation", blocker)
		}
	}
	if document.Recovery == nil || document.Recovery.SourcesAgree || len(document.Recovery.Discrepancies) == 0 {
		t.Fatalf("recovery = %+v, want the observed discrepancies", document.Recovery)
	}
	if origin := document.blocker(t, "recovery.sources_disagree"); origin != "unknown" {
		t.Fatalf("recovery blocker origin = %q, want unknown", origin)
	}

	after, err := opened.Run(context.Background(), "run-json")
	if err != nil || after.Status != store.StatusWaitingForHuman || !after.UpdatedAt.Equal(started) || after.LifecycleReason != "check-repair budget exhausted" {
		t.Fatalf("run after status = %+v (%v), want it unchanged by inspection", after, err)
	}
}

// TestRunStatusJSONReportsMalformedConfiguration verifies a configuration
// that cannot load yields the structured error envelope on stdout.
func TestRunStatusJSONReportsMalformedConfiguration(t *testing.T) {
	t.Parallel()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("schema_version: [not valid\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	document, code, stderr := runStatusJSON(t, configPath)
	if code != 1 || stderr != "" {
		t.Fatalf("status --json exit = %d stderr = %q, want 1 and no stderr", code, stderr)
	}
	if document.Outcome != "error" || document.Error == nil || document.Error.Code != "status.configuration.unavailable" || document.Error.LikelyOrigin != "environment" {
		t.Fatalf("document = %+v, want the configuration error envelope", document)
	}
	if document.Registration.State != "unknown" || document.Supervisor.State != "unknown" || document.Run.State != "unavailable" {
		t.Fatalf("document = %+v, want unknown and unavailable states, not absence", document)
	}
}

// TestRunStatusJSONKeepsUsageErrorsOnStderr verifies the documented
// exception for argument errors.
func TestRunStatusJSONKeepsUsageErrorsOnStderr(t *testing.T) {
	var output, errorsOutput bytes.Buffer
	code := cli.Run(context.Background(), []string{"status", "--json", "extra"}, &output, &errorsOutput)
	if code != 2 || output.Len() != 0 || errorsOutput.Len() == 0 {
		t.Fatalf("exit = %d stdout = %q stderr = %q, want 2 with stderr only", code, output.String(), errorsOutput.String())
	}
}

// registeredStatusFixture initializes and registers one GitHub repository
// and returns its configuration, operational store, and checkout paths.
func registeredStatusFixture(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	repositoryPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeValidRepositoryConfig(t, repositoryPath)
	var output bytes.Buffer
	if code := cli.Run(context.Background(), []string{"init", "--config", configPath}, &output, &output); code != 0 {
		t.Fatalf("init exit code = %d, output = %s", code, output.String())
	}
	operationalPath := filepath.Join(root, "state", "factory.db")
	output.Reset()
	if code := cli.Run(context.Background(), []string{"register", "--config", configPath, "--repository", repositoryPath, "--github-owner", "example", "--github-repository", "project", "--authorized-user", "alice", "--operational-data", operationalPath}, &output, &output); code != 0 {
		t.Fatalf("register exit code = %d, output = %s", code, output.String())
	}
	return configPath, operationalPath, repositoryPath
}

// TestRunStatusJSONReportsAnUnreadableStore verifies a store that cannot be
// opened keeps the known registration, marks the run unavailable instead of
// absent, does not claim an origin, and does not show the store contents.
func TestRunStatusJSONReportsAnUnreadableStore(t *testing.T) {
	t.Parallel()
	configPath, operationalPath, _ := registeredStatusFixture(t)
	const secret = "ghp_secretTokenValue0123456789"
	if err := os.MkdirAll(filepath.Dir(operationalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(operationalPath, []byte("not a database "+secret), 0o600); err != nil {
		t.Fatal(err)
	}

	var output, errorsOutput bytes.Buffer
	code := cli.Run(context.Background(), []string{"status", "--json", "--config", configPath}, &output, &errorsOutput)
	if code != 1 || errorsOutput.Len() != 0 || strings.Contains(output.String(), secret) {
		t.Fatalf("status --json exit = %d stderr = %q stdout = %s, want 1, no stderr, and no store contents", code, errorsOutput.String(), output.String())
	}
	var document statusDocument
	decodeSingleDocument(t, output.String(), &document)
	if document.Error == nil || document.Error.Code != "status.store.unavailable" || document.Error.LikelyOrigin != "unknown" {
		t.Fatalf("error = %+v, want status.store.unavailable with an unknown origin", document.Error)
	}
	if document.Registration.State != "registered" || document.Supervisor.State != "unknown" || document.Run.State != "unavailable" {
		t.Fatalf("document = %+v, want the known registration and unread supervisor and run", document)
	}
}

// TestRunStatusNeverExposesSubprocessOutputInRecovery verifies a recovery
// inspection whose host commands print a secret shows a bounded observation
// in both views instead of the command output.
func TestRunStatusNeverExposesSubprocessOutputInRecovery(t *testing.T) {
	configPath, operationalPath, repositoryPath := registeredStatusFixture(t)
	const secret = "ghp_secretTokenValue0123456789"
	stubs := t.TempDir()
	for _, name := range []string{"gh", "az", "git", "docker"} {
		script := "#!/bin/sh\necho 'token " + secret + "'\necho 'token " + secret + "' >&2\nexit 1\n"
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", stubs)
	worktree := filepath.Join(t.TempDir(), "worktree")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC)
	opened, err := store.Open(context.Background(), operationalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.SaveRun(context.Background(), store.Run{
		ID: "run-secret", RepositoryPath: repositoryPath, IssueNumber: 22, Branch: "factory/22-run-secret",
		Worktree: worktree, CheckpointSHA: "0123456789abcdef0123456789abcdef01234567",
		Stage: store.StageImplementation, Status: store.StatusActive, CreatedAt: started, UpdatedAt: started,
	}); err != nil {
		_ = opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{{"status", "--json", "--config", configPath}, {"status", "--config", configPath}} {
		var output, errorsOutput bytes.Buffer
		cli.Run(context.Background(), args, &output, &errorsOutput)
		if strings.Contains(output.String()+errorsOutput.String(), secret) {
			t.Fatalf("%v exposed subprocess output:\n%s%s", args, output.String(), errorsOutput.String())
		}
		if !strings.Contains(output.String(), "inspection failed") {
			t.Fatalf("%v output = %s, want a bounded recovery observation", args, output.String())
		}
	}
}

// TestRunStatusJSONDoesNotRecreateAMissingStore verifies inspection reports
// a deleted operational store as unavailable instead of creating an empty
// store and reporting that no run exists.
func TestRunStatusJSONDoesNotRecreateAMissingStore(t *testing.T) {
	t.Parallel()
	configPath, operationalPath, _ := registeredStatusFixture(t)
	if err := os.Remove(operationalPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	document, code, _ := runStatusJSON(t, configPath)
	if code != 1 || document.Error == nil || document.Error.Code != "status.store.unavailable" || document.Run.State != "unavailable" {
		t.Fatalf("status --json exit = %d document = %+v, want an unavailable store", code, document)
	}
	if _, err := os.Stat(operationalPath); !os.IsNotExist(err) {
		t.Fatalf("operational store exists after inspection (stat error %v), want it untouched", err)
	}
}

// TestRunStatusSurfacesAWorkerDefectProposal verifies that status names a
// retained worker defect proposal of the latest Run, by Invocation and code
// only, and points to the draft command in text output.
func TestRunStatusSurfacesAWorkerDefectProposal(t *testing.T) {
	t.Parallel()
	configPath, operationalPath, repositoryPath := registeredStatusFixture(t)
	started := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	opened, err := store.Open(context.Background(), operationalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	if err := opened.SaveRun(context.Background(), store.Run{
		ID: "run-defect", RepositoryPath: repositoryPath, IssueNumber: 5,
		Stage: store.StageImplementation, Status: store.StatusWaitingForHuman, CreatedAt: started, UpdatedAt: started,
	}); err != nil {
		t.Fatal(err)
	}
	resultDirectory := filepath.Join(t.TempDir(), "inv-defect")
	if err := opened.SaveInvocation(context.Background(), store.Invocation{
		ID: "inv-defect", RunID: "run-defect", Harness: "codex", Role: "implementation",
		Stage: store.StageImplementation, Status: store.InvocationStatusCannotProceed,
		ResultDirectory: resultDirectory, CreatedAt: started, UpdatedAt: started,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := report.WriteAtomic(resultDirectory, report.Report{
		SchemaVersion: report.SchemaVersion, InvocationID: "inv-defect", RunID: "run-defect",
		Harness: "codex", Role: "implementation", Stage: "implementation",
		Outcome: report.OutcomeCannotProceed, Summary: "blocked",
		Evidence: []report.Evidence{{Kind: "command", Detail: "factory-report exited 1"}},
		SuspectedFactoryDefect: &report.FactoryDefectProposal{
			InvocationID: "inv-defect", Code: "report.handoff_rejected",
			Expected: "accepted", Observed: "private observed text", Reason: "no repository file",
		},
		ReportedAt: started,
	}); err != nil {
		t.Fatal(err)
	}

	document, code, stderr := runStatusJSON(t, configPath)
	if code != 0 || stderr != "" {
		t.Fatalf("status --json exit = %d stderr = %q", code, stderr)
	}
	proposals := document.Run.DefectProposals
	if len(proposals) != 1 || proposals[0].InvocationID != "inv-defect" || proposals[0].Code != "report.handoff_rejected" {
		t.Fatalf("run.defect_proposals = %+v, want the inv-defect proposal", proposals)
	}

	var output, errorsOutput bytes.Buffer
	if code := cli.Run(context.Background(), []string{"status", "--config", configPath}, &output, &errorsOutput); code != 0 {
		t.Fatalf("status exit = %d stderr = %q", code, errorsOutput.String())
	}
	text := output.String()
	if !strings.Contains(text, "defect proposal: invocation=inv-defect code=report.handoff_rejected") || !strings.Contains(text, "factory bug-report --run-id run-defect") || strings.Contains(text, "private observed text") {
		t.Fatalf("status output = %q, want the proposal line and the draft command without worker text", text)
	}
}
