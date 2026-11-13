// Package localpublication publishes approved Markdown batches through Git.
// It creates content and evidence only, and never grants execution authority.
package localpublication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/codehost"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/localissues"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// Policy selects the publication effects displayed in and bound by a preview.
type Policy string

const (
	// LocalCommit is the safe default: publishing stops at an isolated commit.
	LocalCommit Policy = "local_commit"
	// Push additionally authorizes a non-force push of the specification branch.
	Push Policy = "push"
	// PullRequest additionally authorizes a draft specification PR.
	PullRequest Policy = "pull_request"
)

// IdentityStore separates explicit allocation from read-only preview lookup.
type IdentityStore interface {
	AllocateLocalIssueIdentity(context.Context, string, string) (store.LocalIssueIdentity, error)
	RegisterLocalIssueIdentity(context.Context, string, string, string) (store.LocalIssueIdentity, error)
	LocalIssueIdentityByCorrelation(context.Context, string, string) (store.LocalIssueIdentity, error)
}

// Publisher owns one registered repository's local Git publication capability.
type Publisher struct {
	RepositoryPath string
	Store          IdentityStore
	TargetBranch   string
	Policy         Policy
	// WorktreeRoot is private host storage outside the ordinary checkout.
	// Empty uses a factory-owned sibling directory of the checkout.
	WorktreeRoot string
	Runner       gitadapter.CommandRunner
	PullRequests codehost.PullRequestClient
}

// PreparePlan explicitly reserves durable identities before a read-only preview.
// Existing committed identities are imported as content only.
func (p *Publisher) PreparePlan(ctx context.Context, _ tracker.Repository, graph tracker.PlanGraph) error {
	if err := p.validate(); err != nil {
		return err
	}
	baseline, err := p.graphAt(ctx, graph.ContextRevision)
	if err != nil {
		return err
	}
	for _, issue := range baseline.Issues {
		if _, err := p.Store.RegisterLocalIssueIdentity(ctx, p.RepositoryPath, issue.ID, issue.Correlation); err != nil {
			return err
		}
	}
	for _, item := range graph.Items {
		if _, err := p.Store.AllocateLocalIssueIdentity(ctx, p.RepositoryPath, item.Correlation); err != nil {
			return err
		}
	}
	return nil
}

