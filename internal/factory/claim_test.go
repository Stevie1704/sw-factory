package factory_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestIssueClaimsAnEligibleIssueWithAFrozenPacketAndOneStatusComment verifies
// the successful claim boundary and its persisted supervision surface.
func TestIssueClaimsAnEligibleIssueWithAFrozenPacketAndOneStatusComment(t *testing.T) {
	t.Parallel()

	issue := github.Issue{
		Number: 42,
		Title:  "Add claim workflow",
		Body:   "The issue body is frozen at claim time.",
		State:  "open",
		Labels: []string{"enhancement", github.LabelAgentReady},
	}
	repositoryConfig := validRepositoryConfig()
	githubAdapter := &fakeGitHub{issueValue: issue}
	worktree := &fakeWorktree{
		workspace: gitadapter.Workspace{
			BaseSHA:  "0123456789abcdef",
			Branch:   "factory/run-fixed",
			Worktree: "/tmp/.factory-worktrees/project/run-fixed",
		},
		guidance: []gitadapter.GuidanceDocument{
			{Path: "AGENTS.md", Content: "Use the repository guidance.\n"},
			{Path: "docs/agents/conventions.md", Content: "Keep changes focused.\n"},
		},
	}
	runStore := &fakeRunStore{}
	service := newClaimService(githubAdapter, worktree, runStore, repositoryConfig)

	result, err := service.ClaimIssue(context.Background(), issue.Number)
	if err != nil {
		t.Fatalf("ClaimIssue() error = %v", err)
	}
	if result.Run.ID != "run-fixed" {
		t.Fatalf("run id = %q, want run-fixed", result.Run.ID)
	}
	if result.Run.Stage != store.StageClaim || result.Run.Status != store.StatusActive {
		t.Fatalf("run state = stage %q/status %q, want claim/active", result.Run.Stage, result.Run.Status)
	}
	if result.Run.Branch != worktree.workspace.Branch || result.Run.Worktree != worktree.workspace.Worktree {
		t.Fatalf("workspace = %#v, want %#v", result.Run, worktree.workspace)
	}
	if result.Run.CheckpointSHA != worktree.workspace.BaseSHA {
		t.Fatalf("checkpoint = %q, want fetched SHA %q", result.Run.CheckpointSHA, worktree.workspace.BaseSHA)
	}
	if result.Run.StatusCommentID != "comment-1" {
		t.Fatalf("status comment id = %q, want comment-1", result.Run.StatusCommentID)
	}
	if len(githubAdapter.createdComments) != 1 {
		t.Fatalf("created comments = %d, want one", len(githubAdapter.createdComments))
	}
	comment := githubAdapter.createdComments[0]
	for _, expected := range []string{
		"run-fixed",
		"factory/run-fixed",
		worktree.workspace.Worktree,
		"coordinator-test",
		"2026-08-20T10:11:12Z",
		"stage: `claim`",
		"status: `active`",
	} {
		if !strings.Contains(comment, expected) {
			t.Errorf("status comment %q does not contain %q", comment, expected)
		}
	}
	if got, want := githubAdapter.replacedLabels, []string{"enhancement", github.LabelAgentRunning}; !equalStrings(got, want) {
		t.Fatalf("labels = %#v, want %#v", got, want)
	}

	var packet factory.SpecificationPacket
	if err := json.Unmarshal([]byte(result.Run.SpecificationPacket), &packet); err != nil {
		t.Fatalf("decode specification packet: %v", err)
	}
	if packet.Version != 1 || packet.Issue.Title != issue.Title || packet.Issue.Body != issue.Body {
		t.Fatalf("packet issue = %#v, want frozen issue %#v", packet.Issue, issue)
	}
	if packet.RepositoryConfig.TargetBranch != repositoryConfig.TargetBranch {
		t.Fatalf("packet repository config target branch = %q, want %q", packet.RepositoryConfig.TargetBranch, repositoryConfig.TargetBranch)
	}
	if !strings.Contains(packet.RepositoryGuidance, "### AGENTS.md") || !strings.Contains(packet.RepositoryGuidance, "Use the repository guidance.") || !strings.Contains(packet.RepositoryGuidance, "docs/agents/conventions.md") {
		t.Fatalf("packet repository guidance = %q, want captured guidance documents", packet.RepositoryGuidance)
	}
	if strings.Contains(packet.RepositoryGuidance, issue.Body) {
		t.Fatalf("packet repository guidance copied the issue body: %q", packet.RepositoryGuidance)
	}
	if len(runStore.saved) != 2 {
		t.Fatalf("SaveRun calls = %d, want freeze then comment identity", len(runStore.saved))
	}
}

