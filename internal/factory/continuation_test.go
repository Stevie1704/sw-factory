package factory_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// The continuation fixture advances the checkpoint once per implementation
// round so every repair produces a genuinely new exact subject for the gates
// and both reviewers.
const (
	firstRepairCheckpoint  = "1111111111111111111111111111111111111111111111111111111111111111"
	secondRepairCheckpoint = "2222222222222222222222222222222222222222222222222222222222222222"
	continuationMergeSHA   = "3333333333333333333333333333333333333333333333333333333333333333"
	// maintainerRepairCommentID is the identity of the one supervision comment
	// that dispositions a review parked for a human.
	maintainerRepairCommentID = "7001"
)

// TestStartTraversesReviewRepairHumanRepairReadinessAndMergeBeforeTheNextClaim
// is the end-to-end proof for unattended review repair and terminal queue
// continuation. One `factory start` process queues two eligible issues and
// drives the first through the draft pull request, both independent reviews, a
// blocking agent finding, an authorized human `CHANGES_REQUESTED` repair,
// readiness, and merge, and only then claims the second issue.
func TestStartTraversesReviewRepairHumanRepairReadinessAndMergeBeforeTheNextClaim(t *testing.T) {
	t.Parallel()

	fixture := newContinuationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fixture.driveTerminalOutcome(t, cancel, func() { fixture.pullRequests.merge(continuationMergeSHA) })

	if err := fixture.service.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	fixture.assertUnattendedReviewRepairHappened(t)

	first := fixture.firstTerminalRun(t)
	if first.Status != store.StatusComplete {
		t.Fatalf("first run status = %q, want complete after the merge", first.Status)
	}
	if first.MergeCommitSHA != continuationMergeSHA {
		t.Fatalf("first run merge commit = %q, want the observed merge commit", first.MergeCommitSHA)
	}
	fixture.assertDraftRoundTrip(t)
	if first.ProcessedReviewID != "4001" {
		t.Fatalf("review watermark = %q, want the applied GitHub review identity", first.ProcessedReviewID)
	}
	if first.ProcessedReviewRevision == 0 {
		t.Fatal("review watermark has no run revision, so it is not restart-auditable")
	}

	fixture.assertNextIssueClaimed(t)
}

// TestStartResumesImplementationFromAMaintainerRepairComment is the end-to-end
// proof that a single-account host can recover a review parked for human
// disposition. One `factory start` process drives the first issue to a
// specification review that cannot proceed, applies one authorized
// `/factory repair` comment with no pull-request review submitted anywhere,
// resumes implementation with a command-sourced repair packet, reruns both
// reviews on the repaired checkpoint, and reaches readiness and merge.
func TestStartResumesImplementationFromAMaintainerRepairComment(t *testing.T) {
	t.Parallel()

	fixture := newContinuationFixture(t)
	fixture.parkForMaintainerRepair = true
	// The agent-found blocker belongs to the automatic repair path; this test
	// exercises the human disposition that follows a review which stopped.
	fixture.blockingReviewServed = true
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fixture.driveMaintainerRepairOutcome(t, cancel)

	if err := fixture.service.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	first := fixture.firstTerminalRun(t)
	if first.Status != store.StatusComplete || first.MergeCommitSHA != continuationMergeSHA {
		t.Fatalf("first run = %s/%q, want a merged complete run", first.Status, first.MergeCommitSHA)
	}
	if first.ProcessedCommentID != maintainerRepairCommentID {
		t.Fatalf("command watermark = %q, want the applied repair comment", first.ProcessedCommentID)
	}
	if first.ProcessedReviewID != "" {
		t.Fatalf("review watermark = %q, want no submitted review on a single-account host", first.ProcessedReviewID)
	}
	if reads := fixture.reviews.readCount(); reads != 0 {
		t.Fatalf("pull-request review reads with content = %d, want none", reads)
	}
	fixture.mu.Lock()
	packetSeen := fixture.commandRepairPacketSeen
	fixture.mu.Unlock()
	if !packetSeen {
		t.Fatal("no implementation invocation received the command-sourced repair packet")
	}
	implementationStarts := 0
	for _, role := range fixture.harness.roleStarts() {
		if role == workflow.RoleImplementation {
			implementationStarts++
		}
	}
	if implementationStarts < 2 {
		t.Fatalf("implementation starts = %d, want the repair to resume implementation", implementationStarts)
	}
	if reviewed := fixture.reviewedCheckpoints(); reviewed < 2 {
		t.Fatalf("reviewed checkpoints = %d, want the repaired checkpoint reviewed again", reviewed)
	}
	// The review parked before readiness, so the pull request was still a draft
	// when the instruction arrived: the idempotent draft return changes nothing
	// and the only transition is the readiness the repaired checkpoint earns.
	if transitions := fixture.pullRequests.draftTransitions(); len(transitions) != 1 || transitions[0] {
		t.Fatalf("draft transitions = %v, want one transition to ready after the repair", transitions)
	}
	fixture.assertNextIssueClaimed(t)
}

