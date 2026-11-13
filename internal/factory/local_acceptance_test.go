package factory_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
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
	"github.com/Stevie1704/sw-factory/internal/webui"
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

// reconnectBrowser starts a new UI handler with the local issue pages and
// reads its command token, as a reloaded tab of a restarted factory ui does.
func (j *localJourney) reconnectBrowser() {
	j.t.Helper()
	j.ui = webui.NewHandler(j.service, webui.Options{Sessions: j.service, Local: j.service})
	page := j.get("/issues")
	match := journeyCSRF.FindStringSubmatch(page.Body.String())
	if match == nil {
		j.t.Fatalf("local issue page has no command token:\n%s", page.Body.String())
	}
	j.token = match[1]
}

// restart replaces the coordinator and the browser, as a process restart
// and a reloaded tab do. Only Git and the store survive it.
func (j *localJourney) restart() {
	j.t.Helper()
	j.restartCoordinator()
	j.reconnectBrowser()
}

// postScoped sends one backlog or Run command as the local issue and Run
// pages do, with its own subject instead of a session.
func (j *localJourney) postScoped(requestID, scope, subject string, kind sessioncommand.Kind, revision int64, payload any) (int, commandAnswer) {
	j.t.Helper()
	body, err := json.Marshal(map[string]any{
		"request_id": requestID, "scope": scope, "subject_id": subject, "kind": kind, "expected_revision": revision, "payload": payload,
	})
	if err != nil {
		j.t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/commands", strings.NewReader(string(body)))
	request.Host = "127.0.0.1:8765"
	request.Header.Set("Origin", "http://127.0.0.1:8765")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Factory-CSRF", j.token)
	recorder := httptest.NewRecorder()
	j.ui.ServeHTTP(recorder, request)
	var answer commandAnswer
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		j.t.Fatalf("command answer %q: %v", recorder.Body.String(), err)
	}
	return recorder.Code, answer
}

// applyScoped submits one new scoped command, lets the coordinator apply it,
// and requires the applied outcome.
func (j *localJourney) applyScoped(requestID, scope, subject string, kind sessioncommand.Kind, revision int64, payload any) commandAnswer {
	j.t.Helper()
	if status, answer := j.postScoped(requestID, scope, subject, kind, revision, payload); status != http.StatusAccepted {
		j.t.Fatalf("%s %s: status %d, answer %#v; want accepted", kind, requestID, status, answer)
	}
	j.coordinate()
	answer := j.command(requestID)
	if answer.Outcome != string(store.IdeationCommandApplied) {
		j.t.Fatalf("%s %s: outcome %q (%s), want applied", kind, requestID, answer.Outcome, answer.Detail)
	}
	return answer
}

