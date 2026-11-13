// Package localtracker implements tracker ports over immutable committed
// Markdown and durable host-local lifecycle records. Source files confer no
// execution authority and are never changed by status projections.
package localtracker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/doctor"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/localissues"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// Client owns one registered local tracker and no code-host credentials.
type Client struct {
	Registration config.RepositoryRegistration
	TargetBranch string
	Runner       gitadapter.CommandRunner
	OpenStore    func(context.Context, string) (*store.Store, error)
	Now          func() time.Time
	mu           sync.Mutex
}

// Snapshot is a fully validated immutable target/source revision. Issues carries
// durable mappings after Refresh; a read-only Snapshot does not allocate them.
type Snapshot struct {
	Revision string
	Graph    localissues.Graph
	Issues   []store.LocalIssueSource
}

var _ tracker.Client = (*Client)(nil)
var _ tracker.IssuePoller = (*Client)(nil)
var _ tracker.CommentReader = (*Client)(nil)
var _ tracker.ReadinessChecker = (*Client)(nil)
var _ tracker.CommittedIssueReader = (*Client)(nil)

// NewClient selects committed local content and the existing operational store.
func NewClient(registration config.RepositoryRegistration, targetBranch string) *Client {
	return &Client{Registration: registration, TargetBranch: targetBranch, OpenStore: store.Open, Now: time.Now}
}

// run delegates every product Git operation to the common host policy runner.
func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	runner := c.Runner
	if runner == nil {
		runner = gitadapter.NewCommandRunner()
	}
	return runner.Run(ctx, c.Registration.Path, args)
}

// open resolves the coordinator store; read-only preflight never calls it.
func (c *Client) open(ctx context.Context) (*store.Store, error) {
	if c.OpenStore != nil {
		return c.OpenStore(ctx, c.Registration.OperationalDataPath)
	}
	return store.Open(ctx, c.Registration.OperationalDataPath)
}

// now resolves the projection clock without adding time to accepted files.
func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

// Snapshot reads one immutable full commit without a fetch or operational write.
// No working-tree file or mutable index can supply accepted issue content.
func (c *Client) Snapshot(ctx context.Context, revision string) (Snapshot, error) {
	if !codehost.ValidCommitSHA(revision) {
		return Snapshot{}, errors.New("local issue source requires an immutable full commit identity")
	}
	objectType, err := c.run(ctx, "cat-file", "-t", revision)
	if err != nil {
		return Snapshot{}, fmt.Errorf("local source commit unavailable: %w", err)
	}
	if strings.TrimSpace(string(objectType)) != "commit" {
		return Snapshot{}, errors.New("local source identity is not a commit object")
	}
	for _, ancestor := range []string{"docs", "docs/issues"} {
		out, err := c.run(ctx, "ls-tree", "-z", revision, "--", ancestor)
		if err != nil {
			return Snapshot{}, err
		}
		if len(out) > 0 && !strings.HasPrefix(string(out), "040000 tree ") {
			return Snapshot{}, errors.New("local issue source ancestors must be committed directories")
		}
	}
	output, err := c.run(ctx, "ls-tree", "-r", "-z", revision, "--", localissues.Directory)
	if err != nil {
		return Snapshot{}, err
	}
	entries := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
	if len(entries) > localissues.MaxIssues {
		return Snapshot{}, errors.New("local issue snapshot exceeds issue limit")
	}
	files := make([]localissues.File, 0, len(entries))
	totalBytes := int64(0)
	for _, entry := range entries {
		if entry == "" {
			continue
		}
		metadata, name, ok := strings.Cut(entry, "\t")
		if !ok {
			return Snapshot{}, errors.New("malformed local issue tree entry")
		}
		fields := strings.Fields(metadata)
		if len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
			return Snapshot{}, errors.New("local issue source must be a regular committed file")
		}
		if err := localissues.ValidatePath(name); err != nil {
			return Snapshot{}, err
		}
		sizeOutput, err := c.run(ctx, "cat-file", "-s", fields[2])
		if err != nil {
			return Snapshot{}, err
		}
		size, err := strconv.ParseInt(strings.TrimSpace(string(sizeOutput)), 10, 64)
		if err != nil || size < 0 || size > localissues.MaxFileBytes {
			return Snapshot{}, errors.New("local issue blob exceeds file byte limit")
		}
		totalBytes += size
		if totalBytes > localissues.MaxGraphBytes {
			return Snapshot{}, errors.New("local issue snapshot exceeds graph byte limit")
		}
		data, err := c.run(ctx, "cat-file", "blob", fields[2])
		if err != nil {
			return Snapshot{}, err
		}
		files = append(files, localissues.File{Path: name, Data: data, Mode: fs.FileMode(0)})
	}
	graph, err := localissues.Validate(files)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Revision: revision, Graph: graph}, nil
}

