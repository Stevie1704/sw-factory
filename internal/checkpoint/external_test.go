package checkpoint_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/checkpoint"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// fakeExternal is an in-memory tracker, code host, and Git remote. A
// missing object is an error, as an unreachable source is.
type fakeExternal struct {
	branches     map[string]string
	pullRequests map[int]checkpoint.PullRequestState
	issues       map[int]checkpoint.IssueState
	comments     map[string]checkpoint.StatusCommentState
}

// BranchHead returns the recorded head of a branch.
func (f fakeExternal) BranchHead(_ context.Context, branch string) (string, error) {
	head, ok := f.branches[branch]
	if !ok {
		return "", errors.New("remote unreachable")
	}
	return head, nil
}

// PullRequest returns the recorded pull request.
func (f fakeExternal) PullRequest(_ context.Context, number int, _ string) (checkpoint.PullRequestState, error) {
	pullRequest, ok := f.pullRequests[number]
	if !ok {
		return checkpoint.PullRequestState{}, errors.New("code host unreachable")
	}
	return pullRequest, nil
}

// Issue returns the recorded issue.
func (f fakeExternal) Issue(_ context.Context, number int) (checkpoint.IssueState, error) {
	issue, ok := f.issues[number]
	if !ok {
		return checkpoint.IssueState{}, errors.New("tracker unreachable")
	}
	return issue, nil
}

// StatusComment returns the recorded status comment of a run.
func (f fakeExternal) StatusComment(_ context.Context, _ int, runID, _ string) (checkpoint.StatusCommentState, error) {
	comment, ok := f.comments[runID]
	if !ok {
		return checkpoint.StatusCommentState{}, errors.New("tracker unreachable")
	}
	return comment, nil
}

// knownExternal returns a reader that knows the target branch and every
// object of one run with a merged pull request.
func knownExternal() fakeExternal {
	return fakeExternal{
		branches:     map[string]string{"main": "aaa", "factory/run-1": "bbb"},
		pullRequests: map[int]checkpoint.PullRequestState{7: {Number: 7, HeadBranch: "factory/run-1", HeadSHA: "bbb", State: "merged"}},
		issues:       map[int]checkpoint.IssueState{107: {Number: 107, Open: false, FactoryLabels: []string{"agent-complete"}}},
		comments:     map[string]checkpoint.StatusCommentState{"run-1": {RunID: "run-1", IssueNumber: 107, ID: "comment-9", UpdatedAt: time.Unix(500, 0).UTC()}},
	}
}

// knownRun is a run that identifies a branch, a pull request, an issue, and
// a status comment.
var knownRun = store.KnownRun{RunID: "run-1", IssueNumber: 107, Branch: "factory/run-1", PullRequestNumber: 7, StatusCommentID: "comment-9"}

// TestReadExternalStateRecordsEveryKnownObject verifies that the external
// state names the target branch and each object of a known run.
func TestReadExternalStateRecordsEveryKnownObject(t *testing.T) {
	t.Parallel()
	reader := knownExternal()

	state, unread := checkpoint.ReadExternalState(context.Background(), reader, "main", []store.KnownRun{knownRun})
	if len(unread) != 0 {
		t.Fatalf("unread = %v, want none", unread)
	}
	want := checkpoint.ExternalState{
		TargetBranch:   checkpoint.BranchState{Name: "main", Head: "aaa"},
		Branches:       []checkpoint.BranchState{{Name: "factory/run-1", Head: "bbb"}},
		PullRequests:   []checkpoint.PullRequestState{reader.pullRequests[7]},
		Issues:         []checkpoint.IssueState{reader.issues[107]},
		StatusComments: []checkpoint.StatusCommentState{reader.comments["run-1"]},
	}
	if !reflect.DeepEqual(state, want) {
		t.Fatalf("state = %+v, want %+v", state, want)
	}
}

// TestReadExternalStateNamesEachUnreadableObject verifies that an object
// that cannot be read, or that the source does not show, is named, so the
// checkpoint is refused instead of recording unknown evidence.
func TestReadExternalStateNamesEachUnreadableObject(t *testing.T) {
	t.Parallel()
	for object, damage := range map[string]func(*fakeExternal){
		"target branch main":   func(f *fakeExternal) { f.branches["main"] = "" },
		"branch factory/run-1": func(f *fakeExternal) { delete(f.branches, "factory/run-1") },
		"pull request #7":      func(f *fakeExternal) { delete(f.pullRequests, 7) },
		"issue #107":           func(f *fakeExternal) { delete(f.issues, 107) },
		"status comment comment-9 of run run-1": func(f *fakeExternal) {
			f.comments["run-1"] = checkpoint.StatusCommentState{ID: "comment-other"}
		},
	} {
		reader := knownExternal()
		damage(&reader)
		_, unread := checkpoint.ReadExternalState(context.Background(), reader, "main", []store.KnownRun{knownRun})
		if len(unread) != 1 || !slices.ContainsFunc(unread, func(detail string) bool { return len(detail) > len(object) && detail[:len(object)] == object }) {
			t.Fatalf("%s: unread = %v, want exactly that object", object, unread)
		}
	}
}