// PreflightPlan reads committed content and existing identity reservations only.
// Each Body is the exact complete Markdown file; Fields display and bind every
// source, relationship, branch, destination, and optional publication effect.
func (p *Publisher) PreflightPlan(ctx context.Context, repository tracker.Repository, graph tracker.PlanGraph) (tracker.PlanPreview, error) {
	if err := p.validate(); err != nil {
		return tracker.PlanPreview{}, err
	}
	baseline, err := p.graphAt(ctx, graph.ContextRevision)
	if err != nil {
		return tracker.PlanPreview{}, err
	}
	if len(graph.Items) == 0 {
		return tracker.PlanPreview{}, errors.New("local plan has no issues")
	}
	allocated := make(map[string]store.LocalIssueIdentity, len(graph.Items))
	for _, node := range graph.Items {
		if _, found := allocated[node.NodeID]; found || node.NodeID == "" {
			return tracker.PlanPreview{}, errors.New("duplicate or empty plan node")
		}
		identity, err := p.Store.LocalIssueIdentityByCorrelation(ctx, p.RepositoryPath, node.Correlation)
		if err != nil {
			return tracker.PlanPreview{}, fmt.Errorf("prepare local identities before preview: %w", err)
		}
		allocated[node.NodeID] = identity
	}
	preview := tracker.PlanPreview{Target: "local:" + repository.String()}
	files := make([]localissues.File, 0, len(baseline.Issues)+len(graph.Items))
	for _, issue := range baseline.Issues {
		files = append(files, localissues.File{Path: issue.Path, Data: issue.Bytes, Mode: 0})
	}
	for _, node := range graph.Items {
		identity := allocated[node.NodeID]
		kind := localissues.Kind(node.Kind)
		if node.Kind == "spec" {
			kind = localissues.KindParent
		}
		issue := localissues.Issue{SchemaVersion: localissues.SchemaVersion, ID: identity.ID, Title: node.Title, Kind: kind, Correlation: node.Correlation}
		if node.ParentID != "" {
			parent, ok := allocated[node.ParentID]
			if !ok {
				return tracker.PlanPreview{}, errors.New("dangling local plan parent")
			}
			issue.Parent = parent.ID
			preview.Relationships = append(preview.Relationships, tracker.PlanRelationshipMapping{Kind: tracker.PlanRelationshipParent, FromNodeID: node.NodeID, ToNodeID: node.ParentID, Mode: tracker.PlanRelationshipNative})
		}
		for _, dependency := range node.DependsOn {
			needed, ok := allocated[dependency]
			if !ok {
				return tracker.PlanPreview{}, errors.New("dangling local plan dependency")
			}
			issue.Dependencies = append(issue.Dependencies, needed.ID)
			preview.Relationships = append(preview.Relationships, tracker.PlanRelationshipMapping{Kind: tracker.PlanRelationshipDependsOn, FromNodeID: node.NodeID, ToNodeID: dependency, Mode: tracker.PlanRelationshipNative})
		}
		if len(node.AcceptanceCriteria) == 0 {
			return tracker.PlanPreview{}, fmt.Errorf("local issue %q needs observable acceptance criteria", node.NodeID)
		}
		issue.Body = "## Problem\n\n" + strings.TrimSpace(node.Body) + "\n\n## Scope and non-goals\n\nThe scope is the work described above and the acceptance criteria below. No additional work is implied.\n\n## Acceptance criteria\n\n"
		for _, criterion := range node.AcceptanceCriteria {
			issue.Body += "- [ ] " + strings.TrimSpace(criterion) + "\n"
		}
		data, err := localissues.Render(issue)
		if err != nil {
			return tracker.PlanPreview{}, err
		}
		path, _ := localissues.DefaultPath(identity.ID)
		files = append(files, localissues.File{Path: path, Data: data, Mode: 0})
		preview.Items = append(preview.Items, tracker.RenderedPlanItem{NodeID: node.NodeID, Type: string(kind), Title: node.Title, Body: string(data), Correlation: node.Correlation, Fields: map[string]string{
			"path": path, "local_id": identity.ID, "number": strconv.Itoa(identity.Number), "content_digest": localissues.Digest(data), "source_revision": graph.ContextRevision, "base_graph_digest": baseline.Digest,
			"specification_branch": specificationBranch(graph), "publication_policy": string(p.policy()), "target_branch": p.TargetBranch,
		}})
	}
	expected, err := localissues.Validate(files)
	if err != nil {
		return tracker.PlanPreview{}, err
	}
	for i := range preview.Items {
		preview.Items[i].Fields["graph_digest"] = expected.Digest
	}
	return preview, nil
}

// policy resolves omission to local-commit publication, never remote effects.
func (p *Publisher) policy() Policy {
	if p.Policy == "" {
		return LocalCommit
	}
	return p.Policy
}

// validate rejects an incomplete host setup before touching any Git reference.
func (p *Publisher) validate() error {
	if p.Store == nil || !filepath.IsAbs(p.RepositoryPath) || filepath.Clean(p.RepositoryPath) != p.RepositoryPath {
		return errors.New("local publisher requires a registered absolute checkout and identity store")
	}
	if p.TargetBranch == "" || strings.HasPrefix(p.TargetBranch, "-") || strings.ContainsAny(p.TargetBranch, "\x00\r\n ~^:?*[\\") || strings.Contains(p.TargetBranch, "..") {
		return errors.New("invalid publication target branch")
	}
	if p.policy() != LocalCommit && p.policy() != Push && p.policy() != PullRequest {
		return errors.New("unsupported specification publication policy")
	}
	if p.policy() == PullRequest && p.PullRequests == nil {
		return errors.New("specification PR policy requires the selected code host")
	}
	if p.policy() == PullRequest {
		if _, ok := p.PullRequests.(codehost.PullRequestCandidateReader); !ok {
			return errors.New("specification PR policy requires unambiguous code-host branch lookup")
		}
	}
	return nil
}