// Refresh fetches one target revision, validates the complete source graph, and
// atomically imports revision-addressed evidence without granting authority.
func (c *Client) Refresh(ctx context.Context) (Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.TargetBranch == "" || strings.HasPrefix(c.TargetBranch, "-") || strings.ContainsAny(c.TargetBranch, "\x00\r\n") {
		return Snapshot{}, errors.New("local tracker target branch is required")
	}
	if _, err := c.run(ctx, "check-ref-format", "refs/heads/"+c.TargetBranch); err != nil {
		return Snapshot{}, errors.New("invalid local tracker target branch")
	}
	revision, err := gitadapter.FetchTargetSnapshot(ctx, c.Runner, c.Registration.Path, c.TargetBranch)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot, err := c.Snapshot(ctx, revision)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: %w", tracker.ErrInvalidCommittedSource, err)
	}
	opened, err := c.open(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	defer opened.Close()
	snapshot.Issues, err = opened.IndexLocalIssueSnapshot(ctx, c.Registration.Path, snapshot.Revision, snapshot.Graph)
	return snapshot, err
}

// ListIssues enumerates the full committed graph, including organizational items.
func (c *Client) ListIssues(ctx context.Context) (Snapshot, error) { return c.Refresh(ctx) }

// RefreshCommittedIssues imports one validated target snapshot through the
// neutral source port, granting no authorization or eligibility.
func (c *Client) RefreshCommittedIssues(ctx context.Context) (tracker.CommittedSnapshot, error) {
	snapshot, err := c.Refresh(ctx)
	if err != nil {
		return tracker.CommittedSnapshot{}, err
	}
	result := tracker.CommittedSnapshot{Revision: snapshot.Revision, Graph: snapshot.Graph}
	for _, source := range snapshot.Issues {
		result.Issues = append(result.Issues, tracker.CommittedIssue{Number: source.Number, Source: source.Issue})
	}
	return result, nil
}

// RefreshCommittedIssuesAt uses the current target without mutating adapter
// configuration. Changing the registered checkout requires recomposition.
func (c *Client) RefreshCommittedIssuesAt(ctx context.Context, repositoryPath, targetBranch string) (tracker.CommittedSnapshot, error) {
	if repositoryPath != c.Registration.Path {
		return tracker.CommittedSnapshot{}, errors.New("registered checkout changed; restart the coordinator to recompose adapters")
	}
	current := NewClient(c.Registration, targetBranch)
	current.Runner, current.OpenStore, current.Now = c.Runner, c.OpenStore, c.Now
	return current.RefreshCommittedIssues(ctx)
}

// ProjectIssue preserves frozen accepted content while reading operational
// status. Missing or changed Git source cannot prevent Run recovery.
func (c *Client) ProjectIssue(ctx context.Context, _ tracker.Repository, frozen tracker.Issue) (tracker.Issue, error) {
	opened, err := store.OpenReadOnly(ctx, c.Registration.OperationalDataPath)
	if err != nil {
		return tracker.Issue{}, err
	}
	defer opened.Close()
	mapping, err := opened.LocalIssueIdentityByNumber(ctx, c.Registration.Path, frozen.Number)
	if err != nil {
		return tracker.Issue{}, err
	}
	projection, err := opened.LocalIssueProjection(ctx, c.Registration.Path, mapping.ID)
	if err != nil {
		return tracker.Issue{}, err
	}
	frozen.State, frozen.Labels = projection.State, slices.Clone(projection.Labels)
	return frozen, nil
}

