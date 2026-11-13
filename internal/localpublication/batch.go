package localpublication

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/codehost"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/localissues"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// Batch is validated exact approved content and its publication destination.
// GraphDigest includes the complete original graph plus the approved additions.
type Batch struct {
	Branch       string
	BaseSHA      string
	TargetBranch string
	Policy       Policy
	GraphDigest  string
	Files        map[string][]byte
	Items        map[string]tracker.PlanItemRef
	Marker       string
}

// DecodePreview validates every displayed file, identity, relationship and
// publication field before any Git write. It never allocates or authorizes.
func DecodePreview(preview tracker.PlanPreview) (Batch, error) {
	if len(preview.Items) == 0 {
		return Batch{}, errors.New("empty local publication")
	}
	first := preview.Items[0].Fields
	batch := Batch{Branch: first["specification_branch"], BaseSHA: first["source_revision"], TargetBranch: first["target_branch"], Policy: Policy(first["publication_policy"]), GraphDigest: first["graph_digest"], Files: map[string][]byte{}, Items: map[string]tracker.PlanItemRef{}}
	if !strings.HasPrefix(batch.Branch, "factory/specifications/") || len(strings.TrimPrefix(batch.Branch, "factory/specifications/")) != 64 || !validCommit(batch.BaseSHA) {
		return Batch{}, errors.New("invalid local publication ref or source")
	}
	if _, err := strconv.ParseUint(strings.TrimPrefix(batch.Branch, "factory/specifications/")[:16], 16, 64); err != nil {
		return Batch{}, errors.New("invalid specification branch")
	}
	for _, c := range strings.TrimPrefix(batch.Branch, "factory/specifications/") {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return Batch{}, errors.New("invalid specification branch")
		}
	}
	if batch.Policy != LocalCommit && batch.Policy != Push && batch.Policy != PullRequest {
		return Batch{}, errors.New("unsupported approved publication policy")
	}
	nodes := map[string]localissues.Issue{}
	for _, item := range preview.Items {
		fields := item.Fields
		for _, key := range []string{"specification_branch", "source_revision", "target_branch", "publication_policy", "graph_digest", "base_graph_digest"} {
			if fields[key] != first[key] {
				return Batch{}, errors.New("local batch has inconsistent publication fields")
			}
		}
		issue, err := localissues.Parse(fields["path"], []byte(item.Body))
		if err != nil {
			return Batch{}, err
		}
		if issue.ID != fields["local_id"] || issue.ContentDigest != fields["content_digest"] || issue.Title != item.Title || string(issue.Kind) != item.Type || issue.Correlation != item.Correlation {
			return Batch{}, errors.New("local preview does not match its exact Markdown")
		}
		number, err := strconv.Atoi(fields["number"])
		if err != nil || number <= 0 {
			return Batch{}, errors.New("invalid local tracker mapping")
		}
		if _, ok := batch.Files[issue.Path]; ok {
			return Batch{}, errors.New("duplicate local destination")
		}
		if _, ok := nodes[item.NodeID]; ok {
			return Batch{}, errors.New("duplicate local node")
		}
		batch.Files[issue.Path] = issue.Bytes
		batch.Items[item.NodeID] = tracker.PlanItemRef{ExternalID: strconv.Itoa(number), URL: "/issues/" + issue.ID}
		nodes[item.NodeID] = issue
	}
	actual := map[string]bool{}
	for _, link := range preview.Relationships {
		from, ok1 := nodes[link.FromNodeID]
		to, ok2 := nodes[link.ToNodeID]
		if !ok1 || !ok2 || link.Mode != tracker.PlanRelationshipNative {
			return Batch{}, errors.New("invalid local relationship mapping")
		}
		key := string(link.Kind) + "/" + from.ID + "/" + to.ID
		if actual[key] {
			return Batch{}, errors.New("duplicate local relationship")
		}
		actual[key] = true
		if link.Kind == tracker.PlanRelationshipParent {
			if from.Parent != to.ID {
				return Batch{}, errors.New("parent mapping differs from Markdown")
			}
		} else if link.Kind == tracker.PlanRelationshipDependsOn {
			if !slices.Contains(from.Dependencies, to.ID) {
				return Batch{}, errors.New("dependency mapping differs from Markdown")
			}
		} else {
			return Batch{}, errors.New("unsupported local relationship")
		}
	}
	for _, issue := range nodes {
		if issue.Parent != "" && !actual["parent/"+issue.ID+"/"+issue.Parent] {
			return Batch{}, errors.New("missing local parent mapping")
		}
		for _, id := range issue.Dependencies {
			if !actual["depends_on/"+issue.ID+"/"+id] {
				return Batch{}, errors.New("missing local dependency mapping")
			}
		}
	}
	data, _ := json.Marshal(preview)
	batch.Marker = "Factory-Specification-Batch: " + localissues.Digest(data)
	return batch, nil
}

