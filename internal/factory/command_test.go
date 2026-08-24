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
	dependencies := factory.Dependencies{
		Config:    commandConfig{host: commandHost()},
		OpenStore: func(context.Context, string) (factory.OperationalStore, error) { return runStore, nil },
		GitHub:    githubAdapter,
		Comments:  comments,
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

// commandEditedComment records a status-comment edit.
type commandEditedComment struct {
	id   string
	body string
}