// specificationBranch uses a full digest so untrusted session IDs never form refs.
func specificationBranch(graph tracker.PlanGraph) string {
	digest := sha256.Sum256([]byte(graph.SessionID + "/" + strconv.FormatInt(graph.PlanRevision, 10)))
	return "factory/specifications/" + hex.EncodeToString(digest[:])
}

// git runs every production Git command with ADR0015 policy and bounded IO.
func (p *Publisher) git(ctx context.Context, directory string, args ...string) ([]byte, error) {
	runner := p.Runner
	if runner == nil {
		runner = gitadapter.NewCommandRunner()
	}
	return runner.Run(ctx, directory, args)
}

// validCommit permits only immutable full SHA-1/SHA-256 object names.
func validCommit(revision string) bool {
	if len(revision) != 40 && len(revision) != 64 {
		return false
	}
	_, err := hex.DecodeString(revision)
	return err == nil && strings.ToLower(revision) == revision
}

// graphAt reads only immutable committed blobs and rejects unsafe ancestor modes.
func (p *Publisher) graphAt(ctx context.Context, revision string) (localissues.Graph, error) {
	if !validCommit(revision) {
		return localissues.Graph{}, errors.New("publication context must be a full immutable Git commit")
	}
	if _, err := p.git(ctx, p.RepositoryPath, "cat-file", "-e", revision+"^{commit}"); err != nil {
		return localissues.Graph{}, err
	}
	for _, ancestor := range []string{"docs", "docs/issues"} {
		out, err := p.git(ctx, p.RepositoryPath, "ls-tree", revision, "--", ancestor)
		if err != nil {
			return localissues.Graph{}, err
		}
		if len(out) > 0 && !bytes.HasPrefix(out, []byte("040000 tree ")) {
			return localissues.Graph{}, errors.New("local issue ancestor is not a committed directory")
		}
	}
	listing, err := p.git(ctx, p.RepositoryPath, "ls-tree", "-r", "-z", revision, "--", localissues.Directory)
	if err != nil {
		return localissues.Graph{}, err
	}
	var files []localissues.File
	total := 0
	for _, line := range bytes.Split(listing, []byte{0}) {
		if len(line) == 0 {
			continue
		}
		header, name, ok := bytes.Cut(line, []byte{'\t'})
		fields := strings.Fields(string(header))
		if !ok || len(fields) != 3 {
			return localissues.Graph{}, errors.New("malformed committed local issue entry")
		}
		mode := fs.FileMode(0)
		if fields[0] != "100644" && fields[0] != "100755" {
			mode = fs.ModeSymlink
		}
		if err := localissues.ValidatePath(string(name)); err != nil {
			return localissues.Graph{}, err
		}
		size, err := p.git(ctx, p.RepositoryPath, "cat-file", "-s", fields[2])
		if err != nil {
			return localissues.Graph{}, err
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(size)))
		if err != nil || n < 0 || n > localissues.MaxFileBytes || n > localissues.MaxGraphBytes-total {
			return localissues.Graph{}, errors.New("committed local issue exceeds file limit")
		}
		total += n
		data, err := p.git(ctx, p.RepositoryPath, "cat-file", "blob", fields[2])
		if err != nil {
			return localissues.Graph{}, err
		}
		files = append(files, localissues.File{Path: string(name), Data: data, Mode: mode})
		if len(files) > localissues.MaxIssues {
			return localissues.Graph{}, errors.New("committed local issue graph exceeds issue limit")
		}
	}
	return localissues.Validate(files)
}
