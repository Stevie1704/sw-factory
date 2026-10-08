package effect_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/effect"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// The payloads below are literal JSON in the shape that the GitHub-typed
// build wrote to store.PendingEffect.Payload. They are the stored data that a
// run journaled before the tracker and code-host ports must still replay.
const (
	legacyLabelTransitionPayload = `{"Repository":{"Owner":"acme","Name":"widget"},"IssueNumber":42,"Labels":["ordinary","agent-failed"]}`
	legacyCommitStatusPayload    = `{"Repository":{"Owner":"acme","Name":"widget"},"Status":{"SHA":"0123456789abcdef0123456789abcdef01234567","State":"failure","Context":"factory/test","Description":"tests failed","TargetURL":"https://example.test/run"}}`
	legacyPullRequestPayload     = `{"Repository":{"Owner":"acme","Name":"widget"},"Number":9,"Request":{"Title":"Fix the widget","Body":"generated body","HeadBranch":"factory/run-effects","BaseBranch":"main","Draft":true},"PersistRun":false,"Issue":{"Number":0,"Title":"","Body":"","State":"","Labels":null,"IsPullRequest":false,"UpdatedAt":"0001-01-01T00:00:00Z"},"Previous":{},"Next":{}}`
)

// legacyRepository is the repository identity every legacy payload names.
var legacyRepository = tracker.Repository{Owner: "acme", Name: "widget"}

// recordingIssuesForTest records the complete label replacement a replay
// sends to the work tracker.
type recordingIssuesForTest struct {
	journalIssuesForTest
	repository tracker.Repository
	number     int
	labels     []string
}

// Issue returns an issue whose labels differ from the replayed set.
func (r *recordingIssuesForTest) Issue(_ context.Context, _ tracker.Repository, number int) (tracker.Issue, error) {
	return tracker.Issue{Number: number, Labels: []string{"old"}}, nil
}

// ReplaceIssueLabels records the replayed label replacement.
func (r *recordingIssuesForTest) ReplaceIssueLabels(_ context.Context, repository tracker.Repository, number int, labels []string) error {
	r.repository, r.number, r.labels = repository, number, labels
	return nil
}

// recordingStatusesForTest records the commit status a replay publishes.
type recordingStatusesForTest struct {
	repository tracker.Repository
	status     github.CommitStatus
}

// ListCommitStatuses reports no existing status, so replay must publish.
func (*recordingStatusesForTest) ListCommitStatuses(context.Context, tracker.Repository, string) ([]github.CommitStatus, error) {
	return nil, nil
}

// CreateCommitStatus records the replayed status.
func (r *recordingStatusesForTest) CreateCommitStatus(_ context.Context, repository tracker.Repository, status github.CommitStatus) error {
	r.repository, r.status = repository, status
	return nil
}

// recordingPullRequestsForTest records the pull-request update a replay sends.
type recordingPullRequestsForTest struct {
	repository tracker.Repository
	number     int
	request    github.PullRequestRequest
}

// FindPullRequest reports the existing pull request with an outdated body.
func (*recordingPullRequestsForTest) FindPullRequest(_ context.Context, _ tracker.Repository, head, base string) (github.PullRequest, error) {
	return github.PullRequest{Number: 9, Title: "Fix the widget", Body: "outdated", HeadBranch: head, BaseBranch: base, Draft: true}, nil
}

// CreatePullRequest is never reached when the pull request exists.
func (*recordingPullRequestsForTest) CreatePullRequest(context.Context, tracker.Repository, github.PullRequestRequest) (github.PullRequest, error) {
	return github.PullRequest{}, errExternal
}

// UpdatePullRequest records the replayed update.
func (r *recordingPullRequestsForTest) UpdatePullRequest(_ context.Context, repository tracker.Repository, number int, request github.PullRequestRequest) (github.PullRequest, error) {
	r.repository, r.number, r.request = repository, number, request
	return github.PullRequest{Number: number}, nil
}

// TestReplayAcceptsPayloadsJournaledBeforeTheTrackerPorts proves that a
// pending effect written by the GitHub-typed build replays unchanged: the
// adapters receive the same repository, issue, status, and pull-request
// values the old payload recorded.
func TestReplayAcceptsPayloadsJournaledBeforeTheTrackerPorts(t *testing.T) {
	ctx := context.Background()
	run := journalRunForTest()
	issues := &recordingIssuesForTest{}
	statuses := &recordingStatusesForTest{}
	pullRequests := &recordingPullRequestsForTest{}
	journal := effect.New(effect.Adapters{
		Now:            func() time.Time { return time.Unix(10, 0).UTC() },
		Issues:         issues,
		Projector:      journalProjectorForTest{run: run},
		CommitStatuses: statuses,
		PullRequests:   pullRequests,
	})
	replay := func(kind store.PendingEffectKind, payload string) {
		t.Helper()
		pending := store.PendingEffect{RunID: run.ID, ID: string(kind) + ":legacy", Kind: kind, Payload: payload}
		if _, err := journal.Replay(ctx, &journalStoreForTest{run: run}, pending); err != nil {
			t.Fatalf("Replay(%s) error = %v", kind, err)
		}
	}

	replay(store.PendingEffectKindLabelTransition, legacyLabelTransitionPayload)
	if issues.repository != legacyRepository || issues.number != 42 || !reflect.DeepEqual(issues.labels, []string{"ordinary", "agent-failed"}) {
		t.Fatalf("label replay = %v #%d %v, want acme/widget #42 [ordinary agent-failed]", issues.repository, issues.number, issues.labels)
	}

	replay(store.PendingEffectKindCommitStatus, legacyCommitStatusPayload)
	wantStatus := github.CommitStatus{
		SHA: "0123456789abcdef0123456789abcdef01234567", State: github.CommitStatusFailure,
		Context: "factory/test", Description: "tests failed", TargetURL: "https://example.test/run",
	}
	if statuses.repository != legacyRepository || statuses.status != wantStatus {
		t.Fatalf("commit status replay = %v %#v, want acme/widget %#v", statuses.repository, statuses.status, wantStatus)
	}

	replay(store.PendingEffectKindPullRequest, legacyPullRequestPayload)
	wantRequest := github.PullRequestRequest{Title: "Fix the widget", Body: "generated body", HeadBranch: "factory/run-effects", BaseBranch: "main", Draft: true}
	if pullRequests.repository != legacyRepository || pullRequests.number != 9 || pullRequests.request != wantRequest {
		t.Fatalf("pull-request replay = %v #%d %#v, want acme/widget #9 %#v", pullRequests.repository, pullRequests.number, pullRequests.request, wantRequest)
	}
}
