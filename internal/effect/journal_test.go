package effect_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/effect"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// journalKinds is the complete set of durable effect kinds the coordinator
// reserves. Every one of them must resolve to a registered replay path.
var journalKinds = []store.PendingEffectKind{
	store.PendingEffectKindStateTransition,
	store.PendingEffectKindLabelTransition,
	store.PendingEffectKindStatusComment,
	store.PendingEffectKindCommitStatus,
	store.PendingEffectKindPush,
	store.PendingEffectKindPullRequest,
	store.PendingEffectKindCheckpoint,
	store.PendingEffectKindWorkerLaunch,
	store.PendingEffectKindHarnessResume,
	store.PendingEffectKindResultAcceptance,
	store.PendingEffectKindClarificationComment,
}

// journalStoreForTest records every reservation while answering the run and
// invocation reads a handler performs before its external mutation.
type journalStoreForTest struct {
	run        store.Run
	invocation store.Invocation
	reserved   []store.PendingEffect
	cleared    []string
}

// CurrentRun returns the fixed run projection.
func (s *journalStoreForTest) CurrentRun(context.Context) (*store.Run, error) {
	run := s.run
	return &run, nil
}

// SaveRun accepts a run projection without persisting it.
func (s *journalStoreForTest) SaveRun(context.Context, store.Run) error { return nil }

// SaveInvocation accepts an invocation projection without persisting it.
func (s *journalStoreForTest) SaveInvocation(context.Context, store.Invocation) error { return nil }

// Invocation returns the fixed invocation projection.
func (s *journalStoreForTest) Invocation(context.Context, string, string) (*store.Invocation, error) {
	invocation := s.invocation
	return &invocation, nil
}

// PendingEffect returns the most recent reservation.
func (s *journalStoreForTest) PendingEffect(context.Context, string) (*store.PendingEffect, error) {
	if len(s.reserved) == 0 {
		return nil, nil
	}
	pending := s.reserved[len(s.reserved)-1]
	return &pending, nil
}

// SavePendingEffect records one reservation for the surrounding assertion.
func (s *journalStoreForTest) SavePendingEffect(_ context.Context, pending store.PendingEffect) error {
	s.reserved = append(s.reserved, pending)
	return nil
}

// ClearPendingEffect records the acknowledged effect identity.
func (s *journalStoreForTest) ClearPendingEffect(_ context.Context, _ string, effectID string) error {
	s.cleared = append(s.cleared, effectID)
	return nil
}

// journalPresentationForTest renders bounded, deterministic projections.
type journalPresentationForTest struct{}

// StatusCommentMarker returns a deterministic marker.
func (journalPresentationForTest) StatusCommentMarker(runID string) string {
	return "<!-- status " + runID + " -->"
}

// StatusCommentBody returns a deterministic body.
func (journalPresentationForTest) StatusCommentBody(run store.Run) string {
	return "status " + run.ID
}

// ClarificationCommentMarker returns a deterministic clarification marker.
func (journalPresentationForTest) ClarificationCommentMarker(runID string, packetVersion int) string {
	return fmt.Sprintf("<!-- clarification %s v%d -->", runID, packetVersion)
}

// StateLabels returns the run status as its only label.
func (journalPresentationForTest) StateLabels(_ []string, status store.Status) []string {
	return []string{string(status)}
}

// journalProjectorForTest answers the run read and accepts every write.
type journalProjectorForTest struct {
	run store.Run
}

// Read returns the fixed run projection.
func (p journalProjectorForTest) Read(context.Context, effect.RunStore) (*store.Run, error) {
	run := p.run
	return &run, nil
}

// Save accepts a run projection.
func (journalProjectorForTest) Save(context.Context, effect.RunStore, store.Run) error { return nil }

// SaveAtRevision accepts a run projection at an expected revision.
func (journalProjectorForTest) SaveAtRevision(context.Context, effect.RunStore, int64, store.Run) error {
	return nil
}

// SaveInvalidatingResults reports no atomic seam.
func (journalProjectorForTest) SaveInvalidatingResults(context.Context, effect.RunStore, int64, store.Run) (bool, error) {
	return false, nil
}