// TestIssueClaimScopesCommandsToTheRunStart verifies pre-claim commands are
// ignored while a command posted after claim remains available exactly once.
func TestIssueClaimScopesCommandsToTheRunStart(t *testing.T) {
	t.Parallel()

	issue := github.Issue{
		Number: 42,
		Title:  "Scope commands to the claimed run",
		State:  "open",
		Labels: []string{github.LabelAgentReady},
	}
	oldCommand := github.Comment{ID: "100", Author: "alice", Body: "/factory cancel"}
	latestHistory := github.Comment{ID: "102", Author: "alice", Body: "historical discussion"}
	newCommand := github.Comment{ID: "103", Author: "alice", Body: "/factory status"}
	githubAdapter := &fakeGitHub{issueValue: issue, comments: []github.Comment{latestHistory, oldCommand}}
	service := newClaimService(githubAdapter, &fakeWorktree{workspace: gitadapter.Workspace{
		BaseSHA: "base", Branch: "factory/run-fixed", Worktree: "/worktree/run-fixed",
	}}, &fakeRunStore{}, validRepositoryConfig())

	claimed, err := service.ClaimIssue(context.Background(), issue.Number)
	if err != nil {
		t.Fatalf("ClaimIssue() error = %v", err)
	}
	if claimed.Run.ProcessedCommentID != latestHistory.ID {
		t.Fatalf("claim watermark = %q, want latest pre-claim comment %q", claimed.Run.ProcessedCommentID, latestHistory.ID)
	}

	githubAdapter.comments = append(githubAdapter.comments, newCommand)
	first, err := service.PollCommands(context.Background(), factory.CommandPollRequest{RunID: claimed.Run.ID})
	if err != nil {
		t.Fatalf("first PollCommands() error = %v", err)
	}
	if len(first) != 2 || first[0].Outcome != factory.CommandReplayed || first[1].Outcome != factory.CommandAccepted {
		t.Fatalf("first PollCommands() = %#v, want one replayed pre-claim and one accepted post-claim command", first)
	}
	if first[1].Run.Status != store.StatusActive || first[1].Run.LastCommandName != "status" {
		t.Fatalf("post-claim command run = %#v, want active status projection", first[1].Run)
	}
	if len(githubAdapter.replacedLabels) != 1 || githubAdapter.replacedLabels[0] != github.LabelAgentRunning {
		t.Fatalf("label mutations = %#v, want the claim label only", githubAdapter.replacedLabels)
	}
	if len(githubAdapter.editedComments) != 1 {
		t.Fatalf("status comment edits = %d, want one post-claim command edit", len(githubAdapter.editedComments))
	}

	second, err := service.PollCommands(context.Background(), factory.CommandPollRequest{RunID: claimed.Run.ID})
	if err != nil {
		t.Fatalf("second PollCommands() error = %v", err)
	}
	if len(second) != 2 || second[0].Outcome != factory.CommandReplayed || second[1].Outcome != factory.CommandReplayed {
		t.Fatalf("second PollCommands() = %#v, want both comments replayed", second)
	}
	if len(githubAdapter.editedComments) != 1 {
		t.Fatalf("status comment edits after replay = %d, want no duplicate edit", len(githubAdapter.editedComments))
	}
}

// TestIssueClaimFailsClosedWhenCommentHistoryCannotBeRead verifies the claim
// does not create a run when it cannot establish the command cutoff.
func TestIssueClaimFailsClosedWhenCommentHistoryCannotBeRead(t *testing.T) {
	t.Parallel()

	githubAdapter := &fakeGitHub{
		issueValue:  github.Issue{Number: 42, State: "open", Labels: []string{github.LabelAgentReady}},
		commentsErr: errors.New("GitHub comments unavailable"),
	}
	worktree := &fakeWorktree{workspace: gitadapter.Workspace{
		BaseSHA: "base", Branch: "factory/run-fixed", Worktree: "/worktree/run-fixed",
	}}
	runStore := &fakeRunStore{}
	service := newClaimService(githubAdapter, worktree, runStore, validRepositoryConfig())

	_, err := service.ClaimIssue(context.Background(), 42)
	if err == nil || !strings.Contains(err.Error(), "before claim") || !strings.Contains(err.Error(), "GitHub comments unavailable") {
		t.Fatalf("ClaimIssue() error = %v, want comment-cutoff failure", err)
	}
	if worktree.called || len(runStore.saved) != 0 || len(githubAdapter.replacedLabels) != 0 || len(githubAdapter.createdComments) != 0 {
		t.Fatalf("claim effects = workspace=%t/runs=%d/labels=%d/comments=%d, want none", worktree.called, len(runStore.saved), len(githubAdapter.replacedLabels), len(githubAdapter.createdComments))
	}
}

