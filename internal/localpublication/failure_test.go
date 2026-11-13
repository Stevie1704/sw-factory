package localpublication_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/localpublication"
	"github.com/Stevie1704/sw-factory/internal/publication"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// rejectingPushRunner runs real Git, but the remote rejects the first push.
// The rejection is a definite refusal, not a lost response: nothing is pushed.
type rejectingPushRunner struct {
	rejections, pushes int
}

// Run counts pushes and refuses the first ones without contacting the remote.
func (r *rejectingPushRunner) Run(ctx context.Context, dir string, args []string) ([]byte, error) {
	if args[0] == "push" {
		r.pushes++
		if r.rejections > 0 {
			r.rejections--
			return []byte("! [remote rejected] (pre-receive hook declined)"), errors.New("injected rejected push")
		}
	}
	return gitadapter.NewCommandRunner().Run(ctx, dir, args)
}

// TestRejectedPushReleasesNothingAndRetryPushesOnce halts at a rejected push
// with no item released. A retry before reconciliation repeats no push; after
// the operator confirms the push is absent, one retry pushes the commit once.
func TestRejectedPushReleasesNothingAndRetryPushesOnce(t *testing.T) {
	f := newFixture(t)
	f.publisher.Policy = localpublication.Push
	p := f.readyPublisher()
	runner := &rejectingPushRunner{rejections: 1}
	f.publisher.Runner = runner
	first, err := p.Publish(t.Context(), publication.Request{SessionID: f.graph.SessionID, AttemptID: "attempt-1"})
	if err == nil && (first.Receipt.Outcome != store.PublicationOutcomeIncomplete || len(first.Receipt.Items) != 0) {
		t.Fatalf("rejected push released work: %+v", first.Receipt)
	}
	branch := strings.TrimSpace(f.git(f.repo, "for-each-ref", "--format=%(refname:short)", "refs/heads/factory/"))
	if branch == "" {
		t.Fatal("specification commit missing after the rejected push")
	}
	if remote := f.git(f.repo, "ls-remote", "origin", "refs/heads/"+branch); remote != "" {
		t.Fatalf("rejected push reached the remote: %s", remote)
	}
	retryAfterConfirmedAbsence(t, f, p)
	if runner.pushes != 2 {
		t.Fatalf("pushes=%d, want the rejected one and one retry", runner.pushes)
	}
	head := f.git(f.repo, "rev-parse", "refs/heads/"+branch)
	if remote := f.git(f.repo, "ls-remote", "origin", "refs/heads/"+branch); !strings.HasPrefix(remote, head) {
		t.Fatalf("remote=%q, want %s", remote, head)
	}
	if got := f.git(f.repo, "rev-list", "--count", f.graph.ContextRevision+".."+head); got != "1" {
		t.Fatalf("commits=%s", got)
	}
}

// readOnlyDestinationRunner plants one unwritable specification file right
// after Git creates the specification worktree, so exactly one write fails.
type readOnlyDestinationRunner struct {
	path    string
	planted string
}

// Run creates the worktree with real Git and then plants the read-only file.
func (r *readOnlyDestinationRunner) Run(ctx context.Context, dir string, args []string) ([]byte, error) {
	out, err := gitadapter.NewCommandRunner().Run(ctx, dir, args)
	if err == nil && len(args) > 1 && args[0] == "worktree" && args[1] == "add" && r.planted == "" {
		worktree := args[len(args)-2]
		r.planted = filepath.Join(worktree, filepath.FromSlash(r.path))
		if err := os.MkdirAll(filepath.Dir(r.planted), 0o755); err != nil {
			return out, err
		}
		if err := os.WriteFile(r.planted, []byte("stale partial write\n"), 0o444); err != nil {
			return out, err
		}
	}
	return out, err
}