// TestStartCancelsAnUnmergedClosedPullRequestAndClaimsTheNextIssue proves that
// closing the pull request without merging cancels the active run, retains its
// artifacts, releases the queue, and lets the same coordinator continue.
func TestStartCancelsAnUnmergedClosedPullRequestAndClaimsTheNextIssue(t *testing.T) {
	t.Parallel()

	fixture := newContinuationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fixture.driveTerminalOutcome(t, cancel, fixture.pullRequests.closeWithoutMerge)

	if err := fixture.service.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	first := fixture.firstTerminalRun(t)
	if first.Status != store.StatusCancelled {
		t.Fatalf("first run status = %q, want cancelled after the unmerged close", first.Status)
	}
	if first.MergeCommitSHA != "" {
		t.Fatalf("cancelled run merge commit = %q, want none", first.MergeCommitSHA)
	}
	if first.Branch == "" || first.Worktree == "" {
		t.Fatalf("cancelled run lost its retained artifacts: %#v", first)
	}
	fixture.assertNextIssueClaimed(t)
}

// TestRestartDoesNotReapplyAnAlreadyConsumedHumanReview proves the persisted
// review watermark survives a coordinator process boundary: a restarted
// coordinator re-reads the same submitted review and must not start a second
// repair, a second review round, or a second readiness change for it.
func TestRestartDoesNotReapplyAnAlreadyConsumedHumanReview(t *testing.T) {
	t.Parallel()

	fixture := newContinuationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fixture.driveTerminalOutcome(t, cancel, func() { cancel() })

	if err := fixture.service.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	applied := fixture.harness.roleStarts()
	draftTransitions := len(fixture.pullRequests.draftTransitions())
	reads := fixture.reviews.readCount()
	if watermark := fixture.appliedReviewID(t); watermark != "4001" {
		t.Fatalf("review watermark = %q, want the applied review", watermark)
	}

	restarted, restartCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer restartCancel()
	ticks := 0
	fixture.lease.onRenew = func(github.Lease) {
		if ticks++; ticks >= 4 {
			restartCancel()
		}
	}
	if err := fixture.restart().Start(restarted); err != nil {
		t.Fatalf("restarted Start() error = %v", err)
	}

	if got := fixture.harness.roleStarts(); len(got) != len(applied) {
		t.Fatalf("invocations after restart = %d, want the %d from before the restart", len(got), len(applied))
	}
	if got := len(fixture.pullRequests.draftTransitions()); got != draftTransitions {
		t.Fatalf("readiness changes after restart = %d, want the %d from before the restart", got, draftTransitions)
	}
	// Without this the assertions above would pass for a coordinator that
	// never looked at the review again.
	if fixture.reviews.readCount() <= reads {
		t.Fatal("the restarted coordinator never re-read the submitted review")
	}
}

// appliedReviewID reads the review watermark the coordinator persisted.
func (f *continuationFixture) appliedReviewID(t *testing.T) string {
	t.Helper()
	run := f.latestRun(t)
	if run == nil {
		t.Fatal("no persisted run")
	}
	return run.ProcessedReviewID
}

// assertUnattendedReviewRepairHappened checks the evidence that both review
// loops ran without a stage-driving CLI command: two full review rounds per
// checkpoint, one agent repair, and one human repair.
func (f *continuationFixture) assertUnattendedReviewRepairHappened(t *testing.T) {
	t.Helper()
	roles := map[string]int{}
	for _, start := range f.harness.roleStarts() {
		roles[start]++
	}
	if roles[workflow.RoleImplementation] != 3 {
		t.Fatalf("implementation invocations = %d, want the first pass plus the agent and human repairs", roles[workflow.RoleImplementation])
	}
	if roles[workflow.RoleSpecificationReview] != 3 || roles[workflow.RoleStandardsReview] != 3 {
		t.Fatalf("review invocations = spec %d standards %d, want three fresh rounds each", roles[workflow.RoleSpecificationReview], roles[workflow.RoleStandardsReview])
	}
	if f.reviewedCheckpoints() != 3 {
		t.Fatalf("reviewed checkpoints = %d, want one review round per exact checkpoint", f.reviewedCheckpoints())
	}
	// The submitted review stays visible to every later poll, so a second
	// repair here would mean the watermark failed to make it replay-safe.
	if f.reviews.readCount() < 2 {
		t.Fatalf("review polls = %d, want the submitted review to be observed again after it was applied", f.reviews.readCount())
	}
	if !f.humanRepairPacketSeen {
		t.Fatal("no implementation invocation received the human review repair packet")
	}
	if !f.reviewRepairPromptContinued {
		t.Fatal("a resumed review repair replayed the complete first prompt instead of continuing the session")
	}
	// The human repair must not advance, reset, or record a bounded factory
	// round; only the one agent-found blocker consumed the budget.
	terminal := f.firstTerminalRun(t)
	if terminal.ReviewRepairAttempts != 1 {
		t.Fatalf("factory review-repair attempts = %d, want only the one agent-found repair", terminal.ReviewRepairAttempts)
	}
	if len(terminal.ReviewRepairHistory) != 1 || terminal.ReviewRepairHistory[0].Attempt != 1 {
		t.Fatalf("review-repair history = %#v, want one bounded factory round", terminal.ReviewRepairHistory)
	}
	f.assertGatesReranPerCheckpoint(t)
}