// TestIssueRefusesClosedOrUnauthorizedIssuesBeforeCreatingAWorkspace verifies
// eligibility is checked before any host or GitHub mutation.
func TestIssueRefusesClosedOrUnauthorizedIssuesBeforeCreatingAWorkspace(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		state         string
		labels        []string
		isPullRequest bool
		issueErr      error
		message       string
	}{
		{name: "closed", state: "closed", labels: []string{github.LabelAgentReady}, message: "only open issues"},
		{name: "not ready", state: "open", labels: []string{"enhancement"}, message: "not labeled"},
		{name: "pull request", state: "open", labels: []string{github.LabelAgentReady}, isPullRequest: true, message: "pull requests cannot be claimed"},
		{name: "conflicting factory label", state: "open", labels: []string{github.LabelAgentReady, github.LabelAgentRunning}, message: "another factory state label"},
		{name: "GitHub issue lookup", state: "open", labels: []string{github.LabelAgentReady}, issueErr: errors.New("GitHub unavailable"), message: "GitHub unavailable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			githubAdapter := &fakeGitHub{issueValue: github.Issue{Number: 42, State: tc.state, Labels: tc.labels, IsPullRequest: tc.isPullRequest}, issueErr: tc.issueErr}
			worktree := &fakeWorktree{}
			runStore := &fakeRunStore{}
			service := newClaimService(githubAdapter, worktree, runStore, validRepositoryConfig())
			_, err := service.ClaimIssue(context.Background(), 42)
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("Issue() error = %v, want %q", err, tc.message)
			}
			if worktree.called {
				t.Fatal("workspace was created for an ineligible issue")
			}
			if len(githubAdapter.replacedLabels) != 0 || len(githubAdapter.createdComments) != 0 {
				t.Fatalf("GitHub mutations = labels %#v/comments %#v, want none", githubAdapter.replacedLabels, githubAdapter.createdComments)
			}
			if len(runStore.saved) != 0 {
				t.Fatalf("saved runs = %#v, want none", runStore.saved)
			}
		})
	}
}

// TestClaimFailureMarksTheIssueFailed verifies the coordinator compensates a
// successful running-label mutation when status-comment creation fails.
func TestClaimFailureMarksTheIssueFailed(t *testing.T) {
	t.Parallel()

	githubAdapter := &fakeGitHub{
		issueValue:       github.Issue{Number: 42, State: "open", Labels: []string{github.LabelAgentReady}},
		createCommentErr: errors.New("GitHub unavailable"),
	}
	runStore := &fakeRunStore{}
	worktree := &fakeWorktree{workspace: gitadapter.Workspace{BaseSHA: "base", Branch: "factory/run-fixed", Worktree: "/worktree/run-fixed"}}
	service := newClaimService(githubAdapter, worktree, runStore, validRepositoryConfig())
	_, err := service.ClaimIssue(context.Background(), 42)
	if err == nil || !strings.Contains(err.Error(), "GitHub unavailable") {
		t.Fatalf("ClaimIssue() error = %v, want comment failure", err)
	}
	if got, want := githubAdapter.replacedHistory[len(githubAdapter.replacedHistory)-1], []string{github.LabelAgentFailed}; !equalStrings(got, want) {
		t.Fatalf("final labels = %#v, want %#v", got, want)
	}
	if got := runStore.saved[len(runStore.saved)-1].Status; got != store.StatusFailed {
		t.Fatalf("persisted failure status = %q, want failed", got)
	}
	if !worktree.removed {
		t.Fatal("failed claim did not remove its workspace")
	}
}

