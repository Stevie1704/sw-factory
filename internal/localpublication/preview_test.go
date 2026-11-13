package localpublication_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/localpublication"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// fixture uses real local Git and SQLite; no remote tracker can be contacted.
type fixture struct {
	t         *testing.T
	repo      string
	remote    string
	store     *store.Store
	publisher *localpublication.Publisher
	graph     tracker.PlanGraph
}

// TestCompositionAndPreflightDoNotCreateOperationalState guards the UI's
// read-only boundary when an adapter is constructed with an absent store.
func TestCompositionAndPreflightDoNotCreateOperationalState(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(t.TempDir(), "missing", "factory.db")
	p := localpublication.NewPublisher(config.RepositoryRegistration{Path: f.repo, OperationalDataPath: path}, "main", nil)
	if _, err := p.PreflightPlan(t.Context(), tracker.Repository{}, f.graph); err == nil {
		t.Fatal("preview accepted unallocated identities")
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("read-only composition/preflight created operational state: %v", err)
	}
}

// git executes a fixture command, independent of the production adapter.
func (f *fixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.CommandContext(f.t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(bytes.TrimSpace(out))
}

// newFixture creates a target repository and a single accepted-plan candidate.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, repo: filepath.Join(t.TempDir(), "repo"), remote: filepath.Join(t.TempDir(), "origin.git")}
	if err := os.MkdirAll(f.repo, 0700); err != nil {
		t.Fatal(err)
	}
	f.git(f.repo, "init", "-b", "main")
	f.git(f.repo, "config", "user.name", "Factory test")
	f.git(f.repo, "config", "user.email", "factory@example.invalid")
	if err := os.WriteFile(filepath.Join(f.repo, "README.md"), []byte("Original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(f.repo, "add", "README.md")
	f.git(f.repo, "commit", "-m", "Initial")
	f.git(f.repo, "init", "--bare", f.remote)
	f.git(f.repo, "remote", "add", "origin", f.remote)
	f.git(f.repo, "push", "origin", "main")
	opened, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	f.store = opened
	t.Cleanup(func() { _ = opened.Close() })
	f.publisher = &localpublication.Publisher{RepositoryPath: f.repo, Store: opened, TargetBranch: "main"}
	f.graph = tracker.PlanGraph{SessionID: "session-1", PlanRevision: 1, ContextRevision: f.git(f.repo, "rev-parse", "HEAD"), Items: []tracker.PlanGraphItem{{NodeID: "n-1", Kind: "task", Title: "Export history", Body: "Operators need an export.", AcceptanceCriteria: []string{"An export contains the selected records."}, Correlation: tracker.PlanCorrelation("session-1", 1, "n-1")}}}
	return f
}

// TestPreviewRequiresExplicitPreparation proves read-only preflight cannot allocate.
func TestPreviewRequiresExplicitPreparation(t *testing.T) {
	f := newFixture(t)
	_, err := f.publisher.PreflightPlan(context.Background(), tracker.Repository{Owner: "owner", Name: "repo"}, f.graph)
	if err == nil {
		t.Fatal("unallocated preview succeeded")
	}
	if _, err := f.store.LocalIssueIdentityByCorrelation(t.Context(), f.repo, f.graph.Items[0].Correlation); err == nil {
		t.Fatal("read-only preview allocated an identity")
	}
	if err := f.publisher.PreparePlan(t.Context(), tracker.Repository{}, f.graph); err != nil {
		t.Fatal(err)
	}
	a, err := f.publisher.PreflightPlan(t.Context(), tracker.Repository{}, f.graph)
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.publisher.PreflightPlan(t.Context(), tracker.Repository{}, f.graph)
	if err != nil {
		t.Fatal(err)
	}
	if a.Items[0].Body != b.Items[0].Body || a.Items[0].Fields["local_id"] != b.Items[0].Fields["local_id"] {
		t.Fatal("preview changed without input changes")
	}
}
