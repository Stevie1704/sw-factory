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
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestHandleCommandAcceptsAuthorizedStatusAndPersistsTheWatermark verifies
// the public command seam and the single-comment supervision projection.
func TestHandleCommandAcceptsAuthorizedStatusAndPersistsTheWatermark(t *testing.T) {
	t.Parallel()

	run := commandRun(t, store.StatusActive)
	githubAdapter := &commandGitHub{issue: github.Issue{Number: 42, State: "open", Labels: []string{github.LabelAgentRunning}}, statusComment: github.Comment{ID: "status-1"}}
	runStore := &commandRunStore{current: &run, latest: &run}
	service := newCommandService(runStore, githubAdapter, nil)

	result, err := service.HandleCommand(context.Background(), factory.CommandRequest{
		IssueNumber: 42,
		Comment:     github.Comment{ID: "10", Author: "alice", Body: "/factory status"},
	})
	if err != nil {
		t.Fatalf("HandleCommand() error = %v", err)
	}
	if result.Outcome != factory.CommandAccepted || result.Run.ProcessedCommentID != "10" || result.Run.Revision != 1 {
		t.Fatalf("result = %#v, want accepted revision-one watermark", result)
	}
	if len(githubAdapter.replacedLabels) != 0 {
		t.Fatalf("label mutations = %#v, want none for status", githubAdapter.replacedLabels)
	}
	if len(githubAdapter.editedComments) != 1 || !strings.Contains(githubAdapter.editedComments[0].body, "- command: `status`") || !strings.Contains(githubAdapter.editedComments[0].body, "- outcome: `accepted`") {
		t.Fatalf("status comment edits = %#v, want one accepted status projection", githubAdapter.editedComments)
	}
}

// TestHandleCommandRejectsAnUnauthorizedAuthorWithoutWorkflowMutation
// verifies that authorization is checked before retry or configuration logic.
func TestHandleCommandRejectsAnUnauthorizedAuthorWithoutWorkflowMutation(t *testing.T) {
	t.Parallel()

	run := commandRun(t, store.StatusActive)
	githubAdapter := &commandGitHub{issue: github.Issue{Number: 42, State: "open", Labels: []string{github.LabelAgentRunning}}, statusComment: github.Comment{ID: "status-1"}}
	runStore := &commandRunStore{current: &run, latest: &run}
	service := newCommandService(runStore, githubAdapter, nil)

	result, err := service.HandleCommand(context.Background(), factory.CommandRequest{
		IssueNumber: 42,
		Comment:     github.Comment{ID: "11", Author: "mallory", Body: "/factory retry"},
	})
	var rejection *factory.PolicyRejection
	if !errors.As(err, &rejection) || rejection.Code != factory.PolicyRejectionUnauthorized {
		t.Fatalf("HandleCommand() error = %v, want unauthorized PolicyRejection", err)
	}
	if result.Outcome != factory.CommandRejected || result.Run.Status != store.StatusActive {
		t.Fatalf("result = %#v, want rejected active run", result)
	}
	if len(githubAdapter.replacedLabels) != 0 {
		t.Fatalf("label mutations = %#v, want none for unauthorized command", githubAdapter.replacedLabels)
	}
	if len(githubAdapter.editedComments) != 1 || !strings.Contains(githubAdapter.editedComments[0].body, "unauthorized") {
		t.Fatalf("status comment edits = %#v, want visible rejection", githubAdapter.editedComments)
	}
}

// TestHandleCommandNeverReexecutesAnEditedComment verifies the immutable ID
// watermark wins over the edited body and current author.
func TestHandleCommandNeverReexecutesAnEditedComment(t *testing.T) {
	t.Parallel()

	run := commandRun(t, store.StatusActive)
	githubAdapter := &commandGitHub{issue: github.Issue{Number: 42, State: "open", Labels: []string{github.LabelAgentRunning}}, statusComment: github.Comment{ID: "status-1"}}
	runStore := &commandRunStore{current: &run, latest: &run}
	service := newCommandService(runStore, githubAdapter, nil)

	first, err := service.HandleCommand(context.Background(), factory.CommandRequest{IssueNumber: 42, Comment: github.Comment{ID: "12", Author: "alice", Body: "/factory status"}})
	if err != nil || first.Outcome != factory.CommandAccepted {
		t.Fatalf("first HandleCommand() = %#v/%v, want accepted", first, err)
	}
	second, err := service.HandleCommand(context.Background(), factory.CommandRequest{IssueNumber: 42, Comment: github.Comment{ID: "12", Author: "alice", Body: "/factory retry"}})
	if err != nil {
		t.Fatalf("edited HandleCommand() error = %v", err)
	}
	if second.Outcome != factory.CommandReplayed || second.Run.Status != store.StatusActive || len(githubAdapter.replacedLabels) != 0 {
		t.Fatalf("edited command result = %#v, label mutations = %#v, want replay with no workflow effect", second, githubAdapter.replacedLabels)
	}
	if len(githubAdapter.editedComments) != 1 {
		t.Fatalf("status comment edits = %d, want one execution", len(githubAdapter.editedComments))
	}
}

