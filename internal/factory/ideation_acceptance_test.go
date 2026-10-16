package factory_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/azuredevops"
	"github.com/Stevie1704/sw-factory/internal/azuredevops/azuredevopstest"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/github/githubtest"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/webui"
)

// The integrated acceptance scenarios of spec #265. Each scenario drives one
// operator journey through the shipped seams: the web UI handler, the
// durable command inbox, one coordinator inbox pass per step, session roles
// whose reports are deterministic fixtures, and the real GitHub or Azure
// DevOps planning adapter over a scripted gh or az CLI. Checks read
// structured state only. These tests are deterministic evidence; they are
// never evidence of a live provider.

// journeySessionID is the session every journey creates.
const journeySessionID = "idea-1"

// journeySecret is user turn text that must never reach a tracker.
const journeySecret = "PRIVATE-IDEA-TEXT-7f3a"

// journeyNow is the fixed clock of every journey.
var journeyNow = time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)

// journeyCSRF matches the per-process command token that every UI page
// renders.
var journeyCSRF = regexp.MustCompile(`<meta name="factory-csrf-token" content="([0-9a-f]{64})">`)

// publishedGraph is the provider-neutral view of a scripted tracker.
type publishedGraph struct {
	// bodies maps each item title to its body or description.
	bodies map[string]string
	// parents maps each child title to its parent title.
	parents map[string]string
	// dependsOn maps each dependent title to its predecessor titles.
	dependsOn map[string][]string
	// labels lists every label or tag of every item.
	labels []string
	// creates counts item create requests, including failed ones.
	creates int
	// writes counts every write request.
	writes int
}

// journeyProvider is one tracker adapter over a scripted provider CLI.
type journeyProvider struct {
	name     string
	register func(*config.RepositoryRegistration)
	client   tracker.Client
	graph    func() publishedGraph
	// holdCreate makes the create of the titled item succeed remotely and
	// then hang until the coordinator stops.
	holdCreate func(title string)
	// changeMapping changes how the provider maps the approved plan.
	changeMapping func()
}

// githubJourney returns the GitHub adapter over a scripted gh with native
// sub-issues and blocking dependencies.
func githubJourney(t *testing.T) journeyProvider {
	gh := githubtest.New(t, true)
	return journeyProvider{
		name: "github",
		register: func(registration *config.RepositoryRegistration) {
			registration.GitHub = config.GitHubConfig{Owner: githubtest.Owner, Repository: githubtest.Name}
		},
		client:        &github.GhClient{Runner: gh},
		holdCreate:    func(title string) { gh.HoldAfterWrite("create:" + title) },
		changeMapping: func() { gh.SetNative(false) },
		graph: func() publishedGraph {
			graph := publishedGraph{bodies: map[string]string{}, parents: map[string]string{}, dependsOn: map[string][]string{}, labels: []string{}}
			titles := map[int]string{}
			for _, issue := range gh.Issues() {
				titles[issue.Number] = issue.Title
				graph.bodies[issue.Title] = issue.Body
				graph.labels = append(graph.labels, issue.Labels...)
			}
			for parent, children := range gh.SubIssues() {
				for _, child := range children {
					graph.parents[titles[child]] = titles[parent]
				}
			}
			for dependent, blockers := range gh.BlockedBy() {
				for _, blocker := range blockers {
					graph.dependsOn[titles[dependent]] = append(graph.dependsOn[titles[dependent]], titles[blocker])
				}
			}
			graph.creates, graph.writes = gh.Creates(), len(gh.Writes())
			return graph
		},
	}
}

// azureJourney returns the Azure DevOps adapter over a scripted Agile
// project with the default plan mapping.
func azureJourney(t *testing.T) journeyProvider {
	boards := azuredevopstest.NewBoards(t, azuredevopstest.AgileProcess())
	client := &azuredevops.Client{Runner: boards, Organization: azuredevopstest.Organization, AuthorizedUsers: []string{"alice@contoso.com"}}
	return journeyProvider{
		name: "azure-devops",
		register: func(registration *config.RepositoryRegistration) {
			registration.AzureDevOps = config.AzureDevOpsConfig{
				Organization: azuredevopstest.Organization, Project: azuredevopstest.Project, Repository: azuredevopstest.Repository,
			}
		},
		client:     client,
		holdCreate: boards.HoldAfterWrite,
		changeMapping: func() {
			mapping := config.AgileAzurePlanMapping()
			mapping.Levels[1].Required = true
			client.Planning = &mapping
		},
		graph: func() publishedGraph {
			graph := publishedGraph{bodies: map[string]string{}, parents: map[string]string{}, dependsOn: map[string][]string{}, labels: []string{}}
			items := azuredevopstest.Items(boards.Snapshot())
			for _, item := range items {
				title := item.Title()
				graph.bodies[title] = item.Description()
				graph.labels = append(graph.labels, item.Tags()...)
				for _, relation := range item.Relations {
					target := items.Title(relation.URL)
					switch relation.Rel {
					case "System.LinkTypes.Hierarchy-Reverse":
						graph.parents[title] = target
					case "System.LinkTypes.Dependency-Reverse":
						graph.dependsOn[title] = append(graph.dependsOn[title], target)
					}
				}
			}
			graph.creates, graph.writes = boards.Creates(), len(boards.Writes())
			return graph
		},
	}
}

