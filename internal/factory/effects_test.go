package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestExternalEffectsReserveBeforeAndClearAfterFailure verifies the common
// journal boundary for every external mutation named by issue 21. A failure
// before reservation produces no mutation; a failure after the mutation keeps
// the intent durable, and a fresh coordinator retries the same idempotent
// effect without producing a second mutation.
func TestExternalEffectsReserveBeforeAndClearAfterFailure(t *testing.T) {
	t.Parallel()

	effects := []struct {
		name string
		kind store.PendingEffectKind
	}{
		{name: "status comment", kind: store.PendingEffectKindStatusComment},
		{name: "labels", kind: store.PendingEffectKindLabelTransition},
		{name: "commit status", kind: store.PendingEffectKindCommitStatus},
		{name: "push", kind: store.PendingEffectKindPush},
		{name: "pull request creation", kind: store.PendingEffectKindPullRequest},
		{name: "checkpoint commit", kind: store.PendingEffectKindCheckpoint},
		{name: "worker launch", kind: store.PendingEffectKindWorkerLaunch},
		{name: "harness resume", kind: store.PendingEffectKindHarnessResume},
		{name: "result acceptance", kind: store.PendingEffectKindResultAcceptance},
		{name: "clarification comment", kind: store.PendingEffectKindClarificationComment},
	}

	for _, test := range effects {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			databaseDirectory := t.TempDir()
			if err := os.Chmod(databaseDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			databasePath := databaseDirectory + "/effects.db"
			opened, err := store.Open(ctx, databasePath)
			if err != nil {
				t.Fatal(err)
			}
			journal := &effectTestStore{Store: opened, saveErr: errors.New("reservation unavailable")}
			service := &Service{deps: Dependencies{Now: func() time.Time { return time.Unix(10, 0).UTC() }}}
			effect := store.PendingEffect{
				RunID: "run-effects", ID: "effect-" + string(test.kind), Kind: test.kind,
				Payload: "{}", CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC(),
			}
			beforeCalls := 0
			if err := service.withPendingEffect(ctx, journal, effect, func() error {
				beforeCalls++
				return nil
			}); err == nil {
				t.Fatal("withPendingEffect() before failure = nil, want reservation failure")
			}
			if beforeCalls != 0 {
				t.Fatalf("external calls before reservation = %d, want zero", beforeCalls)
			}
			pending, err := journal.PendingEffect(ctx, effect.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if pending != nil {
				t.Fatalf("pending effect after reservation failure = %#v, want nil", pending)
			}

			journal.saveErr = nil
			applied := false
			calls := 0
			firstAttempt := true
			apply := func() error {
				if !applied {
					applied = true
					calls++
				}
				if firstAttempt {
					firstAttempt = false
					return errors.New("response lost after external mutation")
				}
				return nil
			}
			if err := service.withPendingEffect(ctx, journal, effect, apply); err == nil {
				t.Fatal("withPendingEffect() after failure = nil, want lost-response error")
			}
			pending, err = journal.PendingEffect(ctx, effect.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if pending == nil || pending.ID != effect.ID {
				t.Fatalf("pending effect after lost response = %#v, want %q", pending, effect.ID)
			}
			if calls != 1 {
				t.Fatalf("external calls after first attempt = %d, want one", calls)
			}
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}

			reopened, err := store.Open(ctx, databasePath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close() }()
			restarted := &Service{deps: Dependencies{Now: func() time.Time { return time.Unix(20, 0).UTC() }}}
			if err := restarted.withPendingEffect(ctx, reopened, effect, apply); err != nil {
				t.Fatalf("withPendingEffect() after restart = %v", err)
			}
			if calls != 1 {
				t.Fatalf("external calls after idempotent restart = %d, want one", calls)
			}
			pending, err = reopened.PendingEffect(ctx, effect.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if pending != nil {
				t.Fatalf("pending effect after idempotent restart = %#v, want nil", pending)
			}
		})
	}
}

// effectTestStore injects a reservation failure while retaining the real
// SQLite journal implementation for restart durability assertions.
type effectTestStore struct {
	*store.Store
	saveErr error
}

// SavePendingEffect optionally fails before delegating to SQLite.
func (s *effectTestStore) SavePendingEffect(ctx context.Context, effect store.PendingEffect) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	return s.Store.SavePendingEffect(ctx, effect)
}