// SaveInvalidatingAllResults reports no atomic seam.
func (journalProjectorForTest) SaveInvalidatingAllResults(context.Context, effect.RunStore, int64, store.Run) (bool, error) {
	return false, nil
}

// InvalidateResults accepts an invalidation request.
func (journalProjectorForTest) InvalidateResults(context.Context, effect.RunStore, string) error {
	return nil
}

// InvalidateAllResults accepts an invalidation request.
func (journalProjectorForTest) InvalidateAllResults(context.Context, effect.RunStore, string) error {
	return nil
}

// RecordTransition accepts an evaluation projection update.
func (journalProjectorForTest) RecordTransition(context.Context, effect.RunStore, store.Run, store.Run, time.Time) error {
	return nil
}

// errExternal is the failure every adapter in this file returns after the
// journal has already reserved the effect.
var errExternal = errors.New("external mutation unavailable")

// journalIssuesForTest fails every issue mutation after reservation.
type journalIssuesForTest struct{}

// Issue returns the injected external failure.
func (journalIssuesForTest) Issue(context.Context, github.Repository, int) (github.Issue, error) {
	return github.Issue{}, errExternal
}

// ReplaceIssueLabels returns the injected external failure.
func (journalIssuesForTest) ReplaceIssueLabels(context.Context, github.Repository, int, []string) error {
	return errExternal
}

// CreateIssueComment returns the injected external failure.
func (journalIssuesForTest) CreateIssueComment(context.Context, github.Repository, int, string) (github.Comment, error) {
	return github.Comment{}, errExternal
}

// FindStatusComment returns the injected external failure.
func (journalIssuesForTest) FindStatusComment(context.Context, github.Repository, int, string) (github.Comment, error) {
	return github.Comment{}, errExternal
}

// EditIssueComment returns the injected external failure.
func (journalIssuesForTest) EditIssueComment(context.Context, github.Repository, string, string) error {
	return errExternal
}

// recoveringLabelIssuesForTest mutates its label projection before returning
// one lost-response error.
type recoveringLabelIssuesForTest struct {
	issue        github.Issue
	failOnce     bool
	replacements int
}

// Issue returns the current label projection.
func (i *recoveringLabelIssuesForTest) Issue(context.Context, github.Repository, int) (github.Issue, error) {
	return i.issue, nil
}

// ReplaceIssueLabels applies labels and optionally loses the first response.
func (i *recoveringLabelIssuesForTest) ReplaceIssueLabels(_ context.Context, _ github.Repository, _ int, labels []string) error {
	i.issue.Labels = append([]string(nil), labels...)
	i.replacements++
	if i.failOnce {
		i.failOnce = false
		return errExternal
	}
	return nil
}

// CreateIssueComment is unused by the label handler.
func (*recoveringLabelIssuesForTest) CreateIssueComment(context.Context, github.Repository, int, string) (github.Comment, error) {
	return github.Comment{}, errors.New("unexpected comment creation")
}

// FindStatusComment is unused by the label handler.
func (*recoveringLabelIssuesForTest) FindStatusComment(context.Context, github.Repository, int, string) (github.Comment, error) {
	return github.Comment{}, errors.New("unexpected status-comment read")
}

// EditIssueComment is unused by the label handler.
func (*recoveringLabelIssuesForTest) EditIssueComment(context.Context, github.Repository, string, string) error {
	return errors.New("unexpected status-comment edit")
}

// journalWorkspaceForTest fails every Git mutation after reservation.
type journalWorkspaceForTest struct{}

// Create returns the injected external failure.
func (journalWorkspaceForTest) Create(context.Context, string, string, string) (gitadapter.Workspace, error) {
	return gitadapter.Workspace{}, errExternal
}

// Remove returns the injected external failure.
func (journalWorkspaceForTest) Remove(context.Context, string, gitadapter.Workspace) error {
	return errExternal
}

// Inspect returns the injected external failure.
func (journalWorkspaceForTest) Inspect(context.Context, string) (gitadapter.WorktreeState, error) {
	return gitadapter.WorktreeState{}, errExternal
}

