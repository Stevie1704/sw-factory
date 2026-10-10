package factory_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

const (
	// pullRequestWriterStart is the PR-writer section marker in a PR body.
	pullRequestWriterStart = "<!-- factory-pr-writer:start -->"
	// coordinatorSectionStart is the coordinator section marker in a PR body.
	coordinatorSectionStart = "<!-- factory-generated:start -->"
	// humanPullRequestText is text a person added to the PR body.
	humanPullRequestText = "Human note: please check the migration first."
)

// configurePullRequestWriter declares the optional PR-writer role.
func configurePullRequestWriter(policy *config.RepositoryConfig) {
	policy.RoleHarnessDefaults["pr_writer"] = config.HarnessCodex
	policy.ModelOptions["pr_writer"] = []string{"gpt-5"}
}

// reachReviewedCheckpoint drives a run through implementation, its draft PR,
// and both passing review axes, and adds human text to the PR body.
func reachReviewedCheckpoint(t *testing.T, fixture *nonBlockingReadinessFixture) store.Run {
	t.Helper()
	ctx := context.Background()
	claimed, err := fixture.service.ClaimIssue(ctx, 42)
	if err != nil {
		t.Fatalf("ClaimIssue() error = %v", err)
	}
	if _, err := fixture.service.RunBaseline(ctx, factory.BaselineRequest{RunID: claimed.Run.ID}); err != nil {
		t.Fatalf("RunBaseline() error = %v", err)
	}
	implementation, err := fixture.service.StartAgent(ctx, factory.AgentRequest{RunID: claimed.Run.ID})
	if err != nil {
		t.Fatalf("StartAgent(implementation) error = %v", err)
	}
	fixture.workspace.state.ChangedPaths = []string{"internal/factory/lint.go"}
	fixture.acceptReport(t, implementation, report.Report{
		Outcome: report.OutcomeCompleted, Summary: "implementation complete",
		Handoff: &report.Handoff{
			ChangeSummary:          "implemented the behavior",
			AcceptanceMapping:      []report.AcceptanceMapping{{Criterion: "behavior", Evidence: "factory regression"}},
			ProductionFilesChanged: []string{"internal/factory/lint.go"},
			FocusedCommands:        []string{"go test ./internal/factory"},
		},
	})
	fixture.worker.results = append(fixture.worker.results, worker.CommandResult{ExitCode: 0}, worker.CommandResult{ExitCode: 0}, worker.CommandResult{ExitCode: 0})
	draft, err := fixture.service.CreateDraftPullRequest(ctx, factory.DraftPullRequestRequest{RunID: claimed.Run.ID})
	if err != nil {
		t.Fatalf("CreateDraftPullRequest() error = %v", err)
	}
	fixture.pullRequests.existing = fixture.pullRequests.created
	fixture.pullRequests.existing.HeadSHA = draft.Run.CheckpointSHA
	fixture.pullRequests.existing.Body = humanPullRequestText + "\n\n" + fixture.pullRequests.createdRequests[0].Body
	for _, role := range []string{"spec_review", "standards_review"} {
		review, err := fixture.service.StartAgent(ctx, factory.AgentRequest{RunID: claimed.Run.ID, Role: role})
		if err != nil {
			t.Fatalf("StartAgent(%s) error = %v", role, err)
		}
		fixture.acceptReport(t, review, report.Report{
			Outcome: report.OutcomeCompleted, Summary: "review complete",
			ReviewHandoff: &report.ReviewHandoff{ReviewedSHA: draft.Run.CheckpointSHA, UnitID: review.Invocation.ReviewUnitID},
		})
	}
	return fixture.currentRun(t)
}

// drive performs one progression pass and fails the test on an error.
func drive(t *testing.T, fixture *nonBlockingReadinessFixture, sinks ...factory.EventSink) string {
	t.Helper()
	outcome, err := fixture.service.DriveRunForTest(context.Background(), sinks...)
	if err != nil {
		t.Fatalf("DriveRunForTest() error = %v", err)
	}
	return outcome
}

// pullRequestWriterInvocations returns every PR-writer invocation of a run.
func pullRequestWriterInvocations(t *testing.T, fixture *nonBlockingReadinessFixture, runID string) []store.Invocation {
	t.Helper()
	opened, err := store.Open(context.Background(), fixture.operationalPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = opened.Close() }()
	invocations, err := opened.Invocations(context.Background(), runID)
	if err != nil {
		t.Fatalf("Invocations() error = %v", err)
	}
	var writers []store.Invocation
	for _, invocation := range invocations {
		if invocation.Role == "pr_writer" {
			writers = append(writers, invocation)
		}
	}
	return writers
}

// writePullRequestWriterReport publishes one report for a PR-writer
// invocation without accepting it, so progression owns the acceptance.
func writePullRequestWriterReport(t *testing.T, invocation store.Invocation, value report.Report) {
	t.Helper()
	value.SchemaVersion = report.SchemaVersion
	value.InvocationID = invocation.ID
	value.RunID = invocation.RunID
	value.Harness = invocation.Harness
	value.Role = invocation.Role
	value.Stage = string(invocation.Stage)
	value.NativeSessionID = "session-pr-writer"
	value.ReportedAt = time.Now().UTC()
	if _, err := report.WriteAtomicForInvocation(invocation.ResultDirectory, invocation.ID, value); err != nil {
		t.Fatalf("write pr_writer report: %v", err)
	}
}

