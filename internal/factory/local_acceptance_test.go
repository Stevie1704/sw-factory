package factory_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github/githubtest"
	"github.com/Stevie1704/sw-factory/internal/localpublication"
	"github.com/Stevie1704/sw-factory/internal/localtracker"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// The integrated acceptance scenarios of spec #291 (issue #298). Each journey
// drives one operator through the shipped seams: the web UI handler and the
// durable command inbox, session roles whose reports are fixtures, the real
// local Markdown publisher and tracker over a temporary Git repository with a
// bare origin, the coordinator's queue and lifecycle passes, and a scripted
// code host. Human Git steps (merge of the specification branch, merge of the
// implementation) are real Git operations. These tests are deterministic
// evidence; they are never evidence of a live provider or harness.

// localJourneyInstallation is the private local operator registration.
const localJourneyInstallation = "install-00112233445566778899aabbccddeeff"

// localJourneyHost is the scripted code host. Its issue methods fail the
// journey, because a local tracker must make no remote issue call.
type localJourneyHost struct {
	*schedulingHost
	t *testing.T
}

// Issue fails: the local tracker owns issue reads.
func (h *localJourneyHost) Issue(context.Context, tracker.Repository, int) (tracker.Issue, error) {
	h.t.Error("the code host received a remote issue read")
	return tracker.Issue{}, nil
}

// ReplaceIssueLabels fails: local labels are store projections.
func (h *localJourneyHost) ReplaceIssueLabels(context.Context, tracker.Repository, int, []string) error {
	h.t.Error("the code host received a remote issue label write")
	return nil
}

// CreateIssueComment fails: local comments are store projections.
func (h *localJourneyHost) CreateIssueComment(context.Context, tracker.Repository, int, string) (tracker.Comment, error) {
	h.t.Error("the code host received a remote issue comment")
	return tracker.Comment{}, nil
}

// EditIssueComment fails: local comments are store projections.
func (h *localJourneyHost) EditIssueComment(context.Context, tracker.Repository, string, string) error {
	h.t.Error("the code host received a remote issue comment edit")
	return nil
}

// IssueComments fails: local operator input arrives through the inbox.
func (h *localJourneyHost) IssueComments(context.Context, tracker.Repository, int) ([]tracker.Comment, error) {
	h.t.Error("the code host received a remote issue comment read")
	return nil, nil
}

// localJourney is an ideation journey on a registration with the local
// tracker and an independently configured GitHub code host.
type localJourney struct {
	*ideationJourney
	codeHost *localJourneyHost
	manager  *gitadapter.LocalWorktreeManager
	// original is the target commit before any publication.
	original string
}