// TestClaimRetriesAndCompensatesWhenTheFinalPersistenceFails verifies a
// transient save is retried and a persistent save failure leaves a failed,
// recoverable supervision state.
func TestClaimRetriesAndCompensatesWhenTheFinalPersistenceFails(t *testing.T) {
	t.Parallel()

	githubAdapter := &fakeGitHub{issueValue: github.Issue{Number: 42, State: "open", Labels: []string{github.LabelAgentReady}}}
	runStore := &fakeRunStore{saveErrors: []error{nil, errors.New("store unavailable"), errors.New("store unavailable"), nil}}
	worktree := &fakeWorktree{workspace: gitadapter.Workspace{BaseSHA: "base", Branch: "factory/run-fixed", Worktree: "/worktree/run-fixed"}}
	service := newClaimService(githubAdapter, worktree, runStore, validRepositoryConfig())
	_, err := service.ClaimIssue(context.Background(), 42)
	if err == nil || !strings.Contains(err.Error(), "store unavailable") {
		t.Fatalf("ClaimIssue() error = %v, want persistent save failure", err)
	}
	if got := runStore.saved[len(runStore.saved)-1].Status; got != store.StatusFailed {
		t.Fatalf("persisted status = %q, want failed", got)
	}
	if got, want := githubAdapter.replacedHistory[len(githubAdapter.replacedHistory)-1], []string{github.LabelAgentFailed}; !equalStrings(got, want) {
		t.Fatalf("final labels = %#v, want %#v", got, want)
	}
	if len(githubAdapter.editedComments) != 1 {
		t.Fatalf("edited comments = %d, want one failure update", len(githubAdapter.editedComments))
	}
	if !worktree.removed {
		t.Fatal("failed claim did not remove its workspace")
	}
}

// TestClaimSucceedsAfterRetryingFinalPersistence verifies a transient final
// save failure does not turn an otherwise successful claim into a failure.
func TestClaimSucceedsAfterRetryingFinalPersistence(t *testing.T) {
	t.Parallel()

	githubAdapter := &fakeGitHub{issueValue: github.Issue{Number: 42, State: "open", Labels: []string{github.LabelAgentReady}}}
	runStore := &fakeRunStore{saveErrors: []error{nil, errors.New("store unavailable"), nil}}
	service := newClaimService(githubAdapter, &fakeWorktree{workspace: gitadapter.Workspace{BaseSHA: "base", Branch: "factory/run-fixed", Worktree: "/worktree/run-fixed"}}, runStore, validRepositoryConfig())
	result, err := service.ClaimIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("ClaimIssue() error = %v", err)
	}
	if result.Run.StatusCommentID != "comment-1" {
		t.Fatalf("status comment id = %q, want comment-1", result.Run.StatusCommentID)
	}
	if len(runStore.saved) != 2 {
		t.Fatalf("SaveRun calls = %d, want initial save plus successful retry", len(runStore.saved))
	}
}

// TestClaimRemovesWorkspaceWhenInitialPersistenceFails verifies a run that
// never reaches the external transition is cleaned up immediately.
func TestClaimRemovesWorkspaceWhenInitialPersistenceFails(t *testing.T) {
	t.Parallel()

	githubAdapter := &fakeGitHub{issueValue: github.Issue{Number: 42, State: "open", Labels: []string{github.LabelAgentReady}}}
	worktree := &fakeWorktree{
		workspace: gitadapter.Workspace{BaseSHA: "base", Branch: "factory/run-fixed", Worktree: "/worktree/run-fixed"},
	}
	runStore := &fakeRunStore{saveErrors: []error{errors.New("unique active run")}}
	service := newClaimService(githubAdapter, worktree, runStore, validRepositoryConfig())
	_, err := service.ClaimIssue(context.Background(), 42)
	if err == nil || !strings.Contains(err.Error(), "unique active run") {
		t.Fatalf("ClaimIssue() error = %v, want persistence failure", err)
	}
	if !worktree.removed {
		t.Fatal("initial persistence failure did not remove workspace")
	}
	if len(githubAdapter.replacedHistory) != 0 || len(githubAdapter.createdComments) != 0 {
		t.Fatal("claim mutated GitHub before initial persistence succeeded")
	}
}

