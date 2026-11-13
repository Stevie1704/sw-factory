package localpublication_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/localissues"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// TestBatchCommitPreservesCheckoutAndRecovers proves all accepted files share
// one dedicated commit, while ordinary staged/unstaged/untracked work survives.
func TestBatchCommitPreservesCheckoutAndRecovers(t *testing.T) {
	f := newFixture(t)
	f.graph.Items = append(f.graph.Items, tracker.PlanGraphItem{NodeID: "n-2", Kind: "task", Title: "Document export", Body: "Users need instructions.", AcceptanceCriteria: []string{"Help describes export."}, DependsOn: []string{"n-1"}, Correlation: tracker.PlanCorrelation("session-1", 1, "n-2")})
	if err := f.publisher.PreparePlan(t.Context(), tracker.Repository{}, f.graph); err != nil {
		t.Fatal(err)
	}
	preview, err := f.publisher.PreflightPlan(t.Context(), tracker.Repository{}, f.graph)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.repo, "README.md"), []byte("Staged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(f.repo, "add", "README.md")
	if err := os.WriteFile(filepath.Join(f.repo, "README.md"), []byte("Unstaged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.repo, "private.txt"), []byte("conversation secret"), 0600); err != nil {
		t.Fatal(err)
	}
	before := f.git(f.repo, "status", "--porcelain=v1")
	result, err := f.publisher.PublishPlanBatch(t.Context(), tracker.Repository{}, preview)
	if err != nil {
		t.Fatal(err)
	}
	if result.CommitSHA == "" || !result.Complete || result.Pushed {
		t.Fatalf("publication result=%+v", result)
	}
	if got := f.git(f.repo, "status", "--porcelain=v1"); got != before {
		t.Fatalf("ordinary checkout changed: %s => %s", before, got)
	}
	if got := f.git(f.repo, "rev-list", "--count", f.graph.ContextRevision+".."+result.CommitSHA); got != "1" {
		t.Fatalf("dedicated commits=%q", got)
	}
	paths := strings.Split(f.git(f.repo, "diff-tree", "--no-commit-id", "--name-only", "-r", result.CommitSHA), "\n")
	if len(paths) != 2 {
		t.Fatalf("committed paths=%v", paths)
	}
	for _, item := range preview.Items {
		data := f.git(f.repo, "show", result.CommitSHA+":"+item.Fields["path"]) + "\n"
		if data != item.Body {
			t.Fatalf("committed content differs for %s", item.NodeID)
		}
		if _, err := localissues.Parse(item.Fields["path"], []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	adopted, found, err := f.publisher.FindPlanBatch(t.Context(), tracker.Repository{}, preview)
	if err != nil || !found || adopted.CommitSHA != result.CommitSHA {
		t.Fatalf("adopt=%+v,%v,%v", adopted, found, err)
	}
	replay, err := f.publisher.PublishPlanBatch(t.Context(), tracker.Repository{}, preview)
	if err != nil || replay.CommitSHA != result.CommitSHA {
		t.Fatalf("replay=%+v,%v", replay, err)
	}
	visibility, err := f.publisher.ObservePlanTarget(t.Context(), tracker.Repository{}, preview)
	if err != nil || visibility.Visible || visibility.Reason != "waiting for specification merge" {
		t.Fatalf("target visibility=%+v,%v", visibility, err)
	}
}