// TestHandleCommandRetriesAFailedRun verifies the ticketed retry transition
// reuses the existing status comment and does not create a second one.
func TestHandleCommandRetriesAFailedRun(t *testing.T) {
	t.Parallel()

	run := commandRun(t, store.StatusFailed)
	githubAdapter := &commandGitHub{issue: github.Issue{Number: 42, State: "open", Labels: []string{github.LabelAgentFailed}}, statusComment: github.Comment{ID: "status-1"}}
	runStore := &commandRunStore{latest: &run}
	service := newCommandService(runStore, githubAdapter, nil)

	result, err := service.HandleCommand(context.Background(), factory.CommandRequest{IssueNumber: 42, Comment: github.Comment{ID: "13", Author: "alice", Body: "/factory retry"}})
	if err != nil {
		t.Fatalf("HandleCommand() error = %v", err)
	}
	if result.Outcome != factory.CommandAccepted || result.Run.Status != store.StatusActive || result.Run.ProcessedCommentID != "13" {
		t.Fatalf("result = %#v, want active retried run", result)
	}
	if len(githubAdapter.createdComments) != 0 || len(githubAdapter.editedComments) != 1 {
		t.Fatalf("comment mutations = created=%d edited=%d, want edit only", len(githubAdapter.createdComments), len(githubAdapter.editedComments))
	}
	if got, want := githubAdapter.replacedLabels, []string{github.LabelAgentRunning}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("labels = %#v, want %#v", got, want)
	}
}

// TestHandleCommandPersistsHarnessConfiguration verifies authorized policy
// configuration is stored for a later invocation rather than changing the
// frozen specification packet.
func TestHandleCommandPersistsHarnessConfiguration(t *testing.T) {
	t.Parallel()

	run := commandRun(t, store.StatusActive)
	githubAdapter := &commandGitHub{issue: github.Issue{Number: 42, State: "open", Labels: []string{github.LabelAgentRunning}}, statusComment: github.Comment{ID: "status-1"}}
	runStore := &commandRunStore{current: &run, latest: &run}
	service := newCommandService(runStore, githubAdapter, nil)

	result, err := service.HandleCommand(context.Background(), factory.CommandRequest{IssueNumber: 42, Comment: github.Comment{ID: "14", Author: "alice", Body: "/factory config harness=codex"}})
	if err != nil {
		t.Fatalf("HandleCommand() error = %v", err)
	}
	if result.Run.HarnessOverride != "codex" || result.Run.SpecificationPacket != run.SpecificationPacket {
		t.Fatalf("result = %#v, want harness override without packet mutation", result)
	}
}