// assertDraftRoundTrip proves the pull request became ready, was returned to
// draft for the human repair, and became ready again only after the repaired
// checkpoint passed everything.
func (f *continuationFixture) assertDraftRoundTrip(t *testing.T) {
	t.Helper()
	want := []bool{false, true, false}
	got := f.pullRequests.draftTransitions()
	if len(got) != len(want) {
		t.Fatalf("draft transitions = %v, want ready, back to draft for the human repair, then ready again", got)
	}
	for index, draft := range want {
		if got[index] != draft {
			t.Fatalf("draft transitions = %v, want %v", got, want)
		}
	}
}

// assertGatesReranPerCheckpoint proves no gate result survived a code change:
// every checkpoint the run produced has its own published gate status, and
// both reviewers reported against that same exact checkpoint.
func (f *continuationFixture) assertGatesReranPerCheckpoint(t *testing.T) {
	t.Helper()
	published := map[string]map[string]bool{}
	for _, status := range f.statuses.values {
		if published[status.SHA] == nil {
			published[status.SHA] = map[string]bool{}
		}
		published[status.SHA][status.Context] = true
	}
	wanted := []string{"factory/gate/test", factory.SpecificationReviewStatusContext, factory.StandardsReviewStatusContext}
	for _, checkpoint := range []string{implementationCheckpoint, firstRepairCheckpoint, secondRepairCheckpoint} {
		for _, context := range wanted {
			if !published[checkpoint][context] {
				t.Fatalf("checkpoint %s is missing the %s status; published = %v", checkpoint[:6], context, published[checkpoint])
			}
		}
	}
}

// continuationFixture assembles one repository whose harness answers each
// role, whose pull request can be reviewed and terminalized, and whose queue
// holds two eligible issues.
type continuationFixture struct {
	service         *factory.Service
	commands        *continuationGitHub
	supervision     *continuationComments
	worker          *unattendedWorker
	workspace       *unattendedWorkspace
	pullRequests    *continuationPullRequests
	reviews         *continuationReviews
	statuses        *gateStatuses
	lease           *pollingLease
	terminal        *continuationTerminal
	harness         *continuationHarness
	operationalPath string
	configPath      string
	clock           func() time.Time
	dependencies    func() factory.Dependencies

	mu                    sync.Mutex
	terminalRun           *store.Run
	firstRunID            string
	blockingReviewServed  bool
	humanRepairPacketSeen bool
	// parkForMaintainerRepair makes the first specification review stop for
	// human disposition instead of judging the checkpoint, which is the state a
	// maintainer repair command exists to recover.
	parkForMaintainerRepair bool
	cannotProceedServed     bool
	// commandRepairPacketSeen records that a command-sourced repair packet
	// reached an implementation invocation with its own provenance.
	commandRepairPacketSeen bool
	// reviewRepairPromptContinued records that a resumed review repair carried
	// only the changed context rather than a second copy of the first prompt.
	reviewRepairPromptContinued bool
	checkpointsReviewed         map[string]struct{}
}