// CreateCheckpoint returns the injected external failure.
func (journalWorkspaceForTest) CreateCheckpoint(context.Context, gitadapter.CheckpointRequest) (gitadapter.CheckpointResult, error) {
	return gitadapter.CheckpointResult{}, errExternal
}

// Push returns the injected external failure.
func (journalWorkspaceForTest) Push(context.Context, gitadapter.PushRequest) error {
	return errExternal
}

// SynchronizeBase returns the injected external failure.
func (journalWorkspaceForTest) SynchronizeBase(context.Context, gitadapter.BaseSyncRequest) error {
	return errExternal
}

// journalPullRequestsForTest fails every pull-request mutation.
type journalPullRequestsForTest struct{}

// FindPullRequest returns the injected external failure.
func (journalPullRequestsForTest) FindPullRequest(context.Context, github.Repository, string, string) (github.PullRequest, error) {
	return github.PullRequest{}, errExternal
}

// CreatePullRequest returns the injected external failure.
func (journalPullRequestsForTest) CreatePullRequest(context.Context, github.Repository, github.PullRequestRequest) (github.PullRequest, error) {
	return github.PullRequest{}, errExternal
}

// UpdatePullRequest returns the injected external failure.
func (journalPullRequestsForTest) UpdatePullRequest(context.Context, github.Repository, int, github.PullRequestRequest) (github.PullRequest, error) {
	return github.PullRequest{}, errExternal
}

// journalStatusesForTest fails every commit-status publication.
type journalStatusesForTest struct{}

// CreateCommitStatus returns the injected external failure.
func (journalStatusesForTest) CreateCommitStatus(context.Context, github.Repository, github.CommitStatus) error {
	return errExternal
}

// journalWorkerForTest fails every worker launch.
type journalWorkerForTest struct{}

// Start returns the injected external failure.
func (journalWorkerForTest) Start(context.Context, worker.StartRequest) error { return errExternal }

// journalHarnessForTest fails every native resume.
type journalHarnessForTest struct{}

// Capabilities identifies the deterministic test harness.
func (journalHarnessForTest) Capabilities() harness.Capabilities {
	return harness.Capabilities{Name: "codex"}
}

// Start returns the injected external failure.
func (journalHarnessForTest) Start(context.Context, harness.StartRequest) (harness.Session, error) {
	return harness.Session{}, errExternal
}

// Resume returns the injected external failure.
func (journalHarnessForTest) Resume(context.Context, harness.StartRequest) (harness.Session, error) {
	return harness.Session{}, errExternal
}

// Finish returns the injected external failure.
func (journalHarnessForTest) Finish(context.Context, harness.Session) error { return errExternal }

// journalLifecycleForTest refuses every lifecycle operation.
type journalLifecycleForTest struct{}

// StopWorker returns the injected external failure.
func (journalLifecycleForTest) StopWorker(context.Context, string) error { return errExternal }

// StopActiveWorkers returns the injected external failure.
func (journalLifecycleForTest) StopActiveWorkers(context.Context, effect.RunStore, store.Run) error {
	return errExternal
}

// HarnessRuntime returns the deterministic test harness.
func (journalLifecycleForTest) HarnessRuntime(string, string) (harness.Runtime, error) {
	return journalHarnessForTest{}, nil
}

// journalRunForTest is the minimal run projection accepted by store
// validation, which several apply paths perform before reserving.
func journalRunForTest() store.Run {
	return store.Run{
		ID: "run-effects", RepositoryPath: "/repo", IssueNumber: 42, Stage: store.StageImplementation,
		Status: store.StatusActive, Branch: "factory/run-effects", Worktree: "/worktree",
		StatusCommentID: "status-effects", ProcessedCommentID: "comment-7", Revision: 3,
		CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC(),
	}
}