// TestHandleCommandAmendsAReadyPullRequest verifies an authorized revision
// creates a new packet version, drafts the existing PR, invalidates the old
// evidence, and preserves the host-owned checkpoint identity.
func TestHandleCommandAmendsAReadyPullRequest(t *testing.T) {
	t.Parallel()

	run := commandRun(t, store.StatusActive)
	run.Stage = store.StageReady
	run.Branch = "factory/run-command"
	run.Worktree = "/tmp/factory-run-command"
	run.CheckpointSHA = strings.Repeat("a", 64)
	run.PullRequestNumber = 17
	run.PullRequestURL = "https://github.com/example/project/pull/17"
	githubAdapter := &commandGitHub{
		issue:         github.Issue{Number: 42, Title: "Amended issue", Body: "new requirements", State: "open", Labels: []string{github.LabelAgentRunning}},
		statusComment: github.Comment{ID: "status-1"},
		pullRequest:   github.PullRequest{Number: 17, URL: run.PullRequestURL, State: "open", Draft: false, HeadBranch: run.Branch, BaseBranch: "main"},
	}
	runStore := &commandRunStore{current: &run, latest: &run}
	service := newCommandService(runStore, githubAdapter, nil)

	result, err := service.HandleCommand(context.Background(), factory.CommandRequest{
		IssueNumber: 42,
		Comment:     github.Comment{ID: "16", Author: "alice", Body: "/factory revision"},
	})
	if err != nil {
		t.Fatalf("HandleCommand() error = %v", err)
	}
	if result.Outcome != factory.CommandAccepted || result.Run.Stage != store.StageClaim || result.Run.Status != store.StatusActive {
		t.Fatalf("revision result = %#v, want accepted active claim restart for the reduced command store", result)
	}
	if result.Run.ProcessedCommentID != "16" || result.Run.Revision != 1 || result.Run.ProcessedCommentRevision != 1 {
		t.Fatalf("revision watermark = %#v, want comment 16 at revision 1", result.Run)
	}
	if result.Run.Branch != run.Branch || result.Run.Worktree != run.Worktree || result.Run.CheckpointSHA != run.CheckpointSHA || result.Run.BaseCheckpointSHA != run.CheckpointSHA || result.Run.PullRequestNumber != run.PullRequestNumber {
		t.Fatalf("revision destroyed resumable host identity: before=%#v after=%#v", run, result.Run)
	}
	var packet factory.SpecificationPacket
	if err := json.Unmarshal([]byte(result.Run.SpecificationPacket), &packet); err != nil {
		t.Fatalf("decode amended packet: %v", err)
	}
	if packet.Version != 2 || packet.Issue.Body != "new requirements" {
		t.Fatalf("amended packet = %#v, want version 2 with current issue", packet)
	}
	if !githubAdapter.pullRequest.Draft || len(githubAdapter.draftChanges) != 1 || !githubAdapter.draftChanges[0] {
		t.Fatalf("pull request draft changes = %#v/%v, want one draft transition", githubAdapter.draftChanges, githubAdapter.pullRequest)
	}
	if len(runStore.invalidations) != 0 {
		t.Fatalf("downstream invalidations = %#v, want none for a complete amendment invalidation", runStore.invalidations)
	}
	if len(runStore.allInvalidations) != 1 || runStore.allInvalidations[0] != run.ID {
		t.Fatalf("complete invalidations = %#v, want one for %q", runStore.allInvalidations, run.ID)
	}
}

// TestHandleCommandPersistsAClaudeHarnessConfiguration verifies an authorized
// issue comment can select Claude Code now that the adapter exists, and that
// the run records the choice for the next invocation.
func TestHandleCommandPersistsAClaudeHarnessConfiguration(t *testing.T) {
	t.Parallel()

	run := commandRun(t, store.StatusActive)
	githubAdapter := &commandGitHub{issue: github.Issue{Number: 42, State: "open", Labels: []string{github.LabelAgentRunning}}, statusComment: github.Comment{ID: "status-1"}}
	runStore := &commandRunStore{current: &run, latest: &run}
	service := newCommandService(runStore, githubAdapter, nil)

	result, err := service.HandleCommand(context.Background(), factory.CommandRequest{IssueNumber: 42, Comment: github.Comment{ID: "14", Author: "alice", Body: "/factory config harness=claude"}})
	if err != nil {
		t.Fatalf("HandleCommand() error = %v", err)
	}
	if result.Outcome != factory.CommandAccepted || result.Run.HarnessOverride != "claude" {
		t.Fatalf("result = %#v, want an accepted Claude harness override", result)
	}
}