// pageState returns the disposition that the local issue list shows for
// one stable issue identity.
func (j *localJourney) pageState(id string) string {
	j.t.Helper()
	page := j.get("/issues").Body.String()
	match := regexp.MustCompile(`data-local-issue="` + id + `" data-state="([a-z_]+)"`).FindStringSubmatch(page)
	if match == nil {
		j.t.Fatalf("local issue page lacks %s:\n%s", id, page)
	}
	return match[1]
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
	if state := journey.pageState(id); state != "waiting_for_specification_merge" {
		t.Fatalf("local issue page shows %q, want the specification merge wait", state)
	}
	journey.restart()
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
	journey.restart()
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

// featureWithPDFDraft is a planning result with a parent and three tasks; the
// documentation depends on the export and on a PDF export.
func featureWithPDFDraft() report.Report {
	return planReportForTest(
		journeyNode("n-1", "parent", "Run history export", ""),
		journeyNode("n-2", "task", "Add the export command", "n-1"),
		journeyNode("n-3", "task", "Document the export", "n-1", "n-2", "n-4"),
		journeyNode("n-4", "task", "Export to PDF", "n-1", "n-2"),
	)
}

// TestLocalBacklogJourneyRunsAParentsTasksInDependencyOrder covers a parent
// with dependent tasks: a change request and a revised draft, a decline that
// must repair the dependency on the declined task, one specification commit
// for the whole batch, a restart after every durable transition, and claims
// in dependency order. Review-ready does not release the dependent; only the
// human merge of the prerequisite on the fetched target does. The parent
// never starts a Run.
func TestLocalBacklogJourneyRunsAParentsTasksInDependencyOrder(t *testing.T) {
	journey := newLocalJourney(t)
	journey.reachLocalPlan(featureWithPDFDraft())

	journey.apply("revise-1", sessioncommand.KindRegenerateDraft, sessioncommand.PlanRevisionPayload{PlanRevision: journey.journeySession().PlanRevision})
	revised := featureWithPDFDraft()
	revised.PlanDraft.Nodes[1].Title = "Add the factory export command"
	journey.respond(revised)
	nodes := sessioncommand.PlanNodesFromStore(journey.history().Plans[len(journey.history().Plans)-1].Nodes)
	if nodes[1].Title != "Add the factory export command" {
		t.Fatalf("revised draft = %#v, want the revised title", nodes)
	}
	declined := slices.DeleteFunc(slices.Clone(nodes), func(node sessioncommand.PlanNodeJSON) bool { return node.ID == "n-4" })
	if answer := journey.submit("decline-dangling", sessioncommand.KindEditDraft, sessioncommand.DraftPayload{Nodes: declined}); answer.Outcome == string(store.IdeationCommandApplied) {
		t.Fatalf("a decline that leaves a dangling dependency was applied: %#v", answer)
	}
	declined[2].DependsOn = []string{"n-2"}
	journey.apply("decline-pdf", sessioncommand.KindEditDraft, sessioncommand.DraftPayload{Nodes: declined})
	journey.restart()

	approval, items := journey.localPreview("preview-1")
	if len(items) != 3 || items["n-1"].Type != "parent" {
		t.Fatalf("preview items = %#v, want the parent and two tasks without the declined task", items)
	}
	parent, export, docs := items["n-1"].Fields["local_id"], items["n-2"].Fields["local_id"], items["n-3"].Fields["local_id"]
	branch := items["n-2"].Fields["specification_branch"]
	journey.approveStart("approve-start-1", approval, export, docs)
	journey.restart()
	journey.requireReceipt(store.PublicationOutcomePublished)
	journey.requireSpecificationCommit(branch, items["n-1"], items["n-2"], items["n-3"])
	journey.poll(factory.PollNoWork)

	journey.humanMergesSpecification(branch)
	journey.restart()
	first := journey.poll(factory.PollClaimed)
	if item := journey.backlogItem(export); item.RunID != first.Run.ID {
		t.Fatalf("first claim = %#v, want the export task before its dependent", item)
	}
	if state := journey.pageState(docs); state != "blocked_by_dependencies" {
		t.Fatalf("dependent shows %q, want blocked_by_dependencies", state)
	}

	journey.restart()
	ready := journey.reviewReady(first.Run, 91)
	journey.restart()
	journey.poll(factory.PollActiveRun)
	if state := journey.pageState(docs); state != "blocked_by_dependencies" {
		t.Fatalf("dependent shows %q while its prerequisite is only review-ready", state)
	}

	merged := journey.humanMergesImplementation("export.txt")
	journey.restart()
	journey.mergeImplementation(ready, merged)
	journey.restart()
	second := journey.poll(factory.PollClaimed)
	if second.Run.BaseCheckpointSHA != merged || journey.backlogItem(docs).RunID != second.Run.ID {
		t.Fatalf("second claim = %#v, want the dependent frozen at the merge %s", second.Run, merged)
	}

	journey.restart()
	journey.mergeImplementation(journey.reviewReady(second.Run, 92), journey.humanMergesImplementation("docs.txt"))
	journey.restart()
	journey.poll(factory.PollNoWork)
	if item := journey.backlogItem(parent); item.RunID != "" {
		t.Fatalf("parent = %#v, want a grouping item without a Run", item)
	}
	claims, err := journey.store.LocalClaims(t.Context(), journey.repo)
	if err != nil || len(claims) != 2 {
		t.Fatalf("local claims = %#v, %v; want one Run per authorized task", claims, err)
	}
}

// TestLocalBacklogJourneyAuthorizesCommittedWorkAfterTheSessionEnded covers
// commit-only acceptance: publication starts nothing, the published session
// is terminal, and a later exact execution preview and approval on the local
// issue page start the work. A replayed approval adds no Run, and the Run
// page's cancellation uses the ordinary Run semantics.
func TestLocalBacklogJourneyAuthorizesCommittedWorkAfterTheSessionEnded(t *testing.T) {
	journey := newLocalJourney(t)
	journey.reachLocalPlan(planReportForTest(journeyNode("n-1", "task", "Add the factory export command", "")))
	approval, items := journey.localPreview("preview-1")
	id, branch := items["n-1"].Fields["local_id"], items["n-1"].Fields["specification_branch"]
	journey.apply("approve-1", sessioncommand.KindApprove, approval)
	if session := journey.journeySession(); session.State != store.IdeationStatePublished {
		t.Fatalf("session = %s, want published", session.State)
	}
	journey.humanMergesSpecification(branch)
	journey.restart()
	journey.poll(factory.PollNoWork)
	if state := journey.pageState(id); state != "missing_execution_approval" {
		t.Fatalf("committed-only issue shows %q, want missing_execution_approval", state)
	}

	journey.applyScoped("execution-preview-1", "backlog", "repository", sessioncommand.KindPreviewExecution, 0, sessioncommand.ExecutionPreviewPayload{SelectedIssueIDs: []string{id}})
	view, err := journey.service.LocalExecution(t.Context())
	if err != nil || view.Preview == nil {
		t.Fatalf("execution preview = %#v, %v", view, err)
	}
	preview := *view.Preview
	journey.applyScoped("authorize-1", "backlog", "repository", sessioncommand.KindAuthorizeExecution, preview.Revision, authorizePreview(preview))
	if status, replay := journey.postScoped("authorize-1", "backlog", "repository", sessioncommand.KindAuthorizeExecution, preview.Revision, authorizePreview(preview)); status != http.StatusOK || replay.Replayed == nil || !*replay.Replayed {
		t.Fatalf("replayed authorization = %d %#v, want the stored command", status, replay)
	}
	journey.restart()
	run := journey.poll(factory.PollClaimed).Run

	controls, err := journey.service.LocalRunOperator(t.Context(), run.ID)
	if err != nil || !slices.Contains(controls.Actions, string(sessioncommand.KindCancelRun)) {
		t.Fatalf("Run controls = %#v, %v; want cancellation offered", controls, err)
	}
	journey.applyScoped("cancel-1", "run", run.ID, sessioncommand.KindCancelRun, controls.Revision, sessioncommand.RunOperatorPayload{PacketVersion: 1})
	journey.restart()
	journey.poll(factory.PollNoWork)
	claims, err := journey.store.LocalClaims(t.Context(), journey.repo)
	if err != nil || len(claims) != 1 {
		t.Fatalf("local claims = %#v, %v; want the cancelled Run only", claims, err)
	}
}