// newContinuationFixture builds an advisory-policy repository that declares
// both independent reviewers and queues two eligible issues.
func newContinuationFixture(t *testing.T) *continuationFixture {
	t.Helper()
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	worktreePath := filepath.Join(root, "worktree")
	if err := os.MkdirAll(filepath.Join(repositoryPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryPath, ".git", "HEAD"), []byte("ref: refs/heads/factory/run-continuation\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	policy := validRepositoryConfig()
	policy.TestPolicy = config.TestPolicy{Mode: config.TestModeAdvisory}
	policy.RoleHarnessDefaults[workflow.RoleSpecificationReview] = config.HarnessCodex
	policy.RoleHarnessDefaults[workflow.RoleStandardsReview] = config.HarnessCodex
	policy.ModelOptions[workflow.RoleSpecificationReview] = []string{"gpt-5"}
	policy.ModelOptions[workflow.RoleStandardsReview] = []string{"gpt-5"}

	workspace := &unattendedWorkspace{draftGitWorkspace: &draftGitWorkspace{
		workspace: gitadapter.Workspace{BaseSHA: factoryGateCheckpoint, Branch: "factory/run-continuation", Worktree: worktreePath},
		state:     gitadapter.WorktreeState{RepositoryPath: repositoryPath, Branch: "factory/run-continuation", HeadSHA: factoryGateCheckpoint},
		// One distinct checkpoint per implementation round.
		checkpointSHAs: []string{implementationCheckpoint, firstRepairCheckpoint, secondRepairCheckpoint},
	}}
	fixture := &continuationFixture{
		commands: newContinuationGitHub([]github.Issue{
			{Number: 7, Title: "Drive the review loops", Body: "Implement unattended review repair.", State: "open", Labels: []string{github.LabelAgentReady}},
			{Number: 9, Title: "Wait for the queue", Body: "Implement the next queued change.", State: "open", Labels: []string{github.LabelAgentReady}},
		}),
		worker:              &unattendedWorker{},
		workspace:           workspace,
		pullRequests:        newContinuationPullRequests(workspace),
		reviews:             &continuationReviews{},
		statuses:            &gateStatuses{},
		clock:               monotonicClock(time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)),
		lease:               &pollingLease{},
		terminal:            &continuationTerminal{agentTerminal: &agentTerminal{}},
		operationalPath:     filepath.Join(root, "state", "factory.db"),
		firstRunID:          "run-continuation",
		checkpointsReviewed: map[string]struct{}{},
	}
	fixture.harness = &continuationHarness{fixture: fixture}
	fixture.supervision = &continuationComments{}
	fixture.terminal.onNotify = func() {
		run := fixture.latestRun(t)
		if run != nil && store.IsTerminalStatus(run.Status) {
			fixture.recordTerminalRun(*run)
		}
	}

	registration := config.RepositoryRegistration{
		Path:                 repositoryPath,
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		AuthorizedUsers:      []string{"alice"},
		Polling:              config.PollingConfig{Interval: "1ms", Backoff: "1ms"},
		OperationalDataPath:  fixture.operationalPath,
		RepositoryConfigPath: filepath.Join(repositoryPath, "factory.yaml"),
	}
	fixture.configPath = filepath.Join(root, "config.yaml")
	fixture.dependencies = func() factory.Dependencies {
		return factory.Dependencies{
			Config:             &fakeConfig{value: config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{registration}}},
			OpenStore:          func(ctx context.Context, path string) (factory.OperationalStore, error) { return store.Open(ctx, path) },
			LoadRepository:     func(string) (config.RepositoryConfig, error) { return policy, nil },
			GitHub:             fixture.commands,
			IssuePoller:        fixture.commands,
			Comments:           fixture.supervision,
			Lease:              fixture.lease,
			PullRequests:       fixture.pullRequests,
			PullRequestReviews: fixture.reviews,
			CommitStatuses:     fixture.statuses,
			Worktree:           fixture.workspace,
			GitWorkspace:       fixture.workspace,
			Worker:             fixture.worker,
			Terminal:           fixture.terminal,
			Harness:            fixture.harness,
			Now:                fixture.clock,
			NewRunID:           newSequentialRunID(fixture.firstRunID),
			Coordinator:        "coordinator-test",
			StartupDiagnosis: func(context.Context) (factory.DoctorResult, error) {
				return factory.DoctorResult{Report: doctor.Report{Results: []doctor.Result{doctor.Success("startup")}}}, nil
			},
		}
	}
	fixture.service = factory.NewWithDependencies(fixture.configPath, fixture.dependencies())
	return fixture
}

// restart models a coordinator process boundary: a fresh service opens the
// same operational store and adapters with no in-memory continuity.
func (f *continuationFixture) restart() *factory.Service {
	f.service = factory.NewWithDependencies(f.configPath, f.dependencies())
	return f.service
}

// continuationComments keeps unattended continuation tests in the normal
// command-enabled mode while letting one test post a supervision comment
// mid-run. It serves no comments until a maintainer posts one.
type continuationComments struct {
	mu       sync.Mutex
	comments []github.Comment
}

// IssueComments returns every supervision comment posted so far.
func (c *continuationComments) IssueComments(context.Context, github.Repository, int) ([]github.Comment, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]github.Comment(nil), c.comments...), nil
}

// post records one maintainer comment for later polls.
func (c *continuationComments) post(comment github.Comment) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.comments = append(c.comments, comment)
}

