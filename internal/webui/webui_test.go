package webui_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/webui"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

const (
	waitingRunID     = "run-waiting"
	terminalRunID    = "run-terminal"
	baseSHA          = "0123456789abcdef0123456789abcdef01234567"
	checkpointSHA    = "abcdefabcdefabcdefabcdefabcdefabcdefabcd"
	pullRequestURL   = "https://github.com/example/project/pull/77"
	diagnosticOutput = "FAIL TestCheckout: cart total mismatch"
	// deletedRunID names a run whose row was cleaned up while its
	// evaluation summary was retained.
	deletedRunID = "run-deleted"
)

// fixtureNow is the fixed observation time of every fixture page.
var fixtureNow = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// uiFixture is one registered repository with a populated operational store
// and the UI handler that reads it through the factory service.
type uiFixture struct {
	handler    http.Handler
	configPath string
	storePath  string
}

// newUIFixture writes a host configuration and a real operational store with
// one waiting run, one terminal run with an evaluation summary, and one
// retained evaluation summary whose run row is gone, then builds the UI handler over a
// factory service with no tracker adapter.
func newUIFixture(t *testing.T) uiFixture {
	t.Helper()

	root := t.TempDir()
	configPath, repositoryPath, storePath := saveHost(t, root)
	waitingWorktree := filepath.Join(root, "worktrees", waitingRunID)
	opened, err := store.Open(t.Context(), storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	runs := []store.Run{
		{
			ID: terminalRunID, RepositoryPath: repositoryPath, IssueNumber: 41,
			Stage: store.StageReady, Status: store.StatusComplete,
			Branch: "factory/41", Worktree: filepath.Join(root, "worktrees", terminalRunID),
			PullRequestNumber: 77, PullRequestURL: pullRequestURL,
			LifecycleReason:     "pull request merged",
			SpecificationPacket: specificationPacket(t, 41, "Show the cart total", workflow.RouteDefault),
			TerminalAt:          fixtureNow.Add(-2 * time.Hour),
			CreatedAt:           fixtureNow.Add(-4 * time.Hour), UpdatedAt: fixtureNow.Add(-2 * time.Hour),
		},
		{
			ID: waitingRunID, RepositoryPath: repositoryPath, IssueNumber: 42,
			Stage: store.StageCheck, Status: store.StatusWaitingForHuman,
			Branch: "factory/42", Worktree: waitingWorktree,
			BaseCheckpointSHA: baseSHA, CheckpointSHA: checkpointSHA,
			LifecycleReason:     "deterministic gate failure: test",
			PendingQuestions:    []store.PendingQuestion{{ID: "q1", Prompt: "Which currency rounds the total?"}},
			LastCommandName:     "retry",
			LastCommandOutcome:  "rejected",
			LastCommandMessage:  "retry needs a passing baseline",
			CheckRepairAttempts: 2, CheckRepairBudget: 3,
			SpecificationPacket: specificationPacket(t, 42, "Checkout keeps the cart total", workflow.RouteAcceptance),
			CreatedAt:           fixtureNow.Add(-time.Hour), UpdatedAt: fixtureNow.Add(-time.Minute),
		},
	}
	for _, run := range runs {
		if err := opened.SaveRun(t.Context(), run); err != nil {
			t.Fatalf("SaveRun(%s) error = %v", run.ID, err)
		}
	}
	if err := opened.SaveInvocation(t.Context(), store.Invocation{
		ID: "inv-implementation", RunID: waitingRunID, Harness: "codex", Role: "implementation",
		Stage: store.StageImplementation, Model: "gpt-test", Status: store.InvocationStatusCompleted,
		PromptVersion: "implementation-v3", CreatedAt: fixtureNow.Add(-50 * time.Minute), UpdatedAt: fixtureNow.Add(-40 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := opened.SaveGateResults(t.Context(), []store.GateResult{
		{RunID: waitingRunID, CheckpointSHA: baseSHA, Phase: store.GatePhaseBaseline, Ordinal: 0, GateName: "unit-tests", Outcome: store.GateOutcomePassed, Status: "success", Blocking: true},
		{RunID: waitingRunID, CheckpointSHA: checkpointSHA, Phase: store.GatePhaseCheckpoint, Ordinal: 0, GateName: "unit-tests", Outcome: store.GateOutcomeFailed, Status: "failure", Blocking: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := opened.SaveSupervisorHeartbeat(t.Context(), store.SupervisorHeartbeat{
		Coordinator: "host-a", PID: 4242,
		StartedAt: fixtureNow.Add(-time.Hour), RenewedAt: fixtureNow.Add(-10 * time.Second), ExpiresAt: fixtureNow.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := opened.SavePendingEffect(t.Context(), store.PendingEffect{
		RunID: waitingRunID, ID: "effect-push-1", Kind: store.PendingEffectKindPush,
		Payload: `{"secret":"replay-intent"}`, CreatedAt: fixtureNow.Add(-time.Minute), UpdatedAt: fixtureNow.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	for _, summary := range []store.EvaluationSummary{
		{
			RunID: terminalRunID, Outcome: store.EvaluationOutcomeComplete,
			StartedAt: fixtureNow.Add(-4 * time.Hour), CompletedAt: fixtureNow.Add(-2 * time.Hour), TotalWallTime: 2 * time.Hour,
			StageDurations: []store.EvaluationStageDuration{
				{Stage: store.StageImplementation, Duration: 45 * time.Minute},
				{Stage: store.StageReview, Duration: 20 * time.Minute},
			},
			InvocationVersions: []store.EvaluationInvocationVersion{{
				InvocationID: "inv-review", Harness: "claude", Model: "opus-test",
				PromptVersion: "review-v7", WorkerVersion: "worker-1.9.0", ReportSchemaVersion: 4,
			}},
			InvocationCount: 5, CheckRepairCount: 1, TestRevisionCount: 2, ReviewRevisionCount: 3, BudgetExhausted: true,
			Usage: store.EvaluationUsage{
				Available: true, InputTokens: 12000, OutputTokens: 3400, TotalTokens: 15400,
				CostReported: true, CostMicros: 1250000, Currency: "USD",
			},
		},
		{
			RunID: deletedRunID, Outcome: store.EvaluationOutcomeCancelled,
			StartedAt: fixtureNow.Add(-72 * time.Hour), CompletedAt: fixtureNow.Add(-70 * time.Hour), TotalWallTime: 2 * time.Hour,
			InvocationCount: 1,
		},
	} {
		if err := opened.SaveEvaluationSummary(t.Context(), summary); err != nil {
			t.Fatalf("SaveEvaluationSummary(%s) error = %v", summary.RunID, err)
		}
	}
	diagnosticDirectory := filepath.Join(filepath.Dir(waitingWorktree), ".factory-agents", waitingRunID, "gate-failure")
	if err := os.MkdirAll(diagnosticDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(diagnosticDirectory, "checkpoint.log"), []byte("phase: checkpoint\n"+diagnosticOutput+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	return uiFixture{handler: newHandler(configPath), configPath: configPath, storePath: storePath}
}

// saveHost writes a host configuration that registers one repository under
// root. The operational store path it names is not created.
func saveHost(t *testing.T, root string) (configPath, repositoryPath, storePath string) {
	t.Helper()

	repositoryPath = filepath.Join(root, "repo")
	if err := os.MkdirAll(repositoryPath, 0o700); err != nil {
		t.Fatal(err)
	}
	storePath = filepath.Join(root, "state", "factory.db")
	configPath = filepath.Join(root, "config", "host.yaml")
	if err := config.SaveHost(configPath, config.HostConfig{
		SchemaVersion: config.CurrentHostSchemaVersion,
		Repositories: []config.RepositoryRegistration{{
			Path:                 repositoryPath,
			GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
			AuthorizedUsers:      []string{"alice"},
			Polling:              config.PollingConfig{Interval: "30s", Backoff: "5m"},
			OperationalDataPath:  storePath,
			RepositoryConfigPath: filepath.Join(repositoryPath, config.RepositoryConfigFileName),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	return configPath, repositoryPath, storePath
}

// newHandler builds the UI handler over a factory service with no tracker
// adapter and the fixed fixture clock.
func newHandler(configPath string) http.Handler {
	service := factory.NewWithDependencies(configPath, factory.Dependencies{Now: func() time.Time { return fixtureNow }})
	return webui.NewHandler(service, webui.Options{})
}

// specificationPacket serializes a frozen packet that names the issue title
// and the workflow route the pages display.
func specificationPacket(t *testing.T, number int, title string, route workflow.Route) string {
	t.Helper()

	packet, err := json.Marshal(factory.SpecificationPacket{
		Version: 1,
		Issue:   tracker.Issue{Number: number, Title: title},
		Route:   route,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(packet)
}

// get sends one GET request with a loopback Host header, as a browser that
// opened the UI on 127.0.0.1 does.
func get(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Host = "127.0.0.1:8765"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// assertContains fails when body lacks any wanted text.
func assertContains(t *testing.T, body string, wanted ...string) {
	t.Helper()

	for _, text := range wanted {
		if !strings.Contains(body, text) {
			t.Errorf("page does not contain %q", text)
		}
	}
}

// TestRunListShowsEveryRunFromTheStore verifies the run list renders every
// persisted run with its issue, stage, status, waiting reason, route,
// activity, and pull request, plus the supervisor heartbeat.
func TestRunListShowsEveryRunFromTheStore(t *testing.T) {
	t.Parallel()

	fixture := newUIFixture(t)
	response := get(t, fixture.handler, "/")

	if response.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200; body:\n%s", response.Code, response.Body)
	}
	if got := response.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want text/html; charset=utf-8", got)
	}
	body := response.Body.String()
	assertContains(t, body,
		`href="/runs/run-waiting"`, `href="/runs/run-terminal"`,
		"#42", "Checkout keeps the cart total", "#41", "Show the cart total",
		"check", "waiting_for_human", "deterministic gate failure: test", "acceptance", "waiting-for-human",
		"ready", "complete", "terminal",
		`href="`+pullRequestURL+`"`,
		"live", "host-a", "4242",
	)
	if strings.Index(body, "run-waiting") > strings.Index(body, "run-terminal") {
		t.Errorf("run list does not show the newest update first")
	}
}

// TestRunDetailShowsTheRunRecord verifies the run detail renders the stage,
// status, waiting reason and questions, route, checkpoints, gate outcomes per
// checkpoint, invocations, pending effect identity, budgets, and the host-side
// gate failure diagnostic, without the pending effect's replay payload.
func TestRunDetailShowsTheRunRecord(t *testing.T) {
	t.Parallel()

	fixture := newUIFixture(t)
	response := get(t, fixture.handler, "/runs/"+waitingRunID)

	if response.Code != http.StatusOK {
		t.Fatalf("GET /runs/%s status = %d, want 200; body:\n%s", waitingRunID, response.Code, response.Body)
	}
	body := response.Body.String()
	assertContains(t, body,
		"#42", "Checkout keeps the cart total",
		"check", "waiting_for_human", "waiting-for-human",
		"deterministic gate failure: test", "Which currency rounds the total?",
		"retry", "rejected", "retry needs a passing baseline",
		workflow.RouteAcceptance.Description(),
		"factory/42", baseSHA, checkpointSHA,
		"baseline", "checkpoint", "unit-tests", "passed", "failed",
		"inv-implementation", "implementation", "codex", "gpt-test", "completed", "implementation-v3",
		"2 / 3",
		"push", "effect-push-1",
		diagnosticOutput,
		"host-a",
	)
	if strings.Contains(body, "replay-intent") {
		t.Errorf("run detail shows the pending effect payload")
	}

	terminal := get(t, fixture.handler, "/runs/"+terminalRunID).Body.String()
	assertContains(t, terminal, `href="`+pullRequestURL+`"`, "#77", "pull request merged", "complete")
}

// TestUnknownRunIsNotFound verifies an unknown run identity renders a 404
// page instead of an error.
func TestUnknownRunIsNotFound(t *testing.T) {
	t.Parallel()

	fixture := newUIFixture(t)
	response := get(t, fixture.handler, "/runs/run-missing")

	if response.Code != http.StatusNotFound {
		t.Fatalf("GET /runs/run-missing status = %d, want 404", response.Code)
	}
	assertContains(t, response.Body.String(), "Run not found", "run-missing")
}

// TestRunEvaluationShowsTheSummary verifies the evaluation page renders the
// outcome, timing, stage durations, attempts, invocation versions, and the
// usage estimate of a run's retained evaluation summary.
func TestRunEvaluationShowsTheSummary(t *testing.T) {
	t.Parallel()

	fixture := newUIFixture(t)
	response := get(t, fixture.handler, "/runs/"+terminalRunID+"/evaluation")

	if response.Code != http.StatusOK {
		t.Fatalf("GET evaluation status = %d, want 200; body:\n%s", response.Code, response.Body)
	}
	assertContains(t, response.Body.String(),
		`href="/runs/run-terminal"`, "#41",
		"complete", "2026-10-09T08:00:00Z", "2026-10-09T10:00:00Z", "2h0m0s",
		"implementation", "45m0s", "review", "20m0s",
		"<dt>Invocations</dt><dd>5</dd>", "<dt>Check repairs</dt><dd>1</dd>",
		"<dt>Test revisions</dt><dd>2</dd>", "<dt>Review revisions</dt><dd>3</dd>",
		"<dt>Budget exhausted</dt><dd>yes</dd>",
		"inv-review", "claude", "opus-test", "review-v7", "worker-1.9.0", "<td>4</td>",
		"<dt>Input tokens</dt><dd>12000</dd>", "<dt>Output tokens</dt><dd>3400</dd>", "<dt>Total tokens</dt><dd>15400</dd>",
		"1.250000 USD", "reported",
		"host-a",
	)
}

// TestRunEvaluationShowsTheSummaryOfARemovedRun verifies a summary retained
// after cleanup removed its run row still renders, and says the run is gone.
func TestRunEvaluationShowsTheSummaryOfARemovedRun(t *testing.T) {
	t.Parallel()

	fixture := newUIFixture(t)
	response := get(t, fixture.handler, "/runs/"+deletedRunID+"/evaluation")

	if response.Code != http.StatusOK {
		t.Fatalf("GET evaluation status = %d, want 200; body:\n%s", response.Code, response.Body)
	}
	body := response.Body.String()
	assertContains(t, body, deletedRunID, "cancelled", "The run record was removed by cleanup.", "Usage unavailable (not_reported).")
	if strings.Contains(body, `href="/runs/`+deletedRunID+`"`) {
		t.Errorf("evaluation page links to the removed run")
	}
}

// TestRunEvaluationWithoutSummary verifies a run with no retained summary
// renders an explanation instead of an error.
func TestRunEvaluationWithoutSummary(t *testing.T) {
	t.Parallel()

	fixture := newUIFixture(t)
	response := get(t, fixture.handler, "/runs/"+waitingRunID+"/evaluation")

	if response.Code != http.StatusOK {
		t.Fatalf("GET evaluation status = %d, want 200; body:\n%s", response.Code, response.Body)
	}
	assertContains(t, response.Body.String(), `href="/runs/run-waiting"`, "No evaluation summary is retained for this run.")
}

// TestUnknownRunEvaluationIsNotFound verifies the evaluation page is a 404
// when neither the run nor a summary exists.
func TestUnknownRunEvaluationIsNotFound(t *testing.T) {
	t.Parallel()

	fixture := newUIFixture(t)
	response := get(t, fixture.handler, "/runs/run-missing/evaluation")

	if response.Code != http.StatusNotFound {
		t.Fatalf("GET /runs/run-missing/evaluation status = %d, want 404", response.Code)
	}
	assertContains(t, response.Body.String(), "Run not found", "run-missing")
}

// TestRunDetailLinksToTheEvaluation verifies the run detail links to the
// run's evaluation page.
func TestRunDetailLinksToTheEvaluation(t *testing.T) {
	t.Parallel()

	fixture := newUIFixture(t)
	assertContains(t, get(t, fixture.handler, "/runs/"+terminalRunID).Body.String(), `href="/runs/run-terminal/evaluation"`)
}

// pagePaths are the pages every page-wide assertion covers.
var pagePaths = []string{
	"/", "/runs/" + waitingRunID, "/runs/" + terminalRunID, "/runs/run-missing",
	"/runs/" + waitingRunID + "/evaluation", "/runs/" + terminalRunID + "/evaluation",
	"/runs/" + deletedRunID + "/evaluation", "/runs/run-missing/evaluation",
}

// TestPagesLeaveTheStoreUnchanged verifies that reading every page leaves
// the operational store byte-identical, with the same modification time and
// no new journal or backup file, even with write permission removed.
func TestPagesLeaveTheStoreUnchanged(t *testing.T) {
	t.Parallel()

	fixture := newUIFixture(t)
	makeStoreReadOnly(t, fixture.storePath)
	before := snapshotStore(t, fixture.storePath)

	for _, path := range append(slices.Clone(pagePaths), "/assets/style.css") {
		if response := get(t, fixture.handler, path); response.Code >= http.StatusInternalServerError {
			t.Fatalf("GET %s status = %d; body:\n%s", path, response.Code, response.Body)
		}
	}

	if after := snapshotStore(t, fixture.storePath); after != before {
		t.Fatalf("store changed: before %+v, after %+v", before, after)
	}
}

// TestPagesLoadNoExternalResources verifies every page and asset loads only
// same-origin resources and carries the strict security headers.
func TestPagesLoadNoExternalResources(t *testing.T) {
	t.Parallel()

	fixture := newUIFixture(t)
	for _, path := range append(slices.Clone(pagePaths), "/assets/style.css") {
		response := get(t, fixture.handler, path)
		header := response.Header()
		if policy := header.Get("Content-Security-Policy"); !strings.Contains(policy, "default-src 'none'") {
			t.Errorf("GET %s Content-Security-Policy = %q, want default-src 'none'", path, policy)
		}
		if got := header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("GET %s X-Content-Type-Options = %q, want nosniff", path, got)
		}
		if got := header.Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("GET %s Referrer-Policy = %q, want no-referrer", path, got)
		}
		if got := header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("GET %s Cache-Control = %q, want no-store", path, got)
		}
		assertNoExternalResources(t, response.Body.String())
	}
	assertServedAssetsAreSelfContained(t, fixture.handler)
}

// TestForeignHostIsRefused verifies the UI answers only to a loopback Host
// header, so a DNS-rebinding page cannot read run data.
func TestForeignHostIsRefused(t *testing.T) {
	t.Parallel()

	fixture := newUIFixture(t)
	for host, want := range map[string]int{
		"127.0.0.1:8765":              http.StatusOK,
		"localhost:8765":              http.StatusOK,
		"evil.example:8765":           http.StatusMisdirectedRequest,
		"127.0.0.1.evil.example:8765": http.StatusMisdirectedRequest,
	} {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Host = host
		recorder := httptest.NewRecorder()
		fixture.handler.ServeHTTP(recorder, request)
		if recorder.Code != want {
			t.Errorf("GET / with Host %q status = %d, want %d", host, recorder.Code, want)
		}
		if want != http.StatusOK && strings.Contains(recorder.Body.String(), waitingRunID) {
			t.Errorf("GET / with Host %q shows run data", host)
		}
	}
}

// TestMissingStoreRendersAnExplanation verifies a registered repository whose
// operational store does not exist yet renders a 503 page that names the
// problem, and that the read does not create the store.
func TestMissingStoreRendersAnExplanation(t *testing.T) {
	t.Parallel()

	configPath, _, storePath := saveHost(t, t.TempDir())
	response := get(t, newHandler(configPath), "/")

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET / status = %d, want 503", response.Code)
	}
	assertContains(t, response.Body.String(), "Operational store unavailable", "supervisor: unknown")
	assertNoExternalResources(t, response.Body.String())
	if _, err := os.Stat(storePath); !os.IsNotExist(err) {
		t.Fatalf("page read created the operational store; stat error = %v", err)
	}
}