// TestClaimPreservesTheOriginalErrorWhenWorkspaceCleanupFails verifies
// cleanup diagnostics are joined without replacing the claim failure.
func TestClaimPreservesTheOriginalErrorWhenWorkspaceCleanupFails(t *testing.T) {
	t.Parallel()

	githubAdapter := &fakeGitHub{
		issueValue:       github.Issue{Number: 42, State: "open", Labels: []string{github.LabelAgentReady}},
		createCommentErr: errors.New("GitHub unavailable"),
	}
	worktree := &fakeWorktree{
		workspace: gitadapter.Workspace{BaseSHA: "base", Branch: "factory/run-fixed", Worktree: "/worktree/run-fixed"},
		removeErr: errors.New("cleanup unavailable"),
	}
	runStore := &fakeRunStore{}
	service := newClaimService(githubAdapter, worktree, runStore, validRepositoryConfig())
	_, err := service.ClaimIssue(context.Background(), 42)
	if err == nil || !strings.Contains(err.Error(), "GitHub unavailable") || !strings.Contains(err.Error(), "cleanup unavailable") {
		t.Fatalf("ClaimIssue() error = %v, want claim and cleanup failures", err)
	}
}

// TestTransitionEditsThePersistedStatusCommentAndKeepsStageAndStatusSeparate
// verifies the existing comment identity is reused for state updates.
func TestTransitionEditsThePersistedStatusCommentAndKeepsStageAndStatusSeparate(t *testing.T) {
	t.Parallel()

	runStore := &fakeRunStore{}
	githubAdapter := &fakeGitHub{issueValue: github.Issue{Number: 42, State: "open", Labels: []string{"enhancement", github.LabelAgentReady}}}
	worktree := &fakeWorktree{workspace: gitadapter.Workspace{BaseSHA: "base", Branch: "factory/run-fixed", Worktree: "/worktree/run-fixed"}}
	service := newClaimService(githubAdapter, worktree, runStore, validRepositoryConfig())
	if _, err := service.ClaimIssue(context.Background(), 42); err != nil {
		t.Fatalf("ClaimIssue() fixture setup error = %v", err)
	}
	runStore.saved[len(runStore.saved)-1].TestStageSkipped = true
	runStore.saved[len(runStore.saved)-1].TestExemption = &store.TestExemption{Kind: "human", Justification: "transition seam fixture"}
	githubAdapter.createdComments = nil

	got, err := service.Transition(context.Background(), factory.TransitionRequest{
		RunID:  "run-fixed",
		Stage:  store.StageImplementation,
		Status: store.StatusWaitingForHuman,
	})
	if err != nil {
		t.Fatalf("Transition() error = %v", err)
	}
	if got.Stage != store.StageImplementation || got.Status != store.StatusWaitingForHuman {
		t.Fatalf("state = stage %q/status %q, want implementation/waiting_for_human", got.Stage, got.Status)
	}
	if len(githubAdapter.createdComments) != 0 {
		t.Fatalf("created comments = %#v, want none", githubAdapter.createdComments)
	}
	if len(githubAdapter.editedComments) != 1 || githubAdapter.editedComments[0].id != "comment-1" {
		t.Fatalf("edited comments = %#v, want comment-1", githubAdapter.editedComments)
	}
	if gotLabels, want := githubAdapter.replacedLabels, []string{"enhancement", github.LabelAgentNeedsInput}; !equalStrings(gotLabels, want) {
		t.Fatalf("labels = %#v, want %#v", gotLabels, want)
	}
	if !strings.Contains(githubAdapter.editedComments[0].body, "stage: `implementation`") || !strings.Contains(githubAdapter.editedComments[0].body, "status: `waiting_for_human`") {
		t.Fatalf("edited status comment = %q, want orthogonal stage and status", githubAdapter.editedComments[0].body)
	}
}