// journeyProviders returns both publication adapters.
func journeyProviders(t *testing.T) []journeyProvider {
	return []journeyProvider{githubJourney(t), azureJourney(t)}
}

// ideationJourney is one operator journey: a browser on the UI handler, the
// coordinator that applies its commands, session roles that answer with
// report fixtures, and one tracker.
type ideationJourney struct {
	*sessionFixture
	provider journeyProvider
	host     config.HostConfig
	service  *factory.Service
	ui       http.Handler
	token    string
}

// newIdeationJourney composes the coordinator and the UI over a fresh store
// with a live supervisor heartbeat, as factory start and factory ui do.
func newIdeationJourney(t *testing.T, provider journeyProvider) *ideationJourney {
	t.Helper()
	fixture := newSessionFixture(t, config.HarnessCodex)
	registration := config.RepositoryRegistration{
		Path: fixture.repo, OperationalDataPath: fixture.path, RepositoryConfigPath: filepath.Join(fixture.repo, config.RepositoryConfigFileName),
	}
	provider.register(&registration)
	journey := &ideationJourney{
		sessionFixture: fixture, provider: provider,
		host: config.HostConfig{SchemaVersion: config.CurrentHostSchemaVersion, Repositories: []config.RepositoryRegistration{registration}},
	}
	journey.restartCoordinator()
	journey.reconnectBrowser()
	return journey
}

// restartCoordinator replaces the coordinator and reopens the fixture store,
// as a coordinator process restart does. Only durable state survives.
func (j *ideationJourney) restartCoordinator() {
	j.t.Helper()
	if j.service != nil {
		j.restart()
	}
	ids := 0
	j.service = factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:            &fakeConfig{value: j.host},
		OpenStore:         func(ctx context.Context, path string) (factory.OperationalStore, error) { return store.Open(ctx, path) },
		LoadRepository:    func(string) (config.RepositoryConfig, error) { return j.policy, nil },
		Worker:            j.worker,
		HeadlessHarnesses: harness.NewHeadlessAdapters(j.worker),
		Worktree:          snapshotWorktreeForTest{j.snapshots},
		Tracker:           j.provider.client,
		Now:               func() time.Time { return journeyNow },
		NewRunID: func() (string, error) {
			ids++
			return "journey-" + strconv.Itoa(len(j.worker.launches)) + "-" + strconv.Itoa(ids), nil
		},
	})
	if err := j.store.SaveSupervisorHeartbeat(j.t.Context(), store.SupervisorHeartbeat{
		Coordinator: "coordinator-journey", PID: 4242, StartedAt: journeyNow, RenewedAt: journeyNow, ExpiresAt: journeyNow.Add(time.Hour),
	}); err != nil {
		j.t.Fatal(err)
	}
}

// reconnectBrowser starts a new UI handler and reads its command token from
// a page, as a reloaded browser tab of a restarted factory ui does.
func (j *ideationJourney) reconnectBrowser() {
	j.t.Helper()
	j.ui = webui.NewHandler(j.service, webui.Options{Sessions: j.service})
	page := j.get("/sessions")
	match := journeyCSRF.FindStringSubmatch(page.Body.String())
	if match == nil {
		j.t.Fatalf("session page has no command token:\n%s", page.Body.String())
	}
	j.token = match[1]
}

// get sends one browser GET request.
func (j *ideationJourney) get(path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Host = "127.0.0.1:8765"
	recorder := httptest.NewRecorder()
	j.ui.ServeHTTP(recorder, request)
	return recorder
}

// commandAnswer is the part of the command API answer the journeys read.
type commandAnswer struct {
	RequestID string          `json:"request_id"`
	State     string          `json:"state"`
	Outcome   string          `json:"outcome"`
	Detail    string          `json:"detail"`
	Replayed  *bool           `json:"replayed"`
	Result    json.RawMessage `json:"result"`
}

