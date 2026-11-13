package localpublication_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// TestTargetVisibilityAcceptsIdenticalSquashContent distinguishes a commit's
// identity from exact specification identity, rejecting changed source bytes.
func TestTargetVisibilityAcceptsIdenticalSquashContent(t *testing.T) {
	f := newFixture(t)
	if err := f.publisher.PreparePlan(t.Context(), tracker.Repository{}, f.graph); err != nil {
		t.Fatal(err)
	}
	preview, err := f.publisher.PreflightPlan(t.Context(), tracker.Repository{}, f.graph)
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.publisher.PublishPlanBatch(t.Context(), tracker.Repository{}, preview)
	if err != nil {
		t.Fatal(err)
	}
	squash := filepath.Join(t.TempDir(), "squash")
	f.git(f.repo, "worktree", "add", "--detach", squash, f.graph.ContextRevision)
	f.git(squash, "read-tree", "--reset", "-u", result.CommitSHA)
	f.git(squash, "commit", "-m", "Human squash of accepted specifications")
	sha := f.git(squash, "rev-parse", "HEAD")
	if sha == result.CommitSHA {
		t.Fatal("fixture did not create a different commit")
	}
	f.git(squash, "push", "origin", "HEAD:refs/heads/main")
	visible, err := f.publisher.ObservePlanTarget(t.Context(), tracker.Repository{}, preview)
	if err != nil || !visible.Visible || visible.Revision != sha {
		t.Fatalf("squash visibility=%+v,%v", visible, err)
	}
	path := preview.Items[0].Fields["path"]
	if err := os.WriteFile(filepath.Join(squash, filepath.FromSlash(path)), []byte(preview.Items[0].Body+"Changed after approval.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	f.git(squash, "add", path)
	f.git(squash, "commit", "-m", "Changed specification")
	f.git(squash, "push", "origin", "HEAD:refs/heads/main")
	visible, err = f.publisher.ObservePlanTarget(t.Context(), tracker.Repository{}, preview)
	if err != nil || visible.Visible {
		t.Fatalf("changed content accepted=%+v,%v", visible, err)
	}
}

// TestPublicationRefusesGitFilterChangesBeforeCommit retains host-supported Git
// filters while refusing indexed content that differs from explicit approval.
func TestPublicationRefusesGitFilterChangesBeforeCommit(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.repo, ".gitattributes"), []byte("docs/issues/*.md filter=spec\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(f.repo, "add", ".gitattributes")
	f.git(f.repo, "commit", "-m", "Repository attributes")
	f.graph.ContextRevision = f.git(f.repo, "rev-parse", "HEAD")
	f.git(f.repo, "config", "filter.spec.clean", "sed s/Operators/Rewritten/g")
	if err := f.publisher.PreparePlan(t.Context(), tracker.Repository{}, f.graph); err != nil {
		t.Fatal(err)
	}
	preview, err := f.publisher.PreflightPlan(t.Context(), tracker.Repository{}, f.graph)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.publisher.PublishPlanBatch(t.Context(), tracker.Repository{}, preview); err == nil {
		t.Fatal("Git filter silently changed approved source")
	}
	if head := f.git(f.repo, "rev-parse", preview.Items[0].Fields["specification_branch"]); head != f.graph.ContextRevision {
		t.Fatal("transformed source was committed")
	}
}