// approvedBatch revalidates the full baseline+batch graph and current policy.
func (p *Publisher) approvedBatch(ctx context.Context, preview tracker.PlanPreview) (Batch, error) {
	if err := p.validate(); err != nil {
		return Batch{}, err
	}
	batch, err := DecodePreview(preview)
	if err != nil {
		return Batch{}, err
	}
	if batch.Policy != p.policy() || batch.TargetBranch != p.TargetBranch {
		return Batch{}, errors.New("approved publication policy changed; preview and approve again")
	}
	baseline, err := p.graphAt(ctx, batch.BaseSHA)
	if err != nil {
		return Batch{}, err
	}
	if baseline.Digest != preview.Items[0].Fields["base_graph_digest"] {
		return Batch{}, errors.New("approved source graph changed")
	}
	files := make([]localissues.File, 0, len(baseline.Issues)+len(batch.Files))
	for _, issue := range baseline.Issues {
		files = append(files, localissues.File{Path: issue.Path, Data: issue.Bytes})
	}
	for path, data := range batch.Files {
		files = append(files, localissues.File{Path: path, Data: data})
	}
	graph, err := localissues.Validate(files)
	if err != nil {
		return Batch{}, err
	}
	if graph.Digest != batch.GraphDigest {
		return Batch{}, errors.New("approved batch graph identity differs")
	}
	return batch, nil
}

// FindPlanBatch observes the approved branch, push and PR without changing them.
// A ref at the original base is resumable only with its registered owned worktree.
func (p *Publisher) FindPlanBatch(ctx context.Context, repo tracker.Repository, preview tracker.PlanPreview) (tracker.PlanBatchResult, bool, error) {
	batch, err := p.approvedBatch(ctx, preview)
	if err != nil {
		return tracker.PlanBatchResult{}, false, err
	}
	result := tracker.PlanBatchResult{Branch: batch.Branch, BaseSHA: batch.BaseSHA, Items: batch.Items}
	out, err := p.git(ctx, p.RepositoryPath, "show-ref", "--verify", "--hash", "refs/heads/"+batch.Branch)
	if err != nil {
		// A verified exact ref lookup distinguishes absence from inaccessible Git.
		all, listErr := p.git(ctx, p.RepositoryPath, "for-each-ref", "--format=%(refname)", "refs/heads/"+batch.Branch)
		if listErr != nil {
			return result, false, listErr
		}
		if len(bytes.TrimSpace(all)) == 0 {
			return result, false, nil
		}
		return result, false, err
	}
	head := strings.TrimSpace(string(out))
	if head == batch.BaseSHA {
		if err := p.inspectWorktree(ctx, batch); err != nil {
			return result, true, fmt.Errorf("specification ref exists without recoverable owned worktree: %w", err)
		}
		return result, true, nil
	}
	if err := p.verifyCommit(ctx, batch, head); err != nil {
		return result, true, err
	}
	result.CommitSHA = head
	if batch.Policy == LocalCommit {
		result.Complete = true
		return result, true, nil
	}
	remote, err := p.remoteRef(ctx, batch.Branch)
	if err != nil {
		return result, true, err
	}
	if remote != "" && remote != head {
		return result, true, errors.New("remote specification branch conflicts with approved commit; reconcile manually")
	}
	result.Pushed = remote == head
	if batch.Policy == Push {
		result.Complete = result.Pushed
		return result, true, nil
	}
	candidates := p.PullRequests.(codehost.PullRequestCandidateReader)
	prs, err := candidates.FindPullRequestCandidates(ctx, repo, batch.Branch, batch.TargetBranch)
	if err != nil {
		return result, true, err
	}
	if len(prs) > 1 {
		return result, true, errors.New("multiple specification PRs match the approved branch; reconcile explicitly")
	}
	if len(prs) == 1 {
		pr := prs[0]
		if pr.HeadBranch != batch.Branch || pr.BaseBranch != batch.TargetBranch || pr.HeadSHA != "" && pr.HeadSHA != head || !strings.Contains(pr.Body, batch.Marker) {
			return result, true, errors.New("specification PR conflicts with approved publication")
		}
		result.PullRequestNumber, result.PullRequestURL = pr.Number, pr.URL
		result.Complete = result.Pushed
	}
	return result, true, nil
}