// post sends one command as the session page does, at the revision the page
// shows, and returns the HTTP status and answer.
func (j *ideationJourney) post(requestID string, kind sessioncommand.Kind, revision int64, payload any) (int, commandAnswer) {
	j.t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		j.t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"request_id": requestID, "session_id": journeySessionID, "kind": kind, "expected_revision": revision, "payload": json.RawMessage(encoded),
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

// revision returns the session revision the page shows, or 0 before the
// session exists.
func (j *ideationJourney) revision() int64 {
	j.t.Helper()
	session, err := j.store.IdeationSession(j.t.Context(), journeySessionID)
	if errors.Is(err, store.ErrIdeationSessionNotFound) {
		return 0
	}
	if err != nil {
		j.t.Fatal(err)
	}
	return session.Revision
}

// submit sends one new command at the current revision, requires that the
// inbox accepted it, lets the coordinator apply it, and returns its final
// answer from the command API.
func (j *ideationJourney) submit(requestID string, kind sessioncommand.Kind, payload any) commandAnswer {
	j.t.Helper()
	status, answer := j.post(requestID, kind, j.revision(), payload)
	if status != http.StatusAccepted {
		j.t.Fatalf("%s %s: status %d, answer %#v; want accepted", kind, requestID, status, answer)
	}
	j.coordinate()
	return j.command(requestID)
}

// apply submits one command and requires that the coordinator applied it.
func (j *ideationJourney) apply(requestID string, kind sessioncommand.Kind, payload any) commandAnswer {
	j.t.Helper()
	answer := j.submit(requestID, kind, payload)
	if answer.Outcome != string(store.IdeationCommandApplied) {
		j.t.Fatalf("%s %s: outcome %q (%s), want applied", kind, requestID, answer.Outcome, answer.Detail)
	}
	return answer
}

// coordinate runs one coordinator inbox pass.
func (j *ideationJourney) coordinate() {
	j.t.Helper()
	if err := j.service.DrainSessionCommandsForTest(j.t.Context()); err != nil {
		j.t.Fatalf("coordinator inbox pass: %v", err)
	}
}

// command reads one command through the UI command API.
func (j *ideationJourney) command(requestID string) commandAnswer {
	j.t.Helper()
	recorder := j.get("/api/commands/" + requestID)
	var answer commandAnswer
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &answer) != nil {
		j.t.Fatalf("GET command %s: %d %s", requestID, recorder.Code, recorder.Body.String())
	}
	return answer
}

// respond writes value as the report of the one active session role and
// lets the coordinator accept it.
func (j *ideationJourney) respond(value report.Report) store.SessionInvocation {
	j.t.Helper()
	active, err := j.store.ActiveSessionInvocations(j.t.Context())
	if err != nil || len(active) != 1 {
		j.t.Fatalf("active invocations = %#v, %v; want one role at work", active, err)
	}
	j.report(active[0], value)
	if err := j.service.ReconcileSessionRoles(j.t.Context()); err != nil {
		j.t.Fatalf("ReconcileSessionRoles() error = %v", err)
	}
	return j.invocation(active[0].ID)
}

// history reads the journey session's durable history.
func (j *ideationJourney) history() store.IdeationHistory {
	j.t.Helper()
	history, err := j.store.IdeationHistory(j.t.Context(), journeySessionID)
	if err != nil {
		j.t.Fatal(err)
	}
	return history
}

// journeySession returns the journey session.
func (j *ideationJourney) journeySession() store.IdeationSession {
	return j.history().Session
}

// previewDigests previews the current plan revision and returns the
// approval payload the review page sends for it.
func (j *ideationJourney) previewDigests(requestID string, publish bool) sessioncommand.ApprovePayload {
	j.t.Helper()
	revision := j.journeySession().PlanRevision
	answer := j.apply(requestID, sessioncommand.KindPreviewPlan, sessioncommand.PlanRevisionPayload{PlanRevision: revision})
	var preview sessioncommand.PreviewResult
	if err := json.Unmarshal(answer.Result, &preview); err != nil {
		j.t.Fatalf("preview result %s: %v", answer.Result, err)
	}
	return sessioncommand.ApprovePayload{
		PlanRevision: revision, PayloadSHA256: preview.PayloadSHA256, RelationshipMappingSHA256: preview.RelationshipMappingSHA256, Publish: publish,
	}
}

// requireReceipt fails unless the newest receipt has outcome.
func (j *ideationJourney) requireReceipt(outcome store.PublicationOutcome) store.PublicationReceipt {
	j.t.Helper()
	history := j.history()
	if len(history.Receipts) == 0 {
		j.t.Fatalf("session %#v has no publication receipt", history.Session)
	}
	receipt := history.Receipts[len(history.Receipts)-1]
	if receipt.Outcome != outcome {
		j.t.Fatalf("receipt = %#v, want %s", receipt, outcome)
	}
	return receipt
}

// requireInert verifies decision D6 and the raw-conversation rule on the
// tracker: no item carries agent-ready or a factory state label or tag, no
// item contains conversation text, and publication started no run.
func (j *ideationJourney) requireInert() publishedGraph {
	j.t.Helper()
	graph := j.provider.graph()
	for _, label := range graph.labels {
		if label == tracker.LabelAgentReady || tracker.IsFactoryStateLabel(label) {
			j.t.Fatalf("a published item carries %q", label)
		}
	}
	for title, body := range graph.bodies {
		if strings.Contains(body, journeySecret) {
			j.t.Fatalf("item %q contains conversation text", title)
		}
	}
	if run, err := j.store.CurrentRun(j.t.Context()); err != nil || run != nil {
		j.t.Fatalf("CurrentRun() = %#v, %v; the journey must not start a run", run, err)
	}
	return graph
}