// newLocalJourney prepares a Git repository with one commit on main and a
// bare origin, registers it with the local tracker, and composes the
// coordinator and the UI over it.
func newLocalJourney(t *testing.T) *localJourney {
	t.Helper()
	fixture := newSessionFixture(t, config.HarnessCodex)
	repo := fixture.repo
	scheduleGit(t, repo, "checkout", "-q", "-b", "main")
	scheduleGit(t, repo, "config", "user.name", "Test")
	scheduleGit(t, repo, "config", "user.email", "test@example.test")
	scheduleGit(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("project\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scheduleGit(t, repo, "add", "README.md")
	scheduleGit(t, repo, "commit", "-q", "-m", "initial")
	origin := filepath.Join(filepath.Dir(repo), "origin.git")
	scheduleGit(t, filepath.Dir(repo), "init", "-q", "--bare", origin)
	scheduleGit(t, repo, "remote", "add", "origin", origin)
	scheduleGit(t, repo, "push", "-q", "origin", "main")

	registration := config.RepositoryRegistration{
		Path: repo, OperationalDataPath: fixture.path, RepositoryConfigPath: filepath.Join(repo, config.RepositoryConfigFileName),
		Tracker: config.ProviderLocal, CodeHost: config.ProviderGitHub,
		GitHub:        config.GitHubConfig{Owner: githubtest.Owner, Repository: githubtest.Name},
		LocalOperator: config.LocalOperatorConfig{InstallationID: localJourneyInstallation, Name: "operator"},
	}
	journey := &localJourney{
		codeHost: &localJourneyHost{schedulingHost: &schedulingHost{commandGitHub: &commandGitHub{}}, t: t},
		manager:  &gitadapter.LocalWorktreeManager{WorktreeDir: filepath.Join(filepath.Dir(repo), "worktrees")},
		original: scheduleGit(t, repo, "rev-parse", "main"),
	}
	journey.ideationJourney = &ideationJourney{
		sessionFixture: fixture,
		provider:       journeyProvider{name: "local"},
		host:           config.HostConfig{SchemaVersion: config.CurrentHostSchemaVersion, Repositories: []config.RepositoryRegistration{registration}},
		compose:        journey.compose,
	}
	journey.restartCoordinator()
	journey.reconnectBrowser()
	return journey
}

// compose replaces the remote tracker and the fixed snapshots of the
// ideation journey with the production local adapters, as composition of a
// local registration does. Each restart composes fresh adapters, so only Git
// and the store carry state across it.
func (j *localJourney) compose(dependencies *factory.Dependencies) {
	registration := j.ideationJourney.host.Repositories[0]
	source := localtracker.NewClient(registration, "main")
	dependencies.Tracker = localpublication.Wrap(source, localpublication.NewPublisher(registration, "main", j.codeHost))
	dependencies.Worktree = j.manager
	dependencies.GitWorkspace = j.manager
	dependencies.PullRequests = j.codeHost
	dependencies.CommitStatuses = j.codeHost
	dependencies.PullRequestReviews = j.codeHost
	dependencies.PullRequestComments = j.codeHost
	dependencies.PullRequestCommentPublisher = j.codeHost
	dependencies.CodeHostReadiness = j.codeHost
}

// localPlanItem is one previewed local item as the plan page shows it.
type localPlanItem struct {
	NodeID string            `json:"node_id"`
	Type   string            `json:"type"`
	Title  string            `json:"title"`
	Body   string            `json:"body"`
	Fields map[string]string `json:"fields"`
}

// reachLocalPlan runs the discussion to a planning draft: the idea with one
// question round, the answer, the handoff of the ready brief, and the draft.
func (j *localJourney) reachLocalPlan(draft report.Report) {
	j.t.Helper()
	j.startSession()
	j.apply("turn-answer", sessioncommand.KindSubmitTurn, sessioncommand.TurnPayload{Body: "Support engineers read it."})
	j.respond(readyRound())
	j.apply("handoff-1", sessioncommand.KindHandOff, sessioncommand.HandOffPayload{BriefRevision: j.journeySession().BriefRevision})
	j.respond(draft)
}

// localPreview previews the current plan revision through the UI and returns
// the approval digests and the exact previewed items by node.
func (j *localJourney) localPreview(requestID string) (sessioncommand.ApprovePayload, map[string]localPlanItem) {
	j.t.Helper()
	revision := j.journeySession().PlanRevision
	answer := j.apply(requestID, sessioncommand.KindPreviewPlan, sessioncommand.PlanRevisionPayload{PlanRevision: revision})
	var preview sessioncommand.PreviewResult
	if err := json.Unmarshal(answer.Result, &preview); err != nil {
		j.t.Fatalf("preview result %s: %v", answer.Result, err)
	}
	var payload struct {
		Items []localPlanItem `json:"items"`
	}
	if err := json.Unmarshal(preview.Payload, &payload); err != nil {
		j.t.Fatalf("preview payload %s: %v", preview.Payload, err)
	}
	items := map[string]localPlanItem{}
	for _, item := range payload.Items {
		items[item.NodeID] = item
	}
	return sessioncommand.ApprovePayload{
		PlanRevision: revision, PayloadSHA256: preview.PayloadSHA256, RelationshipMappingSHA256: preview.RelationshipMappingSHA256,
	}, items
}

// approveStart sends Approve and start for the selected issues as the plan
// page does, double-clicks it, and lets the coordinator apply it. It returns
// a function that resends the identical request, as a reloaded tab does.
func (j *localJourney) approveStart(requestID string, approval sessioncommand.ApprovePayload, selected ...string) (resend func() (int, commandAnswer)) {
	j.t.Helper()
	payload := sessioncommand.ApproveStartPayload{ApprovePayload: approval, SelectedIssueIDs: selected}
	shown := j.revision()
	if status, answer := j.post(requestID, sessioncommand.KindApproveStart, shown, payload); status != http.StatusAccepted {
		j.t.Fatalf("approve and start = %d %#v, want accepted", status, answer)
	}
	if status, replay := j.post(requestID, sessioncommand.KindApproveStart, shown, payload); status != http.StatusOK || replay.Replayed == nil || !*replay.Replayed {
		j.t.Fatalf("double-clicked approve and start = %d %#v, want the stored command", status, replay)
	}
	j.coordinate()
	if answer := j.command(requestID); answer.Outcome != string(store.IdeationCommandApplied) {
		j.t.Fatalf("approve and start outcome %q (%s), want applied", answer.Outcome, answer.Detail)
	}
	return func() (int, commandAnswer) { return j.post(requestID, sessioncommand.KindApproveStart, shown, payload) }
}

// git runs one Git command in the operator's ordinary checkout.
func (j *localJourney) git(args ...string) string {
	j.t.Helper()
	return scheduleGit(j.t, j.repo, args...)
}

// dirtyCheckout gives the operator's checkout staged, unstaged, and
// untracked changes that publication must not touch.
func (j *localJourney) dirtyCheckout() {
	j.t.Helper()
	for name, content := range map[string]string{"README.md": "project\nlocal edit\n", "staged.txt": "staged\n", "notes.txt": "untracked\n"} {
		if err := os.WriteFile(filepath.Join(j.repo, name), []byte(content), 0o644); err != nil {
			j.t.Fatal(err)
		}
	}
	j.git("add", "staged.txt")
}

// requireCheckoutUntouched verifies that the operator's checkout is still on
// main at the original commit with its own changes.
func (j *localJourney) requireCheckoutUntouched() {
	j.t.Helper()
	if branch := j.git("branch", "--show-current"); branch != "main" {
		j.t.Fatalf("operator checkout is on %q, want main", branch)
	}
	if head := j.git("rev-parse", "HEAD"); head != j.original {
		j.t.Fatalf("operator checkout HEAD = %s, want %s", head, j.original)
	}
	status := j.git("status", "--porcelain")
	for _, want := range []string{"M README.md", "A  staged.txt", "?? notes.txt"} {
		if !strings.Contains(status, want) {
			j.t.Fatalf("operator checkout status lost %q:\n%s", want, status)
		}
	}
}

// requireSpecificationCommit verifies that the specification branch has one
// commit over the original target that adds exactly the previewed files with
// their previewed bytes and no conversation text.
func (j *localJourney) requireSpecificationCommit(branch string, items ...localPlanItem) {
	j.t.Helper()
	if count := j.git("rev-list", "--count", j.original+".."+branch); count != "1" {
		j.t.Fatalf("specification branch has %s commits, want one", count)
	}
	want := []string{}
	for _, item := range items {
		path := item.Fields["path"]
		want = append(want, path)
		content := j.git("show", branch+":"+path)
		if content+"\n" != item.Body {
			j.t.Fatalf("committed %s differs from the preview:\n%s", path, content)
		}
		if strings.Contains(content, journeySecret) {
			j.t.Fatalf("committed %s contains conversation text", path)
		}
	}
	slices.Sort(want)
	if changed := strings.Fields(j.git("diff", "--name-only", j.original, branch)); !slices.Equal(changed, want) {
		j.t.Fatalf("specification commit changes %v, want exactly %v", changed, want)
	}
}

// humanMergesSpecification is the manual publishing path of a local commit:
// the operator pushes the displayed branch, and a reviewer squash-merges it
// into main in another clone. It returns the new target commit.
func (j *localJourney) humanMergesSpecification(branch string) string {
	j.t.Helper()
	j.git("push", "-q", "origin", branch)
	return j.humanCommit(func(clone string) {
		scheduleGit(j.t, clone, "fetch", "-q", "origin", branch)
		scheduleGit(j.t, clone, "merge", "-q", "--squash", "FETCH_HEAD")
	}, "squash specification")
}

// humanMergesImplementation is the human merge of an implementation pull
// request into main. It returns the merge commit.
func (j *localJourney) humanMergesImplementation(name string) string {
	j.t.Helper()
	return j.humanCommit(func(clone string) {
		if err := os.WriteFile(filepath.Join(clone, name), []byte(name+"\n"), 0o644); err != nil {
			j.t.Fatal(err)
		}
		scheduleGit(j.t, clone, "add", name)
	}, "merge implementation "+name)
}

// humanCommit applies change in a reviewer's clone of origin, commits it to
// main, pushes it, and returns the commit.
func (j *localJourney) humanCommit(change func(clone string), message string) string {
	j.t.Helper()
	clone := filepath.Join(filepath.Dir(j.repo), "reviewer")
	if _, err := os.Stat(clone); err != nil {
		scheduleGit(j.t, filepath.Dir(j.repo), "clone", "-q", "-b", "main", filepath.Join(filepath.Dir(j.repo), "origin.git"), clone)
	}
	scheduleGit(j.t, clone, "pull", "-q", "--ff-only", "origin", "main")
	change(clone)
	scheduleGit(j.t, clone, "-c", "commit.gpgsign=false", "commit", "-q", "-m", message)
	scheduleGit(j.t, clone, "push", "-q", "origin", "main")
	return scheduleGit(j.t, clone, "rev-parse", "HEAD")
}

// poll runs one coordinator queue pass and requires outcome.
func (j *localJourney) poll(want factory.PollOutcome) factory.PollResult {
	j.t.Helper()
	pass, err := j.service.PollOnce(j.t.Context())
	if err != nil || pass.Outcome != want {
		j.t.Fatalf("PollOnce() = %#v, %v; want %s", pass, err, want)
	}
	return pass
}

// backlogItem returns the backlog projection of one stable issue identity.
func (j *localJourney) backlogItem(id string) store.LocalBacklogItem {
	j.t.Helper()
	view, err := j.service.LocalBacklog(j.t.Context())
	if err != nil {
		j.t.Fatal(err)
	}
	for _, item := range view.Projection.Items {
		if item.Issue.ID == id {
			return item
		}
	}
	j.t.Fatalf("backlog %#v lacks %s", view.Projection, id)
	return store.LocalBacklogItem{}
}

// reviewReady records the normal review-ready outcome of a Run with an open
// implementation pull request on the code host.
func (j *localJourney) reviewReady(run store.Run, number int) store.Run {
	j.t.Helper()
	run.Stage = store.StageReady
	run.PullRequestNumber = number
	run.PullRequestURL = "https://codehost.test/pull/" + strconv.Itoa(number)
	if err := j.store.SaveRun(j.t.Context(), run); err != nil {
		j.t.Fatal(err)
	}
	j.codeHost.pullRequest = codehost.PullRequest{Number: number, State: "open", HeadBranch: run.Branch, HeadSHA: run.CheckpointSHA, BaseBranch: "main"}
	return run
}

// mergeImplementation records the human merge of the Run's pull request on
// the code host and lets the normal lifecycle complete the Run.
func (j *localJourney) mergeImplementation(run store.Run, merged string) {
	j.t.Helper()
	j.codeHost.pullRequest.Merged = true
	j.codeHost.pullRequest.State = "closed"
	j.codeHost.pullRequest.MergeCommitSHA = merged
	result, err := j.service.PollLifecycle(j.t.Context(), factory.LifecycleRequest{RunID: run.ID})
	if err != nil || result.Outcome != factory.LifecycleCompleted {
		j.t.Fatalf("PollLifecycle() = %#v, %v; want the merged Run completed", result, err)
	}
}

// TestLocalBacklogJourneyStartsOneIssueAfterTheHumanSpecificationMerge
// covers the single-issue path: discussion, exact preview, one Approve and
// start, one specification commit that leaves the operator's checkout alone,
// a visible wait for the specification merge across a restart, the claim at
// the human squash commit, review-ready, and the human implementation merge.
// No remote issue call happens, and replays never add a commit or a Run.
func TestLocalBacklogJourneyStartsOneIssueAfterTheHumanSpecificationMerge(t *testing.T) {
	journey := newLocalJourney(t)
	journey.reachLocalPlan(planReportForTest(journeyNode("n-1", "task", "Add the factory export command", "")))
	approval, items := journey.localPreview("preview-1")
	task := items["n-1"]
	id, branch := task.Fields["local_id"], task.Fields["specification_branch"]
	if task.Fields["publication_policy"] != "local_commit" || branch == "" || id == "" {
		t.Fatalf("preview fields = %#v, want a local commit on a specification branch", task.Fields)
	}

	journey.dirtyCheckout()
	resend := journey.approveStart("approve-start-1", approval, id)
	journey.requireReceipt(store.PublicationOutcomePublished)
	journey.requireSpecificationCommit(branch, task)
	journey.requireCheckoutUntouched()

	journey.poll(factory.PollNoWork)
	if item := journey.backlogItem(id); item.State != "waiting_for_specification_merge" || item.AuthorizationID == "" {
		t.Fatalf("backlog item = %#v, want an authorized wait for the specification merge", item)
	}
	journey.restartCoordinator()
	journey.reconnectBrowser()
	if status, replay := resend(); status != http.StatusOK || replay.Outcome != string(store.IdeationCommandApplied) {
		t.Fatalf("approve and start resent after a restart = %d %#v, want the stored outcome", status, replay)
	}
	journey.coordinate()
	journey.poll(factory.PollNoWork)
	journey.requireSpecificationCommit(branch, task)

	target := journey.humanMergesSpecification(branch)
	claimed := journey.poll(factory.PollClaimed)
	if claimed.Run.BaseCheckpointSHA != target || journey.backlogItem(id).RunID != claimed.Run.ID {
		t.Fatalf("claim = %#v, want the Run frozen at the squash commit %s", claimed, target)
	}
	journey.restartCoordinator()
	ready := journey.reviewReady(claimed.Run, 91)
	journey.poll(factory.PollActiveRun)
	if item := journey.backlogItem(id); item.RunID != ready.ID {
		t.Fatalf("backlog item = %#v, want it linked to Run %s", item, ready.ID)
	}
	journey.mergeImplementation(ready, journey.humanMergesImplementation("export.txt"))
	journey.poll(factory.PollNoWork)
	claims, err := journey.store.LocalClaims(t.Context(), journey.repo)
	if err != nil || len(claims) != 1 {
		t.Fatalf("local claims = %#v, %v; want exactly one Run for the approved revision", claims, err)
	}
	journey.requireCheckoutUntouched()
}