// TestHandleCommandRefusesIssueSuppliedProcessArguments verifies a comment
// cannot inject a flag or an extra argument into a launched harness process.
func TestHandleCommandRefusesIssueSuppliedProcessArguments(t *testing.T) {
	t.Parallel()

	for _, body := range []string{
		"/factory config harness=claude --dangerously-skip-permissions",
		"/factory config harness=claude extra",
		"/factory config --mcp-config",
		"/factory retry --model gpt-9",
		"/factory config model=gpt-9",
	} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()

			run := commandRun(t, store.StatusActive)
			githubAdapter := &commandGitHub{issue: github.Issue{Number: 42, State: "open", Labels: []string{github.LabelAgentRunning}}, statusComment: github.Comment{ID: "status-1"}}
			runStore := &commandRunStore{current: &run, latest: &run}
			service := newCommandService(runStore, githubAdapter, nil)

			result, err := service.HandleCommand(context.Background(), factory.CommandRequest{IssueNumber: 42, Comment: github.Comment{ID: "14", Author: "alice", Body: body}})
			if result.Outcome != factory.CommandRejected {
				t.Fatalf("outcome = %q, want a rejection of issue-supplied process arguments", result.Outcome)
			}
			var rejection *factory.PolicyRejection
			if !errors.As(err, &rejection) || rejection.Code != factory.PolicyRejectionMalformed {
				t.Fatalf("HandleCommand() error = %v, want a typed malformed-command rejection", err)
			}
			if result.Run.HarnessOverride != "" {
				t.Fatalf("harness override = %q, want no recorded configuration", result.Run.HarnessOverride)
			}
		})
	}
}

// TestPollCommandsSkipsThePersistedWatermark verifies polling sees issue
// comments while a second poll, including after a fresh service, is inert.
func TestPollCommandsSkipsThePersistedWatermark(t *testing.T) {
	t.Parallel()

	run := commandRun(t, store.StatusActive)
	githubAdapter := &commandGitHub{
		issue:         github.Issue{Number: 42, State: "open", Labels: []string{github.LabelAgentRunning}},
		statusComment: github.Comment{ID: "status-1"},
		comments:      []github.Comment{{ID: "15", Author: "alice", Body: "/factory refresh"}},
	}
	runStore := &commandRunStore{current: &run, latest: &run}
	service := newCommandService(runStore, githubAdapter, githubAdapter)

	first, err := service.PollCommands(context.Background(), factory.CommandPollRequest{RunID: run.ID})
	if err != nil || len(first) != 1 || first[0].Outcome != factory.CommandAccepted {
		t.Fatalf("first PollCommands() = %#v/%v, want one accepted command", first, err)
	}
	if runStore.closeCount != 1 {
		t.Fatalf("first PollCommands() close count = %d, want one close", runStore.closeCount)
	}
	second, err := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:    commandConfig{host: commandHost()},
		OpenStore: func(context.Context, string) (factory.OperationalStore, error) { return runStore, nil },
		GitHub:    githubAdapter,
		Comments:  githubAdapter,
		Now:       func() time.Time { return time.Date(2026, 8, 23, 10, 11, 12, 0, time.UTC) },
	}).PollCommands(context.Background(), factory.CommandPollRequest{RunID: run.ID})
	if err != nil {
		t.Fatalf("second PollCommands() error = %v", err)
	}
	if len(second) != 1 || second[0].Outcome != factory.CommandReplayed || len(githubAdapter.editedComments) != 1 {
		t.Fatalf("second PollCommands() = %#v, edits = %d, want replay without second edit", second, len(githubAdapter.editedComments))
	}
	if runStore.closeCount != 2 {
		t.Fatalf("second PollCommands() close count = %d, want one additional close", runStore.closeCount)
	}
}

// commandRun creates a persisted command fixture with a frozen valid packet.
func commandRun(t *testing.T, status store.Status) store.Run {
	t.Helper()
	packet, err := json.Marshal(factory.SpecificationPacket{Version: 1, RepositoryConfig: commandRepositoryConfig()})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 23, 10, 11, 12, 0, time.UTC)
	return store.Run{ID: "run-command", RepositoryPath: "/repo", IssueNumber: 42, Stage: store.StageImplementation, Status: status, StatusCommentID: "status-1", SpecificationPacket: string(packet), CreatedAt: now, UpdatedAt: now}
}

// commandRepositoryConfig returns the smallest policy needed by command tests.
func commandRepositoryConfig() config.RepositoryConfig {
	policy := validRepositoryConfig()
	policy.AllowedOverrides = append(policy.AllowedOverrides, config.OverrideHarness)
	policy.RoleHarnessDefaults = map[string]config.Harness{"implementation": config.HarnessCodex}
	return policy
}

// commandHost returns a single registered repository with one authorized user.
func commandHost() config.HostConfig {
	return config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{Path: "/repo", GitHub: config.GitHubConfig{Owner: "example", Repository: "project"}, AuthorizedUsers: []string{"alice"}, OperationalDataPath: "/outside/factory.db", RepositoryConfigPath: "/repo/factory.yaml"}}}
}