// questionRound is an ideation result that asks one grounded question and
// keeps it open in the brief.
func questionRound() report.Report {
	return report.Report{Outcome: report.OutcomeCompleted, Ideation: &report.IdeationHandoff{
		Message:   "internal/cli has no export command. Who reads the export?",
		Questions: []report.Question{{ID: "q-reader", Prompt: "Who reads the export? Recommended: support engineers."}},
		Brief: &report.Brief{
			Problem:             "Operators cannot export run history.",
			Scope:               []report.BriefItem{{ID: "s-export", Text: "Export run history as JSON."}},
			Evidence:            []report.BriefItem{{ID: "ev-cli", Text: "internal/cli/cli.go has no export command."}},
			UnresolvedQuestions: []report.BriefItem{{ID: "uq-reader", Text: "Who reads the export?"}},
		},
		Readiness: &report.ReadinessProposal{Proposal: report.ReadinessInvestigationNeeded, Rationale: "The reader is unknown."},
	}}
}

// readyRound is an ideation result whose brief has no open question.
func readyRound() report.Report {
	return report.Report{Outcome: report.OutcomeCompleted, Ideation: &report.IdeationHandoff{
		Message: "Support engineers read it; the brief is ready for planning.",
		Brief: &report.Brief{
			Problem:   "Operators cannot export run history.",
			Audience:  "Support engineers",
			Scope:     []report.BriefItem{{ID: "s-export", Text: "Export run history as JSON."}, {ID: "s-docs", Text: "Document the export."}},
			Decisions: []report.BriefDecision{{ID: "d-format", Decision: "Export JSON.", Rationale: "Support engineers script with jq."}},
			Evidence:  []report.BriefItem{{ID: "ev-cli", Text: "internal/cli/cli.go has no export command."}},
		},
		Readiness: &report.ReadinessProposal{Proposal: report.ReadinessReadyForPlanning, Rationale: "No open question remains."},
	}}
}

// journeyNode returns a complete plan node.
func journeyNode(id, kind, title, parent string, dependsOn ...string) report.PlanNode {
	return report.PlanNode{
		ID: id, Kind: kind, Title: title, Body: "Problem, outcome, and context of " + title + ".",
		AcceptanceCriteria: []string{title + " is observable."}, ParentID: parent, DependsOn: dependsOn, BriefRefs: []string{"s-export"},
	}
}

// featureDraft is a planning result with a parent and two tasks, where the
// documentation task depends on the export task.
func featureDraft() report.Report {
	return planReportForTest(
		journeyNode("n-1", "parent", "Run history export", ""),
		journeyNode("n-2", "task", "Add the export command", "n-1"),
		journeyNode("n-3", "task", "Document the export", "n-1", "n-2"),
	)
}

// startSession creates the session, submits the idea, and answers one
// question round, so the session waits for the user's answer.
func (j *ideationJourney) startSession() {
	j.t.Helper()
	j.apply("create-1", sessioncommand.KindCreateSession, sessioncommand.CreateSessionPayload{Title: "Run history export"})
	j.apply("turn-idea", sessioncommand.KindSubmitTurn, sessioncommand.TurnPayload{Body: "Operators need to export run history. " + journeySecret})
	j.respond(questionRound())
}

// reachReviewedPlan runs the journey from the idea to a plan revision that
// the user changed after the planning role drafted it: two question rounds,
// a brief the user edited, the handoff, and the user's plan change.
func (j *ideationJourney) reachReviewedPlan() {
	j.t.Helper()
	j.startSession()
	j.apply("turn-answer", sessioncommand.KindSubmitTurn, sessioncommand.TurnPayload{Body: "Support engineers read it."})
	j.respond(readyRound())
	brief := sessioncommand.BriefFromStore(j.history().Briefs[len(j.history().Briefs)-1].Brief)
	brief.SuccessCriteria = []sessioncommand.BriefItemJSON{{ID: "sc-one-command", Text: "One command exports every run."}}
	j.apply("brief-edit", sessioncommand.KindEditBrief, sessioncommand.BriefPayload{Brief: brief})
	j.apply("handoff-1", sessioncommand.KindHandOff, sessioncommand.HandOffPayload{BriefRevision: j.journeySession().BriefRevision})
	j.respond(featureDraft())
	nodes := sessioncommand.PlanNodesFromStore(j.history().Plans[len(j.history().Plans)-1].Nodes)
	nodes[1].Title = "Add the factory export command"
	nodes[1].AcceptanceCriteria = append(nodes[1].AcceptanceCriteria, "The export holds every run of the store.")
	j.apply("draft-edit", sessioncommand.KindEditDraft, sessioncommand.DraftPayload{Nodes: nodes})
}