// ReadCommittedIssues revalidates exact Git bytes and reads existing mappings;
// it never allocates an identity, updates indexes, or changes approvals.
func (c *Client) ReadCommittedIssues(ctx context.Context, revision string) (tracker.CommittedSnapshot, error) {
	snapshot, err := c.Snapshot(ctx, revision)
	if err != nil {
		return tracker.CommittedSnapshot{}, err
	}
	opened, err := store.OpenReadOnly(ctx, c.Registration.OperationalDataPath)
	if err != nil {
		return tracker.CommittedSnapshot{}, err
	}
	defer opened.Close()
	result := tracker.CommittedSnapshot{Revision: snapshot.Revision, Graph: snapshot.Graph}
	for _, issue := range snapshot.Graph.Issues {
		mapping, err := opened.LocalIssueIdentity(ctx, c.Registration.Path, issue.ID)
		if err != nil {
			return tracker.CommittedSnapshot{}, err
		}
		if mapping.Correlation != issue.Correlation {
			return tracker.CommittedSnapshot{}, store.ErrLocalIssueIdentityConflict
		}
		result.Issues = append(result.Issues, tracker.CommittedIssue{Number: mapping.Number, Source: issue})
	}
	return result, nil
}

// Issue revalidates the committed source and joins operational lifecycle data.
func (c *Client) Issue(ctx context.Context, _ tracker.Repository, number int) (tracker.Issue, error) {
	if number <= 0 {
		return tracker.Issue{}, errors.New("local issue number must be positive")
	}
	snapshot, err := c.Refresh(ctx)
	if err != nil {
		return tracker.Issue{}, err
	}
	for _, source := range snapshot.Issues {
		if source.Number == number {
			opened, err := c.open(ctx)
			if err != nil {
				return tracker.Issue{}, err
			}
			defer opened.Close()
			projection, err := opened.LocalIssueProjection(ctx, c.Registration.Path, source.Issue.ID)
			if err != nil {
				return tracker.Issue{}, err
			}
			return tracker.Issue{Number: number, Title: source.Issue.Title, Body: source.Issue.Body, State: projection.State, Labels: slices.Clone(projection.Labels)}, nil
		}
	}
	return tracker.Issue{}, store.ErrLocalIssueSourceNotFound
}

// ListEligibleIssues deliberately grants no authority from imported content or
// labels. The authorization-aware coordinator scheduling capability owns claims.
func (c *Client) ListEligibleIssues(ctx context.Context, _ tracker.Repository) ([]tracker.Issue, error) {
	_, err := c.Refresh(ctx)
	return []tracker.Issue{}, err
}

// AuthenticatedLogin resolves explicit host registration, never a remote account.
func (c *Client) AuthenticatedLogin(context.Context) (string, error) {
	if c.Registration.LocalOperator.Name == "" {
		return "", errors.New("local operator is not explicitly registered")
	}
	return c.Registration.LocalOperator.Name, nil
}

// CreateLabel is a local capability check; label definitions have no remote effect.
func (c *Client) CreateLabel(_ context.Context, _ tracker.Repository, label tracker.Label) error {
	if !tracker.IsFactoryStateLabel(label.Name) {
		return errors.New("local tracker only defines factory lifecycle labels")
	}
	return nil
}

// ReplaceIssueLabels changes local lifecycle evidence without touching source.
func (c *Client) ReplaceIssueLabels(ctx context.Context, _ tracker.Repository, number int, labels []string) error {
	opened, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer opened.Close()
	identity, err := opened.LocalIssueIdentityByNumber(ctx, c.Registration.Path, number)
	if err != nil {
		return err
	}
	return opened.ReplaceLocalIssueLabels(ctx, c.Registration.Path, identity.ID, labels)
}

// CreateIssueComment appends coordinator-owned local status/question history.
func (c *Client) CreateIssueComment(ctx context.Context, _ tracker.Repository, number int, body string) (tracker.Comment, error) {
	opened, err := c.open(ctx)
	if err != nil {
		return tracker.Comment{}, err
	}
	defer opened.Close()
	identity, err := opened.LocalIssueIdentityByNumber(ctx, c.Registration.Path, number)
	if err != nil {
		return tracker.Comment{}, err
	}
	return opened.CreateLocalIssueComment(ctx, c.Registration.Path, identity.ID, "factory", body, c.now())
}