// newJournalForTest wires every seam to an adapter that fails after the
// reservation, so each apply path reserves exactly one journal entry.
func newJournalForTest(run store.Run) *effect.Journal {
	return effect.New(effect.Adapters{
		Now:            func() time.Time { return time.Unix(10, 0).UTC() },
		Issues:         journalIssuesForTest{},
		Presentation:   journalPresentationForTest{},
		Projector:      journalProjectorForTest{run: run},
		Workspace:      journalWorkspaceForTest{},
		PullRequests:   journalPullRequestsForTest{},
		CommitStatuses: journalStatusesForTest{},
		Worker:         journalWorkerForTest{},
		Lifecycle:      journalLifecycleForTest{},
	})
}

// TestJournalRegistersEveryPendingEffectKind protects the dispatch contract:
// every durable kind resolves to a handler, and only an unknown kind produces
// the kernel's typed refusal.
func TestJournalRegistersEveryPendingEffectKind(t *testing.T) {
	ctx := context.Background()
	run := journalRunForTest()
	journal := newJournalForTest(run)
	runStore := &journalStoreForTest{run: run}
	for _, kind := range journalKinds {
		pending := store.PendingEffect{RunID: run.ID, ID: string(kind) + ":identity", Kind: kind, Payload: "{}"}
		_, err := journal.Replay(ctx, runStore, pending)
		var unknown *effect.UnknownKindError
		if errors.As(err, &unknown) {
			t.Fatalf("Replay(%q) = %v, want a registered replay path", kind, err)
		}
	}
	unregistered := store.PendingEffect{RunID: run.ID, ID: "future", Kind: store.PendingEffectKind("future_kind"), Payload: "{}"}
	_, err := journal.Replay(ctx, runStore, unregistered)
	var unknown *effect.UnknownKindError
	if !errors.As(err, &unknown) {
		t.Fatalf("Replay(unregistered) error = %v, want *UnknownKindError", err)
	}
	if unknown.Kind != unregistered.Kind {
		t.Fatalf("UnknownKindError.Kind = %q, want %q", unknown.Kind, unregistered.Kind)
	}
}

// TestJournalLabelApplyReplaysALostResponse verifies the formerly replay-only
// kind now reserves through the public apply interface and recognizes the
// completed label replacement during replay.
func TestJournalLabelApplyReplaysALostResponse(t *testing.T) {
	ctx := context.Background()
	run := journalRunForTest()
	runStore := &journalStoreForTest{run: run}
	issues := &recoveringLabelIssuesForTest{
		issue:    github.Issue{Number: run.IssueNumber, Labels: []string{"old"}},
		failOnce: true,
	}
	journal := effect.New(effect.Adapters{
		Now:       func() time.Time { return time.Unix(10, 0).UTC() },
		Issues:    issues,
		Projector: journalProjectorForTest{run: run},
	})
	repository := github.Repository{Owner: "example", Name: "project"}
	labels := []string{"ordinary", "agent-failed"}
	if err := journal.ApplyLabels(ctx, runStore, run.ID, repository, run.IssueNumber, labels); !errors.Is(err, errExternal) {
		t.Fatalf("ApplyLabels() error = %v, want lost response", err)
	}
	if len(runStore.reserved) != 1 || runStore.reserved[0].Kind != store.PendingEffectKindLabelTransition {
		t.Fatalf("label reservation = %#v, want one label_transition", runStore.reserved)
	}
	if _, err := journal.Replay(ctx, runStore, runStore.reserved[0]); err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if issues.replacements != 1 {
		t.Fatalf("label replacements = %d, want one", issues.replacements)
	}
	if len(runStore.cleared) != 1 || runStore.cleared[0] != runStore.reserved[0].ID {
		t.Fatalf("cleared effects = %#v, want the label reservation", runStore.cleared)
	}
}