// requireFeatureGraph verifies the reviewed feature on the tracker: three
// items with the user's title, both tasks under the parent, and the
// documentation after the export command.
func requireFeatureGraph(t *testing.T, graph publishedGraph) {
	t.Helper()
	titles := slices.Sorted(func(yield func(string) bool) {
		for title := range graph.bodies {
			if !yield(title) {
				return
			}
		}
	})
	want := []string{"Add the factory export command", "Document the export", "Run history export"}
	if !slices.Equal(titles, want) {
		t.Fatalf("published titles = %v, want %v", titles, want)
	}
	if graph.parents["Add the factory export command"] != "Run history export" || graph.parents["Document the export"] != "Run history export" {
		t.Fatalf("parents = %#v, want both tasks under the parent", graph.parents)
	}
	if deps := graph.dependsOn["Document the export"]; !slices.Equal(deps, []string{"Add the factory export command"}) || len(graph.dependsOn) != 1 {
		t.Fatalf("dependencies = %#v, want the documentation after the export command", graph.dependsOn)
	}
	if graph.creates != 3 {
		t.Fatalf("creates = %d, want each item created once", graph.creates)
	}
}

// TestIdeationJourneyPublishesTheReviewedPlanToEachTracker verifies the
// happy path on both trackers: an idea, two question rounds, a brief the
// user edited, a planning draft the user changed, a preview, and one
// approval that publishes the exact reviewed graph as linked items. The
// session then shows every item URL after a browser reconnect, and nothing
// authorizes execution.
func TestIdeationJourneyPublishesTheReviewedPlanToEachTracker(t *testing.T) {
	for _, provider := range journeyProviders(t) {
		t.Run(provider.name, func(t *testing.T) {
			journey := newIdeationJourney(t, provider)
			journey.reachReviewedPlan()
			journey.apply("approve-1", sessioncommand.KindApprove, journey.previewDigests("preview-1", true))

			receipt := journey.requireReceipt(store.PublicationOutcomePublished)
			if len(receipt.Items) != 3 || len(receipt.Relationships) != 3 {
				t.Fatalf("receipt = %#v, want three items and three relationships", receipt)
			}
			requireFeatureGraph(t, journey.requireInert())
			history := journey.history()
			if history.Session.State != store.IdeationStatePublished || len(history.Plans) != 2 || len(history.Briefs) != 3 {
				t.Fatalf("session %s with %d plan and %d brief revisions, want published after 2 and 3", history.Session.State, len(history.Plans), len(history.Briefs))
			}

			journey.reconnectBrowser()
			activity := journey.get("/api/sessions/" + journeySessionID).Body.String()
			for _, item := range receipt.Items {
				if !strings.Contains(activity, `"url":"`+item.URL+`"`) {
					t.Fatalf("activity after reconnect lacks %s:\n%s", item.URL, activity)
				}
			}
		})
	}
}

// TestIdeationJourneyPublishesASingleItem verifies the one-issue path: a
// small idea that planning drafts as one task publishes as exactly one item
// with no relationship.
func TestIdeationJourneyPublishesASingleItem(t *testing.T) {
	for _, provider := range journeyProviders(t) {
		t.Run(provider.name, func(t *testing.T) {
			journey := newIdeationJourney(t, provider)
			journey.apply("create-1", sessioncommand.KindCreateSession, sessioncommand.CreateSessionPayload{Title: "Fix typo"})
			journey.apply("turn-idea", sessioncommand.KindSubmitTurn, sessioncommand.TurnPayload{Body: "The status help has a typo."})
			journey.respond(readyRound())
			journey.apply("handoff-1", sessioncommand.KindHandOff, sessioncommand.HandOffPayload{BriefRevision: journey.journeySession().BriefRevision})
			journey.respond(planReportForTest(journeyNode("n-1", "task", "Fix the status help typo", "")))
			journey.apply("approve-1", sessioncommand.KindApprove, journey.previewDigests("preview-1", true))

			receipt := journey.requireReceipt(store.PublicationOutcomePublished)
			graph := journey.requireInert()
			if len(receipt.Items) != 1 || len(receipt.Relationships) != 0 || len(graph.bodies) != 1 || len(graph.parents)+len(graph.dependsOn) != 0 {
				t.Fatalf("receipt = %#v, graph = %#v; want one unlinked item", receipt, graph)
			}
		})
	}
}

// TestIdeationJourneyEndsWithoutBacklogWork verifies the defer and no-build
// paths: the user parks the session, resumes it, and decides not to build.
// No path writes to the tracker or drafts a plan.
func TestIdeationJourneyEndsWithoutBacklogWork(t *testing.T) {
	for _, provider := range journeyProviders(t) {
		t.Run(provider.name, func(t *testing.T) {
			journey := newIdeationJourney(t, provider)
			journey.startSession()
			journey.apply("defer-1", sessioncommand.KindDefer, sessioncommand.ReasonPayload{Reason: "Wait for the support team."})
			if state := journey.journeySession().State; state != store.IdeationStateDeferred {
				t.Fatalf("state = %q, want deferred", state)
			}
			journey.apply("resume-1", sessioncommand.KindResume, sessioncommand.ReasonPayload{})
			journey.apply("no-build-1", sessioncommand.KindNoBuild, sessioncommand.ReasonPayload{Reason: "Support exports by hand."})

			session := journey.journeySession()
			graph := journey.requireInert()
			if session.State != store.IdeationStateNoBuild || session.PlanRevision != 0 || graph.writes != 0 {
				t.Fatalf("session = %#v with %d tracker writes; want no_build without a plan or a write", session, graph.writes)
			}
		})
	}
}