// newCommandService builds a service at the public command seam.
func newCommandService(runStore *commandRunStore, githubAdapter *commandGitHub, comments github.CommentReader) *factory.Service {
	return newCommandServiceWithStore(runStore, githubAdapter, comments)
}

// newCommandServiceWithStore builds the same command seam over any operational
// store, letting a test add the optional persistence seams it needs.
func newCommandServiceWithStore(runStore factory.OperationalStore, githubAdapter *commandGitHub, comments github.CommentReader) *factory.Service {
	dependencies := factory.Dependencies{
		Config:    commandConfig{host: commandHost()},
		OpenStore: func(context.Context, string) (factory.OperationalStore, error) { return runStore, nil },
		GitHub:    githubAdapter,
		Comments:  comments,
		Terminal:  &lifecycleTerminal{},
		Now:       func() time.Time { return time.Date(2026, 8, 23, 10, 11, 12, 0, time.UTC) },
	}
	return factory.NewWithDependencies("/host/config.yaml", dependencies)
}

// commandConfig is the read-only host configuration adapter for command tests.
type commandConfig struct{ host config.HostConfig }

// Load returns the configured host registration.
func (c commandConfig) Load(string) (config.HostConfig, error) { return c.host, nil }

// Save is not used by command handling.
func (c commandConfig) Save(string, config.HostConfig) error { return nil }

// Create is not used by command handling.
func (c commandConfig) Create(string) (config.HostConfig, error) { return c.host, nil }

// commandRunStore is a restartable in-memory store fixture.
type commandRunStore struct {
	current *store.Run
	latest  *store.Run
	saved   []store.Run
	// closeCount records store lifetime in polling tests.
	closeCount int
	// saveErrors allows tests to inject save failures.
	saveErrors []error
	// lifecycleClaims tracks claimed notification deliveries.
	lifecycleClaims map[string]map[store.Status]bool
	// allInvalidations records complete specification-amendment invalidations.
	allInvalidations []string
	// invalidations records downstream packet-change invalidations.
	invalidations []string
}

// CurrentRun returns the active run when one exists.
func (s *commandRunStore) CurrentRun(context.Context) (*store.Run, error) {
	if s.current == nil {
		return nil, nil
	}
	copy := *s.current
	return &copy, nil
}

// LatestRun returns the most recently claimed run, including terminal runs.
func (s *commandRunStore) LatestRun(context.Context) (*store.Run, error) {
	if s.latest == nil {
		return nil, nil
	}
	copy := *s.latest
	return &copy, nil
}

// SaveRun records the new current state and makes it visible after restart.
func (s *commandRunStore) SaveRun(_ context.Context, run store.Run) error {
	if len(s.saveErrors) > 0 {
		err := s.saveErrors[0]
		s.saveErrors = s.saveErrors[1:]
		if err != nil {
			return err
		}
	}
	s.saved = append(s.saved, run)
	copy := run
	s.latest = &copy
	if store.IsTerminalStatus(run.Status) {
		s.current = nil
	} else {
		s.current = &copy
	}
	return nil
}

// SaveRunIfRevision mirrors the SQLite compare-and-set seam in the command
// fixture so command tests exercise stale-revision rejection as well.
func (s *commandRunStore) SaveRunIfRevision(ctx context.Context, expected int64, run store.Run) error {
	if s.latest != nil && s.latest.Revision != expected {
		return store.ErrRevisionConflict
	}
	return s.SaveRun(ctx, run)
}

// InvalidateRunResults satisfies the packet-version seam for command tests;
// this reduced store has no invocation or gate-result tables to clear.
func (s *commandRunStore) InvalidateRunResults(_ context.Context, runID string) error {
	s.invalidations = append(s.invalidations, runID)
	return nil
}

// InvalidateAllRunResults satisfies the complete packet-amendment seam in the
// reduced command store, which has no separate invocation or gate tables.
func (s *commandRunStore) InvalidateAllRunResults(_ context.Context, runID string) error {
	s.allInvalidations = append(s.allInvalidations, runID)
	return nil
}

// ClaimLifecycleNotification atomically claims notification delivery.
func (s *commandRunStore) ClaimLifecycleNotification(_ context.Context, runID string, terminalStatus store.Status) (bool, error) {
	if s.lifecycleClaims == nil {
		s.lifecycleClaims = make(map[string]map[store.Status]bool)
	}
	if s.lifecycleClaims[runID] == nil {
		s.lifecycleClaims[runID] = make(map[store.Status]bool)
	}
	if s.lifecycleClaims[runID][terminalStatus] {
		return false, nil
	}
	s.lifecycleClaims[runID][terminalStatus] = true
	return true, nil
}