// continuationTerminal records the terminal state before the same polling
// pass can claim the next queued issue.
type continuationTerminal struct {
	*agentTerminal
	onNotify func()
}

// Notify records the terminal notification and captures the persisted state.
func (t *continuationTerminal) Notify(ctx context.Context, notification terminal.Notification) error {
	if err := t.agentTerminal.Notify(ctx, notification); err != nil {
		return err
	}
	if t.onNotify != nil {
		t.onNotify()
	}
	return nil
}

// monotonicClock returns a strictly increasing coordinator clock, so the
// newest persisted run is unambiguous once a second run exists.
func monotonicClock(start time.Time) func() time.Time {
	var ticks int64
	var mu sync.Mutex
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		ticks++
		return start.Add(time.Duration(ticks) * time.Second)
	}
}

// driveTerminalOutcome supplies the two human actions the factory must never
// take itself. It submits one authorized `CHANGES_REQUESTED` review the first
// time the pull request becomes ready, applies the terminal disposition the
// second time, and stops the coordinator once the next issue is claimed.
func (f *continuationFixture) driveTerminalOutcome(t *testing.T, cancel context.CancelFunc, terminalAction func()) {
	t.Helper()
	humanReviewSubmitted := false
	repairObserved := false
	terminalApplied := false
	f.lease.onRenew = func(github.Lease) {
		run := f.latestRun(t)
		switch {
		case run == nil:
			return
		case run.IssueNumber == 9:
			cancel()
		case store.IsTerminalStatus(run.Status):
			f.recordTerminalRun(*run)
		case run.Stage != store.StageReady:
			return
		case !humanReviewSubmitted:
			humanReviewSubmitted = true
			f.reviews.submit(github.PullRequestReview{
				ID: "4001", Author: "alice", State: github.PullRequestReviewChangesRequested,
				Body: "the retry budget must be read from the frozen packet", SubmittedAt: time.Date(2026, 8, 29, 9, 30, 0, 0, time.UTC),
				Comments: []github.PullRequestReviewComment{
					{Path: "internal/factory/polling.go", Line: 42, Body: "this branch never releases the queue"},
				},
			})
		case !repairObserved:
			// Hold the pull request ready for one further observation, so the
			// already-applied review is polled again and must not repeat.
			repairObserved = true
		case !terminalApplied:
			terminalApplied = true
			terminalAction()
		}
	}
}

// driveMaintainerRepairOutcome posts one authorized repair comment once the
// review parks for human disposition, then merges the pull request the repaired
// checkpoint produces. No pull-request review is ever submitted, which is the
// state of a host whose coordinator account is its only authorized user.
func (f *continuationFixture) driveMaintainerRepairOutcome(t *testing.T, cancel context.CancelFunc) {
	t.Helper()
	commentPosted := false
	terminalApplied := false
	f.lease.onRenew = func(github.Lease) {
		run := f.latestRun(t)
		switch {
		case run == nil:
			return
		case run.IssueNumber == 9:
			cancel()
		case store.IsTerminalStatus(run.Status):
			f.recordTerminalRun(*run)
		case !commentPosted:
			if run.Stage != store.StageReview || run.Status != store.StatusWaitingForHuman || len(run.ActiveInvocationIDs) != 0 {
				return
			}
			commentPosted = true
			f.supervision.post(github.Comment{
				ID:     maintainerRepairCommentID,
				Author: "alice",
				Body:   "/factory repair release queue ownership on the terminal transition",
			})
		case !terminalApplied && run.Stage == store.StageReady:
			terminalApplied = true
			f.pullRequests.merge(continuationMergeSHA)
		}
	}
}

// recordTerminalRun snapshots the first run once GitHub terminalized it, while
// it is still the newest persisted run.
func (f *continuationFixture) recordTerminalRun(run store.Run) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.terminalRun == nil {
		f.terminalRun = &run
	}
}

// latestRun reads the newest persisted run, or nil before the first claim.
func (f *continuationFixture) latestRun(t *testing.T) *store.Run {
	t.Helper()
	opened, err := store.Open(context.Background(), f.operationalPath)
	if err != nil {
		t.Fatalf("open operational store: %v", err)
	}
	defer func() { _ = opened.Close() }()
	run, err := opened.LatestRun(context.Background())
	if err != nil {
		t.Fatalf("read latest run: %v", err)
	}
	return run
}

// firstTerminalRun returns the snapshot of the first run taken when the
// coordinator observed its terminal GitHub disposition.
func (f *continuationFixture) firstTerminalRun(t *testing.T) store.Run {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.terminalRun == nil {
		t.Fatal("the first run never reached a terminal state")
	}
	return *f.terminalRun
}