// TestTransitionRecoversACommentIdentityAfterAnInterruptedPersistence verifies
// a later transition edits the original comment instead of creating another.
func TestTransitionRecoversACommentIdentityAfterAnInterruptedPersistence(t *testing.T) {
	t.Parallel()

	runStore := &fakeRunStore{}
	githubAdapter := &fakeGitHub{
		issueValue:    github.Issue{Number: 42, State: "open", Labels: []string{github.LabelAgentReady}},
		statusComment: github.Comment{ID: "comment-recovered", Body: "old status"},
	}
	worktree := &fakeWorktree{workspace: gitadapter.Workspace{BaseSHA: "base", Branch: "factory/run-fixed", Worktree: "/worktree/run-fixed"}}
	service := newClaimService(githubAdapter, worktree, runStore, validRepositoryConfig())
	if _, err := service.ClaimIssue(context.Background(), 42); err != nil {
		t.Fatalf("ClaimIssue() fixture setup error = %v", err)
	}
	runStore.saved[len(runStore.saved)-1].TestStageSkipped = true
	runStore.saved[len(runStore.saved)-1].TestExemption = &store.TestExemption{Kind: "human", Justification: "transition seam fixture"}
	runStore.saved[len(runStore.saved)-1].StatusCommentID = ""

	got, err := service.Transition(context.Background(), factory.TransitionRequest{Stage: store.StageImplementation, Status: store.StatusActive})
	if err != nil {
		t.Fatalf("Transition() error = %v", err)
	}
	if githubAdapter.findCommentCalls != 1 {
		t.Fatalf("FindStatusComment calls = %d, want one recovery lookup", githubAdapter.findCommentCalls)
	}
	if got.StatusCommentID != "comment-recovered" {
		t.Fatalf("recovered comment id = %q, want comment-recovered", got.StatusCommentID)
	}
	if len(githubAdapter.editedComments) != 1 || githubAdapter.editedComments[0].id != "comment-recovered" {
		t.Fatalf("edited comments = %#v, want recovered comment", githubAdapter.editedComments)
	}
}

// TestBootstrapLabelsIsTheExplicitLabelCreationPath verifies label creation is
// explicit and covers the complete factory-owned label set.
func TestBootstrapLabelsIsTheExplicitLabelCreationPath(t *testing.T) {
	t.Parallel()

	githubAdapter := &fakeGitHub{issueValue: github.Issue{Number: 42, State: "open", Labels: []string{github.LabelAgentReady}}}
	service := newClaimService(githubAdapter, &fakeWorktree{}, &fakeRunStore{}, validRepositoryConfig())
	result, err := service.BootstrapLabels(context.Background())
	if err != nil {
		t.Fatalf("BootstrapLabels() error = %v", err)
	}
	if len(githubAdapter.createdLabels) != 6 || !equalStrings(result.Labels, github.FactoryStateLabels) {
		t.Fatalf("created/result labels = %#v/%#v, want all factory labels %#v", githubAdapter.createdLabels, result.Labels, github.FactoryStateLabels)
	}
}

// TestClaimIssueRefusesAnAdapterWithoutInteractiveResumeBeforeGitHubEffects
// verifies the startup capability guard runs before an issue is read or a
// worktree is created.
func TestClaimIssueRefusesAnAdapterWithoutInteractiveResumeBeforeGitHubEffects(t *testing.T) {
	t.Parallel()

	githubAdapter := &fakeGitHub{issueValue: github.Issue{Number: 42, State: "open", Labels: []string{github.LabelAgentReady}}}
	worktree := &fakeWorktree{workspace: gitadapter.Workspace{BaseSHA: factoryGateCheckpoint, Branch: "factory/run-fixed", Worktree: "/worktree/run-fixed"}}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config: &fakeConfig{value: config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{
			Path: "/repo", GitHub: config.GitHubConfig{Owner: "example", Repository: "project"},
			AuthorizedUsers: []string{"alice"}, Polling: config.PollingConfig{Interval: "30s", Backoff: "5m"},
			OperationalDataPath: "/outside/factory.db", RepositoryConfigPath: "/repo/factory.yaml",
		}}}},
		OpenStore:      func(context.Context, string) (factory.OperationalStore, error) { return &fakeRunStore{}, nil },
		LoadRepository: func(string) (config.RepositoryConfig, error) { return validRepositoryConfig(), nil },
		GitHub:         githubAdapter,
		Worktree:       worktree,
		HarnessCapabilities: func(string) (harness.Capabilities, error) {
			return harness.Capabilities{Name: harness.NameCodex}, nil
		},
	})

	_, err := service.ClaimIssue(context.Background(), 42)
	if err == nil || !strings.Contains(err.Error(), "interactive resume") {
		t.Fatalf("ClaimIssue() error = %v, want interactive-resume refusal", err)
	}
	if githubAdapter.issueCalls != 0 || worktree.called {
		t.Fatalf("claim effects: issue calls=%d worktree=%t, want none", githubAdapter.issueCalls, worktree.called)
	}
}