// recordedEvents captures coordinator events for assertions.
type recordedEvents struct {
	values []factory.CoordinatorEvent
}

// Emit records one event.
func (r *recordedEvents) Emit(event factory.CoordinatorEvent) {
	r.values = append(r.values, event)
}

// has reports whether an event of one kind was emitted.
func (r *recordedEvents) has(kind factory.EventKind) bool {
	for _, event := range r.values {
		if event.Kind == kind {
			return true
		}
	}
	return false
}

// TestReadinessWithoutPullRequestWriterIsUnchanged verifies an unconfigured
// role makes no invocation and leaves the PR body without a writer section.
func TestReadinessWithoutPullRequestWriterIsUnchanged(t *testing.T) {
	fixture := newNonBlockingReadinessFixture(t)
	run := reachReviewedCheckpoint(t, fixture)
	drive(t, fixture)

	run = fixture.currentRun(t)
	if run.Stage != store.StageReady || fixture.pullRequests.existing.Draft {
		t.Fatalf("run = %s/%s draft=%v, want ready non-draft", run.Stage, run.Status, fixture.pullRequests.existing.Draft)
	}
	if writers := pullRequestWriterInvocations(t, fixture, run.ID); len(writers) != 0 {
		t.Fatalf("pr_writer invocations = %#v, want none", writers)
	}
	if run.PullRequestSummary != nil || strings.Contains(fixture.pullRequests.existing.Body, pullRequestWriterStart) {
		t.Fatalf("summary = %#v body = %q, want no pr_writer section", run.PullRequestSummary, fixture.pullRequests.existing.Body)
	}
}

// TestPullRequestWriterSummaryPrecedesTheReadinessHandOff verifies the writer
// runs once per hand-off checkpoint before the draft flag changes, its section
// goes above the coordinator section, human text stays unchanged, and a
// readiness retry reuses the saved text without a new invocation.
func TestPullRequestWriterSummaryPrecedesTheReadinessHandOff(t *testing.T) {
	fixture := newNonBlockingReadinessFixture(t, configurePullRequestWriter)
	run := reachReviewedCheckpoint(t, fixture)
	if run.Stage != store.StageReview || !fixture.pullRequests.existing.Draft {
		t.Fatalf("run after reviews = %s draft=%v, want review with a draft PR until the summary exists", run.Stage, fixture.pullRequests.existing.Draft)
	}

	if outcome := drive(t, fixture); outcome != "invocation_active" {
		t.Fatalf("first pass outcome = %q, want the pr_writer executing", outcome)
	}
	writers := pullRequestWriterInvocations(t, fixture, run.ID)
	if len(writers) != 1 || writers[0].Status != store.InvocationStatusActive || writers[0].Stage != "pr_summary" {
		t.Fatalf("pr_writer invocations = %#v, want one active invocation", writers)
	}
	start := fixture.worker.starts[len(fixture.worker.starts)-1]
	if !start.WorktreeReadOnly || start.Role != "pr_writer" {
		t.Fatalf("worker start = %#v, want a read-only pr_writer worker", start)
	}
	packet, err := os.ReadFile(filepath.Join(writers[0].InvocationDirectory, "specification.json"))
	if err != nil {
		t.Fatalf("read pr_writer packet: %v", err)
	}
	if !strings.Contains(string(packet), `"relevant_logs"`) || !strings.Contains(string(packet), run.CheckpointSHA) {
		t.Fatalf("pr_writer packet = %s, want gate results for the checkpoint", packet)
	}

	body := "## Summary\n\nAdds the behavior.\n\n## Merge Danger\n\n**Door:** two-way"
	writePullRequestWriterReport(t, writers[0], report.Report{
		Outcome: report.OutcomeCompleted, Summary: "summary written",
		PullRequestSummary: &report.PullRequestSummary{Body: body},
	})
	// A ready head that differs from the reviewed checkpoint fails the first
	// hand-off after the summary update, so the next pass is a retry.
	fixture.pullRequests.readyHeadSHA = strings.Repeat("e", len(run.CheckpointSHA))
	drive(t, fixture)
	saved := fixture.currentRun(t)
	if saved.PullRequestSummary == nil || saved.PullRequestSummary.Body != body || saved.Stage == store.StageReady {
		t.Fatalf("run after failed hand-off = %s summary=%#v, want the saved summary and no ready state", saved.Stage, saved.PullRequestSummary)
	}
	summaryUpdate := -1
	for index, request := range fixture.pullRequests.updatedRequests {
		if strings.Contains(request.Body, body) {
			summaryUpdate = index
			if !request.Draft {
				t.Fatalf("summary update %d = %#v, want it sent while the PR is still a draft", index, request)
			}
			break
		}
	}
	if summaryUpdate < 0 {
		t.Fatalf("PR updates = %#v, want the summary published", fixture.pullRequests.updatedRequests)
	}

	fixture.pullRequests.readyHeadSHA = ""
	drive(t, fixture)
	ready := fixture.currentRun(t)
	if ready.Stage != store.StageReady || fixture.pullRequests.existing.Draft {
		t.Fatalf("run after retry = %s draft=%v, want ready non-draft", ready.Stage, fixture.pullRequests.existing.Draft)
	}
	if writers := pullRequestWriterInvocations(t, fixture, run.ID); len(writers) != 1 {
		t.Fatalf("pr_writer invocations after retry = %d, want the saved text reused", len(writers))
	}
	final := fixture.pullRequests.existing.Body
	if strings.Count(final, pullRequestWriterStart) != 1 || strings.Count(final, body) != 1 {
		t.Fatalf("final PR body = %q, want exactly one pr_writer section", final)
	}
	if !strings.HasPrefix(final, humanPullRequestText+"\n\n") {
		t.Fatalf("final PR body = %q, want the human text unchanged", final)
	}
	if strings.Index(final, pullRequestWriterStart) > strings.Index(final, coordinatorSectionStart) {
		t.Fatalf("final PR body = %q, want the pr_writer section above the coordinator section", final)
	}
	if !strings.Contains(final, "An agent (`pr_writer`) wrote this summary") {
		t.Fatalf("final PR body = %q, want the advisory note", final)
	}
}