// assertNextIssueClaimed proves the released queue let the same coordinator
// claim the next oldest eligible issue.
func (f *continuationFixture) assertNextIssueClaimed(t *testing.T) {
	t.Helper()
	opened, err := store.Open(context.Background(), f.operationalPath)
	if err != nil {
		t.Fatalf("open operational store: %v", err)
	}
	defer func() { _ = opened.Close() }()
	run, err := opened.CurrentRun(context.Background())
	if err != nil {
		t.Fatalf("read current run: %v", err)
	}
	if run == nil {
		t.Fatal("no run was claimed after the terminal outcome released the queue")
	}
	if run.IssueNumber != 9 {
		t.Fatalf("claimed issue = #%d, want the next oldest eligible issue #9", run.IssueNumber)
	}
}

// reviewedCheckpoints counts the distinct exact checkpoints a reviewer saw.
func (f *continuationFixture) reviewedCheckpoints() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.checkpointsReviewed)
}

// continuationHarness answers implementation and review invocations with the
// structured results one full review-repair traversal requires.
type continuationHarness struct {
	agentHarness
	fixture *continuationFixture

	mu     sync.Mutex
	starts []harness.StartRequest
}

// roleStarts returns the role of every launched invocation.
func (h *continuationHarness) roleStarts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	roles := make([]string, 0, len(h.starts))
	for _, start := range h.starts {
		roles = append(roles, start.Role)
	}
	return roles
}

// Start records the launch and writes the role's structured report.
func (h *continuationHarness) Start(ctx context.Context, request harness.StartRequest) (harness.Session, error) {
	session, err := h.agentHarness.Start(ctx, request)
	if err != nil {
		return session, err
	}
	return session, h.answer(request, session)
}

// Resume answers a natively resumed session with the same report contract.
func (h *continuationHarness) Resume(ctx context.Context, request harness.StartRequest) (harness.Session, error) {
	session, err := h.agentHarness.Resume(ctx, request)
	if err != nil {
		return session, err
	}
	return session, h.answer(request, session)
}

// answer writes one role-appropriate structured report into the mounted result
// directory, standing in for an interactive session that finished its work.
func (h *continuationHarness) answer(request harness.StartRequest, session harness.Session) error {
	h.mu.Lock()
	h.starts = append(h.starts, request)
	h.mu.Unlock()
	value, ok := h.fixture.reportFor(request)
	if !ok {
		// The second run's invocation stays executing, which keeps the proof
		// focused on the first run's traversal.
		return nil
	}
	if session.NativeSessionID != "" {
		value.NativeSessionID = session.NativeSessionID
	}
	_, err := report.WriteAtomicForInvocation(h.fixture.worker.resultPath, request.InvocationID, value)
	return err
}

// reportFor selects the structured result for one launched invocation.
func (f *continuationFixture) reportFor(request harness.StartRequest) (report.Report, bool) {
	if request.RunID != f.firstRunID {
		return report.Report{}, false
	}
	base := report.Report{
		SchemaVersion:   report.SchemaVersion,
		InvocationID:    request.InvocationID,
		RunID:           request.RunID,
		Harness:         string(config.HarnessCodex),
		Role:            request.Role,
		Stage:           request.Stage,
		Outcome:         report.OutcomeCompleted,
		Summary:         "completed the assigned role",
		NativeSessionID: "session-" + request.Role,
		ReportedAt:      time.Date(2026, 8, 29, 9, 5, 0, 0, time.UTC),
	}
	switch request.Role {
	case workflow.RoleImplementation:
		f.observeImplementationPacket(request)
		f.workspace.state.ChangedPaths = []string{"internal/factory/polling.go"}
		base.Handoff = &report.Handoff{
			ChangeSummary:          "implemented the queued change",
			AcceptanceMapping:      []report.AcceptanceMapping{{Criterion: "queue continuation", Evidence: "internal/factory/polling.go"}},
			ProductionFilesChanged: []string{"internal/factory/polling.go"},
			FocusedCommands:        []string{"go test ./internal/factory"},
		}
		return base, true
	case workflow.RoleSpecificationReview, workflow.RoleStandardsReview:
		if request.Role == workflow.RoleSpecificationReview && f.serveCannotProceed() {
			base.Outcome = report.OutcomeCannotProceed
			base.Summary = "the specification axis cannot be judged at this checkpoint"
			base.Evidence = []report.Evidence{{Kind: "limitation", Detail: "the frozen packet does not state the expected queue ownership"}}
			return base, true
		}
		base.ReviewHandoff = &report.ReviewHandoff{ReviewedSHA: f.observeReviewedCheckpoint(request)}
		if request.Role == workflow.RoleSpecificationReview && f.serveBlockingFinding() {
			base.ReviewHandoff.Findings = []report.ReviewFinding{{
				Location:            "internal/factory/polling.go:17",
				Claim:               "the terminal outcome never releases the queue",
				Evidence:            "no test covers the released queue",
				Severity:            report.ReviewSeverityBlocker,
				Category:            report.ReviewCategoryCorrectness,
				SuggestedResolution: "release queue ownership on the terminal transition",
				SuggestedOwner:      workflow.RoleImplementation,
			}}
		}
		return base, true
	default:
		return report.Report{}, false
	}
}