// TestIdeationJourneyPublishesUnresolvedRequirementsAsInvestigationWork
// verifies that an unknown the user defers explicitly becomes investigation
// work: the handoff names the open question, planning drafts an
// investigation item, and the dependent task is linked after it.
func TestIdeationJourneyPublishesUnresolvedRequirementsAsInvestigationWork(t *testing.T) {
	for _, provider := range journeyProviders(t) {
		t.Run(provider.name, func(t *testing.T) {
			journey := newIdeationJourney(t, provider)
			journey.startSession()
			briefRevision := journey.journeySession().BriefRevision
			refused := journey.submit("handoff-1", sessioncommand.KindHandOff, sessioncommand.HandOffPayload{BriefRevision: briefRevision})
			if refused.Outcome != string(store.IdeationCommandRefused) || !strings.Contains(refused.Detail, "uq-reader") {
				t.Fatalf("handoff without deferral = %#v, want a refusal that names uq-reader", refused)
			}
			journey.apply("handoff-2", sessioncommand.KindHandOff, sessioncommand.HandOffPayload{BriefRevision: briefRevision, DeferredQuestions: []string{"uq-reader"}})
			journey.respond(planReportForTest(
				journeyNode("n-1", "investigation", "Find the export readers", ""),
				journeyNode("n-2", "task", "Add the export command", "", "n-1"),
			))
			journey.apply("approve-1", sessioncommand.KindApprove, journey.previewDigests("preview-1", true))

			journey.requireReceipt(store.PublicationOutcomePublished)
			graph := journey.requireInert()
			if len(graph.bodies) != 2 || !slices.Equal(graph.dependsOn["Add the export command"], []string{"Find the export readers"}) {
				t.Fatalf("graph = %#v, want the task after the investigation", graph)
			}
		})
	}
}

// TestIdeationJourneySurvivesRestartsAndDuplicateSubmissions verifies that a
// coordinator restart and a browser reconnect keep accepted turns and
// reviewed revisions, and that a repeated browser submission returns the
// stored outcome without a second paid role launch or tracker effect.
func TestIdeationJourneySurvivesRestartsAndDuplicateSubmissions(t *testing.T) {
	for _, provider := range journeyProviders(t) {
		t.Run(provider.name, func(t *testing.T) {
			journey := newIdeationJourney(t, provider)
			journey.startSession()

			revision := journey.revision()
			if status, _ := journey.post("turn-answer", sessioncommand.KindSubmitTurn, revision, sessioncommand.TurnPayload{Body: "Support engineers read it."}); status != http.StatusAccepted {
				t.Fatalf("first submission status = %d, want accepted", status)
			}
			status, replay := journey.post("turn-answer", sessioncommand.KindSubmitTurn, revision, sessioncommand.TurnPayload{Body: "Support engineers read it."})
			if status != http.StatusOK || replay.Replayed == nil || !*replay.Replayed {
				t.Fatalf("repeated submission = %d %#v, want the stored command", status, replay)
			}
			journey.coordinate()
			journey.coordinate()
			launches := len(journey.worker.launches)
			if launches != 2 {
				t.Fatalf("launches = %d, want one launch for the idea and one for the answer", launches)
			}

			journey.restartCoordinator()
			journey.reconnectBrowser()
			journey.respond(readyRound())
			history := journey.history()
			if len(history.Turns) != 4 || history.Turns[2].ID != "turn-answer" || history.Session.State != store.IdeationStateAwaitingUser {
				t.Fatalf("turns after restart = %#v, want both user turns and both role turns", history.Turns)
			}

			journey.apply("handoff-1", sessioncommand.KindHandOff, sessioncommand.HandOffPayload{BriefRevision: history.Session.BriefRevision})
			journey.respond(featureDraft())
			nodes := sessioncommand.PlanNodesFromStore(journey.history().Plans[0].Nodes)
			nodes[1].Title = "Add the factory export command"
			journey.apply("draft-edit", sessioncommand.KindEditDraft, sessioncommand.DraftPayload{Nodes: nodes})
			journey.restartCoordinator()
			journey.reconnectBrowser()
			if plans := journey.history().Plans; len(plans) != 2 || plans[1].Nodes[1].Title != "Add the factory export command" {
				t.Fatalf("plan revisions after restart = %#v, want the reviewed revision", plans)
			}

			approval := journey.previewDigests("preview-1", true)
			shown := journey.revision()
			if status, _ := journey.post("approve-1", sessioncommand.KindApprove, shown, approval); status != http.StatusAccepted {
				t.Fatalf("approval status = %d, want accepted", status)
			}
			if status, replay := journey.post("approve-1", sessioncommand.KindApprove, shown, approval); status != http.StatusOK || !*replay.Replayed {
				t.Fatalf("double-clicked approval = %d %#v, want the stored command", status, replay)
			}
			journey.coordinate()
			before := journey.provider.graph()
			status, replay = journey.post("approve-1", sessioncommand.KindApprove, shown, approval)
			journey.coordinate()
			after := journey.requireInert()
			if status != http.StatusOK || replay.Outcome != string(store.IdeationCommandApplied) {
				t.Fatalf("approval resent after publication = %d %#v, want the stored applied outcome", status, replay)
			}
			if after.writes != before.writes || len(journey.worker.launches) != launches+1 || len(journey.history().Receipts) != 1 {
				t.Fatalf("resent approval wrote %d times, launched %d roles, and left %d receipts; want no new effect",
					after.writes-before.writes, len(journey.worker.launches)-launches-1, len(journey.history().Receipts))
			}
			requireFeatureGraph(t, after)
		})
	}
}