// ReleaseLifecycleNotification removes a notification claim.
func (s *commandRunStore) ReleaseLifecycleNotification(_ context.Context, runID string, terminalStatus store.Status) error {
	if s.lifecycleClaims != nil && s.lifecycleClaims[runID] != nil {
		delete(s.lifecycleClaims[runID], terminalStatus)
	}
	return nil
}

// Close increments the test store's close counter and returns nil, satisfying
// the operational-store seam.
func (s *commandRunStore) Close() error {
	s.closeCount++
	return nil
}

// commandGitHub records command-related effects.
type commandGitHub struct {
	issue           github.Issue
	comments        []github.Comment
	statusComment   github.Comment
	replacedLabels  []string
	createdComments []string
	editedComments  []commandEditedComment
	pullRequest     github.PullRequest
	draftChanges    []bool
}

// Issue returns the issue projection used by retry transitions.
func (g *commandGitHub) Issue(context.Context, github.Repository, int) (github.Issue, error) {
	return g.issue, nil
}

// CreateLabel is unused by command handling.
func (g *commandGitHub) CreateLabel(context.Context, github.Repository, github.Label) error {
	return nil
}

// ReplaceIssueLabels records the workflow label mutation.
func (g *commandGitHub) ReplaceIssueLabels(_ context.Context, _ github.Repository, _ int, labels []string) error {
	g.replacedLabels = append([]string(nil), labels...)
	g.issue.Labels = append([]string(nil), labels...)
	return nil
}

// CreateIssueComment records accidental duplicate status-comment creation.
func (g *commandGitHub) CreateIssueComment(_ context.Context, _ github.Repository, _ int, body string) (github.Comment, error) {
	g.createdComments = append(g.createdComments, body)
	return github.Comment{ID: "created-status", Body: body}, nil
}

// FindStatusComment returns the existing editable status comment.
func (g *commandGitHub) FindStatusComment(context.Context, github.Repository, int, string) (github.Comment, error) {
	return g.statusComment, nil
}

// EditIssueComment records one edit of the single status comment.
func (g *commandGitHub) EditIssueComment(_ context.Context, _ github.Repository, id, body string) error {
	g.editedComments = append(g.editedComments, commandEditedComment{id: id, body: body})
	return nil
}

// IssueComments supplies the comments observed by polling.
func (g *commandGitHub) IssueComments(context.Context, github.Repository, int) ([]github.Comment, error) {
	return append([]github.Comment(nil), g.comments...), nil
}

// FindPullRequest returns the tracked pull request used by revision commands.
func (g *commandGitHub) FindPullRequest(context.Context, github.Repository, string, string) (github.PullRequest, error) {
	return g.pullRequest, nil
}

// CreatePullRequest is unused by command tests but completes the pull-request
// client seam supplied by the command GitHub fixture.
func (g *commandGitHub) CreatePullRequest(context.Context, github.Repository, github.PullRequestRequest) (github.PullRequest, error) {
	return g.pullRequest, nil
}

// UpdatePullRequest is unused by revision handling because draft transitions
// use the dedicated readiness method.
func (g *commandGitHub) UpdatePullRequest(context.Context, github.Repository, int, github.PullRequestRequest) (github.PullRequest, error) {
	return g.pullRequest, nil
}

// SetPullRequestDraft records the explicit readiness mutation for revisions.
func (g *commandGitHub) SetPullRequestDraft(_ context.Context, _ github.Repository, _ int, draft bool) (github.PullRequest, error) {
	g.draftChanges = append(g.draftChanges, draft)
	g.pullRequest.Draft = draft
	return g.pullRequest, nil
}

// commandEditedComment records a status-comment edit.
type commandEditedComment struct {
	id   string
	body string
}