// observeImplementationPacket records whether the human repair packet reached
// an implementation invocation.
func (f *continuationFixture) observeImplementationPacket(request harness.StartRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Contains(request.Prompt, string(store.ReviewRepairSourceHuman)) && strings.Contains(request.Prompt, "4001") {
		f.humanRepairPacketSeen = true
	}
	if strings.Contains(request.Prompt, string(store.ReviewRepairEventSupervisionCommand)) && strings.Contains(request.Prompt, maintainerRepairCommentID) {
		f.commandRepairPacketSeen = true
	}
	// A repair turn resumes the implementation session that already read the
	// first prompt, so it must not replay the fenced specification.
	if request.ResumeSessionID != "" && strings.Contains(request.Prompt, "Review-repair packet") {
		f.reviewRepairPromptContinued = strings.Contains(request.Prompt, "(continuation)") && !strings.Contains(request.Prompt, "--- BEGIN SPECIFICATION PACKET ---")
	}
}

// observeReviewedCheckpoint records the exact checkpoint one reviewer saw and
// returns it as the reviewed SHA.
func (f *continuationFixture) observeReviewedCheckpoint(request harness.StartRequest) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	checkpoint := f.workspace.state.HeadSHA
	f.checkpointsReviewed[checkpoint] = struct{}{}
	_ = request
	return checkpoint
}

// serveCannotProceed returns true exactly once for a fixture that parks the
// run, so the first specification review stops for human disposition and every
// later round judges the checkpoint normally.
func (f *continuationFixture) serveCannotProceed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.parkForMaintainerRepair || f.cannotProceedServed {
		return false
	}
	f.cannotProceedServed = true
	return true
}

// serveBlockingFinding returns true exactly once, so the first review round
// produces one agent-found blocker and later rounds pass.
func (f *continuationFixture) serveBlockingFinding() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blockingReviewServed {
		return false
	}
	f.blockingReviewServed = true
	return true
}

// continuationGitHub serves a two-issue queue whose claimed issues leave the
// eligible set, and keeps one recoverable status comment per issue.
type continuationGitHub struct {
	mu       sync.Mutex
	issues   map[int]github.Issue
	order    []int
	comments map[string]github.Comment
	nextID   int
}

// newContinuationGitHub indexes the configured queue by issue number.
func newContinuationGitHub(issues []github.Issue) *continuationGitHub {
	value := &continuationGitHub{issues: map[int]github.Issue{}, comments: map[string]github.Comment{}}
	for _, issue := range issues {
		value.issues[issue.Number] = issue
		value.order = append(value.order, issue.Number)
	}
	return value
}

// ListEligibleIssues returns the open issues that still carry the queue label.
func (g *continuationGitHub) ListEligibleIssues(context.Context, github.Repository) ([]github.Issue, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	eligible := make([]github.Issue, 0, len(g.order))
	for _, number := range g.order {
		issue := g.issues[number]
		for _, label := range issue.Labels {
			if label == github.LabelAgentReady {
				eligible = append(eligible, issue)
				break
			}
		}
	}
	return eligible, nil
}

// Issue returns the current projection of one issue.
func (g *continuationGitHub) Issue(_ context.Context, _ github.Repository, number int) (github.Issue, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	issue, ok := g.issues[number]
	if !ok {
		return github.Issue{}, fmt.Errorf("issue #%d is not configured", number)
	}
	return issue, nil
}

// CreateLabel accepts the label bootstrap without recording it.
func (g *continuationGitHub) CreateLabel(context.Context, github.Repository, github.Label) error {
	return nil
}

// ReplaceIssueLabels applies the complete label set to one issue, which also
// removes it from the eligible queue when the coordinator claims it.
func (g *continuationGitHub) ReplaceIssueLabels(_ context.Context, _ github.Repository, number int, labels []string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	issue := g.issues[number]
	issue.Labels = append([]string(nil), labels...)
	g.issues[number] = issue
	return nil
}

// CreateIssueComment stores one recoverable comment per marker.
func (g *continuationGitHub) CreateIssueComment(_ context.Context, _ github.Repository, _ int, body string) (github.Comment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nextID++
	comment := github.Comment{ID: fmt.Sprintf("comment-%d", g.nextID), Body: body}
	g.comments[comment.ID] = comment
	return comment, nil
}

