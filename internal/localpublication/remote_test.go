package localpublication_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/codehost"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/localpublication"
	"github.com/Stevie1704/sw-factory/internal/publication"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// specHost is an external code-host boundary with explicit response loss.
type specHost struct {
	pr           codehost.PullRequest
	creates      int
	loseResponse bool
	hide         bool
	multiple     bool
}

// cancelAfterCommit models an operator cancelling while Git is in flight.
type cancelAfterCommit struct {
	cancel func()
	called bool
}

// Run applies a real commit, then delivers the concurrent protected decision.
func (r *cancelAfterCommit) Run(ctx context.Context, dir string, args []string) ([]byte, error) {
	out, err := gitadapter.NewCommandRunner().Run(ctx, dir, args)
	if err == nil && args[0] == "commit" && !r.called {
		r.called = true
		r.cancel()
	}
	return out, err
}

// TestCancellationDuringCommitStopsLaterPushAndPR preserves completed content
// while ensuring cancellation stops before the next approved external effect.
func TestCancellationDuringCommitStopsLaterPushAndPR(t *testing.T) {
	f := newFixture(t)
	host := &specHost{}
	f.publisher.Policy = localpublication.PullRequest
	f.publisher.PullRequests = host
	p := f.readyPublisher()
	f.publisher.Runner = &cancelAfterCommit{cancel: func() {
		session, err := f.store.IdeationSession(t.Context(), f.graph.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		write, err := workflow.DefaultSessionLifecycle().Write(session, workflow.SessionEventCancelled, "operator cancelled", p.Now())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.TransitionIdeationSession(t.Context(), write); err != nil {
			t.Fatal(err)
		}
	}}
	result, err := p.Publish(t.Context(), publication.Request{SessionID: f.graph.SessionID, AttemptID: "attempt-1"})
	if !errors.Is(err, publication.ErrPublicationCancelled) {
		t.Fatalf("cancellation=%+v,%v", result, err)
	}
	if host.creates != 0 {
		t.Fatalf("cancelled batch created %d later PRs", host.creates)
	}
	if len(result.Receipt.Items) != 0 || result.Receipt.Outcome != store.PublicationOutcomeIncomplete {
		t.Fatalf("cancelled batch released work: %+v", result.Receipt)
	}
}

// FindPullRequestCandidates exposes all matching PRs, including terminal ones.
func (h *specHost) FindPullRequestCandidates(context.Context, tracker.Repository, string, string) ([]codehost.PullRequest, error) {
	if h.hide || h.pr.Number == 0 {
		return nil, nil
	}
	if h.multiple {
		return []codehost.PullRequest{h.pr, h.pr}, nil
	}
	return []codehost.PullRequest{h.pr}, nil
}

// FindPullRequest retains the ordinary code-host single-match API.
func (h *specHost) FindPullRequest(context.Context, tracker.Repository, string, string) (codehost.PullRequest, error) {
	if h.hide {
		return codehost.PullRequest{}, nil
	}
	return h.pr, nil
}

// CreatePullRequest models a successful remote write whose response is lost.
func (h *specHost) CreatePullRequest(_ context.Context, _ tracker.Repository, request codehost.PullRequestRequest) (codehost.PullRequest, error) {
	h.creates++
	h.pr = codehost.PullRequest{Number: 17, URL: "https://example.invalid/pr/17", Body: request.Body, HeadBranch: request.HeadBranch, BaseBranch: request.BaseBranch, Draft: request.Draft}
	if h.loseResponse {
		h.loseResponse = false
		return codehost.PullRequest{}, errors.New("injected lost PR response")
	}
	return h.pr, nil
}

// UpdatePullRequest is unused because publication never rewrites other PRs.
func (h *specHost) UpdatePullRequest(context.Context, tracker.Repository, int, codehost.PullRequestRequest) (codehost.PullRequest, error) {
	return codehost.PullRequest{}, errors.New("unexpected PR mutation")
}

// TestLostPushAndPRResponsesAreAdopted proves each optional approved effect is
// found after restart instead of generating a second commit, push branch or PR.
func TestLostPushAndPRResponsesAreAdopted(t *testing.T) {
	f := newFixture(t)
	host := &specHost{loseResponse: true}
	f.publisher.Policy = localpublication.PullRequest
	f.publisher.PullRequests = host
	p := f.readyPublisher()
	f.publisher.Runner = &lostCommitRunner{after: "push"}
	for _, id := range []string{"attempt-1", "attempt-2"} {
		result, err := p.Publish(t.Context(), publication.Request{SessionID: f.graph.SessionID, AttemptID: id})
		if err != nil {
			t.Fatal(err)
		}
		if result.Receipt.Outcome != store.PublicationOutcomeIncomplete || len(result.Receipt.Items) != 0 {
			t.Fatalf("partial remote publication released work: %+v", result)
		}
	}
	result, err := p.Publish(t.Context(), publication.Request{SessionID: f.graph.SessionID, AttemptID: "attempt-3"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Receipt.Outcome != store.PublicationOutcomePublished || host.creates != 1 {
		t.Fatalf("recovery=%+v,creates=%d", result.Receipt, host.creates)
	}
	if got := f.git(f.repo, "rev-list", "--count", f.graph.ContextRevision+".."+result.Operations[0].ExternalID); got != "1" {
		t.Fatalf("duplicate commits=%s", got)
	}
}

// TestUnobservablePRWriteRequiresReconciliation proves an empty eventual read
// cannot silently authorize a second remote create after an unknown outcome.
func TestUnobservablePRWriteRequiresReconciliation(t *testing.T) {
	f := newFixture(t)
	host := &specHost{loseResponse: true}
	f.publisher.Policy = localpublication.PullRequest
	f.publisher.PullRequests = host
	p := f.readyPublisher()
	if _, err := p.Publish(t.Context(), publication.Request{SessionID: f.graph.SessionID, AttemptID: "attempt-1"}); err != nil {
		t.Fatal(err)
	}
	host.hide = true
	result, err := p.Publish(t.Context(), publication.Request{SessionID: f.graph.SessionID, AttemptID: "attempt-2"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Receipt.Outcome != store.PublicationOutcomeIncomplete || host.creates != 1 {
		t.Fatalf("unknown outcome repeated create: %+v,creates=%d", result.Receipt, host.creates)
	}
	host.hide = false
	result, err = p.Publish(t.Context(), publication.Request{SessionID: f.graph.SessionID, AttemptID: "attempt-3"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Receipt.Outcome != store.PublicationOutcomePublished || host.creates != 1 {
		t.Fatalf("late visibility recovery=%+v,creates=%d", result, host.creates)
	}
}

// TestMultipleMatchingSpecificationPRsStopPublication refuses arbitrary adoption.
func TestMultipleMatchingSpecificationPRsStopPublication(t *testing.T) {
	f := newFixture(t)
	host := &specHost{loseResponse: true}
	f.publisher.Policy = localpublication.PullRequest
	f.publisher.PullRequests = host
	p := f.readyPublisher()
	if _, err := p.Publish(t.Context(), publication.Request{SessionID: f.graph.SessionID, AttemptID: "attempt-1"}); err != nil {
		t.Fatal(err)
	}
	host.multiple = true
	result, err := p.Publish(t.Context(), publication.Request{SessionID: f.graph.SessionID, AttemptID: "attempt-2"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Receipt.Outcome != store.PublicationOutcomeIncomplete || host.creates != 1 {
		t.Fatalf("ambiguous PR adopted: %+v", result)
	}
	host.multiple, host.hide = false, true
	result, err = p.Publish(t.Context(), publication.Request{SessionID: f.graph.SessionID, AttemptID: "attempt-3"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Receipt.Outcome != store.PublicationOutcomeIncomplete || host.creates != 1 {
		t.Fatalf("an empty read after ambiguity repeated PR creation: %+v,creates=%d", result, host.creates)
	}
}