// PublishPlanBatch creates one isolated dedicated commit and optional approved
// remote effects. It adopts each matching effect before continuing; it never
// writes the target branch or changes ordinary checkout/index state.
func (p *Publisher) PublishPlanBatch(ctx context.Context, repo tracker.Repository, preview tracker.PlanPreview, recorders ...tracker.PlanBatchRecorder) (tracker.PlanBatchResult, error) {
	batch, err := p.approvedBatch(ctx, preview)
	if err != nil {
		return tracker.PlanBatchResult{}, tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "%v", err)
	}
	result, found, err := p.FindPlanBatch(ctx, repo, preview)
	if err != nil {
		return result, err
	}
	if result.Complete {
		return result, nil
	}
	if result.CommitSHA == "" {
		result.CommitAttempted = true
		if err := recordBatch(result, recorders); err != nil {
			return result, err
		}
		if err := p.createCommit(ctx, batch, found); err != nil {
			return result, err
		}
		result, _, err = p.FindPlanBatch(ctx, repo, preview)
		if recordErr := recordBatch(result, recorders); recordErr != nil {
			return result, recordErr
		}
		if err != nil {
			return result, err
		}
	}
	if batch.Policy != LocalCommit && !result.Pushed {
		result.PushAttempted = true
		if err := recordBatch(result, recorders); err != nil {
			return result, err
		}
		if _, err = p.git(ctx, p.RepositoryPath, "push", "origin", result.CommitSHA+":refs/heads/"+batch.Branch); err != nil {
			return result, err
		}
		result.Pushed = true
		if err := recordBatch(result, recorders); err != nil {
			return result, err
		}
	}
	if batch.Policy == PullRequest && result.PullRequestNumber == 0 {
		result.PullRequestAttempted = true
		if err := recordBatch(result, recorders); err != nil {
			return result, err
		}
		pr, err := p.PullRequests.CreatePullRequest(ctx, repo, codehost.PullRequestRequest{Title: "Publish accepted Markdown specifications", Body: batch.Marker + "\n\nAccepted specification content only. This PR grants no execution or merge authority.\n", HeadBranch: batch.Branch, BaseBranch: batch.TargetBranch, Draft: true})
		if err != nil {
			return result, err
		}
		result.PullRequestNumber, result.PullRequestURL = pr.Number, pr.URL
		if err := recordBatch(result, recorders); err != nil {
			return result, err
		}
	}
	result.Complete = true
	return result, nil
}

// recordBatch makes the coordinator's journal durable before a subeffect.
func recordBatch(result tracker.PlanBatchResult, recorders []tracker.PlanBatchRecorder) error {
	for _, record := range recorders {
		if err := record(result); err != nil {
			return err
		}
	}
	return nil
}

// worktreePath derives the only checkout the publisher may write.
func (p *Publisher) worktreePath(batch Batch) (string, error) {
	root := p.WorktreeRoot
	if root == "" {
		root = filepath.Join(filepath.Dir(p.RepositoryPath), "."+filepath.Base(p.RepositoryPath)+"-factory-specifications")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(p.RepositoryPath, root)
	if err != nil || relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("specification worktrees must be outside the ordinary checkout")
	}
	resolvedRepository, err := filepath.EvalSymlinks(p.RepositoryPath)
	if err != nil {
		return "", err
	}
	ancestor := root
	var missing []string
	for {
		if _, err := os.Lstat(ancestor); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		missing = append(missing, filepath.Base(ancestor))
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", errors.New("cannot resolve specification storage")
		}
		ancestor = parent
	}
	resolvedRoot, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		resolvedRoot = filepath.Join(resolvedRoot, missing[i])
	}
	relative, err = filepath.Rel(resolvedRepository, resolvedRoot)
	if err != nil || relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("specification storage resolves inside the ordinary checkout")
	}
	return filepath.Join(root, strings.TrimPrefix(batch.Branch, "factory/specifications/")), nil
}