// FindStatusComment recovers the comment whose body carries the marker.
func (g *continuationGitHub) FindStatusComment(_ context.Context, _ github.Repository, _ int, marker string) (github.Comment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, comment := range g.comments {
		if strings.Contains(comment.Body, marker) {
			return comment, nil
		}
	}
	return github.Comment{}, nil
}

// EditIssueComment keeps the recoverable comment body current.
func (g *continuationGitHub) EditIssueComment(_ context.Context, _ github.Repository, id, body string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	comment := g.comments[id]
	comment.ID = id
	comment.Body = body
	g.comments[id] = comment
	return nil
}

// continuationPullRequests keeps one tracked pull request whose head follows
// the pushed checkpoint and whose terminal disposition the test controls.
type continuationPullRequests struct {
	mu           sync.Mutex
	workspace    *unattendedWorkspace
	current      github.PullRequest
	draftChanges []bool
}

// newContinuationPullRequests binds the fake to the pushed-checkpoint source.
func newContinuationPullRequests(workspace *unattendedWorkspace) *continuationPullRequests {
	return &continuationPullRequests{workspace: workspace}
}

// FindPullRequest returns the tracked pull request with its published head.
func (p *continuationPullRequests) FindPullRequest(context.Context, github.Repository, string, string) (github.PullRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.projection(), nil
}

// projection renders the current pull request with the last pushed head.
func (p *continuationPullRequests) projection() github.PullRequest {
	value := p.current
	if value.Number == 0 {
		return github.PullRequest{}
	}
	if pushed := p.workspace.pushedSHAs; len(pushed) > 0 && !value.Merged {
		value.HeadSHA = pushed[len(pushed)-1]
	}
	return value
}

// CreatePullRequest creates the one tracked draft pull request.
func (p *continuationPullRequests) CreatePullRequest(_ context.Context, _ github.Repository, request github.PullRequestRequest) (github.PullRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.current = github.PullRequest{
		Number: 17, URL: "https://github.com/example/project/pull/17",
		Title: request.Title, Body: request.Body, State: "open", Draft: request.Draft,
		HeadBranch: request.HeadBranch, BaseBranch: request.BaseBranch,
	}
	return p.projection(), nil
}

// UpdatePullRequest replaces the generated title and body.
func (p *continuationPullRequests) UpdatePullRequest(_ context.Context, _ github.Repository, _ int, request github.PullRequestRequest) (github.PullRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.current.Title = request.Title
	p.current.Body = request.Body
	return p.projection(), nil
}

// SetPullRequestDraft applies the explicit readiness transition.
func (p *continuationPullRequests) SetPullRequestDraft(_ context.Context, _ github.Repository, _ int, draft bool) (github.PullRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.current.Draft = draft
	p.draftChanges = append(p.draftChanges, draft)
	return p.projection(), nil
}

// draftTransitions returns every explicit readiness change the coordinator
// requested, in order. `false` means the pull request became ready.
func (p *continuationPullRequests) draftTransitions() []bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bool(nil), p.draftChanges...)
}

// merge applies the human merge the factory is never allowed to perform.
func (p *continuationPullRequests) merge(mergeCommitSHA string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.current.Merged = true
	p.current.State = "closed"
	p.current.MergeCommitSHA = mergeCommitSHA
}

// closeWithoutMerge applies the human close that cancels the run.
func (p *continuationPullRequests) closeWithoutMerge() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.current.State = "closed"
}

// continuationReviews serves the completed human reviews of the tracked pull
// request.
type continuationReviews struct {
	mu      sync.Mutex
	reviews []github.PullRequestReview
	reads   int
}

// PullRequestReviews returns every submitted review.
func (r *continuationReviews) PullRequestReviews(context.Context, github.Repository, int) ([]github.PullRequestReview, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reviews) > 0 {
		r.reads++
	}
	return append([]github.PullRequestReview(nil), r.reviews...), nil
}

// readCount reports how many polls observed a non-empty review list.
func (r *continuationReviews) readCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads
}

// submit records one newly submitted review.
func (r *continuationReviews) submit(review github.PullRequestReview) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reviews = append(r.reviews, review)
}

var _ github.Client = (*continuationGitHub)(nil)
var _ github.IssuePoller = (*continuationGitHub)(nil)
var _ github.PullRequestClient = (*continuationPullRequests)(nil)
var _ github.PullRequestDraftClient = (*continuationPullRequests)(nil)
var _ github.PullRequestReviewReader = (*continuationReviews)(nil)
var _ harness.Runtime = (*continuationHarness)(nil)
var _ worker.WorkerRuntime = (*unattendedWorker)(nil)