// TestStateTransitionEffectReplaysTheActualCommentAndLabelMutation verifies
// that a response lost after GitHub accepted a state transition is recovered
// by observing the existing label set and coordinator comment.
func TestStateTransitionEffectReplaysTheActualCommentAndLabelMutation(t *testing.T) {
	ctx := context.Background()
	opened, databasePath, run := openEffectMatrixStore(t, ctx)
	githubRuntime := &effectMatrixGitHub{
		issue:           github.Issue{Number: run.IssueNumber, Labels: []string{github.LabelAgentRunning}},
		failCommentOnce: true,
	}
	service := newEffectMatrixService(githubRuntime, nil, nil, nil)
	previous := run
	next := run
	next.Status = store.StatusWaitingForHuman
	next.Revision++
	blocked := &effectTestStore{Store: opened, saveErr: errors.New("reservation unavailable")}
	if _, err := service.applyStateTransition(ctx, blocked, stateTransition{
		Repository:    github.Repository{Owner: "example", Name: "project"},
		Issue:         githubRuntime.issue,
		Previous:      previous,
		Next:          next,
		CreateComment: true,
	}); err == nil {
		t.Fatal("applyStateTransition() before reservation = nil, want reservation failure")
	}
	if githubRuntime.createCommentCalls != 0 || githubRuntime.replaceLabelCalls != 0 {
		t.Fatalf("GitHub mutations before state-transition reservation = comments %d, labels %d; want zero", githubRuntime.createCommentCalls, githubRuntime.replaceLabelCalls)
	}
	_, err := service.applyStateTransition(ctx, opened, stateTransition{
		Repository:    github.Repository{Owner: "example", Name: "project"},
		Issue:         githubRuntime.issue,
		Previous:      previous,
		Next:          next,
		CreateComment: true,
	})
	if err == nil {
		t.Fatal("applyStateTransition() = nil, want response-loss error")
	}
	pending, err := opened.PendingEffect(ctx, run.ID)
	if err != nil || pending == nil {
		t.Fatalf("pending state transition = %#v, error = %v; want durable effect", pending, err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	restarted := newEffectMatrixService(githubRuntime, nil, nil, nil)
	if _, err := restarted.replayPendingEffect(ctx, reopened, *pending); err != nil {
		t.Fatalf("replayPendingEffect() = %v, want existing GitHub projections to complete the effect", err)
	}
	if githubRuntime.createCommentCalls != 1 || githubRuntime.replaceLabelCalls != 1 {
		t.Fatalf("GitHub mutations after replay = comments %d, labels %d; want one each", githubRuntime.createCommentCalls, githubRuntime.replaceLabelCalls)
	}
	if pending, err := reopened.PendingEffect(ctx, run.ID); err != nil || pending != nil {
		t.Fatalf("pending state transition after replay = %#v, error = %v; want cleared", pending, err)
	}
}

// TestLabelEffectReplaysTheActualIssueLabelMutation verifies standalone label
// replacement recognizes the desired label set after a lost response.
func TestLabelEffectReplaysTheActualIssueLabelMutation(t *testing.T) {
	ctx := context.Background()
	opened, databasePath, run := openEffectMatrixStore(t, ctx)
	githubRuntime := &effectMatrixGitHub{
		issue:         github.Issue{Number: run.IssueNumber, Labels: []string{github.LabelAgentRunning}},
		failLabelOnce: true,
	}
	service := newEffectMatrixService(githubRuntime, nil, nil, nil)
	payload := labelTransitionEffectPayload{
		Repository:  github.Repository{Owner: "example", Name: "project"},
		IssueNumber: run.IssueNumber,
		Labels:      []string{"ordinary", github.LabelAgentFailed},
	}
	effect, err := service.newPendingEffect(run.ID, store.PendingEffectKindLabelTransition, "labels", payload)
	if err != nil {
		t.Fatal(err)
	}
	blocked := &effectTestStore{Store: opened, saveErr: errors.New("reservation unavailable")}
	if err := service.withPendingEffect(ctx, blocked, effect, func() error {
		return githubRuntime.ReplaceIssueLabels(ctx, payload.Repository, payload.IssueNumber, payload.Labels)
	}); err == nil {
		t.Fatal("label mutation before reservation = nil, want reservation failure")
	}
	if githubRuntime.replaceLabelCalls != 0 {
		t.Fatalf("issue label mutations before reservation = %d, want zero", githubRuntime.replaceLabelCalls)
	}
	if err := service.withPendingEffect(ctx, opened, effect, func() error {
		return githubRuntime.ReplaceIssueLabels(ctx, payload.Repository, payload.IssueNumber, payload.Labels)
	}); err == nil {
		t.Fatal("label mutation = nil, want response-loss error")
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	restarted := newEffectMatrixService(githubRuntime, nil, nil, nil)
	pending, err := reopened.PendingEffect(ctx, run.ID)
	if err != nil || pending == nil {
		t.Fatalf("pending label effect = %#v, error = %v", pending, err)
	}
	if _, err := restarted.replayPendingEffect(ctx, reopened, *pending); err != nil {
		t.Fatalf("replayPendingEffect() = %v", err)
	}
	if githubRuntime.replaceLabelCalls != 1 {
		t.Fatalf("issue label mutations = %d, want one", githubRuntime.replaceLabelCalls)
	}
}

// TestCommitStatusEffectReplaysTheActualStatusMutation verifies exact status
// identity lookup suppresses a duplicate after GitHub accepted a status.
func TestCommitStatusEffectReplaysTheActualStatusMutation(t *testing.T) {
	ctx := context.Background()
	opened, databasePath, run := openEffectMatrixStore(t, ctx)
	statusRuntime := &effectMatrixCommitStatus{failOnce: true}
	service := newEffectMatrixService(nil, statusRuntime, nil, nil)
	publisher := commitStatusPublisher{service: service, runStore: opened, runID: run.ID, delegate: statusRuntime}
	status := github.CommitStatus{SHA: "checkpoint", State: github.CommitStatusSuccess, Context: "factory/test", Description: "passed"}
	blocked := publisher
	blocked.runStore = &effectTestStore{Store: opened, saveErr: errors.New("reservation unavailable")}
	if err := blocked.CreateCommitStatus(ctx, github.Repository{Owner: "example", Name: "project"}, status); err == nil {
		t.Fatal("CreateCommitStatus() before reservation = nil, want reservation failure")
	}
	if statusRuntime.createCalls != 0 {
		t.Fatalf("commit status mutations before reservation = %d, want zero", statusRuntime.createCalls)
	}
	if err := publisher.CreateCommitStatus(ctx, github.Repository{Owner: "example", Name: "project"}, status); err == nil {
		t.Fatal("CreateCommitStatus() = nil, want response-loss error")
	}
	if statusRuntime.createCalls != 1 || len(statusRuntime.statuses) != 1 {
		t.Fatalf("status mutation after first attempt = calls %d statuses %d, want one", statusRuntime.createCalls, len(statusRuntime.statuses))
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	restarted := newEffectMatrixService(nil, statusRuntime, nil, nil)
	pending, err := reopened.PendingEffect(ctx, run.ID)
	if err != nil || pending == nil {
		t.Fatalf("pending commit status = %#v, error = %v", pending, err)
	}
	if _, err := restarted.replayPendingEffect(ctx, reopened, *pending); err != nil {
		t.Fatalf("replayPendingEffect() = %v", err)
	}
	if statusRuntime.createCalls != 1 {
		t.Fatalf("commit status mutations after replay = %d, want one", statusRuntime.createCalls)
	}
}

// TestPushEffectReplaysTheActualRemoteMutation verifies remote-head
// observation suppresses a second push after a transport response is lost.
func TestPushEffectReplaysTheActualRemoteMutation(t *testing.T) {
	ctx := context.Background()
	opened, databasePath, run := openEffectMatrixStore(t, ctx)
	workspace := &effectMatrixGitWorkspace{expectedRemoteHead: "checkpoint", checkpointSHA: "checkpoint", failPushOnce: true}
	service := newEffectMatrixService(nil, nil, workspace, nil)
	request := gitadapter.PushRequest{WorktreePath: "/worktree", Branch: run.Branch}
	blocked := &effectTestStore{Store: opened, saveErr: errors.New("reservation unavailable")}
	if err := service.pushWithEffect(ctx, blocked, run.ID, workspace, request, workspace.expectedRemoteHead); err == nil {
		t.Fatal("pushWithEffect() before reservation = nil, want reservation failure")
	}
	if workspace.pushMutations != 0 {
		t.Fatalf("push mutations before reservation = %d, want zero", workspace.pushMutations)
	}
	if err := service.pushWithEffect(ctx, opened, run.ID, workspace, request, workspace.expectedRemoteHead); err == nil {
		t.Fatal("pushWithEffect() = nil, want response-loss error")
	}
	if workspace.pushMutations != 1 {
		t.Fatalf("push mutations after first attempt = %d, want one", workspace.pushMutations)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	restarted := newEffectMatrixService(nil, nil, workspace, nil)
	pending, err := reopened.PendingEffect(ctx, run.ID)
	if err != nil || pending == nil {
		t.Fatalf("pending push = %#v, error = %v", pending, err)
	}
	if _, err := restarted.replayPendingEffect(ctx, reopened, *pending); err != nil {
		t.Fatalf("replayPendingEffect() = %v", err)
	}
	if workspace.pushMutations != 1 {
		t.Fatalf("push mutations after replay = %d, want one", workspace.pushMutations)
	}
}

// TestPushEffectRejectsAChangedLocalHead verifies a replay cannot publish a
// different local commit under the durable effect's original checkpoint ID.
func TestPushEffectRejectsAChangedLocalHead(t *testing.T) {
	ctx := context.Background()
	opened, _, run := openEffectMatrixStore(t, ctx)
	defer func() { _ = opened.Close() }()
	workspace := &effectMatrixGitWorkspace{expectedRemoteHead: "checkpoint", checkpointSHA: "new-local-head"}
	service := newEffectMatrixService(nil, nil, workspace, nil)
	request := gitadapter.PushRequest{WorktreePath: run.Worktree, Branch: run.Branch}

	err := service.pushWithEffect(ctx, opened, run.ID, workspace, request, workspace.expectedRemoteHead)
	if err == nil || !strings.Contains(err.Error(), "local worktree HEAD") {
		t.Fatalf("pushWithEffect() error = %v, want changed-local-HEAD refusal", err)
	}
	if workspace.pushMutations != 0 {
		t.Fatalf("push mutations = %d, want zero for changed local HEAD", workspace.pushMutations)
	}
}

// TestPullRequestEffectReplaysTheActualCreation verifies branch identity
// lookup recognizes a PR created before the create response was lost.
func TestPullRequestEffectReplaysTheActualCreation(t *testing.T) {
	ctx := context.Background()
	opened, databasePath, run := openEffectMatrixStore(t, ctx)
	pullRequests := &effectMatrixPullRequests{failCreateOnce: true}
	service := newEffectMatrixService(nil, nil, nil, pullRequests)
	request := github.PullRequestRequest{Title: "Factory PR", Body: "body", HeadBranch: run.Branch, BaseBranch: "main", Draft: true}
	payload := pullRequestEffectPayload{Repository: github.Repository{Owner: "example", Name: "project"}, Request: request}
	effect, err := service.newPendingEffect(run.ID, store.PendingEffectKindPullRequest, "create", payload)
	if err != nil {
		t.Fatal(err)
	}
	blocked := &effectTestStore{Store: opened, saveErr: errors.New("reservation unavailable")}
	if err := service.withPendingEffect(ctx, blocked, effect, func() error {
		_, err := service.upsertPullRequestRequest(ctx, pullRequests, payload.Repository, 0, request)
		return err
	}); err == nil {
		t.Fatal("pull-request creation before reservation = nil, want reservation failure")
	}
	if pullRequests.createCalls != 0 {
		t.Fatalf("pull-request creations before reservation = %d, want zero", pullRequests.createCalls)
	}
	if err := service.withPendingEffect(ctx, opened, effect, func() error {
		_, err := service.upsertPullRequestRequest(ctx, pullRequests, payload.Repository, 0, request)
		return err
	}); err == nil {
		t.Fatal("pull-request creation = nil, want response-loss error")
	}
	if pullRequests.createCalls != 1 {
		t.Fatalf("pull-request creations after first attempt = %d, want one", pullRequests.createCalls)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	restarted := newEffectMatrixService(nil, nil, nil, pullRequests)
	pending, err := reopened.PendingEffect(ctx, run.ID)
	if err != nil || pending == nil {
		t.Fatalf("pending pull request = %#v, error = %v", pending, err)
	}
	if _, err := restarted.replayPendingEffect(ctx, reopened, *pending); err != nil {
		t.Fatalf("replayPendingEffect() = %v", err)
	}
	if pullRequests.createCalls != 1 {
		t.Fatalf("pull-request creations after replay = %d, want one", pullRequests.createCalls)
	}
}

// TestPullRequestReplayRejectsNewerPersistedRevisionBeforeMutation verifies a
// response-loss replay refuses a stale run before inspecting or updating its PR.
func TestPullRequestReplayRejectsNewerPersistedRevisionBeforeMutation(t *testing.T) {
	ctx := context.Background()
	opened, _, run := openEffectMatrixStore(t, ctx)
	defer func() { _ = opened.Close() }()
	pullRequests := &effectMatrixPullRequests{failCreateOnce: true}
	service := newEffectMatrixService(nil, nil, nil, pullRequests)
	repository := github.Repository{Owner: "example", Name: "project"}
	previous := run
	next := run
	next.Revision++
	request := github.PullRequestRequest{Title: "Factory PR", Body: "body", HeadBranch: run.Branch, BaseBranch: "main", Draft: true}
	if _, _, err := service.upsertPullRequestAndPersistWithEffect(ctx, opened, pullRequests, repository, github.Issue{Number: run.IssueNumber}, previous, next, request, 0); err == nil {
		t.Fatal("upsertPullRequestAndPersistWithEffect() = nil, want response-loss error")
	}
	if pullRequests.createCalls != 1 {
		t.Fatalf("pull-request creations after first attempt = %d, want one", pullRequests.createCalls)
	}
	pending, err := opened.PendingEffect(ctx, run.ID)
	if err != nil || pending == nil {
		t.Fatalf("pending pull request = %#v, error = %v", pending, err)
	}

	newer := run
	newer.Revision = next.Revision + 1
	if err := opened.SaveRun(ctx, newer); err != nil {
		t.Fatalf("persist newer run revision: %v", err)
	}
	pullRequests.existing.Body = "operator changed body"
	if _, err := service.replayPendingEffect(ctx, opened, *pending); err == nil || !strings.Contains(err.Error(), "older than current revision") {
		t.Fatalf("replayPendingEffect() error = %v, want newer-revision refusal", err)
	}
	if pullRequests.findCalls != 1 || pullRequests.updateCalls != 0 || pullRequests.createCalls != 1 {
		t.Fatalf("pull-request effects after stale replay = finds=%d updates=%d creates=%d, want 1/0/1", pullRequests.findCalls, pullRequests.updateCalls, pullRequests.createCalls)
	}
}

// TestCheckpointEffectReplaysTheActualCommit verifies the Git checkpoint
// marker adapter returns the existing commit after a lost commit response.
func TestCheckpointEffectReplaysTheActualCommit(t *testing.T) {
	ctx := context.Background()
	opened, databasePath, run := openEffectMatrixStore(t, ctx)
	next := run
	next.Revision++
	githubRuntime := &effectMatrixGitHub{
		issue:         github.Issue{Number: run.IssueNumber, Labels: []string{github.LabelAgentRunning}},
		statusComment: github.Comment{ID: run.StatusCommentID},
	}
	githubRuntime.statusComment.Body = statusCommentBody(next)
	workspace := &effectMatrixGitWorkspace{failCheckpointOnce: true}
	service := newEffectMatrixService(githubRuntime, nil, workspace, nil)
	request := gitadapter.CheckpointRequest{RunID: run.ID, WorktreePath: run.Worktree, ParentSHA: "parent", Kind: gitadapter.CheckpointKindImplementation, Message: "checkpoint"}
	payload := checkpointEffectPayload{
		Request:    checkpointRequestJSON{RunID: request.RunID, WorktreePath: request.WorktreePath, ParentSHA: request.ParentSHA, Kind: string(request.Kind), Message: request.Message},
		Repository: github.Repository{Owner: "example", Name: "project"},
		Issue:      githubRuntime.issue,
		Previous:   run,
		Next:       next,
	}
	effect, err := service.newPendingEffect(run.ID, store.PendingEffectKindCheckpoint, "checkpoint", payload)
	if err != nil {
		t.Fatal(err)
	}
	blocked := &effectTestStore{Store: opened, saveErr: errors.New("reservation unavailable")}
	if err := service.withPendingEffect(ctx, blocked, effect, func() error {
		_, err := workspace.CreateCheckpoint(ctx, request)
		return err
	}); err == nil {
		t.Fatal("checkpoint creation before reservation = nil, want reservation failure")
	}
	if workspace.checkpointMutations != 0 {
		t.Fatalf("checkpoint commits before reservation = %d, want zero", workspace.checkpointMutations)
	}
	if err := service.withPendingEffect(ctx, opened, effect, func() error {
		_, err := workspace.CreateCheckpoint(ctx, request)
		return err
	}); err == nil {
		t.Fatal("checkpoint creation = nil, want response-loss error")
	}
	if workspace.checkpointMutations != 1 {
		t.Fatalf("checkpoint commits after first attempt = %d, want one", workspace.checkpointMutations)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	restarted := newEffectMatrixService(githubRuntime, nil, workspace, nil)
	pending, err := reopened.PendingEffect(ctx, run.ID)
	if err != nil || pending == nil {
		t.Fatalf("pending checkpoint = %#v, error = %v", pending, err)
	}
	if _, err := restarted.replayPendingEffect(ctx, reopened, *pending); err != nil {
		t.Fatalf("replayPendingEffect() = %v", err)
	}
	if workspace.checkpointMutations != 1 {
		t.Fatalf("checkpoint commits after replay = %d, want one", workspace.checkpointMutations)
	}
}

// TestWorkerLaunchEffectReplaysTheActualWorkerStart verifies Docker-style
// run identity reuse prevents a second worker creation after response loss.
func TestWorkerLaunchEffectReplaysTheActualWorkerStart(t *testing.T) {
	ctx := context.Background()
	opened, databasePath, run := openEffectMatrixStore(t, ctx)
	workerRuntime := &effectMatrixWorker{failStartOnce: true}
	service := newEffectMatrixService(nil, nil, nil, nil)
	service.deps.Worker = workerRuntime
	request := worker.StartRequest{RunID: run.ID, WorktreePath: run.Worktree, GitMetadataPath: "/git", Image: "worker", ImageDigest: "digest", Role: "implementation"}
	blocked := &effectTestStore{Store: opened, saveErr: errors.New("reservation unavailable")}
	if err := service.startWorkerWithEffect(ctx, blocked, request); err == nil {
		t.Fatal("startWorkerWithEffect() before reservation = nil, want reservation failure")
	}
	if workerRuntime.startMutations != 0 {
		t.Fatalf("worker creations before reservation = %d, want zero", workerRuntime.startMutations)
	}
	if err := service.startWorkerWithEffect(ctx, opened, request); err == nil {
		t.Fatal("startWorkerWithEffect() = nil, want response-loss error")
	}
	if workerRuntime.startMutations != 1 {
		t.Fatalf("worker creations after first attempt = %d, want one", workerRuntime.startMutations)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	restarted := newEffectMatrixService(nil, nil, nil, nil)
	restarted.deps.Worker = workerRuntime
	pending, err := reopened.PendingEffect(ctx, run.ID)
	if err != nil || pending == nil {
		t.Fatalf("pending worker launch = %#v, error = %v", pending, err)
	}
	if _, err := restarted.replayPendingEffect(ctx, reopened, *pending); err != nil {
		t.Fatalf("replayPendingEffect() = %v", err)
	}
	if workerRuntime.startMutations != 1 {
		t.Fatalf("worker creations after replay = %d, want one", workerRuntime.startMutations)
	}
}

// TestResultAcceptanceEffectReplaysTheActualFinish verifies a lost harness
// finalization response is safely abandoned rather than issued a second time
// before the remaining durable projections are completed.
func TestResultAcceptanceEffectReplaysTheActualFinish(t *testing.T) {
	ctx := context.Background()
	opened, databasePath, run := openEffectMatrixStore(t, ctx)
	githubRuntime := &effectMatrixGitHub{
		issue:         github.Issue{Number: run.IssueNumber, Labels: []string{github.LabelAgentRunning}},
		statusComment: github.Comment{ID: run.StatusCommentID},
	}
	harnessRuntime := &effectMatrixHarness{failFinishOnce: true}
	service := newEffectMatrixService(githubRuntime, nil, nil, nil)
	service.deps.Harness = harnessRuntime
	invocation := store.Invocation{
		ID: "inv-effects", RunID: run.ID, Harness: harness.NameCodex, Role: "implementation", Stage: store.StageImplementation,
		NativeSessionID: "session-effects", WorkspaceID: "workspace-effects", ImplementationSurfaceID: "surface-effects",
		Status: store.InvocationStatusActive, CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt,
	}
	if err := opened.SaveInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	accepted := invocation
	accepted.Status = store.InvocationStatusCompleted
	next := run
	next.Revision++
	githubRuntime.statusComment.Body = statusCommentBody(next)
	session := harness.Session{InvocationID: invocation.ID, NativeSessionID: invocation.NativeSessionID, Surface: terminal.Surface{ID: terminal.SurfaceID(invocation.ImplementationSurfaceID), WorkspaceID: terminal.WorkspaceID(invocation.WorkspaceID)}}
	testReport := report.Report{
		SchemaVersion: 1, InvocationID: invocation.ID, RunID: run.ID,
		Harness: invocation.Harness, Role: invocation.Role, Stage: string(invocation.Stage),
		Outcome: report.OutcomeCompleted, Summary: "test acceptance",
	}
	blocked := &effectTestStore{Store: opened, saveErr: errors.New("reservation unavailable")}
	if _, _, err := service.acceptResultWithEffect(ctx, blocked, blocked, effectMatrixRegistration(), harnessRuntime, session, accepted, run, next, false, testReport); err == nil {
		t.Fatal("acceptResultWithEffect() before reservation = nil, want reservation failure")
	}
	if harnessRuntime.finishMutations != 0 {
		t.Fatalf("harness finishes before result-acceptance reservation = %d, want zero", harnessRuntime.finishMutations)
	}
	if _, _, err := service.acceptResultWithEffect(ctx, opened, opened, effectMatrixRegistration(), harnessRuntime, session, accepted, run, next, false, testReport); err == nil {
		t.Fatal("acceptResultWithEffect() = nil, want response-loss error")
	}
	if harnessRuntime.finishMutations != 1 {
		t.Fatalf("harness finishes after first attempt = %d, want one", harnessRuntime.finishMutations)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	restarted := newEffectMatrixService(githubRuntime, nil, nil, nil)
	restarted.deps.Harness = harnessRuntime
	pending, err := reopened.PendingEffect(ctx, run.ID)
	if err != nil || pending == nil {
		t.Fatalf("pending result acceptance = %#v, error = %v", pending, err)
	}
	if _, err := restarted.replayPendingEffect(ctx, reopened, *pending); err != nil {
		t.Fatalf("replayPendingEffect() = %v", err)
	}
	if harnessRuntime.finishMutations != 1 || harnessRuntime.finishCalls != 1 {
		t.Fatalf("harness finish calls/mutations after replay = %d/%d, want 1/1", harnessRuntime.finishCalls, harnessRuntime.finishMutations)
	}
}

// TestRepeatedTestAcceptanceResumesAnUnfinishedStageProjection verifies a
// terminal invocation is not mistaken for a fully projected test-stage result
// when the durable run still points at that stage.
func TestRepeatedTestAcceptanceResumesAnUnfinishedStageProjection(t *testing.T) {
	ctx := context.Background()
	opened, _, run := openEffectMatrixStore(t, ctx)
	run.Stage = store.StageTest
	run.SpecificationPacket = "{}"
	if err := opened.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	resultDirectory := t.TempDir()
	invocation := store.Invocation{
		ID: "inv-test-projection", RunID: run.ID, Harness: harness.NameCodex, Role: "test", Stage: store.StageTest,
		NativeSessionID: "session-test-projection", ResultDirectory: resultDirectory,
		Status: store.InvocationStatusCompleted, CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt,
	}
	if err := opened.SaveInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	value := report.Report{
		SchemaVersion: report.SchemaVersion, InvocationID: invocation.ID, RunID: run.ID,
		Harness: invocation.Harness, Role: invocation.Role, Stage: string(invocation.Stage),
		Outcome: report.OutcomeCompleted, Summary: "red test is ready",
		TestHandoff: &report.TestHandoff{FocusedTestCommand: "go test ./...", ExpectedFailureReason: "wanted failure"},
		ReportedAt:  time.Unix(2, 0).UTC(),
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resultDirectory, report.ReportFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	registration := config.RepositoryRegistration{Path: run.RepositoryPath, OperationalDataPath: "effects.db", GitHub: config.GitHubConfig{Owner: "example", Repository: "project"}}
	workspace := &effectMatrixGitWorkspace{checkpointSHA: run.CheckpointSHA}
	service := newEffectMatrixService(nil, nil, workspace, nil)
	service.configPath = "/host/config.yaml"
	service.deps.Config = effectMatrixConfig{host: config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{registration}}}
	service.deps.OpenStore = func(context.Context, string) (OperationalStore, error) { return opened, nil }
	service.startupChecked = true

	_, err = service.AcceptAgentReport(ctx, AgentReportRequest{RunID: run.ID, InvocationID: invocation.ID})
	if err == nil || !strings.Contains(err.Error(), "worker runtime is required for red-test verification") {
		t.Fatalf("AcceptAgentReport() error = %v, want unfinished test projection to resume", err)
	}
}

// TestResultAcceptanceReplayRejectsNewerPersistedRevisionBeforeSideEffects
// verifies a response-loss replay refuses a stale run before reserving or
// repeating harness finalization and worker shutdown.
func TestResultAcceptanceReplayRejectsNewerPersistedRevisionBeforeSideEffects(t *testing.T) {
	ctx := context.Background()
	opened, _, run := openEffectMatrixStore(t, ctx)
	defer func() { _ = opened.Close() }()
	harnessRuntime := &effectMatrixHarness{failFinishOnce: true}
	workerRuntime := &effectMatrixWorker{started: true}
	service := newEffectMatrixService(nil, nil, nil, nil)
	service.deps.Harness = harnessRuntime
	service.deps.Worker = workerRuntime
	invocation := store.Invocation{
		ID: "inv-stale-acceptance", RunID: run.ID, Harness: harness.NameCodex, Role: "implementation", Stage: store.StageImplementation,
		NativeSessionID: "session-stale-acceptance", Status: store.InvocationStatusActive, CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt,
	}
	if err := opened.SaveInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	accepted := invocation
	accepted.Status = store.InvocationStatusCompleted
	previous := run
	next := run
	next.Revision++
	session := harness.Session{InvocationID: invocation.ID, NativeSessionID: invocation.NativeSessionID}
	testReport := report.Report{
		SchemaVersion: 1, InvocationID: invocation.ID, RunID: run.ID,
		Harness: invocation.Harness, Role: invocation.Role, Stage: string(invocation.Stage),
		Outcome: report.OutcomeCompleted, Summary: "test stale acceptance",
	}
	if _, _, err := service.acceptResultWithEffect(ctx, opened, opened, effectMatrixRegistration(), harnessRuntime, session, accepted, previous, next, true, testReport); err == nil {
		t.Fatal("acceptResultWithEffect() = nil, want response-loss error")
	}
	if harnessRuntime.finishCalls != 1 || harnessRuntime.finishMutations != 1 || workerRuntime.stopCalls != 0 {
		t.Fatalf("first result acceptance effects = finishes=%d/%d stops=%d, want 1/1/0", harnessRuntime.finishCalls, harnessRuntime.finishMutations, workerRuntime.stopCalls)
	}
	pending, err := opened.PendingEffect(ctx, run.ID)
	if err != nil || pending == nil {
		t.Fatalf("pending result acceptance = %#v, error = %v", pending, err)
	}

	newer := run
	newer.Revision = next.Revision + 1
	if err := opened.SaveRun(ctx, newer); err != nil {
		t.Fatalf("persist newer run revision: %v", err)
	}
	if _, err := service.replayPendingEffect(ctx, opened, *pending); err == nil || !strings.Contains(err.Error(), "older than current revision") {
		t.Fatalf("replayPendingEffect() error = %v, want newer-revision refusal", err)
	}
	if harnessRuntime.finishCalls != 1 || harnessRuntime.finishMutations != 1 || workerRuntime.stopCalls != 0 {
		t.Fatalf("stale result acceptance effects = finishes=%d/%d stops=%d, want unchanged 1/1/0", harnessRuntime.finishCalls, harnessRuntime.finishMutations, workerRuntime.stopCalls)
	}
	persisted, err := opened.Invocation(ctx, run.ID, invocation.ID)
	if err != nil || persisted == nil || persisted.Status != store.InvocationStatusCompleted {
		t.Fatalf("persisted invocation after stale replay = %#v, error = %v; want original terminal reservation", persisted, err)
	}
}

// TestHarnessResumeEffectDoesNotDuplicateAfterResponseLoss verifies the
// persisted resume reservation prevents a second native command after the
// first native response is lost.
func TestHarnessResumeEffectDoesNotDuplicateAfterResponseLoss(t *testing.T) {
	ctx := context.Background()
	opened, databasePath, run := openEffectMatrixStore(t, ctx)
	invocation := store.Invocation{
		ID: "inv-resume-effects", RunID: run.ID, Harness: harness.NameCodex, Role: "implementation", Stage: store.StageImplementation,
		NativeSessionID: "session-resume-effects", Status: store.InvocationStatusActive, CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt,
	}
	if err := opened.SaveInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	runtime := &journalRecoveryHarness{nativeSessionID: invocation.NativeSessionID, failResumeOnce: true}
	service := newEffectMatrixService(nil, nil, nil, nil)
	request := harness.StartRequest{InvocationID: invocation.ID, RunID: run.ID, Role: invocation.Role, Stage: string(invocation.Stage), ResumeSessionID: invocation.NativeSessionID}
	if _, err := service.resumeHarnessWithEffect(ctx, opened, opened, "", runtime, invocation, request); err == nil {
		t.Fatal("resumeHarnessWithEffect() = nil, want response-loss error")
	}
	if runtime.resumeCalls != 1 {
		t.Fatalf("native resumes after first attempt = %d, want one", runtime.resumeCalls)
	}
	persisted, err := opened.Invocation(ctx, run.ID, invocation.ID)
	if err != nil || persisted == nil || persisted.RecoveryResumeCount != 1 {
		t.Fatalf("reserved invocation = %#v, error = %v; want resume count one", persisted, err)
	}
	pending, err := opened.PendingEffect(ctx, run.ID)
	if err != nil || pending == nil || pending.Kind != store.PendingEffectKindHarnessResume {
		t.Fatalf("pending harness resume = %#v, error = %v", pending, err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	restarted := newEffectMatrixService(nil, nil, nil, nil)
	restarted.deps.Harness = runtime
	if _, err := restarted.replayPendingHarnessResume(ctx, reopened, *pending); err != nil {
		t.Fatalf("replayPendingHarnessResume() = %v", err)
	}
	if runtime.resumeCalls != 1 {
		t.Fatalf("native resumes after replay = %d, want no duplicate", runtime.resumeCalls)
	}
	pending, err = reopened.PendingEffect(ctx, run.ID)
	if err != nil || pending != nil {
		t.Fatalf("pending harness resume after replay = %#v, error = %v; want cleared", pending, err)
	}
}

// TestHarnessResumeRateLimitRollsBackTheAutomaticReservation verifies a
// temporary capacity failure leaves the one-shot recovery slot unused and the
// journal clear for a later retry.
func TestHarnessResumeRateLimitRollsBackTheAutomaticReservation(t *testing.T) {
	ctx := context.Background()
	opened, _, run := openEffectMatrixStore(t, ctx)
	defer func() { _ = opened.Close() }()
	invocation := store.Invocation{
		ID: "inv-rate-limit", RunID: run.ID, Harness: harness.NameCodex,
		Role: "implementation", Stage: store.StageImplementation,
		NativeSessionID: "session-rate-limit", Status: store.InvocationStatusActive,
		CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt,
	}
	if err := opened.SaveInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	runtime := &effectMatrixHarness{resumeErr: harness.NewRateLimitError(harness.NameCodex)}
	service := newEffectMatrixService(nil, nil, nil, nil)
	request := harness.StartRequest{
		InvocationID: invocation.ID, RunID: run.ID, Role: invocation.Role,
		Stage: string(invocation.Stage), ResumeSessionID: invocation.NativeSessionID,
	}
	if _, err := service.resumeHarnessWithEffect(ctx, opened, opened, "", runtime, invocation, request); !harness.IsRateLimited(err) {
		t.Fatalf("resumeHarnessWithEffect() error = %v, want typed rate limit", err)
	}
	persisted, err := opened.Invocation(ctx, run.ID, invocation.ID)
	if err != nil || persisted == nil {
		t.Fatalf("persisted invocation = %#v, error = %v", persisted, err)
	}
	if persisted.RecoveryResumeCount != 0 || persisted.AttachRequired {
		t.Fatalf("persisted rate-limit reservation = %#v, want count zero and no attach gate", persisted)
	}
	pending, err := opened.PendingEffect(ctx, run.ID)
	if err != nil || pending != nil {
		t.Fatalf("pending rate-limit effect = %#v, error = %v; want cleared", pending, err)
	}
	if runtime.resumeCalls != 1 {
		t.Fatalf("rate-limit resume calls = %d, want one", runtime.resumeCalls)
	}
}

// TestHarnessResumeAuthenticationFailureIsTypedAndRedacted verifies an
// expired credential pauses before consuming the automatic resume slot.
func TestHarnessResumeAuthenticationFailureIsTypedAndRedacted(t *testing.T) {
	ctx := context.Background()
	opened, _, run := openEffectMatrixStore(t, ctx)
	defer func() { _ = opened.Close() }()
	invocation := store.Invocation{
		ID: "inv-auth-expired", RunID: run.ID, Harness: harness.NameCodex,
		Role: "implementation", Stage: store.StageImplementation,
		NativeSessionID: "session-auth-expired", Status: store.InvocationStatusActive,
		CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt,
	}
	if err := opened.SaveInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	secret := "token=super-secret"
	runtime := &effectMatrixHarness{resumeErr: errors.New("HTTP 401: " + secret)}
	service := newEffectMatrixService(nil, nil, nil, nil)
	request := harness.StartRequest{
		InvocationID: invocation.ID, RunID: run.ID, Role: invocation.Role,
		Stage: string(invocation.Stage), ResumeSessionID: invocation.NativeSessionID,
	}
	err := error(nil)
	_, err = service.resumeHarnessWithEffect(ctx, opened, opened, "", runtime, invocation, request)
	if !harness.IsAuthenticationExpired(err) {
		t.Fatalf("resumeHarnessWithEffect() error = %v, want typed authentication failure", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("authentication error leaked credential: %v", err)
	}
	persisted, lookupErr := opened.Invocation(ctx, run.ID, invocation.ID)
	if lookupErr != nil || persisted == nil {
		t.Fatalf("persisted invocation = %#v, error = %v", persisted, lookupErr)
	}
	if persisted.RecoveryResumeCount != 0 {
		t.Fatalf("persisted authentication reservation = %#v, want count zero", persisted)
	}
	if pending, pendingErr := opened.PendingEffect(ctx, run.ID); pendingErr != nil || pending != nil {
		t.Fatalf("pending authentication effect = %#v, error = %v; want cleared", pending, pendingErr)
	}
}

// TestManualHarnessResumeSetsAnAttachGateWithoutConsumingAutomaticBudget
// verifies explicit recovery is durable and cannot be mistaken for an
// automatically authorized continuation.
func TestManualHarnessResumeSetsAnAttachGateWithoutConsumingAutomaticBudget(t *testing.T) {
	ctx := context.Background()
	opened, _, run := openEffectMatrixStore(t, ctx)
	defer func() { _ = opened.Close() }()
	invocation := store.Invocation{
		ID: "inv-manual-resume", RunID: run.ID, Harness: harness.NameCodex,
		Role: "implementation", Stage: store.StageImplementation,
		NativeSessionID: "session-manual-resume", Status: store.InvocationStatusActive,
		CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt,
	}
	if err := opened.SaveInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	runtime := &effectMatrixHarness{}
	service := newEffectMatrixService(nil, nil, nil, nil)
	request := harness.StartRequest{
		InvocationID: invocation.ID, RunID: run.ID, Role: invocation.Role,
		Stage: string(invocation.Stage), ResumeSessionID: invocation.NativeSessionID,
	}
	updated, err := service.resumeHarnessManuallyWithEffect(ctx, opened, opened, "", runtime, invocation, request)
	if err != nil {
		t.Fatalf("resumeHarnessManuallyWithEffect() error = %v", err)
	}
	if !updated.AttachRequired || updated.RecoveryResumeCount != 0 {
		t.Fatalf("manual resume = %#v, want attach gate and unchanged automatic count", updated)
	}
	if err := service.ensureInvocationAttached(ctx, opened, run); err == nil {
		t.Fatal("ensureInvocationAttached() = nil, want manual attach gate")
	} else {
		var gateErr *ManualResumeRequiredError
		if !errors.As(err, &gateErr) {
			t.Fatalf("ensureInvocationAttached() error = %v, want ManualResumeRequiredError", err)
		}
	}
	if runtime.resumeCalls != 1 {
		t.Fatalf("manual resume calls = %d, want one", runtime.resumeCalls)
	}
}

// TestCommandProjectionEffectReplaysTheWatermarkAndComment verifies a
// response lost after the command comment edit leaves a replayable watermark
// and does not edit the same comment twice after restart.
func TestCommandProjectionEffectReplaysTheWatermarkAndComment(t *testing.T) {
	ctx := context.Background()
	opened, databasePath, run := openEffectMatrixStore(t, ctx)
	githubRuntime := &effectMatrixGitHub{
		issue:         github.Issue{Number: run.IssueNumber, Labels: []string{github.LabelAgentRunning}},
		statusComment: github.Comment{ID: run.StatusCommentID, Body: statusCommentBody(run)},
		failEditOnce:  true,
	}
	service := newEffectMatrixService(githubRuntime, nil, nil, nil)
	repository := github.Repository{Owner: "example", Name: "project"}
	previous := run
	next := run
	next.Revision++
	next.ProcessedCommentID = "comment-command-effects"
	next.ProcessedCommentRevision = next.Revision
	next.LastCommandName = "answer"
	next.LastCommandOutcome = string(CommandAccepted)
	next.LastCommandMessage = "clarification accepted"
	blocked := &effectTestStore{Store: opened, saveErr: errors.New("reservation unavailable")}
	if _, err := service.persistCommandProjectionWithEffect(ctx, blocked, repository, previous, next); err == nil {
		t.Fatal("persistCommandProjectionWithEffect() before reservation = nil, want reservation failure")
	}
	if githubRuntime.editCommentCalls != 0 {
		t.Fatalf("comment edits before command reservation = %d, want zero", githubRuntime.editCommentCalls)
	}
	if _, err := service.persistCommandProjectionWithEffect(ctx, opened, repository, previous, next); err == nil {
		t.Fatal("persistCommandProjectionWithEffect() = nil, want response-loss error")
	}
	if githubRuntime.editCommentCalls != 1 {
		t.Fatalf("comment edits after first command attempt = %d, want one", githubRuntime.editCommentCalls)
	}
	pending, err := opened.PendingEffect(ctx, run.ID)
	if err != nil || pending == nil || pending.Kind != store.PendingEffectKindStatusComment {
		t.Fatalf("pending command projection = %#v, error = %v", pending, err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	restarted := newEffectMatrixService(githubRuntime, nil, nil, nil)
	if _, err := restarted.replayPendingEffect(ctx, reopened, *pending); err != nil {
		t.Fatalf("replayPendingEffect() = %v", err)
	}
	if githubRuntime.editCommentCalls != 1 {
		t.Fatalf("comment edits after command replay = %d, want no duplicate", githubRuntime.editCommentCalls)
	}
	current, err := reopened.CurrentRun(ctx)
	if err != nil || current == nil || current.ProcessedCommentID != next.ProcessedCommentID || current.Revision != next.Revision {
		t.Fatalf("persisted command projection = %#v, error = %v; want watermark revision %d", current, err, next.Revision)
	}
}

// TestClarificationPublicationRecoversACommentAfterResponseLoss verifies the
// marker lookup closes the create-to-persist gap without a second comment.
func TestClarificationPublicationRecoversACommentAfterResponseLoss(t *testing.T) {
	ctx := context.Background()
	opened, databasePath, run := openEffectMatrixStore(t, ctx)
	packet, err := json.Marshal(SpecificationPacket{Version: specificationPacketVersion})
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.StatusWaitingForHuman
	run.PendingQuestions = []store.PendingQuestion{{ID: "format", Prompt: "Which format should be used?"}}
	run.SpecificationPacket = string(packet)
	run.ClarificationNotificationSent = true
	if err := opened.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	githubRuntime := &effectMatrixGitHub{issue: github.Issue{Number: run.IssueNumber}, failCommentOnce: true}
	service := newEffectMatrixService(githubRuntime, nil, nil, nil)
	blocked := &effectTestStore{Store: opened, saveErr: errors.New("reservation unavailable")}
	if _, err := service.ensureClarificationPublication(ctx, effectMatrixRegistration(), blocked, run); err == nil {
		t.Fatal("ensureClarificationPublication() before reservation = nil, want reservation failure")
	}
	if githubRuntime.createCommentCalls != 0 {
		t.Fatalf("clarification comment creates before reservation = %d, want zero", githubRuntime.createCommentCalls)
	}
	if _, err := service.ensureClarificationPublication(ctx, effectMatrixRegistration(), opened, run); err == nil {
		t.Fatal("ensureClarificationPublication() = nil, want response-loss error")
	}
	if githubRuntime.createCommentCalls != 1 {
		t.Fatalf("clarification comment creates after first attempt = %d, want one", githubRuntime.createCommentCalls)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	current, err := reopened.CurrentRun(ctx)
	if err != nil || current == nil {
		t.Fatalf("current waiting run = %#v, error = %v", current, err)
	}
	restarted := newEffectMatrixService(githubRuntime, nil, nil, nil)
	if _, err := restarted.ensureClarificationPublication(ctx, effectMatrixRegistration(), reopened, *current); err != nil {
		t.Fatalf("ensureClarificationPublication() after restart = %v", err)
	}
	if githubRuntime.createCommentCalls != 1 {
		t.Fatalf("clarification comment creates after restart = %d, want one", githubRuntime.createCommentCalls)
	}
	if current, err := reopened.CurrentRun(ctx); err != nil || current == nil || current.ClarificationCommentID != "comment-effects" {
		t.Fatalf("persisted clarification identity = %#v, error = %v; want comment-effects", current, err)
	}
}

// openEffectMatrixStore creates a real SQLite operational store with one run
// so each operation-level test can close and reopen the journal.
func openEffectMatrixStore(t *testing.T, ctx context.Context) (*store.Store, string, store.Run) {
	t.Helper()
	databaseDirectory := t.TempDir()
	if err := os.Chmod(databaseDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(databaseDirectory, "effects.db")
	opened, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	run := store.Run{
		ID: "run-effects", RepositoryPath: "/repo", IssueNumber: 42, Stage: store.StageImplementation,
		Status: store.StatusActive, Branch: "factory/run-effects", Worktree: "/worktree", StatusCommentID: "status-effects",
		CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC(),
	}
	if err := opened.SaveRun(ctx, run); err != nil {
		_ = opened.Close()
		t.Fatal(err)
	}
	return opened, databasePath, run
}

// newEffectMatrixService wires only the external seam needed by one effect
// replay test, leaving all production defaults out of the assertion.
func newEffectMatrixService(githubRuntime github.Client, statuses github.CommitStatusPublisher, workspace gitadapter.GitWorkspace, pullRequests github.PullRequestClient) *Service {
	return &Service{deps: Dependencies{GitHub: githubRuntime, CommitStatuses: statuses, GitWorkspace: workspace, PullRequests: pullRequests, Now: func() time.Time { return time.Unix(10, 0).UTC() }}}
}

// effectMatrixRegistration supplies the repository identity needed by result
// acceptance payloads without loading host configuration.
func effectMatrixRegistration() config.RepositoryRegistration {
	return config.RepositoryRegistration{GitHub: config.GitHubConfig{Owner: "example", Repository: "project"}}
}

// effectMatrixConfig supplies one host registration to public-seam effect
// tests without involving the filesystem-backed configuration repository.
type effectMatrixConfig struct {
	host config.HostConfig
}

// Load returns the configured host registration.
func (c effectMatrixConfig) Load(string) (config.HostConfig, error) { return c.host, nil }

// Save satisfies ConfigRepository for the read-only fixture.
func (effectMatrixConfig) Save(string, config.HostConfig) error { return nil }

// Create returns the configured host registration.
func (c effectMatrixConfig) Create(string) (config.HostConfig, error) { return c.host, nil }

// effectMatrixGitHub is a stateful GitHub fake that mutates its projection
// before optionally returning a lost-response error.
type effectMatrixGitHub struct {
	issue              github.Issue
	statusComment      github.Comment
	failLabelOnce      bool
	failCommentOnce    bool
	failEditOnce       bool
	replaceLabelCalls  int
	createCommentCalls int
	editCommentCalls   int
}

// Issue returns the mutable issue projection used by effect replay.
func (g *effectMatrixGitHub) Issue(context.Context, github.Repository, int) (github.Issue, error) {
	issue := g.issue
	issue.Labels = append([]string(nil), g.issue.Labels...)
	return issue, nil
}

// CreateLabel satisfies the GitHub client seam for effect tests.
func (*effectMatrixGitHub) CreateLabel(context.Context, github.Repository, github.Label) error {
	return nil
}

// ReplaceIssueLabels mutates the issue before optionally losing its response.
func (g *effectMatrixGitHub) ReplaceIssueLabels(_ context.Context, _ github.Repository, _ int, labels []string) error {
	g.replaceLabelCalls++
	g.issue.Labels = append([]string(nil), labels...)
	if g.failLabelOnce {
		g.failLabelOnce = false
		return errors.New("GitHub label response lost")
	}
	return nil
}

// CreateIssueComment mutates the status comment before optionally losing its
// response, modeling the create boundary that caused duplicate comments.
func (g *effectMatrixGitHub) CreateIssueComment(_ context.Context, _ github.Repository, _ int, body string) (github.Comment, error) {
	g.createCommentCalls++
	g.statusComment = github.Comment{ID: "comment-effects", Body: body}
	if g.failCommentOnce {
		g.failCommentOnce = false
		return github.Comment{}, errors.New("GitHub comment response lost")
	}
	return g.statusComment, nil
}

// FindStatusComment returns the coordinator-owned comment projection.
func (g *effectMatrixGitHub) FindStatusComment(context.Context, github.Repository, int, string) (github.Comment, error) {
	return g.statusComment, nil
}

// EditIssueComment updates the existing coordinator-owned comment.
func (g *effectMatrixGitHub) EditIssueComment(_ context.Context, _ github.Repository, id, body string) error {
	g.editCommentCalls++
	g.statusComment.ID = id
	g.statusComment.Body = body
	if g.failEditOnce {
		g.failEditOnce = false
		return errors.New("GitHub comment edit response lost")
	}
	return nil
}

var _ github.Client = (*effectMatrixGitHub)(nil)

// effectMatrixCommitStatus is a stateful Commit Status fake with exact-status
// readback and a response-loss injection after the first mutation.
type effectMatrixCommitStatus struct {
	statuses    []github.CommitStatus
	failOnce    bool
	createCalls int
}

// ListCommitStatuses returns all statuses currently attached to the SHA.
func (s *effectMatrixCommitStatus) ListCommitStatuses(context.Context, github.Repository, string) ([]github.CommitStatus, error) {
	return append([]github.CommitStatus(nil), s.statuses...), nil
}

// CreateCommitStatus records one status and can lose its response afterward.
func (s *effectMatrixCommitStatus) CreateCommitStatus(_ context.Context, _ github.Repository, status github.CommitStatus) error {
	s.createCalls++
	s.statuses = append(s.statuses, status)
	if s.failOnce {
		s.failOnce = false
		return errors.New("GitHub status response lost")
	}
	return nil
}

var _ github.CommitStatusPublisher = (*effectMatrixCommitStatus)(nil)
var _ github.CommitStatusReader = (*effectMatrixCommitStatus)(nil)

// effectMatrixPullRequests models branch-scoped PR creation and discovery.
type effectMatrixPullRequests struct {
	existing       github.PullRequest
	failCreateOnce bool
	findCalls      int
	createCalls    int
	updateCalls    int
}

// FindPullRequest returns the PR created by the first attempt, if any.
func (p *effectMatrixPullRequests) FindPullRequest(context.Context, github.Repository, string, string) (github.PullRequest, error) {
	p.findCalls++
	return p.existing, nil
}

// CreatePullRequest creates one PR and can lose its response after mutation.
func (p *effectMatrixPullRequests) CreatePullRequest(_ context.Context, _ github.Repository, request github.PullRequestRequest) (github.PullRequest, error) {
	p.createCalls++
	p.existing = github.PullRequest{Number: 17, URL: "https://github.com/example/project/pull/17", Title: request.Title, Body: request.Body, Draft: request.Draft, HeadBranch: request.HeadBranch, BaseBranch: request.BaseBranch}
	if p.failCreateOnce {
		p.failCreateOnce = false
		return github.PullRequest{}, errors.New("GitHub pull-request response lost")
	}
	return p.existing, nil
}

// UpdatePullRequest satisfies the pull-request seam for this creation test.
func (p *effectMatrixPullRequests) UpdatePullRequest(_ context.Context, _ github.Repository, _ int, request github.PullRequestRequest) (github.PullRequest, error) {
	p.updateCalls++
	p.existing.Title = request.Title
	p.existing.Body = request.Body
	p.existing.Draft = request.Draft
	return p.existing, nil
}

var _ github.PullRequestClient = (*effectMatrixPullRequests)(nil)

// effectMatrixGitWorkspace models Git's idempotent checkpoint and remote-head
// projections while exposing semantic mutation counts to the tests.
type effectMatrixGitWorkspace struct {
	expectedRemoteHead  string
	remoteHead          string
	failPushOnce        bool
	pushMutations       int
	checkpointSHA       string
	failCheckpointOnce  bool
	checkpointMutations int
}

// Create creates no worktree in this focused effect fake.
func (*effectMatrixGitWorkspace) Create(context.Context, string, string, string) (gitadapter.Workspace, error) {
	return gitadapter.Workspace{}, nil
}

// Remove removes no worktree in this focused effect fake.
func (*effectMatrixGitWorkspace) Remove(context.Context, string, gitadapter.Workspace) error {
	return nil
}

// Inspect returns the parent checkpoint as the stable worktree projection.
func (w *effectMatrixGitWorkspace) Inspect(context.Context, string) (gitadapter.WorktreeState, error) {
	return gitadapter.WorktreeState{HeadSHA: w.checkpointSHA}, nil
}

// CreateCheckpoint creates one semantic checkpoint and returns it on every
// later call, even when the first response was lost.
func (w *effectMatrixGitWorkspace) CreateCheckpoint(context.Context, gitadapter.CheckpointRequest) (gitadapter.CheckpointResult, error) {
	if w.checkpointSHA == "" {
		w.checkpointSHA = strings.Repeat("a", 40)
		w.checkpointMutations++
		if w.failCheckpointOnce {
			w.failCheckpointOnce = false
			return gitadapter.CheckpointResult{SHA: w.checkpointSHA, Created: true}, errors.New("Git commit response lost")
		}
		return gitadapter.CheckpointResult{SHA: w.checkpointSHA, Created: true}, nil
	}
	return gitadapter.CheckpointResult{SHA: w.checkpointSHA}, nil
}

// Push publishes the expected remote head and can lose the first response.
func (w *effectMatrixGitWorkspace) Push(context.Context, gitadapter.PushRequest) error {
	w.remoteHead = w.expectedRemoteHead
	w.pushMutations++
	if w.failPushOnce {
		w.failPushOnce = false
		return errors.New("Git push response lost")
	}
	return nil
}

// SynchronizeBase satisfies the Git workspace seam for effect tests.
func (*effectMatrixGitWorkspace) SynchronizeBase(context.Context, gitadapter.BaseSyncRequest) error {
	return nil
}

// RemoteBranchHead returns the remote projection used to suppress a replayed
// push.
func (w *effectMatrixGitWorkspace) RemoteBranchHead(context.Context, gitadapter.PushRequest) (string, error) {
	return w.remoteHead, nil
}

var _ gitadapter.GitWorkspace = (*effectMatrixGitWorkspace)(nil)
var _ gitadapter.RemoteBranchInspector = (*effectMatrixGitWorkspace)(nil)

// effectMatrixWorker models Docker's run-identity reuse behavior.
type effectMatrixWorker struct {
	started        bool
	failStartOnce  bool
	startMutations int
	stopCalls      int
}

// Start creates one worker and reuses it on replay.
func (w *effectMatrixWorker) Start(context.Context, worker.StartRequest) error {
	if w.started {
		return nil
	}
	w.started = true
	w.startMutations++
	if w.failStartOnce {
		w.failStartOnce = false
		return errors.New("Docker start response lost")
	}
	return nil
}

// Resume satisfies the worker seam for effect tests.
func (*effectMatrixWorker) Resume(context.Context, worker.ResumeRequest) error { return nil }

// RunCommand satisfies the worker seam for effect tests.
func (*effectMatrixWorker) RunCommand(context.Context, worker.CommandRequest) (worker.CommandResult, error) {
	return worker.CommandResult{}, nil
}

// Stop records the stop call and returns nil.
func (w *effectMatrixWorker) Stop(context.Context, string) error {
	w.stopCalls++
	return nil
}

// Inspect returns the current worker projection.
func (w *effectMatrixWorker) Inspect(context.Context, string) (worker.Inspection, error) {
	return worker.Inspection{Exists: w.started, Running: w.started}, nil
}

var _ worker.WorkerRuntime = (*effectMatrixWorker)(nil)

// effectMatrixHarness models an idempotent harness finalization command whose
// first response is lost after the visible mutation.
type effectMatrixHarness struct {
	finished        bool
	failFinishOnce  bool
	finishCalls     int
	finishMutations int
	resumeCalls     int
	resumeErr       error
}

// Capabilities reports the harness identity used by the result payload.
func (*effectMatrixHarness) Capabilities() harness.Capabilities {
	return harness.Capabilities{Name: harness.NameCodex, InteractiveResume: true}
}

// Start satisfies the harness lifecycle seam for effect tests.
func (*effectMatrixHarness) Start(context.Context, harness.StartRequest) (harness.Session, error) {
	return harness.Session{}, nil
}

// Resume satisfies the harness lifecycle seam for effect tests.
func (h *effectMatrixHarness) Resume(_ context.Context, request harness.StartRequest) (harness.Session, error) {
	h.resumeCalls++
	if h.resumeErr != nil {
		return harness.Session{InvocationID: request.InvocationID, NativeSessionID: request.ResumeSessionID, Surface: request.Surface}, h.resumeErr
	}
	return harness.Session{InvocationID: request.InvocationID, NativeSessionID: request.ResumeSessionID, Surface: request.Surface}, nil
}

// Finish performs one semantic finalization and reports a lost first response.
func (h *effectMatrixHarness) Finish(context.Context, harness.Session) error {
	h.finishCalls++
	if !h.finished {
		h.finished = true
		h.finishMutations++
		if h.failFinishOnce {
			h.failFinishOnce = false
			return errors.New("harness finish response lost")
		}
	}
	return nil
}

var _ harness.Runtime = (*effectMatrixHarness)(nil)

// TestStateTransitionRetryAfterTransientFailureIsNotBlocked verifies that a
// retried transition is not refused by its own leftover reservation. Every
// production caller stamps a fresh UpdatedAt and reads the issue again before
// retrying, so the second attempt's payload always differs from the first even
// though the effect identity is unchanged. Treating that as a conflict blocked
// all further progression for the run until the coordinator process restarted.
func TestStateTransitionRetryAfterTransientFailureIsNotBlocked(t *testing.T) {
	ctx := context.Background()
	opened, _, run := openEffectMatrixStore(t, ctx)
	defer func() { _ = opened.Close() }()

	githubRuntime := &effectMatrixGitHub{
		issue:           github.Issue{Number: run.IssueNumber, Labels: []string{github.LabelAgentRunning}},
		failCommentOnce: true,
	}
	service := newEffectMatrixService(githubRuntime, nil, nil, nil)
	repository := github.Repository{Owner: "example", Name: "project"}

	next := run
	next.Status = store.StatusWaitingForHuman
	next.Revision++
	next.UpdatedAt = time.Unix(100, 0).UTC()
	if _, err := service.applyStateTransition(ctx, opened, stateTransition{
		Repository: repository, Issue: githubRuntime.issue,
		Previous: run, Next: next, CreateComment: true,
	}); err == nil {
		t.Fatal("applyStateTransition() first attempt = nil, want the injected response loss")
	}
	pending, err := opened.PendingEffect(ctx, run.ID)
	if err != nil || pending == nil {
		t.Fatalf("pending effect after response loss = %#v, error = %v; want a durable reservation", pending, err)
	}

	// The caller retries the same logical transition. Only the wall clock and
	// the freshly read issue snapshot moved.
	retry := next
	retry.UpdatedAt = time.Unix(101, 0).UTC()
	updated, err := service.applyStateTransition(ctx, opened, stateTransition{
		Repository: repository, Issue: githubRuntime.issue,
		Previous: run, Next: retry, CreateComment: true,
	})
	if err != nil {
		t.Fatalf("applyStateTransition() retry = %v, want the reservation to be reused", err)
	}
	if errors.Is(err, store.ErrPendingEffectConflict) {
		t.Fatal("retry was refused by its own leftover reservation")
	}
	if updated.Status != store.StatusWaitingForHuman {
		t.Fatalf("retried transition status = %q, want %q", updated.Status, store.StatusWaitingForHuman)
	}
	if githubRuntime.createCommentCalls != 1 {
		t.Fatalf("status comment creations = %d, want exactly one across both attempts", githubRuntime.createCommentCalls)
	}
	if pending, err := opened.PendingEffect(ctx, run.ID); err != nil || pending != nil {
		t.Fatalf("pending effect after successful retry = %#v, error = %v; want cleared", pending, err)
	}
}

// markerAwareGitHub resolves FindStatusComment by the requested marker, so a
// test can tell one coordinator-owned comment apart from another on the same
// issue. The shared effectMatrixGitHub deliberately keeps a single comment.
type markerAwareGitHub struct {
	issue    github.Issue
	comments []github.Comment
	creates  int
	edits    int
}

// Issue returns the fixed issue projection.
func (g *markerAwareGitHub) Issue(context.Context, github.Repository, int) (github.Issue, error) {
	return g.issue, nil
}

// CreateLabel is unused by clarification publication.
func (g *markerAwareGitHub) CreateLabel(context.Context, github.Repository, github.Label) error {
	return nil
}

// ReplaceIssueLabels records the complete desired label set.
func (g *markerAwareGitHub) ReplaceIssueLabels(_ context.Context, _ github.Repository, _ int, labels []string) error {
	g.issue.Labels = append([]string(nil), labels...)
	return nil
}

// CreateIssueComment appends a new comment with a distinct identity.
func (g *markerAwareGitHub) CreateIssueComment(_ context.Context, _ github.Repository, _ int, body string) (github.Comment, error) {
	g.creates++
	comment := github.Comment{ID: fmt.Sprintf("comment-%d", g.creates), Body: body}
	g.comments = append(g.comments, comment)
	return comment, nil
}

// FindStatusComment returns the first comment carrying the requested marker.
func (g *markerAwareGitHub) FindStatusComment(_ context.Context, _ github.Repository, _ int, marker string) (github.Comment, error) {
	for _, comment := range g.comments {
		if strings.Contains(comment.Body, marker) {
			return comment, nil
		}
	}
	return github.Comment{}, nil
}

// EditIssueComment rewrites one existing comment body in place.
func (g *markerAwareGitHub) EditIssueComment(_ context.Context, _ github.Repository, id, body string) error {
	g.edits++
	for index, comment := range g.comments {
		if comment.ID == id {
			g.comments[index].Body = body
			return nil
		}
	}
	return fmt.Errorf("unknown comment %q", id)
}

// TestClarificationRoundsDoNotOverwriteEachOther verifies that a second round
// of questions is published as its own comment. The marker is scoped to the
// specification packet version, so replay stays idempotent within a round while
// an answered round's questions remain readable on the issue.
func TestClarificationRoundsDoNotOverwriteEachOther(t *testing.T) {
	ctx := context.Background()
	opened, _, run := openEffectMatrixStore(t, ctx)
	defer func() { _ = opened.Close() }()

	firstPacket, err := json.Marshal(SpecificationPacket{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.StatusWaitingForHuman
	run.PendingQuestions = []store.PendingQuestion{{ID: "format", Prompt: "Which format should be used?"}}
	run.SpecificationPacket = string(firstPacket)
	run.ClarificationNotificationSent = true
	if err := opened.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	githubRuntime := &markerAwareGitHub{issue: github.Issue{Number: run.IssueNumber}}
	service := newEffectMatrixService(githubRuntime, nil, nil, nil)

	published, err := service.ensureClarificationPublication(ctx, effectMatrixRegistration(), opened, run)
	if err != nil {
		t.Fatalf("first clarification round = %v", err)
	}
	// Publishing the same round again must recognize the existing comment.
	republished, err := service.ensureClarificationPublication(ctx, effectMatrixRegistration(), opened, published)
	if err != nil {
		t.Fatalf("replayed first clarification round = %v", err)
	}
	if githubRuntime.creates != 1 {
		t.Fatalf("comment creates after replaying one round = %d, want one", githubRuntime.creates)
	}

	// The answer advances the packet version and clears the round identity.
	secondPacket, err := json.Marshal(SpecificationPacket{Version: 2})
	if err != nil {
		t.Fatal(err)
	}
	secondRound := republished
	secondRound.SpecificationPacket = string(secondPacket)
	secondRound.PendingQuestions = []store.PendingQuestion{{ID: "scope", Prompt: "Which scope applies?"}}
	secondRound.ClarificationCommentID = ""
	if _, err := service.ensureClarificationPublication(ctx, effectMatrixRegistration(), opened, secondRound); err != nil {
		t.Fatalf("second clarification round = %v", err)
	}
	if githubRuntime.creates != 2 {
		t.Fatalf("comment creates after a second round = %d, want two", githubRuntime.creates)
	}
	if len(githubRuntime.comments) != 2 {
		t.Fatalf("comments on the issue = %d, want both rounds retained", len(githubRuntime.comments))
	}
	if !strings.Contains(githubRuntime.comments[0].Body, "Which format should be used?") {
		t.Fatalf("first round comment was overwritten: %q", githubRuntime.comments[0].Body)
	}
	if !strings.Contains(githubRuntime.comments[1].Body, "Which scope applies?") {
		t.Fatalf("second round comment = %q, want the new questions", githubRuntime.comments[1].Body)
	}
}