// TestHandleCommandRefreshKeepsAClaimStageRunAtClaim verifies a refresh before
// the frozen baseline ran leaves the run at claim. Only the claim stage runs
// the baseline suite, so a jump to a post-baseline stage would wedge the run on
// an incomplete baseline gate.
func TestHandleCommandRefreshKeepsAClaimStageRunAtClaim(t *testing.T) {
	t.Parallel()

	run := claimStageCommandRun(t, store.StatusActive)
	githubAdapter := &commandGitHub{issue: github.Issue{Number: 42, State: "open", Labels: []string{github.LabelAgentRunning}}, statusComment: github.Comment{ID: "status-1"}}
	runStore := &commandInvocationRunStore{commandRunStore: &commandRunStore{current: &run, latest: &run}}
	service := newCommandServiceWithStore(runStore, githubAdapter, nil)

	result, err := service.HandleCommand(context.Background(), factory.CommandRequest{
		IssueNumber: 42,
		Comment:     github.Comment{ID: "20", Author: "alice", Body: "/factory refresh"},
	})
	if err != nil {
		t.Fatalf("HandleCommand() error = %v", err)
	}
	if result.Outcome != factory.CommandAccepted || result.Run.Stage != store.StageClaim || result.Run.Status != store.StatusActive {
		t.Fatalf("result = %#v, want an accepted refresh that stays at the claim stage", result)
	}
	if len(runStore.invocations) != 0 {
		t.Fatalf("invocations started = %#v, want none before the baseline ran", runStore.invocations)
	}
}

// TestHandleCommandAnswerKeepsAClaimStageRunAtClaim verifies an answer that
// arrives before the frozen baseline ran resumes the run at claim rather than
// at a post-baseline stage it can never satisfy.
func TestHandleCommandAnswerKeepsAClaimStageRunAtClaim(t *testing.T) {
	t.Parallel()

	run := claimStageCommandRun(t, store.StatusWaitingForHuman)
	run.PendingQuestions = []store.PendingQuestion{{ID: "format", Prompt: "Which format should be used?"}}
	githubAdapter := &commandGitHub{issue: github.Issue{Number: 42, State: "open", Labels: []string{github.LabelAgentNeedsInput}}, statusComment: github.Comment{ID: "status-1"}}
	runStore := &commandInvocationRunStore{commandRunStore: &commandRunStore{current: &run, latest: &run}}
	service := newCommandServiceWithStore(runStore, githubAdapter, nil)

	result, err := service.HandleCommand(context.Background(), factory.CommandRequest{
		IssueNumber: 42,
		Comment:     github.Comment{ID: "21", Author: "alice", Body: "/factory answer format use the existing JSON format"},
	})
	if err != nil {
		t.Fatalf("HandleCommand() error = %v", err)
	}
	if result.Outcome != factory.CommandAccepted || result.Run.Stage != store.StageClaim || result.Run.Status != store.StatusActive {
		t.Fatalf("result = %#v, want an accepted answer that stays at the claim stage", result)
	}
	if len(result.Run.PendingQuestions) != 0 {
		t.Fatalf("pending questions = %#v, want the answered question removed", result.Run.PendingQuestions)
	}
	if len(runStore.invocations) != 0 {
		t.Fatalf("invocations started = %#v, want none before the baseline ran", runStore.invocations)
	}
}

// claimStageCommandRun creates a run that was claimed but has not yet passed
// its frozen baseline suite. Its packet declares the complete test-stage
// policy, so only the missing baseline can block a post-baseline stage.
func claimStageCommandRun(t *testing.T, status store.Status) store.Run {
	t.Helper()
	packet, err := json.Marshal(factory.SpecificationPacket{Version: 1, RepositoryConfig: validRepositoryConfig()})
	if err != nil {
		t.Fatal(err)
	}
	run := commandRun(t, status)
	run.Stage = store.StageClaim
	run.Worktree = "/worktree/run-command"
	run.SpecificationPacket = string(packet)
	return run
}

// commandInvocationRunStore adds the visible-invocation seam to the command
// fixture so packet-change resumption takes its agent-launching path.
type commandInvocationRunStore struct {
	*commandRunStore
	invocations []store.Invocation
}

// SaveInvocation records a launched invocation for resumption assertions.
func (s *commandInvocationRunStore) SaveInvocation(_ context.Context, invocation store.Invocation) error {
	s.invocations = append(s.invocations, invocation)
	return nil
}

// Invocation returns a previously recorded invocation.
func (s *commandInvocationRunStore) Invocation(_ context.Context, runID, invocationID string) (*store.Invocation, error) {
	for index := range s.invocations {
		if s.invocations[index].RunID == runID && s.invocations[index].ID == invocationID {
			found := s.invocations[index]
			return &found, nil
		}
	}
	return nil, nil
}
