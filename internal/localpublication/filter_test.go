package localpublication_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/tracker"
)

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