// newClaimService constructs a deterministic service for claim seam tests.
func newClaimService(githubAdapter *fakeGitHub, worktree *fakeWorktree, runStore *fakeRunStore, repositoryConfig config.RepositoryConfig) *factory.Service {
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{
		Path:                 "/repo",
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		AuthorizedUsers:      []string{"alice"},
		OperationalDataPath:  "/outside/factory.db",
		RepositoryConfigPath: "/repo/factory.yaml",
	}}}
	return factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config: &fakeConfig{value: host},
		OpenStore: func(context.Context, string) (factory.OperationalStore, error) {
			return runStore, nil
		},
		LoadRepository: func(string) (config.RepositoryConfig, error) { return repositoryConfig, nil },
		GitHub:         githubAdapter,
		Worktree:       worktree,
		Now: func() time.Time {
			return time.Date(2026, 8, 20, 10, 11, 12, 0, time.UTC)
		},
		NewRunID:    func() (string, error) { return "run-fixed", nil },
		Coordinator: "coordinator-test",
	})
}

// validRepositoryConfig returns the smallest valid repository policy used by
// claim tests.
func validRepositoryConfig() config.RepositoryConfig {
	return config.RepositoryConfig{
		SchemaVersion:          1,
		TargetBranch:           "main",
		Setup:                  "go mod download",
		SetupEnvironmentPolicy: config.EnvironmentPolicyClean,
		Gates: []config.GateConfig{{
			Name:              "test",
			Command:           "go test ./...",
			Timeout:           "5m",
			Blocking:          true,
			EnvironmentPolicy: config.EnvironmentPolicyClean,
		}},
		RoleHarnessDefaults: map[string]config.Harness{"test": config.HarnessCodex, "implementation": config.HarnessCodex, "architecture": config.HarnessCodex},
		ModelOptions:        map[string][]string{"test": {"gpt-5"}, "implementation": {"gpt-5"}, "architecture": {"gpt-5"}},
		Timeouts:            config.TimeoutConfig{Setup: "5m", Agent: "30m", Gate: "5m", Review: "10m"},
		RetryLimits:         config.RetryLimits{CheckRepair: 3, ReviewRepair: 2, TestRevision: 2},
		TestPolicy:          config.TestPolicy{Mode: config.TestModeRequired},
		AllowedOverrides:    []config.OverrideName{config.OverrideModel},
		WorkerBuild: config.WorkerBuildConfig{
			Image:      "ghcr.io/example/factory-worker",
			Digest:     "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			Definition: "worker/Dockerfile",
		},
		BaseSynchronization: config.BaseSynchronization{Mode: config.BaseSynchronizationNever},
	}
}

// fakeGitHub records the GitHub mutations made by the coordinator.
type fakeGitHub struct {
	issueValue       github.Issue
	issueErr         error
	issueCalls       int
	createdLabels    []github.Label
	replacedLabels   []string
	replacedHistory  [][]string
	createdComments  []string
	editedComments   []editedComment
	createCommentErr error
	statusComment    github.Comment
	findCommentCalls int
	// comments contains the issue history returned to command polling.
	comments []github.Comment
	// commentsErr simulates a failure while establishing the claim cutoff.
	commentsErr error
}

// Issue returns the configured issue snapshot.
func (f *fakeGitHub) Issue(context.Context, github.Repository, int) (github.Issue, error) {
	f.issueCalls++
	if f.issueErr != nil {
		return github.Issue{}, f.issueErr
	}
	return f.issueValue, nil
}

// CreateLabel records explicit label bootstrap calls.
func (f *fakeGitHub) CreateLabel(_ context.Context, _ github.Repository, label github.Label) error {
	f.createdLabels = append(f.createdLabels, label)
	return nil
}

// ReplaceIssueLabels records the complete issue label set on each mutation.
func (f *fakeGitHub) ReplaceIssueLabels(_ context.Context, _ github.Repository, _ int, labels []string) error {
	f.replacedLabels = append([]string(nil), labels...)
	f.replacedHistory = append(f.replacedHistory, append([]string(nil), labels...))
	f.issueValue.Labels = append([]string(nil), labels...)
	return nil
}

// CreateIssueComment records the one-comment claim path.
func (f *fakeGitHub) CreateIssueComment(_ context.Context, _ github.Repository, _ int, body string) (github.Comment, error) {
	if f.createCommentErr != nil {
		return github.Comment{}, f.createCommentErr
	}
	f.createdComments = append(f.createdComments, body)
	return github.Comment{ID: "comment-1", Body: body}, nil
}

// FindStatusComment returns the configured recoverable status comment.
func (f *fakeGitHub) FindStatusComment(_ context.Context, _ github.Repository, _ int, _ string) (github.Comment, error) {
	f.findCommentCalls++
	return f.statusComment, nil
}