// TestFailedSpecificationWriteRetriesIntoOneExactCommit fails one file write.
// After the operator confirms no commit exists, a retry commits once with
// exactly the approved files and bytes.
func TestFailedSpecificationWriteRetriesIntoOneExactCommit(t *testing.T) {
	f := newFixture(t)
	f.graph.Items = append(f.graph.Items, tracker.PlanGraphItem{NodeID: "n-2", Kind: "task", Title: "Document export", Body: "Users need instructions.", AcceptanceCriteria: []string{"Help describes export."}, DependsOn: []string{"n-1"}, Correlation: tracker.PlanCorrelation("session-1", 1, "n-2")})
	p := f.readyPublisher()
	preview, err := f.publisher.PreflightPlan(t.Context(), tracker.Repository{}, f.graph)
	if err != nil {
		t.Fatal(err)
	}
	runner := &readOnlyDestinationRunner{path: preview.Items[1].Fields["path"]}
	f.publisher.Runner = runner
	first, err := p.Publish(t.Context(), publication.Request{SessionID: f.graph.SessionID, AttemptID: "attempt-1"})
	if runner.planted == "" {
		t.Fatal("failure was not injected")
	}
	if err == nil && (first.Receipt.Outcome != store.PublicationOutcomeIncomplete || len(first.Receipt.Items) != 0) {
		t.Fatalf("failed write released work: %+v", first.Receipt)
	}
	if refs := f.git(f.repo, "for-each-ref", "--format=%(objectname)", "refs/heads/factory/"); refs != "" && refs != f.graph.ContextRevision {
		t.Fatalf("failed write committed: %s", refs)
	}
	if err := os.Chmod(runner.planted, 0o644); err != nil {
		t.Fatal(err)
	}
	retryAfterConfirmedAbsence(t, f, p)
	head := f.git(f.repo, "rev-parse", "refs/heads/"+preview.Items[0].Fields["specification_branch"])
	if got := f.git(f.repo, "rev-list", "--count", f.graph.ContextRevision+".."+head); got != "1" {
		t.Fatalf("commits=%s", got)
	}
	changed := strings.Split(f.git(f.repo, "diff-tree", "--no-commit-id", "--name-only", "-r", head), "\n")
	if len(changed) != len(preview.Items) {
		t.Fatalf("committed paths=%v", changed)
	}
	for _, item := range preview.Items {
		if data := f.git(f.repo, "show", head+":"+item.Fields["path"]) + "\n"; data != item.Body {
			t.Fatalf("committed content differs for %s", item.NodeID)
		}
	}
}

// retryAfterConfirmedAbsence retries once and expects a halt with nothing
// released, confirms the attempted batch effect is absent, and retries again
// to a published receipt.
func retryAfterConfirmedAbsence(t *testing.T, f *fixture, p *publication.Publisher) {
	t.Helper()
	halted, err := p.Publish(t.Context(), publication.Request{SessionID: f.graph.SessionID, AttemptID: "retry-before-reconcile"})
	if err != nil || halted.Receipt.Outcome != store.PublicationOutcomeIncomplete || len(halted.Receipt.Items) != 0 {
		t.Fatalf("unreconciled retry=%+v,%v", halted.Receipt, err)
	}
	history, err := f.store.IdeationHistory(t.Context(), f.graph.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	batchOperation := ""
	for _, operation := range history.Operations {
		if operation.Kind == store.PublicationOperationGitBatch {
			batchOperation = operation.ID
		}
	}
	if _, err := p.Reconcile(t.Context(), publication.Reconciliation{SessionID: f.graph.SessionID, OperationID: batchOperation}); err != nil {
		t.Fatalf("confirm absent: %v", err)
	}
	result, err := p.Publish(t.Context(), publication.Request{SessionID: f.graph.SessionID, AttemptID: "retry-after-reconcile"})
	if err != nil || result.Receipt.Outcome != store.PublicationOutcomePublished {
		t.Fatalf("reconciled retry=%+v,%v", result.Receipt, err)
	}
}