// IssueComments reads durable local history, never commands from Markdown.
func (c *Client) IssueComments(ctx context.Context, _ tracker.Repository, number int) ([]tracker.Comment, error) {
	opened, err := c.open(ctx)
	if err != nil {
		return nil, err
	}
	defer opened.Close()
	identity, err := opened.LocalIssueIdentityByNumber(ctx, c.Registration.Path, number)
	if err != nil {
		return nil, err
	}
	return opened.LocalIssueComments(ctx, c.Registration.Path, identity.ID)
}

// FindStatusComment adopts the unique coordinator comment with the exact marker.
func (c *Client) FindStatusComment(ctx context.Context, repository tracker.Repository, number int, marker string) (tracker.Comment, error) {
	comments, err := c.IssueComments(ctx, repository, number)
	if err != nil {
		return tracker.Comment{}, err
	}
	var found tracker.Comment
	for _, comment := range comments {
		if strings.Contains(comment.Body, marker) {
			if found.ID != "" {
				return tracker.Comment{}, errors.New("ambiguous local status comment marker")
			}
			found = comment
		}
	}
	return found, nil
}

// EditIssueComment updates operational history only.
func (c *Client) EditIssueComment(ctx context.Context, _ tracker.Repository, id, body string) error {
	opened, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer opened.Close()
	return opened.EditLocalIssueComment(ctx, c.Registration.Path, id, body, c.now())
}

// IssueURL is a local UI link based on the stable source identity.
func (c *Client) IssueURL(ctx context.Context, _ tracker.Repository, number int) (string, error) {
	opened, err := store.OpenReadOnly(ctx, c.Registration.OperationalDataPath)
	if err != nil {
		return "", err
	}
	defer opened.Close()
	identity, err := opened.LocalIssueIdentityByNumber(ctx, c.Registration.Path, number)
	if err != nil {
		return "", err
	}
	return "/issues/" + identity.ID, nil
}

// SeparateIssueCommentStream marks local and code-host IDs as independent.
func (c *Client) SeparateIssueCommentStream() bool { return true }

// StartupChecks validates committed source without importing or allocating IDs.
func (c *Client) StartupChecks(_ tracker.Repository) []doctor.Check {
	return []doctor.Check{func(ctx context.Context) doctor.Result {
		output, err := c.run(ctx, "rev-parse", "--verify", "refs/remotes/origin/"+c.TargetBranch+"^{commit}")
		if err == nil {
			_, err = c.Snapshot(ctx, strings.TrimSpace(string(output)))
		}
		if err != nil {
			if c.hasActiveClaim(ctx) {
				return doctor.Warning(doctor.Condition{Code: "local_tracker.source.changed_with_frozen_run", Origin: doctor.OriginEnvironment, Effect: doctor.EffectInspection, Topic: "debug", Unobserved: true}, "local Markdown source", "current source cannot be validated; the active Run retains its frozen accepted source", "repair the committed graph before approving further work; supported recovery for the existing Run remains available")
			}
			return doctor.Failure(doctor.Condition{Code: "local_tracker.source.unavailable", Origin: doctor.OriginEnvironment, Effect: doctor.EffectInspection, Topic: "setup", Unobserved: true}, "local Markdown source", "the committed local issue graph or target ref cannot be validated", "fetch the configured target branch and repair docs/issues schema, paths, and relationships; no remote issue login is required")
		}
		return doctor.Success("local_tracker.source.ready", "local Markdown source")
	}}
}

// hasActiveClaim recognizes durable coordinator-created local claim evidence;
// a file or label cannot make invalid source pass startup diagnosis.
func (c *Client) hasActiveClaim(ctx context.Context) bool {
	opened, err := store.OpenReadOnly(ctx, c.Registration.OperationalDataPath)
	if err != nil {
		return false
	}
	defer opened.Close()
	run, err := opened.CurrentRun(ctx)
	if err != nil || run == nil {
		return false
	}
	claims, err := opened.LocalClaims(ctx, c.Registration.Path)
	if err != nil {
		return false
	}
	for _, claim := range claims {
		if claim.RunID == run.ID && claim.State == "claimed" {
			return true
		}
	}
	return false
}
