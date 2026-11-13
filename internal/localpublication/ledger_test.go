package localpublication_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/publication"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// lostCommitRunner models Git applying a commit and losing the response once.
type lostCommitRunner struct {
	lost  bool
	after string
}

// Run executes real Git and injects loss only after the commit took effect.
func (r *lostCommitRunner) Run(ctx context.Context, dir string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", gitadapter.HostCommandArgs(args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	after := r.after
	if after == "" {
		after = "commit"
	}
	if err == nil && args[0] == after && !r.lost {
		r.lost = true
		return nil, errors.New("injected lost commit response")
	}
	return out, err
}

// receiptFailure leaves succeeded effects but interrupts the receipt write.
type receiptFailure struct {
	*store.Store
	failed bool
}

// RecordPublicationReceipt injects interruption between ledger and receipt.
func (s *receiptFailure) RecordPublicationReceipt(ctx context.Context, write store.IdeationWrite, receipt store.PublicationReceipt, operations ...store.PublicationOperation) (store.IdeationSession, store.PublicationReceipt, error) {
	if !s.failed {
		s.failed = true
		return store.IdeationSession{}, store.PublicationReceipt{}, errors.New("injected receipt write failure")
	}
	return s.Store.RecordPublicationReceipt(ctx, write, receipt, operations...)
}

// TestMissingRefAfterLedgerSuccessCannotFabricatePublishedReceipt catches an
// interrupted receipt followed by an externally removed specification ref.
func TestMissingRefAfterLedgerSuccessCannotFabricatePublishedReceipt(t *testing.T) {
	f := newFixture(t)
	p := f.readyPublisher()
	p.Store = &receiptFailure{Store: f.store}
	first, err := p.Publish(t.Context(), publication.Request{SessionID: f.graph.SessionID, AttemptID: "attempt-1"})
	if err == nil {
		t.Fatal("receipt injection did not fail")
	}
	if len(first.Operations) == 0 || first.Operations[0].State != store.PublicationOperationSucceeded {
		t.Fatalf("expected succeeded ledger: %+v", first)
	}
	var detail publication.BatchOperationDetail
	if err := json.Unmarshal([]byte(first.Operations[0].Detail), &detail); err != nil {
		t.Fatal(err)
	}
	f.git(f.repo, "update-ref", "-d", "refs/heads/"+detail.Evidence.Branch)
	second, err := p.Publish(t.Context(), publication.Request{SessionID: f.graph.SessionID, AttemptID: "attempt-2"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Receipt.Outcome != store.PublicationOutcomeIncomplete {
		t.Fatalf("missing ref became published: %+v", second.Receipt)
	}
}

// readyPublisher creates a reviewed plan through ordinary session/store seams.
func (f *fixture) readyPublisher() *publication.Publisher {
	f.t.Helper()
	ctx := f.t.Context()
	life := workflow.DefaultSessionLifecycle()
	now := time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)
	session, err := f.store.CreateIdeationSession(ctx, store.IdeationSession{ID: f.graph.SessionID, RepositoryPath: f.repo, State: life.InitialState(), CreatedAt: now})
	if err != nil {
		f.t.Fatal(err)
	}
	write := func(event workflow.SessionEvent) store.IdeationWrite {
		w, err := life.Write(session, event, "", now)
		if err != nil {
			f.t.Fatal(err)
		}
		return w
	}
	session, _, err = f.store.AppendIdeationTurn(ctx, write(workflow.SessionEventTurnAccepted), store.IdeationTurn{ID: "private-turn", Author: store.IdeationTurnAuthorUser, Body: "PRIVATE CONVERSATION"})
	if err != nil {
		f.t.Fatal(err)
	}
	session, brief, err := f.store.SaveBriefRevision(ctx, write(workflow.SessionEventEdited), store.Brief{Problem: "Operators need an export."})
	if err != nil {
		f.t.Fatal(err)
	}
	session, err = f.store.TransitionIdeationSession(ctx, write(workflow.SessionEventRoleStarted))
	if err != nil {
		f.t.Fatal(err)
	}
	var nodes []store.PlanNode
	for _, item := range f.graph.Items {
		nodes = append(nodes, store.PlanNode{ID: item.NodeID, Kind: item.Kind, Title: item.Title, Body: item.Body, AcceptanceCriteria: item.AcceptanceCriteria, ParentID: item.ParentID, DependsOn: item.DependsOn})
	}
	session, _, err = f.store.SavePlanRevision(ctx, write(workflow.SessionEventPlanDrafted), store.PlanRevision{SourceBriefRevision: brief.Revision, ContextRevision: f.graph.ContextRevision, Nodes: nodes})
	if err != nil {
		f.t.Fatal(err)
	}
	p := &publication.Publisher{Store: f.store, Tracker: f.publisher, Lifecycle: life, Now: func() time.Time { return now }}
	if err := p.Prepare(ctx, session.ID); err != nil {
		f.t.Fatal(err)
	}
	binding, err := p.Preview(ctx, session.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	if strings.Contains(string(binding.Payload), "PRIVATE CONVERSATION") {
		f.t.Fatal("conversation entered publication")
	}
	if _, _, err = f.store.ApprovePlan(ctx, write(workflow.SessionEventApproved), binding.Approval("approval-1")); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// TestPublicationLedgerAdoptsLostCommitAsOneAtomicBatch verifies durable batch
// recovery exposes no published item until every approved effect is complete.
func TestPublicationLedgerAdoptsLostCommitAsOneAtomicBatch(t *testing.T) {
	f := newFixture(t)
	p := f.readyPublisher()
	f.publisher.Runner = &lostCommitRunner{}
	first, err := p.Publish(t.Context(), publication.Request{SessionID: f.graph.SessionID, AttemptID: "attempt-1"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Receipt.Outcome != store.PublicationOutcomeIncomplete || len(first.Receipt.Items) != 0 {
		t.Fatalf("partial publication released items: %+v", first)
	}
	second, err := p.Publish(t.Context(), publication.Request{SessionID: f.graph.SessionID, AttemptID: "attempt-2"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Receipt.Outcome != store.PublicationOutcomePublished || len(second.Receipt.Items) != 1 {
		t.Fatalf("recovered publication=%+v", second)
	}
	if second.Operations[0].Kind != store.PublicationOperationGitBatch || second.Operations[0].ExternalID == "" {
		t.Fatalf("missing durable Git evidence: %+v", second.Operations)
	}
	if got := f.git(f.repo, "rev-list", "--count", f.graph.ContextRevision+".."+second.Operations[0].ExternalID); got != "1" {
		t.Fatalf("commit count=%s", got)
	}
}