// inspectWorktree refuses foreign/moved/symlinked worktrees before resumption.
func (p *Publisher) inspectWorktree(ctx context.Context, batch Batch) error {
	worktree, err := p.worktreePath(batch)
	if err != nil {
		return err
	}
	info, err := os.Lstat(worktree)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("specification worktree is not a regular directory")
	}
	out, err := p.git(ctx, worktree, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	actual, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	if err != nil {
		return err
	}
	expected, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return err
	}
	if actual != expected {
		return errors.New("foreign specification worktree")
	}
	common, err := p.git(ctx, worktree, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	original, err := p.git(ctx, p.RepositoryPath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	commonPath, err := filepath.EvalSymlinks(strings.TrimSpace(string(common)))
	if err != nil {
		return err
	}
	originalPath, err := filepath.EvalSymlinks(strings.TrimSpace(string(original)))
	if err != nil {
		return err
	}
	if commonPath != originalPath {
		return errors.New("specification checkout belongs to another repository")
	}
	out, err = p.git(ctx, worktree, "symbolic-ref", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) != "refs/heads/"+batch.Branch {
		return errors.New("specification worktree moved to another ref")
	}
	return nil
}

// createCommit resumes bounded approved writes in the factory-owned checkout.
func (p *Publisher) createCommit(ctx context.Context, batch Batch, exists bool) error {
	worktree, err := p.worktreePath(batch)
	if err != nil {
		return err
	}
	if !exists {
		if _, err := os.Lstat(worktree); !errors.Is(err, os.ErrNotExist) {
			return errors.New("specification worktree destination already exists or is inaccessible")
		}
		if err := os.MkdirAll(filepath.Dir(worktree), 0700); err != nil {
			return err
		}
		if _, err := p.git(ctx, p.RepositoryPath, "worktree", "add", "-b", batch.Branch, worktree, batch.BaseSHA); err != nil {
			return err
		}
	}
	if err := p.inspectWorktree(ctx, batch); err != nil {
		return err
	}
	status, err := p.git(ctx, worktree, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return err
	}
	for _, entry := range bytes.Split(status, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		if len(entry) < 4 {
			return errors.New("invalid publication worktree status")
		}
		if _, allowed := batch.Files[string(entry[3:])]; !allowed {
			return errors.New("specification worktree contains unrelated changes; reconcile before retry")
		}
	}
	for _, directory := range []string{"docs", "docs/issues"} {
		path := filepath.Join(worktree, filepath.FromSlash(directory))
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(path, 0755); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe specification directory")
		}
	}
	paths := make([]string, 0, len(batch.Files))
	for path, data := range batch.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		full := filepath.Join(worktree, filepath.FromSlash(path))
		if info, err := os.Lstat(full); err == nil && !info.Mode().IsRegular() {
			return errors.New("unsafe specification destination")
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.WriteFile(full, data, 0644); err != nil {
			return err
		}
		paths = append(paths, path)
	}
	slices.Sort(paths)
	args := append([]string{"add", "--"}, paths...)
	if _, err := p.git(ctx, worktree, args...); err != nil {
		return err
	}
	staged, err := p.git(ctx, worktree, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return err
	}
	expected := strings.Join(paths, "\x00") + "\x00"
	if string(staged) != expected {
		return errors.New("specification index contains other changes or incomplete approved files")
	}
	indexed, err := p.git(ctx, worktree, append([]string{"ls-files", "--stage", "-z", "--"}, paths...)...)
	if err != nil {
		return err
	}
	for _, entry := range bytes.Split(indexed, []byte{0}) {
		if len(entry) > 0 && !bytes.HasPrefix(entry, []byte("100644 ")) {
			return errors.New("Git index changed an approved Markdown file mode")
		}
	}
	for _, path := range paths {
		data, err := p.git(ctx, worktree, "show", ":"+path)
		if err != nil {
			return err
		}
		if !bytes.Equal(data, batch.Files[path]) {
			return errors.New("Git filters changed approved specification bytes; adjust host policy and approve again")
		}
	}
	_, err = p.git(ctx, worktree, "commit", "-m", "Publish accepted Markdown specifications\n\n"+batch.Marker)
	return err
}

