package factory_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/azuredevops"
	"github.com/Stevie1704/sw-factory/internal/azuredevops/azuredevopstest"
	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// The integrated acceptance scenarios of spec #302 (issue #307) for the
// Azure DevOps CLI extension route (ADR 0030). Two installations, one per
// Git repository, share one scripted Azure DevOps project: each has its own
// host registration, operational store, and real Azure DevOps client on the
// devops_extension route over the same stateful scripted az CLI. These tests
// are deterministic evidence; they are never evidence of a live provider.

// azureAuthorizedUser is the maintainer whose agent-ready tag authorizes
// execution in both installations.
const azureAuthorizedUser = "alice@contoso.com"

// epicPBIPlanMapping is the explicit Epic/PBI workflow of ADR 0030 decision
// 14: each PBI links directly to its Epic, with no Feature.
func epicPBIPlanMapping() *config.AzurePlanMapping {
	return &config.AzurePlanMapping{Levels: []config.AzurePlanLevel{
		{Type: "Epic", Kinds: []string{"parent"}},
		{Type: "Product Backlog Item", Kinds: []string{"task", "investigation"}},
	}}
}

// azureExtensionRegistration is the extension-route registration of
// repository in the scripted project. The service repository may assign
// PBIs to the web repository.
func azureExtensionRegistration(repository string) config.AzureDevOpsConfig {
	azure := config.AzureDevOpsConfig{
		Organization: azuredevopstest.Organization, Project: azuredevopstest.Project, Repository: repository,
		Route: config.AzureRouteDevOpsExtension, Planning: epicPBIPlanMapping(),
	}
	if repository == azuredevopstest.Repository {
		azure.PlanRepositories = []string{azuredevopstest.WebRepository}
	}
	return azure
}

// azureExtensionClient returns the client that the composition root builds
// for the registration, over the scripted az CLI.
func azureExtensionClient(boards *azuredevopstest.Boards, azure config.AzureDevOpsConfig) *azuredevops.Client {
	client := azuredevops.NewClientForRoute(azure.Organization, []string{azureAuthorizedUser}, azure.EffectiveRoute())
	client.Runner = boards
	client.Planning = azure.Planning
	client.PlanRepositories = azure.PlanRepositories
	return client
}

// azureExtensionJourney returns the Azure DevOps adapter on the CLI
// extension route over a scripted Scrum project with the Epic/PBI mapping,
// registered for the service repository.
func azureExtensionJourney(t *testing.T, boards *azuredevopstest.Boards) journeyProvider {
	azure := azureExtensionRegistration(azuredevopstest.Repository)
	client := azureExtensionClient(boards, azure)
	return journeyProvider{
		name: "azure-devops-extension",
		register: func(registration *config.RepositoryRegistration) {
			registration.AzureDevOps = azure
			registration.AuthorizedUsers = []string{azureAuthorizedUser}
		},
		client:     client,
		holdCreate: boards.HoldAfterWrite,
		changeMapping: func() {
			mapping := epicPBIPlanMapping()
			mapping.Levels[1].Fields = map[string]string{"Microsoft.VSTS.Common.Priority": "1"}
			client.Planning = mapping
		},
		graph: func() publishedGraph {
			graph := publishedGraph{bodies: map[string]string{}, parents: map[string]string{}, dependsOn: map[string][]string{}, labels: []string{}}
			items := azuredevopstest.Items(boards.Snapshot())
			for _, item := range items {
				title := item.Title()
				graph.bodies[title] = item.Description()
				graph.labels = append(graph.labels, item.Tags()...)
				for _, relation := range item.Relations {
					switch relation.Rel {
					case "System.LinkTypes.Hierarchy-Reverse":
						graph.parents[title] = items.Title(relation.URL)
					case "System.LinkTypes.Dependency-Reverse":
						graph.dependsOn[title] = append(graph.dependsOn[title], items.Title(relation.URL))
					}
				}
			}
			writes := boards.ExtensionWrites()
			graph.writes = len(writes) + len(boards.Writes())
			for _, write := range writes {
				if write.Route() == "boards work-item create" {
					graph.creates++
				}
			}
			return graph
		},
	}
}