// TestIdeationJourneyRefusesStaleApprovalAndChangedMapping verifies that an
// approval names exactly the reviewed rendering: an approval of a preview
// that a later plan change replaced is refused, a provider mapping change
// after approval refuses publication before any tracker write, and the
// user's recovery (save the plan again, preview, approve) publishes under the
// new mapping.
func TestIdeationJourneyRefusesStaleApprovalAndChangedMapping(t *testing.T) {
	for _, provider := range journeyProviders(t) {
		t.Run(provider.name, func(t *testing.T) {
			journey := newIdeationJourney(t, provider)
			journey.reachReviewedPlan()
			stale := journey.previewDigests("preview-1", true)
			nodes := sessioncommand.PlanNodesFromStore(journey.history().Plans[1].Nodes)
			nodes[2].Title = "Document the export for support"
			journey.apply("draft-edit-2", sessioncommand.KindEditDraft, sessioncommand.DraftPayload{Nodes: nodes})
			refused := journey.submit("approve-stale", sessioncommand.KindApprove, stale)
			if refused.Outcome != string(store.IdeationCommandRefused) || !strings.Contains(refused.Detail, "is not the current plan revision") {
				t.Fatalf("stale approval = %#v, want a refusal", refused)
			}

			journey.apply("approve-1", sessioncommand.KindApprove, journey.previewDigests("preview-2", false))
			journey.provider.changeMapping()
			changed := journey.submit("publish-1", sessioncommand.KindPublish, sessioncommand.PublishPayload{})
			if changed.Outcome != string(store.IdeationCommandRefused) || !strings.Contains(changed.Detail, "does not bind the current rendered publication") {
				t.Fatalf("publication after a mapping change = %#v, want a stale approval refusal", changed)
			}
			if graph := journey.requireInert(); graph.writes != 0 {
				t.Fatalf("tracker writes = %d before a current approval, want none", graph.writes)
			}

			current := sessioncommand.PlanNodesFromStore(journey.history().Plans[2].Nodes)
			journey.apply("draft-save", sessioncommand.KindEditDraft, sessioncommand.DraftPayload{Nodes: current})
			journey.apply("approve-2", sessioncommand.KindApprove, journey.previewDigests("preview-3", true))
			journey.requireReceipt(store.PublicationOutcomePublished)
			journey.requireInert()
		})
	}
}

// TestIdeationJourneyRefusesAMalformedPlanGraph verifies that a planning
// draft with a dependency cycle never becomes a reviewable plan and that a
// user edit with a malformed graph is refused with the problem named, while
// the earlier valid revision stays approvable.
func TestIdeationJourneyRefusesAMalformedPlanGraph(t *testing.T) {
	journey := newIdeationJourney(t, githubJourney(t))
	journey.startSession()
	journey.apply("handoff-1", sessioncommand.KindHandOff, sessioncommand.HandOffPayload{
		BriefRevision: journey.journeySession().BriefRevision, DeferredQuestions: []string{"uq-reader"},
	})
	journey.respond(planReportForTest(
		journeyNode("n-1", "task", "Write the export", "", "n-2"),
		journeyNode("n-2", "task", "Read the store", "", "n-1"),
	))
	if session := journey.journeySession(); session.PlanRevision != 0 || !strings.Contains(session.StateReason, "dependency cycle") {
		t.Fatalf("session = %#v, want no plan and the cycle named", session)
	}

	journey.apply("regenerate-1", sessioncommand.KindHandOff, sessioncommand.HandOffPayload{
		BriefRevision: journey.journeySession().BriefRevision, DeferredQuestions: []string{"uq-reader"},
	})
	journey.respond(featureDraft())
	nodes := sessioncommand.PlanNodesFromStore(journey.history().Plans[0].Nodes)
	nodes[2].ParentID = ""
	refused := journey.submit("draft-edit", sessioncommand.KindEditDraft, sessioncommand.DraftPayload{Nodes: nodes})
	if refused.Outcome != string(store.IdeationCommandRefused) || !strings.Contains(refused.Detail, "n-1") {
		t.Fatalf("malformed edit = %#v, want a refusal that names the parent", refused)
	}
	journey.apply("approve-1", sessioncommand.KindApprove, journey.previewDigests("preview-1", true))
	journey.requireReceipt(store.PublicationOutcomePublished)
}