// EditIssueComment records edits to the persisted status comment.
func (f *fakeGitHub) EditIssueComment(_ context.Context, _ github.Repository, id, body string) error {
	f.editedComments = append(f.editedComments, editedComment{id: id, body: body})
	return nil
}

// IssueComments returns the issue comments visible to command polling.
func (f *fakeGitHub) IssueComments(context.Context, github.Repository, int) ([]github.Comment, error) {
	if f.commentsErr != nil {
		return nil, f.commentsErr
	}
	return append([]github.Comment(nil), f.comments...), nil
}

// editedComment records one status-comment edit.
type editedComment struct {
	id   string
	body string
}

// fakeWorktree returns a deterministic isolated workspace.
type fakeWorktree struct {
	workspace gitadapter.Workspace
	guidance  []gitadapter.GuidanceDocument
	called    bool
	removed   bool
	removeErr error
}

// Create records a worktree request and returns the configured workspace.
func (f *fakeWorktree) Create(context.Context, string, string, string) (gitadapter.Workspace, error) {
	f.called = true
	if f.workspace.Branch == "" {
		return gitadapter.Workspace{}, errors.New("unexpected workspace call")
	}
	return f.workspace, nil
}

// ReadRepositoryGuidance returns the documents configured for the exact base
// checkpoint requested by the claim test.
func (f *fakeWorktree) ReadRepositoryGuidance(context.Context, string, string) ([]gitadapter.GuidanceDocument, error) {
	return append([]gitadapter.GuidanceDocument(nil), f.guidance...), nil
}

// Remove records cleanup of a created workspace.
func (f *fakeWorktree) Remove(context.Context, string, gitadapter.Workspace) error {
	f.removed = true
	return f.removeErr
}

// fakeRunStore keeps run records in insertion order for coordinator tests.
type fakeRunStore struct {
	saved           []store.Run
	saveErrors      []error
	lifecycleClaims map[string]map[store.Status]bool
}

// CurrentRun returns the newest non-terminal record.
func (f *fakeRunStore) CurrentRun(context.Context) (*store.Run, error) {
	for index := len(f.saved) - 1; index >= 0; index-- {
		run := f.saved[index]
		if run.Status == store.StatusComplete || run.Status == store.StatusCancelled || run.Status == store.StatusFailed {
			continue
		}
		return &run, nil
	}
	return nil, nil
}

// SaveRun appends a persisted run record.
func (f *fakeRunStore) SaveRun(_ context.Context, run store.Run) error {
	if len(f.saveErrors) > 0 {
		err := f.saveErrors[0]
		f.saveErrors = f.saveErrors[1:]
		if err != nil {
			return err
		}
	}
	f.saved = append(f.saved, run)
	return nil
}

// ClaimLifecycleNotification atomically claims notification delivery.
func (f *fakeRunStore) ClaimLifecycleNotification(_ context.Context, runID string, terminalStatus store.Status) (bool, error) {
	if f.lifecycleClaims == nil {
		f.lifecycleClaims = make(map[string]map[store.Status]bool)
	}
	if f.lifecycleClaims[runID] == nil {
		f.lifecycleClaims[runID] = make(map[store.Status]bool)
	}
	if f.lifecycleClaims[runID][terminalStatus] {
		return false, nil
	}
	f.lifecycleClaims[runID][terminalStatus] = true
	return true, nil
}

// ReleaseLifecycleNotification removes a notification claim.
func (f *fakeRunStore) ReleaseLifecycleNotification(_ context.Context, runID string, terminalStatus store.Status) error {
	if f.lifecycleClaims != nil && f.lifecycleClaims[runID] != nil {
		delete(f.lifecycleClaims[runID], terminalStatus)
	}
	return nil
}

// Close satisfies the operational-store seam.
func (f *fakeRunStore) Close() error { return nil }

// fakeConfig supplies the registered repository to the coordinator tests.
type fakeConfig struct {
	value config.HostConfig
}

// Load returns the configured host registration.
func (f *fakeConfig) Load(string) (config.HostConfig, error) { return f.value, nil }

// Save is unused by claim tests and succeeds for interface completeness.
func (f *fakeConfig) Save(string, config.HostConfig) error { return nil }

// Create is unused by claim tests and returns the configured host.
func (f *fakeConfig) Create(string) (config.HostConfig, error) {
	return f.value, nil
}

// equalStrings compares ordered label sets for test assertions.
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
