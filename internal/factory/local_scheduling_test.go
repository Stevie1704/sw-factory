package factory_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/localissues"
	"github.com/Stevie1704/sw-factory/internal/localpublication"
	"github.com/Stevie1704/sw-factory/internal/localtracker"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scheduleGit runs actual fixture Git operations with a fixed test identity.
func scheduleGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", gitadapter.HostCommandArgs(args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.test", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}

// schedulingHost supplies normal code-host lifecycle observations and readiness.
type schedulingHost struct {
	*commandGitHub
	blocked     bool
	lastRequest codehost.PullRequestRequest
}

// UpdatePullRequest records the host-visible body and title after regeneration.
func (h *schedulingHost) UpdatePullRequest(_ context.Context, _ tracker.Repository, _ int, request codehost.PullRequestRequest) (codehost.PullRequest, error) {
	h.lastRequest = request
	h.pullRequest.Title, h.pullRequest.Body = request.Title, request.Body
	return h.pullRequest, nil
}

// CreateCommitStatus simulates the normal independent status write boundary.
func (*schedulingHost) CreateCommitStatus(context.Context, tracker.Repository, codehost.CommitStatus) error {
	return nil
}

// ListCommitStatuses permits ordinary effect recovery without duplicate writes.
func (*schedulingHost) ListCommitStatuses(context.Context, tracker.Repository, string) ([]codehost.CommitStatus, error) {
	return nil, nil
}

// PullRequestReviews supplies the independent review observation boundary.
func (*schedulingHost) PullRequestReviews(context.Context, tracker.Repository, int) ([]codehost.PullRequestReview, error) {
	return nil, nil
}

// PullRequestComments never reads the local issue-number namespace.
func (*schedulingHost) PullRequestComments(context.Context, tracker.Repository, int) ([]tracker.Comment, error) {
	return nil, nil
}

// FindPullRequestComment is unused before PR creation in these claim scenarios.
func (*schedulingHost) FindPullRequestComment(context.Context, tracker.Repository, int, string) (tracker.Comment, error) {
	return tracker.Comment{}, nil
}

// CreatePullRequestComment supplies the ordinary host comment effect boundary.
func (*schedulingHost) CreatePullRequestComment(context.Context, tracker.Repository, int, string) (tracker.Comment, error) {
	return tracker.Comment{ID: "host-comment"}, nil
}

// EditPullRequestComment supplies the ordinary host comment update boundary.
func (*schedulingHost) EditPullRequestComment(context.Context, tracker.Repository, int, string, string) error {
	return nil
}

// CodeHostStartupChecks simulates only the external host access boundary.
func (h *schedulingHost) CodeHostStartupChecks(tracker.Repository) []doctor.Check {
	return []doctor.Check{func(context.Context) doctor.Result {
		if h.blocked {
			return doctor.Failure(doctor.Condition{Code: "test.host.blocked"}, "code host", "blocked", "restore access")
		}
		return doctor.Success("test.host.ready", "code host")
	}}
}

// scheduleFixture combines real Git/SQLite and local tracker with scripted host IO.
type scheduleFixture struct {
	sessionCommandFixture
	registration config.RepositoryRegistration
	policy       *config.RepositoryConfig
	source       *localtracker.Client
	host         *schedulingHost
	manager      *gitadapter.LocalWorktreeManager
	planner      *localpublication.Publisher
}