// TestJournalReservesByteIdenticalEffectIdentities protects the durable
// identity of every apply path. A changed identity string would make an
// in-flight journal entry written by the previous build unrecognisable, so
// each expectation below is the exact NUL-delimited input that build used.
func TestJournalReservesByteIdenticalEffectIdentities(t *testing.T) {
	ctx := context.Background()
	run := journalRunForTest()
	next := run
	next.Revision = run.Revision + 1
	invocation := store.Invocation{
		ID: "inv-1", RunID: run.ID, Role: "implementation", Stage: store.StageImplementation,
		Status: store.InvocationStatusCompleted, Harness: "codex", RecoveryResumeCount: 2,
	}
	repository := github.Repository{Owner: "example", Name: "project"}
	issue := github.Issue{Number: run.IssueNumber}
	pushRequest := gitadapter.PushRequest{WorktreePath: "/worktree", Branch: "factory/run-effects"}
	checkpointRequest := gitadapter.CheckpointRequest{
		RunID: run.ID, WorktreePath: "/worktree", ParentSHA: "parent",
		Kind: gitadapter.CheckpointKindImplementation, Paths: []string{"a", "b"}, Message: "checkpoint",
	}
	pullRequest := github.PullRequestRequest{Title: "title", Body: "body", HeadBranch: "factory/run-effects", BaseBranch: "main", Draft: true}
	status := github.CommitStatus{SHA: "checkpoint", State: github.CommitStatusSuccess, Context: "factory/test", Description: "passed"}
	workerRequest := worker.StartRequest{RunID: run.ID, Role: "implementation", InvocationPath: "/invocation", ResultPath: "/result"}
	resumeRequest := harness.StartRequest{InvocationID: invocation.ID, RunID: run.ID, Role: invocation.Role}

	tests := []struct {
		name     string
		kind     store.PendingEffectKind
		identity string
		reserve  func(journal *effect.Journal, runStore *journalStoreForTest)
	}{
		{
			name: "state transition", kind: store.PendingEffectKindStateTransition,
			identity: fmt.Sprintf("revision=%d", next.Revision),
			reserve: func(journal *effect.Journal, runStore *journalStoreForTest) {
				_, _ = journal.ApplyStateTransition(ctx, runStore, effect.StateTransition{
					Repository: repository, Issue: issue, Previous: run, Next: next,
				}, next)
			},
		},
		{
			name: "label transition", kind: store.PendingEffectKindLabelTransition,
			identity: "issue=42\x00labels=ordinary\x00agent-failed",
			reserve: func(journal *effect.Journal, runStore *journalStoreForTest) {
				_ = journal.ApplyLabels(ctx, runStore, run.ID, repository, 42, []string{"ordinary", "agent-failed"})
			},
		},
		{
			name: "status comment", kind: store.PendingEffectKindStatusComment,
			identity: fmt.Sprintf("revision=%d\x00comment=%s", next.Revision, next.ProcessedCommentID),
			reserve: func(journal *effect.Journal, runStore *journalStoreForTest) {
				_, _ = journal.PersistCommandProjection(ctx, runStore, repository, run, next)
			},
		},
		{
			name: "clarification comment", kind: store.PendingEffectKindClarificationComment,
			identity: "target=42\x00version=7",
			reserve: func(journal *effect.Journal, runStore *journalStoreForTest) {
				_, _ = journal.PublishClarificationComment(ctx, runStore, repository, 42, run, 7, "questions")
			},
		},
		{
			name: "commit status", kind: store.PendingEffectKindCommitStatus,
			identity: status.SHA + "\x00" + status.Context + "\x00" + string(status.State) + "\x00" + status.Description,
			reserve: func(journal *effect.Journal, runStore *journalStoreForTest) {
				publisher := journal.CommitStatusPublisher(runStore, run.ID, journalStatusesForTest{})
				_ = publisher.CreateCommitStatus(ctx, repository, status)
			},
		},
		{
			name: "push", kind: store.PendingEffectKindPush,
			identity: pushRequest.WorktreePath + "\x00" + pushRequest.Branch + "\x00" + run.CheckpointSHA,
			reserve: func(journal *effect.Journal, runStore *journalStoreForTest) {
				_ = journal.Push(ctx, runStore, run.ID, pushRequest, run.CheckpointSHA)
			},
		},
		{
			name: "checkpoint", kind: store.PendingEffectKindCheckpoint,
			identity: checkpointRequest.ParentSHA + "\x00" + string(checkpointRequest.Kind) + "\x00" + strings.Join(checkpointRequest.Paths, "\x00"),
			reserve: func(journal *effect.Journal, runStore *journalStoreForTest) {
				_, _, _ = journal.Checkpoint(ctx, runStore, checkpointRequest, repository, issue, run, next)
			},
		},
		{
			name: "pull request creation", kind: store.PendingEffectKindPullRequest,
			identity: pullRequest.HeadBranch + "\x00" + pullRequest.BaseBranch + "\x00" + pullRequest.Body,
			reserve: func(journal *effect.Journal, runStore *journalStoreForTest) {
				_, _, _ = journal.UpsertPullRequestAndPersist(ctx, runStore, repository, issue, run, next, pullRequest, 0)
			},
		},
		{
			name: "pull request update", kind: store.PendingEffectKindPullRequest,
			identity: fmt.Sprintf("update=%d\x00%s", 11, pullRequest.Body),
			reserve: func(journal *effect.Journal, runStore *journalStoreForTest) {
				_ = journal.UpdatePullRequest(ctx, runStore, run.ID, repository, 11, pullRequest)
			},
		},
		{
			name: "worker launch", kind: store.PendingEffectKindWorkerLaunch,
			identity: workerRequest.Role + "\x00" + workerRequest.InvocationPath + "\x00" + workerRequest.ResultPath,
			reserve: func(journal *effect.Journal, runStore *journalStoreForTest) {
				_ = journal.StartWorker(ctx, runStore, workerRequest)
			},
		},
		{
			name: "harness resume", kind: store.PendingEffectKindHarnessResume,
			identity: invocation.ID + "\x00" + fmt.Sprint(invocation.RecoveryResumeCount+1),
			reserve: func(journal *effect.Journal, runStore *journalStoreForTest) {
				_, _ = journal.ResumeHarness(ctx, runStore, runStore, "/socket", journalHarnessForTest{}, invocation, resumeRequest)
			},
		},
		{
			name: "manual harness resume", kind: store.PendingEffectKindHarnessResume,
			identity: invocation.ID + "\x00manual",
			reserve: func(journal *effect.Journal, runStore *journalStoreForTest) {
				_, _ = journal.ResumeHarnessManually(ctx, runStore, runStore, "/socket", journalHarnessForTest{}, invocation, resumeRequest)
			},
		},
		{
			name: "result acceptance", kind: store.PendingEffectKindResultAcceptance,
			identity: invocation.ID + "\x00" + string(invocation.Status) + "\x00" + fmt.Sprint(next.Revision),
			reserve: func(journal *effect.Journal, runStore *journalStoreForTest) {
				_, _, _ = journal.AcceptResult(ctx, runStore, runStore, effect.ResultAcceptance{
					Repository: repository, SocketPath: "/socket", WorkerID: run.ID,
					Harness: journalHarnessForTest{}, Session: harness.Session{InvocationID: invocation.ID},
					Invocation: invocation, Previous: run, Next: next, Report: report.Report{},
				})
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			runStore := &journalStoreForTest{run: run, invocation: invocation}
			test.reserve(newJournalForTest(run), runStore)
			if len(runStore.reserved) != 1 {
				t.Fatalf("reservations = %d, want exactly one", len(runStore.reserved))
			}
			reserved := runStore.reserved[0]
			if reserved.Kind != test.kind {
				t.Fatalf("reserved kind = %q, want %q", reserved.Kind, test.kind)
			}
			want := legacyPendingEffectID(run.ID, test.kind, test.identity)
			if reserved.ID != want {
				t.Fatalf("reserved identity = %q, want %q derived from %q", reserved.ID, want, test.identity)
			}
		})
	}
}

// legacyPendingEffectID independently derives the persisted journal identity
// so the external-interface test does not require the kernel helper to remain
// exported.
func legacyPendingEffectID(runID string, kind store.PendingEffectKind, identity string) string {
	digest := sha256.Sum256([]byte(runID + "\x00" + string(kind) + "\x00" + identity))
	return string(kind) + ":" + hex.EncodeToString(digest[:])
}