// verifyCommit accepts only the exact dedicated approved child of its base.
func (p *Publisher) verifyCommit(ctx context.Context, batch Batch, commit string) error {
	out, err := p.git(ctx, p.RepositoryPath, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return err
	}
	parents := strings.Fields(string(out))
	if len(parents) != 2 || parents[1] != batch.BaseSHA {
		return errors.New("specification branch moved or contains unrelated commits")
	}
	out, err = p.git(ctx, p.RepositoryPath, "show", "-s", "--format=%B", commit)
	if err != nil {
		return err
	}
	if !strings.Contains(string(out), batch.Marker) {
		return errors.New("specification commit has no matching publication correlation")
	}
	out, err = p.git(ctx, p.RepositoryPath, "diff-tree", "--no-commit-id", "--name-only", "-r", "-z", commit)
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(batch.Files))
	for path := range batch.Files {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	if string(out) != strings.Join(paths, "\x00")+"\x00" {
		return errors.New("specification commit includes unrelated or incomplete changes")
	}
	for path, expected := range batch.Files {
		actual, err := p.git(ctx, p.RepositoryPath, "show", commit+":"+path)
		if err != nil {
			return err
		}
		if !bytes.Equal(actual, expected) {
			return errors.New("specification commit differs from approved bytes")
		}
	}
	graph, err := p.graphAt(ctx, commit)
	if err != nil {
		return err
	}
	if graph.Digest != batch.GraphDigest {
		return errors.New("specification commit graph differs from approval")
	}
	return nil
}

// remoteRef observes the exact approved destination without updating refs.
func (p *Publisher) remoteRef(ctx context.Context, branch string) (string, error) {
	out, err := p.git(ctx, p.RepositoryPath, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", nil
	}
	if len(fields) != 2 || fields[1] != "refs/heads/"+branch {
		return "", errors.New("ambiguous remote specification ref")
	}
	return fields[0], nil
}

// ObservePlanTarget fetches once and compares exact complete graph identity at
// that immutable snapshot. Identical squash/cherry-pick content preserves it.
func (p *Publisher) ObservePlanTarget(ctx context.Context, _ tracker.Repository, preview tracker.PlanPreview) (tracker.PlanTargetVisibility, error) {
	batch, err := p.approvedBatch(ctx, preview)
	if err != nil {
		return tracker.PlanTargetVisibility{}, err
	}
	revision, err := gitadapter.FetchTargetSnapshot(ctx, p.Runner, p.RepositoryPath, batch.TargetBranch)
	if err != nil {
		return tracker.PlanTargetVisibility{}, err
	}
	graph, err := p.graphAt(ctx, revision)
	if err != nil {
		return tracker.PlanTargetVisibility{Revision: revision}, err
	}
	result := tracker.PlanTargetVisibility{Revision: revision, Visible: graph.Digest == batch.GraphDigest}
	if !result.Visible {
		result.Reason = "waiting for specification merge"
	}
	return result, nil
}

// Per-item mutation is intentionally unsupported: only the batch may commit.
func (p *Publisher) CreatePlanItem(context.Context, tracker.Repository, tracker.RenderedPlanItem) (tracker.PlanItemRef, error) {
	return tracker.PlanItemRef{}, tracker.NewPlanError(tracker.PlanErrorUnsupportedCapability, "local publication requires an atomic Git batch")
}

// FindPlanItems cannot substitute for an exact batch lookup.
func (p *Publisher) FindPlanItems(context.Context, tracker.Repository, string) ([]tracker.PlanItemRef, error) {
	return nil, tracker.NewPlanError(tracker.PlanErrorUnsupportedCapability, "local publication requires batch reconciliation")
}

// CreatePlanRelationship refuses a second mutable relationship representation.
func (p *Publisher) CreatePlanRelationship(context.Context, tracker.Repository, tracker.PlanRelationship) error {
	return tracker.NewPlanError(tracker.PlanErrorUnsupportedCapability, "local relationships are committed in the atomic Markdown batch")
}

// FindPlanRelationship refuses independent item lookups outside the batch.
func (p *Publisher) FindPlanRelationship(context.Context, tracker.Repository, tracker.PlanRelationship) (bool, error) {
	return false, tracker.NewPlanError(tracker.PlanErrorUnsupportedCapability, "local relationships require batch verification")
}

var _ tracker.PlanPublisher = (*Publisher)(nil)
var _ tracker.PlanBatchPublisher = (*Publisher)(nil)
var _ tracker.PlanPreparer = (*Publisher)(nil)