// azureInstallation is one Factory installation for one repository of the
// shared project: its own host registration, operational store, and
// coordinator, as a separate factory start --config runs them.
type azureInstallation struct {
	t          *testing.T
	repository string
	storeDB    string
	workspace  *draftGitWorkspace
	service    *factory.Service
}

// newAzureInstallation registers repository of the scripted project on the
// extension route. The Git workspace and the gate worker are in-memory
// fixtures; the tracker and the code host are the real Azure DevOps client.
func newAzureInstallation(t *testing.T, boards *azuredevopstest.Boards, repository string) *azureInstallation {
	t.Helper()
	root := t.TempDir()
	repositoryPath := filepath.Join(root, repository)
	worktreePath := filepath.Join(root, "worktree")
	for _, directory := range []string{filepath.Join(repositoryPath, ".git"), worktreePath} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runID := "run-" + repository
	branch := "factory/" + runID
	installation := &azureInstallation{
		t: t, repository: repository, storeDB: filepath.Join(root, "state", "factory.db"),
		workspace: &draftGitWorkspace{
			workspace: gitadapter.Workspace{BaseSHA: factoryGateCheckpoint, Branch: branch, Worktree: worktreePath},
			state:     gitadapter.WorktreeState{Branch: branch, HeadSHA: factoryGateCheckpoint, ChangedPaths: []string{"internal/export.go"}},
		},
	}
	azure := azureExtensionRegistration(repository)
	host := config.HostConfig{SchemaVersion: config.CurrentHostSchemaVersion, Repositories: []config.RepositoryRegistration{{
		Path: repositoryPath, AzureDevOps: azure, AuthorizedUsers: []string{azureAuthorizedUser},
		OperationalDataPath: installation.storeDB, RepositoryConfigPath: filepath.Join(repositoryPath, config.RepositoryConfigFileName),
	}}}
	policy := validRepositoryConfig()
	policy.Gates = []config.GateConfig{{Name: "test", Command: "test", Timeout: "5m", Blocking: true, EnvironmentPolicy: config.EnvironmentPolicyClean}}
	installation.service = factory.NewWithDependencies(filepath.Join(root, "host.yaml"), factory.Dependencies{
		Config:         &fakeConfig{value: host},
		OpenStore:      func(ctx context.Context, path string) (factory.OperationalStore, error) { return store.Open(ctx, path) },
		LoadRepository: func(string) (config.RepositoryConfig, error) { return policy, nil },
		Tracker:        azureExtensionClient(boards, azure),
		Worktree:       installation.workspace,
		GitWorkspace:   installation.workspace,
		Worker:         &gateWorker{results: []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}}},
		CommitStatuses: &gateStatuses{},
		Now:            func() time.Time { return time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC) },
		NewRunID:       func() (string, error) { return runID, nil },
		Coordinator:    "coordinator-" + repository,
	})
	return installation
}

