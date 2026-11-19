package checkpoint_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/checkpoint"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// fakeOwned lists the factory-owned objects of the fake tracker and code
// host. A listing with an error fails, as an unreachable source does.
type fakeOwned struct {
	branches     []string
	pullRequests []checkpoint.PullRequestState
	issues       []checkpoint.IssueState
	unreachable  string
}

// FactoryBranches lists the remote factory/* branches.
func (f fakeOwned) FactoryBranches(context.Context) ([]string, error) {
	if f.unreachable == "branches" {
		return nil, errors.New("remote unreachable")
	}
	return f.branches, nil
}

// FactoryPullRequests lists the pull requests from factory/* branches.
func (f fakeOwned) FactoryPullRequests(context.Context) ([]checkpoint.PullRequestState, error) {
	if f.unreachable == "pull_requests" {
		return nil, errors.New("code host unreachable")
	}
	return f.pullRequests, nil
}

// FactoryLabeledIssues lists the issues with a factory run-state label.
func (f fakeOwned) FactoryLabeledIssues(context.Context) ([]checkpoint.IssueState, error) {
	if f.unreachable == "issues" {
		return nil, errors.New("tracker unreachable")
	}
	return f.issues, nil
}

// recordedState returns the external state that a checkpoint of knownRun
// records, and an owned listing that agrees with it.
func recordedState(t *testing.T) (checkpoint.ExternalState, fakeOwned) {
	t.Helper()
	state, unread := checkpoint.ReadExternalState(context.Background(), knownExternal(), "main", []store.KnownRun{knownRun}, nil)
	if len(unread) != 0 {
		t.Fatal(unread)
	}
	return state, fakeOwned{
		branches:     []string{"factory/run-1"},
		pullRequests: []checkpoint.PullRequestState{state.PullRequests[0]},
		issues:       []checkpoint.IssueState{state.Issues[0]},
	}
}

// TestCompareExternalStateAcceptsTheRecordedState verifies that an
// unchanged tracker, code host, and remote show no divergence, also after
// the target branch moved.
func TestCompareExternalStateAcceptsTheRecordedState(t *testing.T) {
	t.Parallel()
	state, owned := recordedState(t)
	external := knownExternal()
	external.branches["main"] = "moved-by-a-human-merge"
	divergence := checkpoint.CompareExternalState(context.Background(), external, owned, state)
	if len(divergence.Diverged) != 0 || len(divergence.Unverified) != 0 {
		t.Fatalf("divergence = %+v, want none", divergence)
	}
}

// TestCompareExternalStateNamesEachDivergedObject verifies that each
// changed known object and each unknown factory-owned object is a
// divergence that names that object.
func TestCompareExternalStateNamesEachDivergedObject(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]struct {
		object string
		apply  func(*fakeExternal, *fakeOwned)
	}{
		"moved known branch": {"branch factory/run-1", func(e *fakeExternal, _ *fakeOwned) { e.branches["factory/run-1"] = "ccc" }},
		"changed pull request head": {"pull request #7", func(e *fakeExternal, _ *fakeOwned) {
			pullRequest := e.pullRequests[7]
			pullRequest.HeadSHA = "ccc"
			e.pullRequests[7] = pullRequest
		}},
		"changed pull request state": {"pull request #7", func(e *fakeExternal, _ *fakeOwned) {
			pullRequest := e.pullRequests[7]
			pullRequest.State = "closed"
			e.pullRequests[7] = pullRequest
		}},
		"changed issue label": {"issue #107", func(e *fakeExternal, _ *fakeOwned) {
			e.issues[107] = checkpoint.IssueState{Number: 107, FactoryLabels: []string{"agent-running"}}
		}},
		"changed status comment": {"status comment comment-9", func(e *fakeExternal, _ *fakeOwned) {
			comment := e.comments["run-1"]
			comment.UpdatedAt = time.Unix(900, 0).UTC()
			e.comments["run-1"] = comment
		}},
		"unknown factory branch": {"branch factory/run-2", func(_ *fakeExternal, o *fakeOwned) { o.branches = append(o.branches, "factory/run-2") }},
		"unknown factory pull request": {"pull request #8", func(_ *fakeExternal, o *fakeOwned) {
			o.pullRequests = append(o.pullRequests, checkpoint.PullRequestState{Number: 8, HeadBranch: "factory/run-2", State: "open"})
		}},
		"unknown labeled issue": {"issue #108", func(_ *fakeExternal, o *fakeOwned) {
			o.issues = append(o.issues, checkpoint.IssueState{Number: 108, Open: true, FactoryLabels: []string{"agent-running"}})
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			state, owned := recordedState(t)
			external := knownExternal()
			change.apply(&external, &owned)
			divergence := checkpoint.CompareExternalState(context.Background(), external, owned, state)
			if len(divergence.Diverged) != 1 || !strings.HasPrefix(divergence.Diverged[0], change.object+":") || len(divergence.Unverified) != 0 {
				t.Fatalf("divergence = %+v, want only %s", divergence, change.object)
			}
		})
	}
}

// TestCompareExternalStateNamesEachUnreadableSourceAndObject verifies that
// an unreadable known object or listing is unverified, never a pass.
func TestCompareExternalStateNamesEachUnreadableSourceAndObject(t *testing.T) {
	t.Parallel()
	for name, apply := range map[string]func(*fakeExternal, *fakeOwned){
		"branch":            func(e *fakeExternal, _ *fakeOwned) { delete(e.branches, "factory/run-1") },
		"pull request":      func(e *fakeExternal, _ *fakeOwned) { delete(e.pullRequests, 7) },
		"issue":             func(e *fakeExternal, _ *fakeOwned) { delete(e.issues, 107) },
		"status comment":    func(e *fakeExternal, _ *fakeOwned) { delete(e.comments, "run-1") },
		"branch listing":    func(_ *fakeExternal, o *fakeOwned) { o.unreachable = "branches" },
		"pull request list": func(_ *fakeExternal, o *fakeOwned) { o.unreachable = "pull_requests" },
		"issue listing":     func(_ *fakeExternal, o *fakeOwned) { o.unreachable = "issues" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			state, owned := recordedState(t)
			external := knownExternal()
			apply(&external, &owned)
			divergence := checkpoint.CompareExternalState(context.Background(), external, owned, state)
			if len(divergence.Unverified) != 1 || len(divergence.Diverged) != 0 {
				t.Fatalf("divergence = %+v, want one unverified object", divergence)
			}
		})
	}
}

// TestCompareExternalStateWithoutAListerIsUnverified verifies that a
// tracker or code host that cannot list factory-owned objects never passes.
func TestCompareExternalStateWithoutAListerIsUnverified(t *testing.T) {
	t.Parallel()
	state, _ := recordedState(t)
	divergence := checkpoint.CompareExternalState(context.Background(), knownExternal(), nil, state)
	if len(divergence.Unverified) == 0 {
		t.Fatalf("divergence = %+v, want the missing listing unverified", divergence)
	}
}
