package checkpoint_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/checkpoint"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// knownPublications are the objects of one completed publication: a
// tracker issue, and the branch and the pull request of a Git batch.
var knownPublications = []store.KnownPublication{
	{SessionID: "session-1", ApprovalID: "approval-1", OperationID: "op-1", Branch: "factory/spec-1", PullRequestNumber: 12},
	{SessionID: "session-1", ApprovalID: "approval-1", OperationID: "op-2", IssueNumber: 41},
}

// publishedExternal returns a reader that knows the target branch and the
// objects of knownPublications.
func publishedExternal() fakeExternal {
	return fakeExternal{
		branches:     map[string]string{"main": "aaa", "factory/spec-1": "ccc"},
		pullRequests: map[int]checkpoint.PullRequestState{12: {Number: 12, HeadBranch: "factory/spec-1", HeadSHA: "ccc", State: "open"}},
		issues:       map[int]checkpoint.IssueState{41: {Number: 41, Open: true, FactoryLabels: []string{}}},
		comments:     map[string]checkpoint.StatusCommentState{},
	}
}

// TestReadExternalStateRecordsThePublicationObjects verifies that the
// external state of a store without runs names every object that the
// publication ledger records.
func TestReadExternalStateRecordsThePublicationObjects(t *testing.T) {
	t.Parallel()
	reader := publishedExternal()

	state, unread := checkpoint.ReadExternalState(context.Background(), reader, "main", nil, knownPublications)
	if len(unread) != 0 {
		t.Fatalf("unread = %v, want none", unread)
	}
	want := checkpoint.ExternalState{
		TargetBranch:   checkpoint.BranchState{Name: "main", Head: "aaa"},
		Branches:       []checkpoint.BranchState{{Name: "factory/spec-1", Head: "ccc"}},
		PullRequests:   []checkpoint.PullRequestState{reader.pullRequests[12]},
		Issues:         []checkpoint.IssueState{reader.issues[41]},
		StatusComments: []checkpoint.StatusCommentState{},
	}
	if !reflect.DeepEqual(state, want) {
		t.Fatalf("state = %+v, want %+v", state, want)
	}
}

// TestReadExternalStateRefusesAnUnreadablePublicationObject verifies that a
// publication object that cannot be read is named, so the checkpoint is
// refused.
func TestReadExternalStateRefusesAnUnreadablePublicationObject(t *testing.T) {
	t.Parallel()
	for object, damage := range map[string]func(*fakeExternal){
		"branch factory/spec-1": func(f *fakeExternal) { delete(f.branches, "factory/spec-1") },
		"pull request #12":      func(f *fakeExternal) { delete(f.pullRequests, 12) },
		"issue #41":             func(f *fakeExternal) { delete(f.issues, 41) },
	} {
		reader := publishedExternal()
		damage(&reader)
		_, unread := checkpoint.ReadExternalState(context.Background(), reader, "main", nil, knownPublications)
		if len(unread) != 1 || !strings.HasPrefix(unread[0], object) {
			t.Fatalf("%s: unread = %v, want exactly that object", object, unread)
		}
	}
}

// TestCompareExternalStateComparesThePublicationObjects verifies that a
// restore compares the publication objects that the checkpoint recorded: a
// changed publication pull request diverges, and a publication branch and
// pull request are known factory-owned objects.
func TestCompareExternalStateComparesThePublicationObjects(t *testing.T) {
	t.Parallel()
	state, unread := checkpoint.ReadExternalState(context.Background(), publishedExternal(), "main", nil, knownPublications)
	if len(unread) != 0 {
		t.Fatal(unread)
	}
	owned := fakeOwned{
		branches:     []string{"factory/spec-1"},
		pullRequests: []checkpoint.PullRequestState{state.PullRequests[0]},
		issues:       []checkpoint.IssueState{},
	}
	if divergence := checkpoint.CompareExternalState(context.Background(), publishedExternal(), owned, state); len(divergence.Diverged) != 0 || len(divergence.Unverified) != 0 {
		t.Fatalf("divergence = %+v, want none for the recorded publication", divergence)
	}
	merged := publishedExternal()
	merged.pullRequests[12] = checkpoint.PullRequestState{Number: 12, HeadBranch: "factory/spec-1", HeadSHA: "ccc", State: "merged"}

	divergence := checkpoint.CompareExternalState(context.Background(), merged, owned, state)
	if len(divergence.Diverged) != 1 || !strings.HasPrefix(divergence.Diverged[0], "pull request #12") {
		t.Fatalf("diverged = %v, want pull request #12", divergence.Diverged)
	}
}