// newScheduleFixture prepares committed accepted files, never execution authority.
func newScheduleFixture(t *testing.T, issues ...localissues.Issue) *scheduleFixture {
	t.Helper()
	f := newSessionCommandFixture(t)
	host, err := config.LoadHost(f.configPath)
	if err != nil {
		t.Fatal(err)
	}
	r := &host.Repositories[0]
	r.Tracker = config.ProviderLocal
	r.CodeHost = config.ProviderGitHub
	r.LocalOperator = config.LocalOperatorConfig{InstallationID: "install-00112233445566778899aabbccddeeff", Name: "operator"}
	if err := config.SaveHost(f.configPath, host); err != nil {
		t.Fatal(err)
	}
	scheduleGit(t, r.Path, "init", "-b", "main")
	scheduleGit(t, r.Path, "config", "user.name", "Test")
	scheduleGit(t, r.Path, "config", "user.email", "test@example.test")
	scheduleGit(t, r.Path, "config", "commit.gpgsign", "false")
	if err := os.MkdirAll(filepath.Join(r.Path, "docs/issues"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, issue := range issues {
		data, err := localissues.Render(issue)
		if err != nil {
			t.Fatal(err)
		}
		name, _ := localissues.DefaultPath(issue.ID)
		if err := os.WriteFile(filepath.Join(r.Path, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	scheduleGit(t, r.Path, "add", ".")
	scheduleGit(t, r.Path, "-c", "commit.gpgsign=false", "commit", "-m", "accepted source")
	remote := filepath.Join(filepath.Dir(r.Path), "origin.git")
	scheduleGit(t, filepath.Dir(r.Path), "init", "--bare", remote)
	scheduleGit(t, r.Path, "remote", "add", "origin", remote)
	scheduleGit(t, r.Path, "push", "origin", "main")
	policy := validRepositoryConfig()
	source := localtracker.NewClient(*r, "main")
	manager := &gitadapter.LocalWorktreeManager{WorktreeDir: filepath.Join(filepath.Dir(r.Path), "worktrees")}
	h := &schedulingHost{commandGitHub: &commandGitHub{}}
	result := &scheduleFixture{sessionCommandFixture: f, registration: *r, policy: &policy, source: source, host: h, manager: manager}
	result.recompose(nil)
	result.heartbeat(sessionCommandNow.Add(time.Hour))
	return result
}

// recompose models process restart while retaining only durable Git/store data.
func (f *scheduleFixture) recompose(open factory.StoreOpener) {
	client := tracker.Client(f.source)
	if f.planner != nil {
		client = localpublication.Wrap(f.source, f.planner)
	}
	f.service = factory.NewWithDependencies(f.configPath, factory.Dependencies{Tracker: client, Worktree: f.manager, GitWorkspace: f.manager, Worker: &agentWorker{}, HeadlessHarnesses: testHeadlessHarnesses(&agentHarness{}), PullRequests: f.host, CommitStatuses: f.host, PullRequestReviews: f.host, PullRequestComments: f.host, PullRequestCommentPublisher: f.host, CodeHostReadiness: f.host, OpenStore: open, LoadRepository: func(string) (config.RepositoryConfig, error) { return *f.policy, nil }, Now: func() time.Time { return sessionCommandNow }})
}

// schedulingTask is a canonical accepted source item.
func schedulingTask(id, title string) localissues.Issue {
	return localissues.Issue{SchemaVersion: 1, ID: id, Title: title, Kind: localissues.KindTask, Correlation: "import/" + id, Body: "## Problem\n\nExport needed.\n\n## Scope and non-goals\n\nCSV only.\n\n## Acceptance criteria\n\nCSV downloads.\n"}
}

// authorize records explicit approval through the existing protected command seam.
func (f *scheduleFixture) authorize(t *testing.T, id string, selected ...string) {
	t.Helper()
	preview := submitExecutionCommand(t, f.sessionCommandFixture, scopedCommand(t, id+"-preview", "backlog", "repository", sessioncommand.KindPreviewExecution, 0, sessioncommand.ExecutionPreviewPayload{SelectedIssueIDs: selected}))
	if preview.Outcome != store.IdeationCommandApplied {
		t.Fatalf("preview=%#v", preview)
	}
	view, err := f.service.LocalExecution(t.Context())
	if err != nil || view.Preview == nil {
		t.Fatalf("preview view=%#v,%v", view, err)
	}
	out := submitExecutionCommand(t, f.sessionCommandFixture, scopedCommand(t, id, "backlog", "repository", sessioncommand.KindAuthorizeExecution, view.Preview.Revision, authorizePreview(*view.Preview)))
	if out.Outcome != store.IdeationCommandApplied {
		t.Fatalf("authorization=%#v", out)
	}
}

// TestLocalClaimRequiresExplicitAuthorityAndFreezesExactBase covers both direct
// admission and the normal scheduler against real accepted Git content.
func TestLocalClaimRequiresExplicitAuthorityAndFreezesExactBase(t *testing.T) {
	issue := schedulingTask("local-00112233445566778899aabbccddeeff", "Export")
	f := newScheduleFixture(t, issue)
	snapshot, err := f.source.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	number := snapshot.Issues[0].Number
	if err := f.source.ReplaceIssueLabels(t.Context(), tracker.Repository{}, number, []string{tracker.LabelAgentReady}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ClaimIssue(t.Context(), number); err == nil {
		t.Fatal("label/import authorized direct local claim")
	}
	f.authorize(t, "approve", issue.ID)
	result, err := f.service.ClaimIssue(t.Context(), number)
	if err != nil {
		t.Fatal(err)
	}
	if result.Run.BaseCheckpointSHA != snapshot.Revision || result.Packet.LocalSource == nil || result.Packet.LocalSource.IssueID != issue.ID || result.Packet.LocalSource.Revision != snapshot.Revision {
		t.Fatalf("claim source/base diverged: %#v", result)
	}
	if head := scheduleGit(t, result.Run.Worktree, "rev-parse", "HEAD"); head != snapshot.Revision {
		t.Fatalf("workspace base=%s", head)
	}
	if _, err := f.service.ClaimIssue(t.Context(), number); err == nil {
		t.Fatal("created another active Run")
	}
	view, err := f.service.LocalBacklog(t.Context())
	if err != nil || len(view.Projection.Items) != 1 || view.Projection.Items[0].RunID != result.Run.ID {
		t.Fatalf("backlog=%#v,%v", view, err)
	}
}

// writeIssueCommit updates accepted fixture content on the ordinary main branch.
func (f *scheduleFixture) writeIssueCommit(t *testing.T, issue localissues.Issue) string {
	t.Helper()
	data, err := localissues.Render(issue)
	if err != nil {
		t.Fatal(err)
	}
	name, _ := localissues.DefaultPath(issue.ID)
	if err := os.WriteFile(filepath.Join(f.registration.Path, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
	scheduleGit(t, f.registration.Path, "add", name)
	scheduleGit(t, f.registration.Path, "-c", "commit.gpgsign=false", "commit", "-m", "changed accepted source")
	scheduleGit(t, f.registration.Path, "push", "origin", "main")
	return scheduleGit(t, f.registration.Path, "rev-parse", "HEAD")
}

// TestObservedSourceChangesRequireANewDecisionEvenWhenBytesReturn proves
// content restoration cannot revive an authorization already observed stale.
func TestObservedSourceChangesRequireANewDecisionEvenWhenBytesReturn(t *testing.T) {
	issue := schedulingTask("local-00112233445566778899aabbccddeeff", "Export")
	f := newScheduleFixture(t, issue)
	f.authorize(t, "old", issue.ID)
	changed := issue
	changed.Title = "Changed export"
	f.writeIssueCommit(t, changed)
	pass, err := f.service.PollOnce(t.Context())
	if err != nil || pass.Outcome != factory.PollNoWork {
		t.Fatalf("changed source admitted: %#v,%v", pass, err)
	}
	restored := f.writeIssueCommit(t, issue)
	pass, err = f.service.PollOnce(t.Context())
	if err != nil || pass.Outcome != factory.PollNoWork {
		t.Fatalf("restored bytes revived grant: %#v,%v", pass, err)
	}
	view, err := f.service.LocalBacklog(t.Context())
	if err != nil || view.Projection.Items[0].State != "stale_authorization" {
		t.Fatalf("stale projection=%#v,%v", view, err)
	}
	f.authorize(t, "fresh", issue.ID)
	pass, err = f.service.PollOnce(t.Context())
	if err != nil || pass.Outcome != factory.PollClaimed || pass.Run.BaseCheckpointSHA != restored {
		t.Fatalf("fresh exact decision=%#v,%v", pass, err)
	}
}

// TestCurrentTargetOverridesCompositionForPreviewAndClaim guards policy drift
// after Service construction and keeps the fetched source/base aligned.
func TestCurrentTargetOverridesCompositionForPreviewAndClaim(t *testing.T) {
	issue := schedulingTask("local-00112233445566778899aabbccddeeff", "Main export")
	f := newScheduleFixture(t, issue)
	scheduleGit(t, f.registration.Path, "checkout", "-b", "release")
	issue.Title = "Release export"
	data, _ := localissues.Render(issue)
	name, _ := localissues.DefaultPath(issue.ID)
	if err := os.WriteFile(filepath.Join(f.registration.Path, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
	scheduleGit(t, f.registration.Path, "add", name)
	scheduleGit(t, f.registration.Path, "-c", "commit.gpgsign=false", "commit", "-m", "release source")
	scheduleGit(t, f.registration.Path, "push", "origin", "release")
	release := scheduleGit(t, f.registration.Path, "rev-parse", "HEAD")
	scheduleGit(t, f.registration.Path, "checkout", "main")
	f.policy.TargetBranch = "release" // Adapter was constructed with main.
	f.authorize(t, "release-approval", issue.ID)
	preview, err := f.service.LocalExecution(t.Context())
	if err != nil || preview.Preview.Binding.SourceRevision != release {
		t.Fatalf("captured target leaked into preview: %#v,%v", preview, err)
	}
	pass, err := f.service.PollOnce(t.Context())
	if err != nil || pass.Outcome != factory.PollClaimed || pass.Run.BaseCheckpointSHA != release {
		t.Fatalf("release claim=%#v,%v", pass, err)
	}
}

// schedulingLeaseFunc changes private configuration after Start has captured
// registration, at the public lease boundary before the first queue pass.
type schedulingLeaseFunc func()

// RenewLease invokes the deterministic running-service observation hook.
func (f schedulingLeaseFunc) RenewLease(context.Context, tracker.Repository, tracker.Lease) error {
	f()
	return nil
}

// TestRunningLocalSupervisorRejectsPrivateRegistrationDrift covers a live
// service rather than a fresh direct-claim request that already reloads config.
func TestRunningLocalSupervisorRejectsPrivateRegistrationDrift(t *testing.T) {
	for _, kind := range []string{"installation", "operator", "code-host", "repository-path"} {
		t.Run(kind, func(t *testing.T) {
			issue := schedulingTask("local-00112233445566778899aabbccddeeff", "Export")
			f := newScheduleFixture(t, issue)
			f.authorize(t, "approve", issue.ID)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			changed := false
			lease := schedulingLeaseFunc(func() {
				if changed {
					return
				}
				changed = true
				host, err := config.LoadHost(f.configPath)
				if err != nil {
					t.Fatal(err)
				}
				r := &host.Repositories[0]
				switch kind {
				case "installation":
					r.LocalOperator.InstallationID = "install-112233445566778899aabbccddeeff00"
				case "operator":
					r.LocalOperator.Name = "another-operator"
				case "code-host":
					r.GitHub.Repository = "another-service"
				case "repository-path":
					r.Path = t.TempDir()
					r.OperationalDataPath = filepath.Join(r.Path, "factory.db")
				}
				if err := config.SaveHost(f.configPath, host); err != nil {
					t.Fatal(err)
				}
			})
			service := factory.NewWithDependencies(f.configPath, factory.Dependencies{Tracker: f.source, Worktree: f.manager, GitWorkspace: f.manager, Worker: &agentWorker{}, HeadlessHarnesses: testHeadlessHarnesses(&agentHarness{}), PullRequests: f.host, CommitStatuses: f.host, PullRequestReviews: f.host, PullRequestComments: f.host, PullRequestCommentPublisher: f.host, CodeHostReadiness: f.host, Lease: lease, StartupDiagnosis: func(context.Context) (factory.DoctorResult, error) { return factory.DoctorResult{}, nil }, LoadRepository: func(string) (config.RepositoryConfig, error) { return *f.policy, nil }, Now: func() time.Time { return sessionCommandNow }})
			err := service.Start(ctx, factory.EventSinkFunc(func(event factory.CoordinatorEvent) {
				if event.Kind == factory.EventPollPass {
					cancel()
				}
			}))
			if !changed {
				t.Fatal("Start did not reach the private registration change")
			}
			if err != nil && !strings.Contains(err.Error(), "recompose") {
				t.Fatalf("registration drift refusal=%v", err)
			}
			f.withStore(func(db *store.Store) {
				run, err := db.CurrentRun(t.Context())
				if err != nil || run != nil {
					t.Fatalf("old captured authority started work: %#v,%v", run, err)
				}
			})
		})
	}
}

// TestCodeHostUnavailablePreventsDirectLocalClaim checks the finite mutation
// seam as well as the supervised queue; no Run or worker slot is created.
func TestCodeHostUnavailablePreventsDirectLocalClaim(t *testing.T) {
	issue := schedulingTask("local-00112233445566778899aabbccddeeff", "Export")
	f := newScheduleFixture(t, issue)
	f.authorize(t, "approve", issue.ID)
	f.host.blocked = true
	snapshot, err := f.source.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ClaimIssue(t.Context(), snapshot.Issues[0].Number); err == nil || !strings.Contains(err.Error(), "code_host_unavailable") {
		t.Fatalf("host refusal=%v", err)
	}
	f.withStore(func(db *store.Store) {
		run, err := db.CurrentRun(t.Context())
		if err != nil || run != nil {
			t.Fatalf("unavailable host created Run: %#v,%v", run, err)
		}
	})
	missing := factory.NewWithDependencies(f.configPath, factory.Dependencies{Tracker: f.source, Worktree: f.manager, GitWorkspace: f.manager, LoadRepository: func(string) (config.RepositoryConfig, error) { return *f.policy, nil }})
	if _, err := missing.ClaimIssue(t.Context(), snapshot.Issues[0].Number); err == nil || !strings.Contains(err.Error(), "code_host_unavailable") {
		t.Fatalf("missing independent adapter refusal=%v", err)
	}
}

// TestSourceDeletionDoesNotPreventCancellationOrReplaceFrozenSpecification
// proves accepted file changes cannot mutate an active packet or its controls.
func TestSourceDeletionDoesNotPreventCancellationOrReplaceFrozenSpecification(t *testing.T) {
	issue := schedulingTask("local-00112233445566778899aabbccddeeff", "Export")
	f := newScheduleFixture(t, issue)
	f.authorize(t, "approve", issue.ID)
	first, err := f.service.PollOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	name, _ := localissues.DefaultPath(issue.ID)
	scheduleGit(t, f.registration.Path, "rm", name)
	scheduleGit(t, f.registration.Path, "-c", "commit.gpgsign=false", "commit", "-m", "remove accepted source")
	scheduleGit(t, f.registration.Path, "push", "origin", "main")
	refused, err := f.service.HandleCommand(t.Context(), factory.CommandRequest{RunID: first.Run.ID, IssueNumber: first.Run.IssueNumber, Stream: "issue", Comment: tracker.Comment{ID: "1", Author: "alice", Body: "/factory refresh"}})
	if err == nil || refused.Outcome != factory.CommandRejected {
		t.Fatalf("local refresh replaced source: %#v,%v", refused, err)
	}
	cancelled, err := f.service.HandleCommand(t.Context(), factory.CommandRequest{RunID: first.Run.ID, IssueNumber: first.Run.IssueNumber, Stream: "issue", Comment: tracker.Comment{ID: "2", Author: "alice", Body: "/factory cancel"}})
	if err != nil || cancelled.Run.Status != store.StatusCancelled || cancelled.Run.SpecificationPacket != first.Run.SpecificationPacket {
		t.Fatalf("deleted-source cancellation=%#v,%v", cancelled, err)
	}
}

// TestDeletedSourceStillAcceptsTheFrozenQuestion keeps supported operator
// answers independent of the current issue graph after admission.
func TestDeletedSourceStillAcceptsTheFrozenQuestion(t *testing.T) {
	issue := schedulingTask("local-00112233445566778899aabbccddeeff", "Export")
	f := newScheduleFixture(t, issue)
	f.authorize(t, "approve", issue.ID)
	snapshot, err := f.source.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.service.ClaimIssue(t.Context(), snapshot.Issues[0].Number)
	if err != nil {
		t.Fatal(err)
	}
	run := first.Run
	run.Stage, run.Status = store.StageImplementation, store.StatusWaitingForHuman
	run.PendingQuestions = []store.PendingQuestion{{ID: "format", Prompt: "CSV or JSON?"}}
	f.withStore(func(db *store.Store) {
		if err := db.SaveRun(t.Context(), run); err != nil {
			t.Fatal(err)
		}
	})
	name, _ := localissues.DefaultPath(issue.ID)
	scheduleGit(t, f.registration.Path, "rm", name)
	scheduleGit(t, f.registration.Path, "commit", "-m", "remove accepted source")
	scheduleGit(t, f.registration.Path, "push", "origin", "main")
	payload := sessioncommand.RunOperatorPayload{PacketVersion: first.Packet.Version, QuestionID: "format", QuestionDigest: factory.LocalQuestionDigest(run.ID, first.Packet.Version, run.PendingQuestions[0]), Answer: "CSV"}
	answer := submitExecutionCommand(t, f.sessionCommandFixture, scopedCommand(t, "deleted-source-answer", "run", run.ID, sessioncommand.KindAnswerRun, run.Revision, payload))
	if answer.Outcome != store.IdeationCommandApplied {
		t.Fatalf("deleted-source answer=%#v", answer)
	}
	f.withStore(func(db *store.Store) {
		saved, err := db.Run(t.Context(), run.ID)
		if err != nil || saved == nil {
			t.Fatalf("saved=%#v,%v", saved, err)
		}
		var packet factory.SpecificationPacket
		if err := json.Unmarshal([]byte(saved.SpecificationPacket), &packet); err != nil {
			t.Fatal(err)
		}
		if packet.LocalSource == nil || string(packet.LocalSource.Bytes) != string(first.Packet.LocalSource.Bytes) || len(packet.Clarifications) != 1 || len(saved.PendingQuestions) != 0 {
			t.Fatalf("answer mutated source or lost clarification: %#v", packet)
		}
	})
}

// TestDependenciesNeedMergedImplementationVisibleAtTheClaimSnapshot uses normal
// lifecycle observation and real Git ancestry, including retention of proof.
func TestDependenciesNeedMergedImplementationVisibleAtTheClaimSnapshot(t *testing.T) {
	a := schedulingTask("local-00112233445566778899aabbccddeeff", "First")
	b := schedulingTask("local-112233445566778899aabbccddeeff00", "Second")
	b.Dependencies = []string{a.ID}
	parent := schedulingTask("local-00000000000000000000000000000001", "Group")
	parent.Kind = localissues.KindParent
	a.Parent, b.Parent = parent.ID, parent.ID
	f := newScheduleFixture(t, parent, a, b)
	f.authorize(t, "batch", a.ID, b.ID)
	first, err := f.service.PollOnce(t.Context())
	if err != nil || first.Outcome != factory.PollClaimed {
		t.Fatalf("first claim=%#v,%v", first, err)
	}
	ready := first.Run
	ready.Stage = store.StageReady
	ready.PullRequestNumber = 91
	ready.PullRequestURL = "https://codehost.test/pull/91"
	f.withStore(func(db *store.Store) {
		if err := db.SaveRun(t.Context(), ready); err != nil {
			t.Fatal(err)
		}
	})
	f.host.pullRequest = codehost.PullRequest{Number: 91, State: "open", HeadBranch: ready.Branch, HeadSHA: ready.CheckpointSHA, BaseBranch: "main"}
	pass, err := f.service.PollOnce(t.Context())
	if err != nil || pass.Outcome != factory.PollActiveRun {
		t.Fatalf("review-ready released slot: %#v,%v", pass, err)
	}
	// A merged-host observation precedes the actual target fetch containing it.
	scheduleGit(t, f.registration.Path, "checkout", "-b", "implementation-merged")
	if err := os.WriteFile(filepath.Join(f.registration.Path, "implemented.txt"), []byte("first implementation\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scheduleGit(t, f.registration.Path, "add", "implemented.txt")
	scheduleGit(t, f.registration.Path, "-c", "commit.gpgsign=false", "commit", "-m", "human merge")
	merged := scheduleGit(t, f.registration.Path, "rev-parse", "HEAD")
	scheduleGit(t, f.registration.Path, "checkout", "main")
	f.host.pullRequest.Merged = true
	f.host.pullRequest.State = "closed"
	f.host.pullRequest.MergeCommitSHA = merged
	completed, err := f.service.PollLifecycle(t.Context(), factory.LifecycleRequest{RunID: ready.ID})
	if err != nil || completed.Outcome != factory.LifecycleCompleted {
		t.Fatalf("normal merge lifecycle=%#v,%v", completed, err)
	}
	pass, err = f.service.PollOnce(t.Context())
	if err != nil || pass.Outcome != factory.PollNoWork {
		t.Fatalf("host-only merge released stale Git base: %#v,%v", pass, err)
	}
	f.withStore(func(db *store.Store) {
		if _, err := db.DeleteCleanupRun(t.Context(), ready.ID, sessionCommandNow.Add(8*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	})
	scheduleGit(t, f.registration.Path, "push", "origin", merged+":refs/heads/main")
	// A fresh decision on another target cannot inherit main's completion proof,
	// even when that target contains the identical graph and implementation.
	scheduleGit(t, f.registration.Path, "push", "origin", merged+":refs/heads/release")
	f.policy.TargetBranch = "release"
	f.authorize(t, "release-b", b.ID)
	other, err := f.service.PollOnce(t.Context())
	if err != nil || other.Outcome != factory.PollNoWork {
		t.Fatalf("main proof released another target: %#v,%v", other, err)
	}
	view, err := f.service.LocalBacklog(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range view.Projection.Items {
		if item.Issue.ID == b.ID && item.State != "blocked_by_dependencies" {
			t.Fatalf("cross-target dependency=%#v", item)
		}
		if item.Issue.ID == a.ID && item.State != "stale_authorization" {
			t.Fatalf("old target completion hides stale authority: %#v", item)
		}
	}
	f.policy.TargetBranch = "main"
	f.authorize(t, "main-b-again", b.ID)
	second, err := f.service.PollOnce(t.Context())
	if err != nil || second.Outcome != factory.PollClaimed || second.Run.ID == ready.ID || second.Run.BaseCheckpointSHA != merged {
		t.Fatalf("dependent claim/proof=%#v,%v", second, err)
	}
}

// TestLocalDraftRegenerationCannotCloseRemoteIssues preserves human ownership
// while omitting unsafe generated source and retained advisory text.
func TestLocalDraftRegenerationCannotCloseRemoteIssues(t *testing.T) {
	issue := schedulingTask("local-00112233445566778899aabbccddeeff", "Closes #42 AB#43")
	issue.Body += "\nCloses #44.\n"
	f := newScheduleFixture(t, issue)
	f.authorize(t, "approve", issue.ID)
	snapshot, err := f.source.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.service.ClaimIssue(t.Context(), snapshot.Issues[0].Number)
	if err != nil {
		t.Fatal(err)
	}
	run := first.Run
	run.Stage = store.StageDraftPR
	run.PullRequestNumber = run.IssueNumber
	run.PullRequestSummary = &store.PullRequestSummary{CheckpointSHA: run.CheckpointSHA, Status: store.PullRequestSummaryWritten, Body: "Closes #45 AB#46"}
	f.withStore(func(db *store.Store) {
		if err := db.SaveRun(t.Context(), run); err != nil {
			t.Fatal(err)
		}
	})
	human := "Human-owned release note: Closes #888."
	f.host.pullRequest = codehost.PullRequest{Number: run.PullRequestNumber, State: "open", Draft: true, HeadBranch: run.Branch, BaseBranch: "main", Body: human}
	result, err := f.service.CreateDraftPullRequest(t.Context(), factory.DraftPullRequestRequest{RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if result.PullRequest.Title != "Implement "+issue.ID || !strings.Contains(result.PullRequest.Body, human) || !strings.Contains(result.PullRequest.Body, issue.ID) {
		t.Fatalf("local PR=%#v", result.PullRequest)
	}
	for _, unsafe := range []string{"#42", "AB#43", "#44", "#45", "AB#46"} {
		if strings.Contains(f.host.lastRequest.Title+f.host.lastRequest.Body, unsafe) {
			t.Fatalf("generated remote issue authority %q: %#v", unsafe, f.host.lastRequest)
		}
	}
}

// TestLocalAdvisoryWriterCannotAddRemoteIssueAuthority exercises report
// acceptance rather than trusting the writer prompt to suppress closing syntax.
func TestLocalAdvisoryWriterCannotAddRemoteIssueAuthority(t *testing.T) {
	issue := schedulingTask("local-00112233445566778899aabbccddeeff", "Export")
	f := newScheduleFixture(t, issue)
	f.authorize(t, "approve", issue.ID)
	snapshot, err := f.source.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.service.ClaimIssue(t.Context(), snapshot.Issues[0].Number)
	if err != nil {
		t.Fatal(err)
	}
	run := first.Run
	run.Stage = store.StageReview
	run.PullRequestNumber = 91
	dir := t.TempDir()
	writer := store.Invocation{ID: "writer-local", RunID: run.ID, Harness: "codex", Role: "pr_writer", Stage: "pr_summary", Model: "gpt-5", InvocationDirectory: dir, ResultDirectory: dir, Status: store.InvocationStatusActive, CreatedAt: sessionCommandNow, UpdatedAt: sessionCommandNow}
	f.withStore(func(db *store.Store) {
		if err := db.SaveRun(t.Context(), run); err != nil {
			t.Fatal(err)
		}
		if err := db.SaveInvocation(t.Context(), writer); err != nil {
			t.Fatal(err)
		}
	})
	f.host.pullRequest = codehost.PullRequest{Number: 91, State: "open", Draft: true, HeadBranch: run.Branch, HeadSHA: run.CheckpointSHA, BaseBranch: "main", Body: "Human release note."}
	writePullRequestWriterReport(t, writer, report.Report{Outcome: report.OutcomeCompleted, Summary: "summary written", PullRequestSummary: &report.PullRequestSummary{Body: "Closes #987 and AB#986"}})
	_, err = f.service.AcceptAgentReport(t.Context(), factory.AgentReportRequest{RunID: run.ID, InvocationID: writer.ID})
	if err != nil {
		t.Fatal(err)
	}
	f.withStore(func(db *store.Store) {
		saved, err := db.Run(t.Context(), run.ID)
		if err != nil || saved == nil || saved.PullRequestSummary == nil || saved.PullRequestSummary.Status != store.PullRequestSummaryFailed || saved.PullRequestSummary.Body != "" {
			t.Fatalf("unsafe accepted summary=%#v,%v", saved, err)
		}
	})
	if strings.Contains(f.host.pullRequest.Body, "#987") || strings.Contains(f.host.pullRequest.Body, "AB#986") {
		t.Fatalf("unsafe writer published: %s", f.host.pullRequest.Body)
	}
}

// TestUnsuccessfulPrerequisitesDoNotReleaseDependents uses ordinary cancellation,
// a failed terminal Run, and closed-unmerged lifecycle handling with the same
// exact approved graph.
func TestUnsuccessfulPrerequisitesDoNotReleaseDependents(t *testing.T) {
	for _, disposition := range []string{"cancelled", "failed", "closed-unmerged"} {
		t.Run(disposition, func(t *testing.T) {
			a := schedulingTask("local-00112233445566778899aabbccddeeff", "First")
			b := schedulingTask("local-112233445566778899aabbccddeeff00", "Second")
			b.Dependencies = []string{a.ID}
			f := newScheduleFixture(t, a, b)
			f.authorize(t, "approve", a.ID, b.ID)
			first, err := f.service.PollOnce(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			switch disposition {
			case "cancelled":
				_, err = f.service.HandleCommand(t.Context(), factory.CommandRequest{RunID: first.Run.ID, IssueNumber: first.Run.IssueNumber, Stream: "issue", Comment: tracker.Comment{ID: "1", Author: "alice", Body: "/factory cancel"}})
			case "failed":
				run := first.Run
				run.Status = store.StatusFailed
				f.withStore(func(db *store.Store) {
					if err := db.SaveRun(t.Context(), run); err != nil {
						t.Fatal(err)
					}
				})
			default:
				run := first.Run
				run.Stage, run.PullRequestNumber = store.StageReady, 91
				f.withStore(func(db *store.Store) {
					if err := db.SaveRun(t.Context(), run); err != nil {
						t.Fatal(err)
					}
				})
				f.host.pullRequest = codehost.PullRequest{Number: 91, State: "closed", HeadBranch: run.Branch, HeadSHA: run.CheckpointSHA, BaseBranch: "main"}
				_, err = f.service.PollLifecycle(t.Context(), factory.LifecycleRequest{RunID: run.ID})
			}
			if err != nil {
				t.Fatal(err)
			}
			pass, err := f.service.PollOnce(t.Context())
			if err != nil || pass.Outcome != factory.PollNoWork {
				t.Fatalf("unsuccessful prerequisite released work: %#v,%v", pass, err)
			}
			view, err := f.service.LocalBacklog(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range view.Projection.Items {
				if item.Issue.ID == b.ID && item.State != "blocked_by_dependencies" {
					t.Fatalf("dependency=%#v", item)
				}
			}
		})
	}
}

// claimSaveFault injects a stop on either side of atomic Run/consumption commit.
type claimSaveFault struct {
	*store.Store
	fail  *bool
	after bool
}

// SaveLocalClaimRun exposes response loss at the public persistence seam.
func (s claimSaveFault) SaveLocalClaimRun(ctx context.Context, claim store.LocalClaim, run store.Run) (store.Run, error) {
	if !s.after && *s.fail {
		*s.fail = false
		return store.Run{}, errors.New("injected before Run save")
	}
	saved, err := s.Store.SaveLocalClaimRun(ctx, claim, run)
	if err != nil {
		return saved, err
	}
	if s.after && *s.fail {
		*s.fail = false
		return store.Run{}, errors.New("injected after Run save")
	}
	return saved, nil
}

// TestClaimReservationRecoversWorkspaceAndAtomicRunSave exercises real Git/store
// interruption windows and refuses reset while an unlisted claim owns artifacts.
func TestClaimReservationRecoversWorkspaceAndAtomicRunSave(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprint(after), func(t *testing.T) {
			issue := schedulingTask("local-00112233445566778899aabbccddeeff", "Export")
			f := newScheduleFixture(t, issue)
			f.authorize(t, "approve", issue.ID)
			snapshot, err := f.source.Refresh(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			fail := true
			f.recompose(func(ctx context.Context, path string) (factory.OperationalStore, error) {
				db, err := store.Open(ctx, path)
				if err != nil {
					return nil, err
				}
				return claimSaveFault{Store: db, fail: &fail, after: after}, nil
			})
			if _, err := f.service.ClaimIssue(t.Context(), snapshot.Issues[0].Number); err == nil {
				t.Fatal("failure boundary was not exercised")
			}
			var reservedID string
			f.withStore(func(db *store.Store) {
				claims, err := db.LocalClaims(t.Context(), f.registration.Path)
				if err != nil || len(claims) != 1 {
					t.Fatalf("durable intent=%#v,%v", claims, err)
				}
				reservedID = claims[0].RunID
			})
			if !after {
				if _, err := f.service.Reset(t.Context(), factory.ResetRequest{}); err == nil || !strings.Contains(err.Error(), "pending local claim") {
					t.Fatalf("reset lost reservation inventory: %v", err)
				}
			}
			f.recompose(nil)
			pass, err := f.service.PollOnce(t.Context())
			if err != nil || (pass.Outcome != factory.PollClaimed && pass.Outcome != factory.PollActiveRun) || pass.Run.ID != reservedID {
				t.Fatalf("restart duplicated/lost claim: %#v,%v", pass, err)
			}
			f.withStore(func(db *store.Store) {
				claims, err := db.LocalClaims(t.Context(), f.registration.Path)
				if err != nil || len(claims) != 1 || claims[0].State != "claimed" {
					t.Fatalf("consumption=%#v,%v", claims, err)
				}
				run, err := db.Run(t.Context(), reservedID)
				if err != nil || run == nil || run.StatusCommentID == "" {
					t.Fatalf("claim projection not recovered: %#v,%v", run, err)
				}
			})
			if refs := scheduleGit(t, f.registration.Path, "for-each-ref", "--format=%(refname)", "refs/heads/factory/"); strings.Count(refs, "refs/heads/factory/") != 1 {
				t.Fatalf("duplicate branches=%s", refs)
			}
		})
	}
}

// TestRenamePreservesApprovalAndFreezesTheActualSnapshotPath proves identity
// survives rename without inventing a path that is absent at the claim base.
func TestRenamePreservesApprovalAndFreezesTheActualSnapshotPath(t *testing.T) {
	issue := schedulingTask("local-00112233445566778899aabbccddeeff", "Export")
	f := newScheduleFixture(t, issue)
	f.authorize(t, "approve", issue.ID)
	old, _ := localissues.DefaultPath(issue.ID)
	scheduleGit(t, f.registration.Path, "mv", old, "docs/issues/renamed.md")
	scheduleGit(t, f.registration.Path, "-c", "commit.gpgsign=false", "commit", "-m", "rename")
	scheduleGit(t, f.registration.Path, "push", "origin", "main")
	claimed, err := f.service.PollOnce(t.Context())
	if err != nil || claimed.Outcome != factory.PollClaimed {
		t.Fatalf("renamed claim=%#v,%v", claimed, err)
	}
	var packet factory.SpecificationPacket
	if err := json.Unmarshal([]byte(claimed.Run.SpecificationPacket), &packet); err != nil {
		t.Fatal(err)
	}
	actual, err := f.manager.ReadFileAtCheckpoint(t.Context(), f.registration.Path, claimed.Run.BaseCheckpointSHA, packet.LocalSource.Path)
	if err != nil || string(actual) != string(packet.LocalSource.Bytes) || packet.LocalSource.Path != "docs/issues/renamed.md" {
		t.Fatalf("frozen path is absent/wrong at base: %q,%v", packet.LocalSource.Path, err)
	}
}

// TestKindChangesDoNotReviveAuthorityAfterRevert checks validation before the
// current parent/investigation non-executable branch of the queue projection.
func TestKindChangesDoNotReviveAuthorityAfterRevert(t *testing.T) {
	for _, kind := range []localissues.Kind{localissues.KindParent, localissues.KindInvestigation} {
		t.Run(string(kind), func(t *testing.T) {
			issue := schedulingTask("local-00112233445566778899aabbccddeeff", "Export")
			f := newScheduleFixture(t, issue)
			f.authorize(t, "approve", issue.ID)
			changed := issue
			changed.Kind = kind
			f.writeIssueCommit(t, changed)
			if pass, err := f.service.PollOnce(t.Context()); err != nil || pass.Outcome != factory.PollNoWork {
				t.Fatalf("kind change=%#v,%v", pass, err)
			}
			f.writeIssueCommit(t, issue)
			if pass, err := f.service.PollOnce(t.Context()); err != nil || pass.Outcome != factory.PollNoWork {
				t.Fatalf("kind revert revived authority: %#v,%v", pass, err)
			}
		})
	}
}

// TestChangingTrackerCannotReplayAnOldRemoteIssueNumberIntoLocalState guards
// a colliding integer and pending label effect before any provider mutation.
func TestChangingTrackerCannotReplayAnOldRemoteIssueNumberIntoLocalState(t *testing.T) {
	issue := schedulingTask("local-00112233445566778899aabbccddeeff", "Export")
	f := newScheduleFixture(t, issue)
	snapshot, err := f.source.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	legacy := commandRun(t, store.StatusActive)
	legacy.RepositoryPath = f.registration.Path
	legacy.IssueNumber = snapshot.Issues[0].Number
	f.withStore(func(db *store.Store) {
		if err := db.SaveRun(t.Context(), legacy); err != nil {
			t.Fatal(err)
		}
		payload, _ := json.Marshal(map[string]any{"Repository": tracker.Repository{Owner: "example", Name: "project"}, "IssueNumber": legacy.IssueNumber, "Labels": []string{tracker.LabelAgentComplete}})
		if err := db.SavePendingEffect(t.Context(), store.PendingEffect{RunID: legacy.ID, ID: "remote-label", Kind: store.PendingEffectKindLabelTransition, Payload: string(payload), CreatedAt: sessionCommandNow, UpdatedAt: sessionCommandNow}); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := f.service.HandleCommand(t.Context(), factory.CommandRequest{RunID: legacy.ID, IssueNumber: legacy.IssueNumber, Comment: tracker.Comment{ID: "1", Author: "alice", Body: "/factory status"}}); err == nil || !strings.Contains(err.Error(), "restore its original tracker") {
		t.Fatalf("mismatched tracker replay was admitted: %v", err)
	}
	f.withStore(func(db *store.Store) {
		pending, err := db.PendingEffect(t.Context(), legacy.ID)
		if err != nil || pending == nil {
			t.Fatal("old effect was acknowledged through wrong adapter")
		}
		projection, err := db.LocalIssueProjection(t.Context(), f.registration.Path, issue.ID)
		if err != nil || len(projection.Labels) != 0 {
			t.Fatalf("colliding local issue changed: %#v,%v", projection, err)
		}
	})
}
