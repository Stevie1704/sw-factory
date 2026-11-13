package localpublication

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// NewPublisher constructs a lazy adapter without opening or migrating a store.
// Only explicit preparation opens a writer; preflight opens existing read-only
// identity projections, including when factory ui constructs the adapter.
func NewPublisher(registration config.RepositoryRegistration, targetBranch string, host codehost.PullRequestClient) *Publisher {
	return &Publisher{RepositoryPath: registration.Path, Store: lazyIdentityStore{path: registration.OperationalDataPath}, TargetBranch: targetBranch, WorktreeRoot: filepath.Join(filepath.Dir(registration.OperationalDataPath), "specifications"), PullRequests: host}
}

// Adapter composes committed issue lifecycle behavior with Git publication.
// The composition root owns this pairing; workflow code sees only ports.
type Adapter struct {
	tracker.Client
	*Publisher
}

// RefreshCommittedIssuesAt keeps current-target observation behind its port.
func (a *Adapter) RefreshCommittedIssuesAt(ctx context.Context, path, target string) (tracker.CommittedSnapshot, error) {
	reader, ok := a.Client.(tracker.TargetCommittedIssueReader)
	if !ok {
		return tracker.CommittedSnapshot{}, errors.New("local tracker cannot observe current target policy")
	}
	return reader.RefreshCommittedIssuesAt(ctx, path, target)
}

// ProjectIssue keeps frozen Run content independent of live source validity.
func (a *Adapter) ProjectIssue(ctx context.Context, repo tracker.Repository, frozen tracker.Issue) (tracker.Issue, error) {
	reader, ok := a.Client.(tracker.IssueProjectionReader)
	if !ok {
		return tracker.Issue{}, errors.New("local tracker cannot read lifecycle projection")
	}
	return reader.ProjectIssue(ctx, repo, frozen)
}

// Wrap preserves tracker capabilities while adding content-only publication.
func Wrap(client tracker.Client, publisher *Publisher) *Adapter {
	return &Adapter{Client: client, Publisher: publisher}
}

// ListEligibleIssues delegates only to the tracker's authorized queue seam.
func (a *Adapter) ListEligibleIssues(ctx context.Context, repo tracker.Repository) ([]tracker.Issue, error) {
	if poller, ok := a.Client.(tracker.IssuePoller); ok {
		return poller.ListEligibleIssues(ctx, repo)
	}
	return nil, errors.New("local tracker has no queue capability")
}

// IssueComments keeps issue records distinct from code-host PR conversations.
func (a *Adapter) IssueComments(ctx context.Context, repo tracker.Repository, number int) ([]tracker.Comment, error) {
	if reader, ok := a.Client.(tracker.CommentReader); ok {
		return reader.IssueComments(ctx, repo, number)
	}
	return nil, errors.New("local tracker has no comment reader")
}

// AuthenticatedLogin returns host registration provenance through the tracker.
func (a *Adapter) AuthenticatedLogin(ctx context.Context) (string, error) {
	if account, ok := a.Client.(tracker.AccountReader); ok {
		return account.AuthenticatedLogin(ctx)
	}
	return "", errors.New("local operator identity is unavailable")
}

// StartupChecks retains local source/index readiness without remote issue APIs.
func (a *Adapter) StartupChecks(repo tracker.Repository) []doctor.Check {
	if readiness, ok := a.Client.(tracker.ReadinessChecker); ok {
		return readiness.StartupChecks(repo)
	}
	return nil
}

// SeparateIssueCommentStream prevents local issue and PR-number aliasing.
func (a *Adapter) SeparateIssueCommentStream() bool { return true }

// IssueURL returns the original stable local issue UI link.
func (a *Adapter) IssueURL(ctx context.Context, repo tracker.Repository, number int) (string, error) {
	if links, ok := a.Client.(interface {
		IssueURL(context.Context, tracker.Repository, int) (string, error)
	}); ok {
		return links.IssueURL(ctx, repo, number)
	}
	return "", errors.New("local issue link is unavailable")
}

// RefreshCommittedIssues retains coordinator-owned content import without
// interpreting files or publication evidence as execution authorization.
func (a *Adapter) RefreshCommittedIssues(ctx context.Context) (tracker.CommittedSnapshot, error) {
	if reader, ok := a.Client.(tracker.CommittedIssueReader); ok {
		return reader.RefreshCommittedIssues(ctx)
	}
	return tracker.CommittedSnapshot{}, errors.New("local tracker has no committed source reader")
}

// ReadCommittedIssues revalidates one immutable source revision without writes.
func (a *Adapter) ReadCommittedIssues(ctx context.Context, revision string) (tracker.CommittedSnapshot, error) {
	if reader, ok := a.Client.(tracker.CommittedIssueReader); ok {
		return reader.ReadCommittedIssues(ctx, revision)
	}
	return tracker.CommittedSnapshot{}, errors.New("local tracker has no committed source reader")
}

// lazyIdentityStore opens private SQLite handles only for the selected action.
type lazyIdentityStore struct{ path string }

// AllocateLocalIssueIdentity is an explicit preparation-only coordinator write.
func (s lazyIdentityStore) AllocateLocalIssueIdentity(ctx context.Context, repo, correlation string) (store.LocalIssueIdentity, error) {
	opened, err := store.Open(ctx, s.path)
	if err != nil {
		return store.LocalIssueIdentity{}, err
	}
	defer opened.Close()
	return opened.AllocateLocalIssueIdentity(ctx, repo, correlation)
}

// RegisterLocalIssueIdentity imports committed source identity without authority.
func (s lazyIdentityStore) RegisterLocalIssueIdentity(ctx context.Context, repo, id, correlation string) (store.LocalIssueIdentity, error) {
	opened, err := store.Open(ctx, s.path)
	if err != nil {
		return store.LocalIssueIdentity{}, err
	}
	defer opened.Close()
	return opened.RegisterLocalIssueIdentity(ctx, repo, id, correlation)
}

// LocalIssueIdentityByCorrelation never creates, migrates, or writes the store.
func (s lazyIdentityStore) LocalIssueIdentityByCorrelation(ctx context.Context, repo, correlation string) (store.LocalIssueIdentity, error) {
	opened, err := store.OpenReadOnly(ctx, s.path)
	if err != nil {
		return store.LocalIssueIdentity{}, err
	}
	defer opened.Close()
	return opened.LocalIssueIdentityByCorrelation(ctx, repo, correlation)
}

var _ tracker.Client = (*Adapter)(nil)
var _ tracker.PlanPublisher = (*Adapter)(nil)
var _ tracker.PlanBatchPublisher = (*Adapter)(nil)
var _ tracker.CommittedIssueReader = (*Adapter)(nil)