// TestIdeationJourneyCancellationLeavesNoTrackerEffect verifies that a user
// who cancels while a role works stops the role, that its late report is not
// applied, and that a cancelled session refuses publication without a
// tracker write.
func TestIdeationJourneyCancellationLeavesNoTrackerEffect(t *testing.T) {
	journey := newIdeationJourney(t, githubJourney(t))
	journey.apply("create-1", sessioncommand.KindCreateSession, sessioncommand.CreateSessionPayload{Title: "Run history export"})
	journey.apply("turn-idea", sessioncommand.KindSubmitTurn, sessioncommand.TurnPayload{Body: "Operators need to export run history."})
	active, err := journey.store.ActiveSessionInvocations(t.Context())
	if err != nil || len(active) != 1 {
		t.Fatalf("active = %#v, %v; want the ideation role at work", active, err)
	}
	journey.apply("cancel-1", sessioncommand.KindCancel, sessioncommand.ReasonPayload{Reason: "Wrong repository."})
	journey.report(active[0], questionRound())
	if err := journey.service.ReconcileSessionRoles(t.Context()); err != nil {
		t.Fatal(err)
	}
	history := journey.history()
	if history.Session.State != store.IdeationStateCancelled || history.Session.BriefRevision != 0 || len(history.Turns) != 1 {
		t.Fatalf("history = %#v, want a cancelled session without the late role result", history)
	}
	if got := journey.invocation(active[0].ID); got.Status == store.SessionInvocationActive || got.Status == store.SessionInvocationAccepted ||
		!slices.Contains(journey.worker.cancelled, active[0].ID) {
		t.Fatalf("invocation = %#v, cancelled processes %v; want the role process stopped without acceptance", got, journey.worker.cancelled)
	}
	refused := journey.submit("publish-1", sessioncommand.KindPublish, sessioncommand.PublishPayload{})
	if refused.Outcome == string(store.IdeationCommandApplied) {
		t.Fatalf("publication of a cancelled session = %#v, want a refusal", refused)
	}
	if graph := journey.requireInert(); graph.writes != 0 {
		t.Fatalf("tracker writes = %d, want none", graph.writes)
	}
}

// TestIdeationJourneyRecoversRemoteSuccessAfterACoordinatorStop verifies
// the remote-success, local-failure case: the coordinator stops while an
// item create is in flight after the tracker created the item. The attempt
// records an incomplete receipt with the create's outcome unknown, never a
// failure. After a coordinator restart, the user's retry adopts the created
// item by its correlation and completes the graph without a duplicate.
// TestRecoveryAfterRemoteSuccessBeforeLocalReceipt in internal/publication
// covers a hard stop before the ledger records anything.
func TestIdeationJourneyRecoversRemoteSuccessAfterACoordinatorStop(t *testing.T) {
	for _, provider := range journeyProviders(t) {
		t.Run(provider.name, func(t *testing.T) {
			journey := newIdeationJourney(t, provider)
			journey.reachReviewedPlan()
			approval := journey.previewDigests("preview-1", true)
			journey.provider.holdCreate("Add the factory export command")
			if status, _ := journey.post("approve-1", sessioncommand.KindApprove, journey.revision(), approval); status != http.StatusAccepted {
				t.Fatalf("approve status = %d, want accepted", status)
			}
			stopping, stop := context.WithCancel(t.Context())
			go func() {
				deadline := time.Now().Add(10 * time.Second)
				for time.Now().Before(deadline) && journey.provider.graph().creates < 2 {
					time.Sleep(5 * time.Millisecond)
				}
				stop()
			}()
			_ = journey.service.DrainSessionCommandsForTest(stopping)
			receipt := journey.requireReceipt(store.PublicationOutcomeIncomplete)
			if !strings.Contains(receipt.Detail, "item/n-2 is ambiguous") || journey.journeySession().State != store.IdeationStatePublicationIncomplete {
				t.Fatalf("receipt after the stop = %#v, want item/n-2 with an unknown outcome", receipt)
			}

			journey.restartCoordinator()
			journey.reconnectBrowser()
			journey.apply("publish-retry", sessioncommand.KindPublish, sessioncommand.PublishPayload{})
			journey.requireReceipt(store.PublicationOutcomePublished)
			requireFeatureGraph(t, journey.requireInert())
		})
	}
}
