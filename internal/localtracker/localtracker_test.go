package localtracker_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/localissues"
	"github.com/Stevie1704/sw-factory/internal/localtracker"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// git runs fixture Git operations, never product operations.
func git(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.test", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.test")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %v", args, output, err)
	}
	return strings.TrimSpace(string(output))
}

// fixture makes a committed local issue and a separately registered local store.
func fixture(t *testing.T) (*localtracker.Client, config.RepositoryRegistration, string) {
	t.Helper()
	root := t.TempDir()
	remote, checkout := filepath.Join(root, "remote.git"), filepath.Join(root, "checkout")
	git(t, root, "init", "--bare", remote)
	git(t, root, "clone", remote, checkout)
	git(t, checkout, "checkout", "-b", "main")
	issue := localissues.Issue{SchemaVersion: 1, ID: "local-11111111111111111111111111111111", Title: "Committed task", Kind: localissues.KindTask, Correlation: "idea/1/task", Body: "## Problem\n\nMissing output.\n\n## Scope and non-goals\n\nAdd output only.\n\n## Acceptance criteria\n\n- Output is visible.\n"}
	data, err := localissues.Render(issue)
	if err != nil {
		t.Fatal(err)
	}
	name, _ := localissues.DefaultPath(issue.ID)
	if err := os.MkdirAll(filepath.Join(checkout, "docs/issues"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, checkout, "add", ".")
	git(t, checkout, "-c", "commit.gpgsign=false", "commit", "-m", "seed")
	git(t, checkout, "push", "origin", "main")
	registration := config.RepositoryRegistration{Path: checkout, Tracker: config.ProviderLocal, CodeHost: config.ProviderGitHub, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"}, OperationalDataPath: filepath.Join(root, "private", "operations.sqlite"), LocalOperator: config.LocalOperatorConfig{InstallationID: "install-11111111111111111111111111111111", Name: "operator"}}
	return localtracker.NewClient(registration, "main"), registration, name
}

// TestCommittedContentIgnoresWorkingTree proves lookup uses the fetched commit.
func TestCommittedContentIgnoresWorkingTree(t *testing.T) {
	client, registration, name := fixture(t)
	if err := os.WriteFile(filepath.Join(registration.Path, name), []byte("uncommitted forged content"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Issues) != 1 || snapshot.Issues[0].Issue.Title != "Committed task" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	issue, err := client.Issue(t.Context(), tracker.Repository{Owner: "example", Name: "project"}, snapshot.Issues[0].Number)
	if err != nil {
		t.Fatal(err)
	}
	if issue.Title != "Committed task" || len(issue.Labels) != 0 {
		t.Fatalf("issue = %#v", issue)
	}
	committed, err := client.ReadCommittedIssues(t.Context(), snapshot.Revision)
	if err != nil || len(committed.Issues) != 1 || committed.Issues[0].Number != issue.Number || committed.Graph.Digest != snapshot.Graph.Digest {
		t.Fatalf("neutral committed source = %#v, %v", committed, err)
	}
	queue, err := client.ListEligibleIssues(t.Context(), tracker.Repository{Owner: "example", Name: "project"})
	if err != nil || len(queue) != 0 {
		t.Fatalf("import granted execution: %#v, %v", queue, err)
	}
	opened, err := store.OpenReadOnly(t.Context(), registration.OperationalDataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	mapping, err := opened.LocalIssueIdentityByNumber(t.Context(), registration.Path, issue.Number)
	if err != nil || mapping.ID != "local-11111111111111111111111111111111" {
		t.Fatalf("mapping = %#v, %v", mapping, err)
	}
}

// TestLifecycleProjectionsSurviveRestartWithoutEditingSpecifications exercises
// label/status/question storage through the tracker port and then a new adapter.
func TestLifecycleProjectionsSurviveRestartWithoutEditingSpecifications(t *testing.T) {
	client, registration, name := fixture(t)
	snapshot, err := client.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	number := snapshot.Issues[0].Number
	repository := tracker.Repository{Owner: "example", Name: "project"}
	before, err := os.ReadFile(filepath.Join(registration.Path, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ReplaceIssueLabels(t.Context(), repository, number, []string{tracker.LabelAgentRunning}); err != nil {
		t.Fatal(err)
	}
	comment, err := client.CreateIssueComment(t.Context(), repository, number, "<!-- factory:run-1 -->\nAwaiting answer: which format?")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := client.CreateIssueComment(t.Context(), repository, number, "<!-- factory:run-1 -->\nAwaiting answer: which format?")
	if err != nil || replay.ID != comment.ID {
		t.Fatalf("comment replay = %#v, %v", replay, err)
	}
	client = localtracker.NewClient(registration, "main")
	if err := client.EditIssueComment(t.Context(), repository, comment.ID, "<!-- factory:run-1 -->\nAnswer recorded: JSON."); err != nil {
		t.Fatal(err)
	}
	status, err := client.FindStatusComment(t.Context(), repository, number, "<!-- factory:run-1 -->")
	if err != nil || status.ID != comment.ID || !strings.Contains(status.Body, "JSON") {
		t.Fatalf("status = %#v, %v", status, err)
	}
	issue, err := client.Issue(t.Context(), repository, number)
	if err != nil || len(issue.Labels) != 1 || issue.Labels[0] != tracker.LabelAgentRunning {
		t.Fatalf("projection = %#v, %v", issue, err)
	}
	link, err := client.IssueURL(t.Context(), repository, number)
	if err != nil || link != "/issues/local-11111111111111111111111111111111" {
		t.Fatalf("local link = %q, %v", link, err)
	}
	after, err := os.ReadFile(filepath.Join(registration.Path, name))
	if err != nil || string(after) != string(before) || git(t, registration.Path, "status", "--porcelain") != "" {
		t.Fatal("lifecycle history changed accepted source")
	}
}

// TestRenameAndDeletionKeepRevisionAddressedEvidence proves paths are not identity.
func TestRenameAndDeletionKeepRevisionAddressedEvidence(t *testing.T) {
	client, registration, name := fixture(t)
	first, err := client.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	git(t, registration.Path, "mv", name, "docs/issues/renamed.md")
	git(t, registration.Path, "-c", "commit.gpgsign=false", "commit", "-m", "rename")
	git(t, registration.Path, "push", "origin", "main")
	renamed, err := client.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if first.Issues[0].Number != renamed.Issues[0].Number || first.Graph.Digest != renamed.Graph.Digest || renamed.Issues[0].Issue.Path != "docs/issues/renamed.md" {
		t.Fatalf("rename changed identity: %#v", renamed)
	}
	git(t, registration.Path, "rm", "docs/issues/renamed.md")
	git(t, registration.Path, "-c", "commit.gpgsign=false", "commit", "-m", "delete")
	git(t, registration.Path, "push", "origin", "main")
	if _, err := client.Issue(t.Context(), tracker.Repository{}, first.Issues[0].Number); err == nil {
		t.Fatal("deleted source remained available")
	}
	opened, err := store.OpenReadOnly(t.Context(), registration.OperationalDataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	old, err := opened.LocalIssueSources(t.Context(), registration.Path, first.Revision)
	if err != nil || len(old) != 1 || old[0].Issue.Path != name {
		t.Fatalf("immutable evidence = %#v, %v", old, err)
	}
	if _, err := client.Snapshot(t.Context(), strings.Repeat("f", 40)); err == nil {
		t.Fatal("missing source ref was accepted")
	}
}

// TestCommittedSymlinksAreNeverAccepted rejects a tracked source whose bytes
// would otherwise be read through a filesystem indirection.
func TestCommittedSymlinksAreNeverAccepted(t *testing.T) {
	client, registration, name := fixture(t)
	git(t, registration.Path, "rm", name)
	if err := os.MkdirAll(filepath.Join(registration.Path, "docs/issues"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../secret", filepath.Join(registration.Path, name)); err != nil {
		t.Fatal(err)
	}
	git(t, registration.Path, "add", name)
	git(t, registration.Path, "-c", "commit.gpgsign=false", "commit", "-m", "symlink")
	git(t, registration.Path, "push", "origin", "main")
	if _, err := client.Refresh(t.Context()); err == nil {
		t.Fatal("committed issue symlink was accepted")
	}
}

// TestSnapshotRequiresAnActualCommit rejects a tag object's SHA as a claim base.
func TestSnapshotRequiresAnActualCommit(t *testing.T) {
	client, registration, _ := fixture(t)
	git(t, registration.Path, "-c", "tag.gpgsign=false", "tag", "-a", "accepted", "-m", "tag")
	tag := git(t, registration.Path, "rev-parse", "accepted")
	if _, err := client.Snapshot(t.Context(), tag); err == nil {
		t.Fatal("tag object was accepted as an exact commit base")
	}
}

// TestOversizedCommittedIssueIsRejectedBeforeIndexing covers bounded Git input.
func TestOversizedCommittedIssueIsRejectedBeforeIndexing(t *testing.T) {
	client, registration, name := fixture(t)
	if err := os.WriteFile(filepath.Join(registration.Path, name), []byte(strings.Repeat("x", localissues.MaxFileBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, registration.Path, "add", name)
	git(t, registration.Path, "-c", "commit.gpgsign=false", "commit", "-m", "oversize")
	git(t, registration.Path, "push", "origin", "main")
	if _, err := client.Refresh(t.Context()); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("oversize content = %v", err)
	}
}

// TestConflictingCorrelationNeverOverwritesEarlierEvidence rejects file-authored
// attempts to rebind a previously indexed stable identity.
func TestConflictingCorrelationNeverOverwritesEarlierEvidence(t *testing.T) {
	client, registration, name := fixture(t)
	first, err := client.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	changed := first.Graph.Issues[0]
	changed.Correlation = "forged/new/correlation"
	data, err := localissues.Render(changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registration.Path, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, registration.Path, "add", name)
	git(t, registration.Path, "-c", "commit.gpgsign=false", "commit", "-m", "conflicting identity")
	git(t, registration.Path, "push", "origin", "main")
	if _, err := client.Refresh(t.Context()); !errors.Is(err, store.ErrLocalIssueIdentityConflict) {
		t.Fatalf("conflicting source = %v", err)
	}
	opened, err := store.OpenReadOnly(t.Context(), registration.OperationalDataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	old, err := opened.LocalIssueSources(t.Context(), registration.Path, first.Revision)
	if err != nil || len(old) != 1 || old[0].Issue.Correlation != "idea/1/task" {
		t.Fatalf("prior evidence was overwritten: %#v, %v", old, err)
	}
}

// TestARecreatedStoreImportsContentWithoutLifecycleAuthority represents reset
// or a second installation seeing the same committed specification.
func TestARecreatedStoreImportsContentWithoutLifecycleAuthority(t *testing.T) {
	client, registration, _ := fixture(t)
	first, err := client.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ReplaceIssueLabels(t.Context(), tracker.Repository{}, first.Issues[0].Number, []string{tracker.LabelAgentReady}); err != nil {
		t.Fatal(err)
	}
	registration.OperationalDataPath = filepath.Join(filepath.Dir(registration.OperationalDataPath), "recreated", "factory.db")
	client = localtracker.NewClient(registration, "main")
	second, err := client.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	issue, err := client.Issue(t.Context(), tracker.Repository{}, second.Issues[0].Number)
	if err != nil || len(issue.Labels) != 0 {
		t.Fatalf("content import restored lifecycle authority: %#v, %v", issue, err)
	}
	queue, err := client.ListEligibleIssues(t.Context(), tracker.Repository{})
	if err != nil || len(queue) != 0 {
		t.Fatalf("content import became executable: %#v, %v", queue, err)
	}
}

// concurrentFetchRunner injects an independent fetch after the source fetch
// returns, reproducing shared FETCH_HEAD mutation at the process boundary.
type concurrentFetchRunner struct {
	other       string
	interleaved bool
}

// Run executes actual policy-controlled Git and then the competing read fetch.
func (r *concurrentFetchRunner) Run(ctx context.Context, directory string, args []string) ([]byte, error) {
	base := gitadapter.NewCommandRunner()
	output, err := base.Run(ctx, directory, args)
	if err == nil && len(args) > 0 && args[0] == "fetch" && !r.interleaved {
		r.interleaved = true
		if _, err := base.Run(ctx, directory, []string{"fetch", "--no-tags", "origin", "refs/heads/" + r.other}); err != nil {
			return nil, err
		}
	}
	return output, err
}

// TestRefreshCannotReadAnotherFetchesHead proves a concurrent session/run fetch
// cannot substitute its source commit for the configured target snapshot.
func TestRefreshCannotReadAnotherFetchesHead(t *testing.T) {
	client, registration, name := fixture(t)
	want := git(t, registration.Path, "rev-parse", "main")
	git(t, registration.Path, "checkout", "-b", "other")
	data, err := os.ReadFile(filepath.Join(registration.Path, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registration.Path, name), []byte(strings.ReplaceAll(string(data), "Committed task", "Other branch")), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, registration.Path, "add", name)
	git(t, registration.Path, "-c", "commit.gpgsign=false", "commit", "-m", "other source")
	git(t, registration.Path, "push", "origin", "other")
	git(t, registration.Path, "checkout", "main")
	client.Runner = &concurrentFetchRunner{other: "other"}
	snapshot, err := client.Refresh(t.Context())
	if err != nil || snapshot.Revision != want || snapshot.Issues[0].Issue.Title != "Committed task" {
		t.Fatalf("competing fetch substituted source: %#v, %v", snapshot, err)
	}
	if refs := git(t, registration.Path, "for-each-ref", "--format=%(refname)", "refs/factory/visibility/"); refs != "" {
		t.Fatalf("temporary target refs retained: %s", refs)
	}
}