// TestPullRequestWriterFailureDoesNotBlockTheHandOff verifies a failed,
// invalid, timed-out, or unlaunchable writer leaves the hand-off with the
// coordinator section only and emits a warning event.
func TestPullRequestWriterFailureDoesNotBlockTheHandOff(t *testing.T) {
	cases := map[string]func(t *testing.T, fixture *nonBlockingReadinessFixture, writer store.Invocation){
		"cannot proceed": func(t *testing.T, _ *nonBlockingReadinessFixture, writer store.Invocation) {
			writePullRequestWriterReport(t, writer, report.Report{
				Outcome: report.OutcomeCannotProceed, Summary: "no summary",
				Evidence: []report.Evidence{{Kind: "diff", Detail: "diff unreadable"}},
			})
		},
		"empty body": func(t *testing.T, _ *nonBlockingReadinessFixture, writer store.Invocation) {
			data := `{"schema_version":1,"invocation_id":"` + writer.ID + `","run_id":"` + writer.RunID + `","harness":"codex","role":"pr_writer","stage":"pr_summary","outcome":"completed","summary":"empty","pull_request_summary":{"body":"  "},"native_session_id":"session-pr-writer","reported_at":"2026-10-05T10:00:00Z"}`
			if err := os.WriteFile(filepath.Join(writer.ResultDirectory, report.ReportFileName), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"timeout": func(_ *testing.T, fixture *nonBlockingReadinessFixture, _ store.Invocation) {
			*fixture.now = fixture.now.Add(time.Hour)
		},
	}
	for name, fail := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newNonBlockingReadinessFixture(t, configurePullRequestWriter)
			run := reachReviewedCheckpoint(t, fixture)
			drive(t, fixture)
			writers := pullRequestWriterInvocations(t, fixture, run.ID)
			if len(writers) != 1 {
				t.Fatalf("pr_writer invocations = %#v, want one", writers)
			}
			fail(t, fixture, writers[0])
			events := &recordedEvents{}
			drive(t, fixture, events)
			assertHandOffWithoutSummary(t, fixture, events)
		})
	}
	t.Run("launch failure", func(t *testing.T) {
		fixture := newNonBlockingReadinessFixture(t, configurePullRequestWriter)
		reachReviewedCheckpoint(t, fixture)
		fixture.harness.startErr = errors.New("harness unavailable")
		events := &recordedEvents{}
		drive(t, fixture, events)
		assertHandOffWithoutSummary(t, fixture, events)
	})
}

// assertHandOffWithoutSummary verifies a ready PR without a writer section,
// a saved failed summary, and an emitted warning event.
func assertHandOffWithoutSummary(t *testing.T, fixture *nonBlockingReadinessFixture, events *recordedEvents) {
	t.Helper()
	run := fixture.currentRun(t)
	if run.Stage != store.StageReady || fixture.pullRequests.existing.Draft {
		t.Fatalf("run = %s/%s (%s) draft=%v, want ready non-draft", run.Stage, run.Status, run.LifecycleReason, fixture.pullRequests.existing.Draft)
	}
	if run.PullRequestSummary == nil || run.PullRequestSummary.Status != store.PullRequestSummaryFailed {
		t.Fatalf("summary = %#v, want a failed summary for the checkpoint", run.PullRequestSummary)
	}
	if strings.Contains(fixture.pullRequests.existing.Body, pullRequestWriterStart) {
		t.Fatalf("PR body = %q, want no pr_writer section", fixture.pullRequests.existing.Body)
	}
	if !events.has(factory.EventPullRequestSummaryWarning) {
		t.Fatalf("events = %#v, want a pr_summary_warning event", events.values)
	}
	for _, id := range run.ActiveInvocationIDs {
		t.Fatalf("active invocation %q remained after the failed summary", id)
	}
}