// currentRun reads the installation's active Run from its own store.
func (i *azureInstallation) currentRun() *store.Run {
	i.t.Helper()
	opened, err := store.Open(i.t.Context(), i.storeDB)
	if err != nil {
		i.t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	run, err := opened.CurrentRun(i.t.Context())
	if err != nil {
		i.t.Fatal(err)
	}
	return run
}

// advanceToImplementation records that the claimed Run finished its
// implementation turn, as the worker does before the draft pull request.
// It is a fixture step: the implementation turn itself is not driven here.
func (i *azureInstallation) advanceToImplementation() {
	i.t.Helper()
	run := i.currentRun()
	run.Stage = store.StageImplementation
	run.Status = store.StatusActive
	opened, err := store.Open(i.t.Context(), i.storeDB)
	if err != nil {
		i.t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	if err := opened.SaveRun(i.t.Context(), *run); err != nil {
		i.t.Fatalf("SaveRun() fixture step error = %v", err)
	}
}

// poll runs one queue pass and requires outcome.
func (i *azureInstallation) poll(outcome factory.PollOutcome) factory.PollResult {
	i.t.Helper()
	result, err := i.service.PollOnce(i.t.Context())
	if err != nil {
		i.t.Fatalf("%s PollOnce() error = %v", i.repository, err)
	}
	if result.Outcome != outcome {
		i.t.Fatalf("%s poll = %+v, want %s", i.repository, result, outcome)
	}
	return result
}

// boardsItem returns the scripted work item titled title.
func boardsItem(t *testing.T, boards *azuredevopstest.Boards, title string) azuredevopstest.Item {
	t.Helper()
	for _, item := range boards.Snapshot() {
		if item.Title() == title {
			return item
		}
	}
	t.Fatalf("no work item titled %q", title)
	return azuredevopstest.Item{}
}

// sortedTags returns the tags of the work item with id, sorted.
func sortedTags(t *testing.T, boards *azuredevopstest.Boards, id int) []string {
	t.Helper()
	for _, item := range boards.Snapshot() {
		if item.ID == id {
			return slices.Sorted(slices.Values(item.Tags()))
		}
	}
	t.Fatalf("no work item %d", id)
	return nil
}

// Titles of the reviewed feature plan (featureDraft after the user's edit).
const (
	epicTitle   = "Run history export"
	exportTitle = "Add the factory export command"
	docsTitle   = "Document the export"
)

// TestAzureExtensionJourneyRunsAnEpicAcrossTwoRepositories verifies the
// two-repository acceptance of spec #302 on the extension route. The
// service installation previews and approves, through the UI, one Epic
// whose two PBIs target the service and the web repository. Publication
// creates exactly one Epic and two PBIs directly below it, with
// correlation and assignment tags, and authorizes nothing. After a
// maintainer adds agent-ready, each installation claims only its own PBI;
// missing, ambiguous, malformed, unknown, and foreign assignments are
// refused on the direct claim path with no effect and never listed by the
// queue. The claim freezes the validated repository and keeps both
// metadata tags through the state-tag update. A later retag does not
// redirect the Run: its draft pull request is created in the frozen
// repository with a native link to its PBI. Review feedback and readiness
// then read and change that pull request in its repository.
func TestAzureExtensionJourneyRunsAnEpicAcrossTwoRepositories(t *testing.T) {
	boards := azuredevopstest.NewBoards(t, azuredevopstest.ScrumProcess())
	serviceTag := azuredevops.RepositoryAssignmentTag(azuredevopstest.RepositoryID)
	webTag := azuredevops.RepositoryAssignmentTag(azuredevopstest.WebRepositoryID)

	// Preview and approval on the publishing installation, through the UI.
	journey := newIdeationJourney(t, azureExtensionJourney(t, boards))
	journey.reachReviewedPlan()
	nodes := sessioncommand.PlanNodesFromStore(journey.history().Plans[len(journey.history().Plans)-1].Nodes)
	nodes[2].TargetRepository = azuredevopstest.WebRepository
	journey.apply("draft-target", sessioncommand.KindEditDraft, sessioncommand.DraftPayload{Nodes: nodes})
	approval := journey.previewDigests("preview-1")
	if page := journey.get("/sessions/" + journeySessionID + "/plan").Body.String(); !strings.Contains(page, azuredevopstest.WebRepositoryID) || !strings.Contains(page, azuredevopstest.RepositoryID) {
		t.Fatalf("plan review page does not show each PBI's target repository:\n%s", page)
	}
	journey.apply("approve-1", sessioncommand.KindApprove, approval)
	journey.requireReceipt(store.PublicationOutcomePublished)
	requireFeatureGraph(t, journey.requireInert())

	epic, export, docs := boardsItem(t, boards, epicTitle), boardsItem(t, boards, exportTitle), boardsItem(t, boards, docsTitle)
	if epic.Type != "Epic" || export.Type != "Product Backlog Item" || docs.Type != "Product Backlog Item" {
		t.Fatalf("types = %s, %s, %s; want one Epic and two PBIs", epic.Type, export.Type, docs.Type)
	}
	if tags := sortedTags(t, boards, epic.ID); len(tags) != 1 || !strings.HasPrefix(tags[0], "factory-plan:") {
		t.Fatalf("Epic tags = %v, want only its correlation tag", tags)
	}
	exportCorrelation, docsCorrelation := "factory-plan:"+journeySessionID+"/3/n-2", "factory-plan:"+journeySessionID+"/3/n-3"
	if got := sortedTags(t, boards, export.ID); !slices.Equal(got, []string{exportCorrelation, serviceTag}) {
		t.Fatalf("service PBI tags = %v, want its correlation and service assignment", got)
	}
	if got := sortedTags(t, boards, docs.ID); !slices.Equal(got, []string{docsCorrelation, webTag}) {
		t.Fatalf("web PBI tags = %v, want its correlation and web assignment", got)
	}

	// Publication authorizes nothing in either installation.
	service := newAzureInstallation(t, boards, azuredevopstest.Repository)
	web := newAzureInstallation(t, boards, azuredevopstest.WebRepository)
	service.poll(factory.PollNoWork)
	web.poll(factory.PollNoWork)
	if _, err := service.service.ClaimIssue(t.Context(), export.ID); err == nil || !strings.Contains(err.Error(), tracker.LabelAgentReady) {
		t.Fatalf("direct claim of an unauthorized PBI = %v, want a refusal that names %s", err, tracker.LabelAgentReady)
	}

	// The maintainer authorizes four PBIs without a usable assignment in the
	// same project. Neither queue lists them, and a direct claim refuses each
	// one without an effect.
	unknownTag := azuredevops.RepositoryAssignmentTag("0f0e0d0c-0b0a-4908-8706-050403020100")
	invalid := map[string]int{
		"missing":   boards.Seed("Product Backlog Item", "Unassigned"),
		"ambiguous": boards.Seed("Product Backlog Item", "Two repositories", serviceTag, webTag),
		"malformed": boards.Seed("Product Backlog Item", "Repository name", "factory-repo:service"),
		"unknown":   boards.Seed("Product Backlog Item", "Unknown repository", unknownTag),
	}
	for _, id := range invalid {
		boards.AddTags(id, azureAuthorizedUser, tracker.LabelAgentReady)
	}
	service.poll(factory.PollNoWork)
	web.poll(factory.PollNoWork)
	refuseDirectClaim := func(name string, id int) {
		t.Helper()
		before := sortedTags(t, boards, id)
		_, err := service.service.ClaimIssue(t.Context(), id)
		if err == nil || !strings.Contains(err.Error(), serviceTag) {
			t.Fatalf("direct claim of the %s assignment = %v, want a refusal that names %s", name, err, serviceTag)
		}
		if service.currentRun() != nil || !slices.Equal(sortedTags(t, boards, id), before) {
			t.Fatalf("refused %s claim left a Run or changed tags %v", name, sortedTags(t, boards, id))
		}
	}
	for name, id := range invalid {
		refuseDirectClaim(name, id)
	}

	// The web PBI is authorized first: the service installation neither
	// polls nor directly claims it, and the web installation claims it.
	boards.AddTags(docs.ID, azureAuthorizedUser, tracker.LabelAgentReady)
	service.poll(factory.PollNoWork)
	refuseDirectClaim("foreign", docs.ID)
	webRun := web.poll(factory.PollClaimed).Run
	boards.AddTags(export.ID, azureAuthorizedUser, tracker.LabelAgentReady)
	serviceRun := service.poll(factory.PollClaimed).Run
	if webRun.IssueNumber != docs.ID || serviceRun.IssueNumber != export.ID {
		t.Fatalf("claims = web #%d, service #%d; want web #%d and service #%d", webRun.IssueNumber, serviceRun.IssueNumber, docs.ID, export.ID)
	}
	for _, check := range []struct {
		installation *azureInstallation
		id, name     string
	}{{service, azuredevopstest.RepositoryID, azuredevopstest.Repository}, {web, azuredevopstest.WebRepositoryID, azuredevopstest.WebRepository}} {
		frozen := check.installation.currentRun().TrackerRepository
		if frozen == nil || frozen.RepositoryID != check.id || frozen.Repository != check.name || frozen.ProjectID != azuredevopstest.ProjectID {
			t.Fatalf("%s Run tracker repository = %+v, want %s %s", check.name, frozen, check.name, check.id)
		}
	}
	if got := sortedTags(t, boards, export.ID); !slices.Equal(got, []string{tracker.LabelAgentRunning, exportCorrelation, serviceTag}) {
		t.Fatalf("service PBI tags after claim = %v, want the state tag with both metadata tags", got)
	}
	if got := sortedTags(t, boards, docs.ID); !slices.Equal(got, []string{tracker.LabelAgentRunning, docsCorrelation, webTag}) {
		t.Fatalf("web PBI tags after claim = %v, want the state tag with both metadata tags", got)
	}
	for name, id := range invalid {
		if slices.Contains(sortedTags(t, boards, id), tracker.LabelAgentRunning) {
			t.Fatalf("the %s PBI was claimed", name)
		}
	}

	// A retag after the claim does not redirect the service Run.
	boards.SetTags(export.ID, azureAuthorizedUser, tracker.LabelAgentRunning, exportCorrelation, webTag)
	service.advanceToImplementation()
	draft, err := service.service.CreateDraftPullRequest(t.Context(), factory.DraftPullRequestRequest{RunID: serviceRun.ID})
	if err != nil || draft.Repair != nil {
		t.Fatalf("CreateDraftPullRequest() = %#v, %v; want a draft pull request", draft, err)
	}
	if frozen := service.currentRun().TrackerRepository; frozen == nil || frozen.RepositoryID != azuredevopstest.RepositoryID {
		t.Fatalf("Run tracker repository after retag = %+v, want the frozen service repository", frozen)
	}
	pullRequests := boards.PullRequests()
	if len(pullRequests) != 1 || pullRequests[0].RepositoryID != azuredevopstest.RepositoryID || !pullRequests[0].Draft ||
		!slices.Equal(pullRequests[0].WorkItems, []int{export.ID}) {
		t.Fatalf("pull requests = %+v, want one draft in the service repository linked to PBI %d", pullRequests, export.ID)
	}

	// Review feedback and readiness on that pull request, in its repository.
	number := pullRequests[0].ID
	boards.RequestChanges(number, azureAuthorizedUser, "Add a test for CSV quoting.")
	client := azureExtensionClient(boards, azureExtensionRegistration(azuredevopstest.Repository))
	repository := tracker.Repository{Owner: azuredevopstest.Organization, Project: azuredevopstest.Project, Name: azuredevopstest.Repository}
	reviews, err := client.PullRequestReviews(t.Context(), repository, number)
	if err != nil || len(reviews) != 1 || reviews[0].Author != azureAuthorizedUser || reviews[0].State != codehost.PullRequestReviewChangesRequested ||
		reviews[0].Body != "Add a test for CSV quoting." {
		t.Fatalf("PullRequestReviews() = %#v, %v; want the maintainer's changes-requested review with its finding", reviews, err)
	}
	if _, err := client.SetPullRequestDraft(t.Context(), repository, number, false); err != nil {
		t.Fatalf("SetPullRequestDraft() error = %v", err)
	}
	webRepository := tracker.Repository{Owner: azuredevopstest.Organization, Project: azuredevopstest.Project, Name: azuredevopstest.WebRepository}
	if _, err := azureExtensionClient(boards, azureExtensionRegistration(azuredevopstest.WebRepository)).SetPullRequestDraft(t.Context(), webRepository, number, true); err == nil {
		t.Fatal("the web installation changed a pull request of the service repository")
	}
	if ready := boards.PullRequests()[0]; ready.Draft || ready.RepositoryID != azuredevopstest.RepositoryID {
		t.Fatalf("pull request after readiness = %+v, want ready in the service repository", ready)
	}
	if calls := boards.Calls(); len(calls) != 0 {
		t.Fatalf("az rest calls = %d, want none on the extension route", len(calls))
	}
}
