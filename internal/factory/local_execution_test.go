package factory_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/localissues"
	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// executionTracker is a scripted immutable source seam with ordinary local projections.
type executionTracker struct {
	*commandGitHub
	snapshot tracker.CommittedSnapshot
}

// EditIssueComment keeps the scripted external projection current for restart diagnosis.
func (t *executionTracker) EditIssueComment(ctx context.Context, repo tracker.Repository, id, body string) error {
	t.statusComment = tracker.Comment{ID: id, Body: body}
	return t.commandGitHub.EditIssueComment(ctx, repo, id, body)
}

// RefreshCommittedIssues returns the scripted target without granting authority.
func (t *executionTracker) RefreshCommittedIssues(context.Context) (tracker.CommittedSnapshot, error) {
	return t.snapshot, nil
}

// ReadCommittedIssues resolves only the scripted immutable source revision.
func (t *executionTracker) ReadCommittedIssues(_ context.Context, revision string) (tracker.CommittedSnapshot, error) {
	if revision != t.snapshot.Revision {
		return tracker.CommittedSnapshot{}, errors.New("source unavailable")
	}
	return t.snapshot, nil
}

// SeparateIssueCommentStream preserves independent operator and PR replay origins.
func (t *executionTracker) SeparateIssueCommentStream() bool { return true }

// localExecutionFixture turns the ordinary session fixture into a local host.
func localExecutionFixture(t *testing.T) (sessionCommandFixture, *executionTracker, *config.RepositoryConfig) {
	t.Helper()
	f := newSessionCommandFixture(t)
	host, err := config.LoadHost(f.configPath)
	if err != nil {
		t.Fatal(err)
	}
	registration := &host.Repositories[0]
	registration.Tracker = config.ProviderLocal
	registration.CodeHost = config.ProviderGitHub
	registration.LocalOperator = config.LocalOperatorConfig{InstallationID: "install-00112233445566778899aabbccddeeff", Name: "operator"}
	if err := config.SaveHost(f.configPath, host); err != nil {
		t.Fatal(err)
	}
	issue := localissues.Issue{SchemaVersion: 1, ID: "local-00112233445566778899aabbccddeeff", Title: "Export", Kind: localissues.KindTask, Correlation: "import/1/export", Body: "## Problem\n\nExport needed.\n\n## Scope and non-goals\n\nCSV only.\n\n## Acceptance criteria\n\nCSV downloads.\n"}
	data, err := localissues.Render(issue)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := localissues.Validate([]localissues.File{{Path: "docs/issues/export.md", Data: data}})
	if err != nil {
		t.Fatal(err)
	}
	revision := strings.Repeat("a", 40)
	var number int
	f.withStore(func(db *store.Store) {
		sources, err := db.IndexLocalIssueSnapshot(t.Context(), registration.Path, revision, graph)
		if err != nil {
			t.Fatal(err)
		}
		number = sources[0].Number
	})
	source := &executionTracker{commandGitHub: &commandGitHub{issue: tracker.Issue{Number: number, State: "open", Labels: []string{tracker.LabelAgentRunning}}, statusComment: tracker.Comment{ID: "status-1"}}, snapshot: tracker.CommittedSnapshot{Revision: revision, Graph: graph, Issues: []tracker.CommittedIssue{{Number: number, Source: graph.Issues[0]}}}}
	policy := validRepositoryConfig()
	f.service = factory.NewWithDependencies(f.configPath, factory.Dependencies{Tracker: source, Worker: &agentWorker{}, LoadRepository: func(string) (config.RepositoryConfig, error) { return policy, nil }, Now: func() time.Time { return sessionCommandNow }})
	f.heartbeat(sessionCommandNow.Add(time.Hour))
	return f, source, &policy
}

// scopedCommand parses exactly the browser's public envelope.
func scopedCommand(t *testing.T, id, scope, subject string, kind sessioncommand.Kind, revision int64, payload any) store.IdeationCommand {
	t.Helper()
	data, err := json.Marshal(map[string]any{"request_id": id, "scope": scope, "subject_id": subject, "kind": kind, "expected_revision": revision, "payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	command, err := sessioncommand.ParseRequest(data)
	if err != nil {
		t.Fatal(err)
	}
	return command
}

// submitExecutionCommand appends and drives the normal coordinator inbox seam.
func submitExecutionCommand(t *testing.T, f sessionCommandFixture, command store.IdeationCommand) store.IdeationCommand {
	t.Helper()
	if _, err := f.service.SubmitSessionCommand(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	if err := f.service.DrainSessionCommandsForTest(t.Context()); err != nil {
		t.Fatal(err)
	}
	view, err := f.service.SessionCommand(t.Context(), command.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	return view.Command
}

// previewExecution renders a target preview and reads its retained exact bytes.
func previewExecution(t *testing.T, f sessionCommandFixture, id string) store.LocalExecutionPreview {
	t.Helper()
	command := submitExecutionCommand(t, f, scopedCommand(t, id, "backlog", "repository", sessioncommand.KindPreviewExecution, 0, sessioncommand.ExecutionPreviewPayload{SelectedIssueIDs: []string{"local-00112233445566778899aabbccddeeff"}}))
	if command.Outcome != store.IdeationCommandApplied {
		t.Fatalf("preview=%#v", command)
	}
	if len(command.Result) > 4096 {
		t.Fatal("command result duplicated exact graph instead of its durable reference")
	}
	view, err := f.service.LocalExecution(t.Context())
	if err != nil || view.Preview == nil {
		t.Fatalf("view=%#v,%v", view, err)
	}
	return *view.Preview
}

// authorizePreview creates a payload that binds every displayed digest and selection.
func authorizePreview(preview store.LocalExecutionPreview) sessioncommand.ExecutionAuthorizationPayload {
	return sessioncommand.ExecutionAuthorizationPayload{PreviewID: preview.ID, PayloadSHA256: preview.Binding.PayloadSHA256, RelationshipMappingSHA256: preview.Binding.RelationshipMappingSHA256, GraphDigest: preview.Binding.GraphDigest, SelectedIssueIDs: preview.Binding.SelectedIssueIDs}
}

// TestTerminalSessionIndependentExecutionApprovalAndReplay proves imports/session closure convey no authority.
func TestTerminalSessionIndependentExecutionApprovalAndReplay(t *testing.T) {
	f, _, _ := localExecutionFixture(t)
	f.withStore(func(db *store.Store) {
		session, err := db.IdeationSession(t.Context(), "session-1")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.TransitionIdeationSession(t.Context(), store.IdeationWrite{SessionID: session.ID, ExpectedRevision: session.Revision, State: store.IdeationStateCancelled, At: sessionCommandNow}); err != nil {
			t.Fatal(err)
		}
	})
	initial, err := f.service.LocalExecution(t.Context())
	if err != nil || len(initial.Authorizations) != 0 {
		t.Fatalf("import granted authority: %#v,%v", initial, err)
	}
	preview := previewExecution(t, f, "target-preview")
	if len(preview.Binding.Issues) != 1 || string(preview.Binding.Issues[0].Bytes) == "" {
		t.Fatal("exact source absent from retained preview")
	}
	command := scopedCommand(t, "target-approval", "backlog", "repository", sessioncommand.KindAuthorizeExecution, preview.Revision, authorizePreview(preview))
	applied := submitExecutionCommand(t, f, command)
	if applied.Outcome != store.IdeationCommandApplied {
		t.Fatalf("authorization=%#v", applied)
	}
	f.heartbeat(sessionCommandNow.Add(-time.Second))
	replay, err := f.service.SubmitSessionCommand(t.Context(), command)
	if err != nil || !replay.Replayed {
		t.Fatalf("stopped replay=%#v,%v", replay, err)
	}
	view, err := f.service.LocalExecution(t.Context())
	if err != nil || len(view.Authorizations) != 1 {
		t.Fatalf("authority duplicated: %#v,%v", view, err)
	}
	history, err := f.service.SessionActivity(t.Context(), "session-1")
	if err != nil || history.History.Session.State != store.IdeationStateCancelled || history.History.Session.Revision != 2 {
		t.Fatalf("terminal session mutated: %#v,%v", history, err)
	}
}

// TestStaleCommittedSourceAndChangedTargetPolicyCannotAuthorize checks both post-append revalidation gates.
func TestStaleCommittedSourceAndChangedTargetPolicyCannotAuthorize(t *testing.T) {
	for _, change := range []string{"content", "target"} {
		t.Run(change, func(t *testing.T) {
			f, source, policy := localExecutionFixture(t)
			preview := previewExecution(t, f, "preview")
			command := scopedCommand(t, "approve", "backlog", "repository", sessioncommand.KindAuthorizeExecution, preview.Revision, authorizePreview(preview))
			if _, err := f.service.SubmitSessionCommand(t.Context(), command); err != nil {
				t.Fatal(err)
			}
			if change == "content" {
				source.snapshot.Graph.Digest = "sha256:" + strings.Repeat("b", 64)
			} else {
				policy.TargetBranch = "release"
			}
			if err := f.service.DrainSessionCommandsForTest(t.Context()); err != nil {
				t.Fatal(err)
			}
			outcome, err := f.service.SessionCommand(t.Context(), command.RequestID)
			if err != nil || outcome.Command.Outcome != store.IdeationCommandConflict {
				t.Fatalf("stale=%#v,%v", outcome, err)
			}
			view, err := f.service.LocalExecution(t.Context())
			if err != nil || len(view.Authorizations) != 0 {
				t.Fatal("stale request granted authority")
			}
		})
	}
}

// localCompletionFault loses the inbox completion after a supported Run transition.
type localCompletionFault struct {
	*store.Store
	fail *bool
}

// CompleteIdeationCommand injects one lost outcome while preserving durable transition evidence.
func (s localCompletionFault) CompleteIdeationCommand(ctx context.Context, result store.IdeationCommandResult) (store.IdeationCommand, error) {
	if (result.RequestID == "answer-request" || result.RequestID == "resume-request") && *s.fail {
		*s.fail = false
		return store.IdeationCommand{}, errors.New("injected completion loss")
	}
	return s.Store.CompleteIdeationCommand(ctx, result)
}

// TestLocalAnswerRecoveryContinuesWithOneNewPacketThroughNormalProgression checks the actual worker path.
func TestLocalAnswerRecoveryContinuesWithOneNewPacketThroughNormalProgression(t *testing.T) {
	_, memory, runtime, harnessRuntime := newAgentService(t)
	run := *memory.current
	run.CheckpointSHA = strings.Repeat("a", 40)
	run.BaseCheckpointSHA = run.CheckpointSHA
	memory.worktree.state.HeadSHA = run.CheckpointSHA
	memory.worktree.state.RepositoryPath = run.RepositoryPath
	for i := range memory.gateResults[run.ID] {
		memory.gateResults[run.ID][i].CheckpointSHA = run.CheckpointSHA
	}
	run.Stage = store.StageImplementation
	run.Status = store.StatusWaitingForHuman
	run.Revision = 3
	run.PendingQuestions = []store.PendingQuestion{{ID: "q-1", Prompt: "CSV or JSON?"}}
	databasePath := filepath.Join(t.TempDir(), "private", "factory.db")
	db, err := store.Open(t.Context(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveGateResults(t.Context(), memory.gateResults[run.ID]); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSupervisorHeartbeat(t.Context(), store.SupervisorHeartbeat{Coordinator: "test", PID: 123, StartedAt: sessionCommandNow, RenewedAt: sessionCommandNow, ExpiresAt: sessionCommandNow.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	host := commandHost()
	registration := &host.Repositories[0]
	registration.Path = run.RepositoryPath
	registration.OperationalDataPath = databasePath
	registration.RepositoryConfigPath = filepath.Join(run.RepositoryPath, "factory.yaml")
	registration.Tracker = config.ProviderLocal
	registration.CodeHost = config.ProviderGitHub
	registration.LocalOperator = config.LocalOperatorConfig{InstallationID: "install-00112233445566778899aabbccddeeff", Name: "operator"}
	source := &executionTracker{commandGitHub: &commandGitHub{issue: tracker.Issue{Number: run.IssueNumber, State: "open", Labels: []string{tracker.LabelAgentRunning}}, statusComment: tracker.Comment{ID: run.StatusCommentID}}}
	fail := true
	counter := 0
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{Config: commandConfig{host: host}, Tracker: source, Worker: runtime, HeadlessHarnesses: testHeadlessHarnesses(harnessRuntime), Worktree: memory.worktree, Now: func() time.Time { return sessionCommandNow }, LoadRepository: func(string) (config.RepositoryConfig, error) { return validRepositoryConfig(), nil }, OpenStore: func(ctx context.Context, path string) (factory.OperationalStore, error) {
		db, err := store.Open(ctx, path)
		if err != nil {
			return nil, err
		}
		return localCompletionFault{Store: db, fail: &fail}, nil
	}, NewRunID: func() (string, error) { counter++; return fmt.Sprintf("continued-%d", counter), nil }})
	packet := factory.SpecificationPacket{}
	if err := json.Unmarshal([]byte(run.SpecificationPacket), &packet); err != nil {
		t.Fatal(err)
	}
	payload := sessioncommand.RunOperatorPayload{PacketVersion: packet.Version, QuestionID: "q-1", QuestionDigest: factory.LocalQuestionDigest(run.ID, packet.Version, run.PendingQuestions[0]), Answer: "CSV only"}
	command := scopedCommand(t, "answer-request", "run", run.ID, sessioncommand.KindAnswerRun, run.Revision, payload)
	if _, err := service.SubmitSessionCommand(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	if err := service.DrainSessionCommandsForTest(t.Context()); err == nil {
		t.Fatal("completion loss not injected")
	}
	if err := service.DrainSessionCommandsForTest(t.Context()); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(t.Context(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	answered, err := db.Run(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var updated factory.SpecificationPacket
	if err := json.Unmarshal([]byte(answered.SpecificationPacket), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Version != packet.Version+1 || len(updated.Clarifications) != 1 || len(answered.PendingQuestions) != 0 {
		t.Fatalf("answered=%#v, packet=%#v", answered, updated)
	}
	db.Close()
	if outcome, err := service.DriveRunForTest(t.Context()); err != nil {
		t.Fatal(err)
	} else {
		t.Logf("progression=%s", outcome)
	}
	if outcome, err := service.DriveRunForTest(t.Context()); err != nil {
		t.Fatal(err)
	} else {
		t.Logf("progression=%s", outcome)
	}
	if len(harnessRuntime.starts) != 1 {
		db, _ := store.Open(t.Context(), databasePath)
		current, _ := db.Run(t.Context(), run.ID)
		db.Close()
		t.Logf("current=%#v", current)
		t.Fatalf("launches=%d; want exactly one continuation", len(harnessRuntime.starts))
	}
	db, err = store.Open(t.Context(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	invocations, err := db.Invocations(t.Context(), run.ID)
	if err != nil || len(invocations) != 1 {
		t.Fatalf("invocations=%#v,%v", invocations, err)
	}
	invoked := readInvocationPacket(t, invocations[0].InvocationDirectory)
	if !strings.Contains(invoked.SpecificationPacket, `"version":2`) || !strings.Contains(invoked.SpecificationPacket, "CSV only") {
		t.Fatalf("worker packet=%s", invoked.SpecificationPacket)
	}
}

// TestLocalCancellationRetryAndUnsupportedResumeUseOrdinaryRunSemantics exercises bound recovery commands.
func TestLocalCancellationRetryAndUnsupportedResumeUseOrdinaryRunSemantics(t *testing.T) {
	f, source, _ := localExecutionFixture(t)
	host, err := config.LoadHost(f.configPath)
	if err != nil {
		t.Fatal(err)
	}
	run := commandRun(t, store.StatusActive)
	run.RepositoryPath = host.Repositories[0].Path
	run.IssueNumber = source.snapshot.Issues[0].Number
	run.Revision = 1
	source.statusComment = tracker.Comment{ID: run.StatusCommentID, Body: factory.StatusCommentBody(run)}
	f.withStore(func(db *store.Store) {
		if err := db.SaveRun(t.Context(), run); err != nil {
			t.Fatal(err)
		}
	})
	controls, err := f.service.LocalRunOperator(t.Context(), run.ID)
	if err != nil || len(controls.Actions) != 1 || controls.Actions[0] != string(sessioncommand.KindCancelRun) {
		t.Fatalf("controls=%#v,%v", controls, err)
	}
	cancel := scopedCommand(t, "cancel-run", "run", run.ID, sessioncommand.KindCancelRun, run.Revision, sessioncommand.RunOperatorPayload{PacketVersion: 1})
	cancelled := submitExecutionCommand(t, f, cancel)
	if cancelled.Outcome != store.IdeationCommandApplied {
		t.Fatalf("cancelled=%#v", cancelled)
	}
	replay, err := f.service.SubmitSessionCommand(t.Context(), cancel)
	if err != nil || !replay.Replayed {
		t.Fatalf("cancel replay=%#v,%v", replay, err)
	}
	controls, err = f.service.LocalRunOperator(t.Context(), run.ID)
	if err != nil || len(controls.Actions) != 1 || controls.Actions[0] != string(sessioncommand.KindRetryRun) {
		t.Fatalf("cancelled controls=%#v,%v", controls, err)
	}
	retried := submitExecutionCommand(t, f, scopedCommand(t, "retry-run", "run", run.ID, sessioncommand.KindRetryRun, controls.Revision, sessioncommand.RunOperatorPayload{PacketVersion: 1}))
	if retried.Outcome != store.IdeationCommandApplied {
		t.Fatalf("retry=%#v", retried)
	}
	controls, err = f.service.LocalRunOperator(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	unsupported := submitExecutionCommand(t, f, scopedCommand(t, "unsupported-resume", "run", run.ID, sessioncommand.KindResumeRun, controls.Revision, sessioncommand.RunOperatorPayload{PacketVersion: 1}))
	if unsupported.Outcome != store.IdeationCommandRefused {
		t.Fatalf("unsupported resume=%#v", unsupported)
	}
	f.heartbeat(sessionCommandNow.Add(-time.Second))
	controls, err = f.service.LocalRunOperator(t.Context(), run.ID)
	if err != nil || controls.Supervisor.Live {
		t.Fatalf("stopped read=%#v,%v", controls, err)
	}
	if _, err := f.service.SubmitSessionCommand(t.Context(), scopedCommand(t, "unavailable", "run", run.ID, sessioncommand.KindCancelRun, controls.Revision, sessioncommand.RunOperatorPayload{PacketVersion: 1})); !errors.Is(err, factory.ErrSupervisorUnavailable) {
		t.Fatalf("stopped mutation=%v", err)
	}
}

// TestLargeExactPreviewUsesABoundedCommandReference retains source beyond the inbox result bound without truncation.
func TestLargeExactPreviewUsesABoundedCommandReference(t *testing.T) {
	f, source, _ := localExecutionFixture(t)
	host, err := config.LoadHost(f.configPath)
	if err != nil {
		t.Fatal(err)
	}
	files := []localissues.File{}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("local-%032x", i+1)
		correlation := fmt.Sprintf("import/1/item-%d", i)
		if i == 0 {
			id = "local-00112233445566778899aabbccddeeff"
			correlation = "import/1/export"
		}
		issue := localissues.Issue{SchemaVersion: 1, ID: id, Title: "Large accepted specification", Kind: localissues.KindTask, Correlation: correlation, Body: "## Problem\n\n" + strings.Repeat("x", 900*1024) + "\n\n## Scope and non-goals\n\nCSV only.\n\n## Acceptance criteria\n\nCSV downloads.\n"}
		data, err := localissues.Render(issue)
		if err != nil {
			t.Fatal(err)
		}
		name, _ := localissues.DefaultPath(id)
		files = append(files, localissues.File{Path: name, Data: data})
	}
	graph, err := localissues.Validate(files)
	if err != nil {
		t.Fatal(err)
	}
	revision := strings.Repeat("b", 40)
	f.withStore(func(db *store.Store) {
		sources, err := db.IndexLocalIssueSnapshot(t.Context(), host.Repositories[0].Path, revision, graph)
		if err != nil {
			t.Fatal(err)
		}
		source.snapshot = tracker.CommittedSnapshot{Revision: revision, Graph: graph}
		for _, item := range sources {
			source.snapshot.Issues = append(source.snapshot.Issues, tracker.CommittedIssue{Number: item.Number, Source: item.Issue})
		}
	})
	preview := previewExecution(t, f, "large-preview")
	total := 0
	for _, issue := range preview.Binding.Issues {
		total += len(issue.Bytes)
	}
	if total <= store.MaxIdeationCommandResultBytes || len(preview.Binding.Issues) != 5 {
		t.Fatalf("large source was shortened: files=%d bytes=%d", len(preview.Binding.Issues), total)
	}
	command := submitExecutionCommand(t, f, scopedCommand(t, "large-authorization", "backlog", "repository", sessioncommand.KindAuthorizeExecution, preview.Revision, authorizePreview(preview)))
	if command.Outcome != store.IdeationCommandApplied {
		t.Fatalf("large authorization=%#v", command)
	}
}

// TestRejectedLocalRunCommandKeepsItsOutcomeAfterLostCompletion preserves refusal across ordinary projections/replay.
func TestRejectedLocalRunCommandKeepsItsOutcomeAfterLostCompletion(t *testing.T) {
	f, source, _ := localExecutionFixture(t)
	host, err := config.LoadHost(f.configPath)
	if err != nil {
		t.Fatal(err)
	}
	run := commandRun(t, store.StatusActive)
	run.RepositoryPath = host.Repositories[0].Path
	run.IssueNumber = source.snapshot.Issues[0].Number
	run.Revision = 1
	source.statusComment = tracker.Comment{ID: run.StatusCommentID, Body: factory.StatusCommentBody(run)}
	f.withStore(func(db *store.Store) {
		if err := db.SaveRun(t.Context(), run); err != nil {
			t.Fatal(err)
		}
	})
	fail := true
	f.service = factory.NewWithDependencies(f.configPath, factory.Dependencies{Tracker: source, Worker: &agentWorker{}, Now: func() time.Time { return sessionCommandNow }, LoadRepository: func(string) (config.RepositoryConfig, error) { return validRepositoryConfig(), nil }, OpenStore: func(ctx context.Context, path string) (factory.OperationalStore, error) {
		db, err := store.Open(ctx, path)
		if err != nil {
			return nil, err
		}
		return localCompletionFault{Store: db, fail: &fail}, nil
	}})
	command := scopedCommand(t, "resume-request", "run", run.ID, sessioncommand.KindResumeRun, run.Revision, sessioncommand.RunOperatorPayload{PacketVersion: 1})
	if _, err := f.service.SubmitSessionCommand(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	if err := f.service.DrainSessionCommandsForTest(t.Context()); err == nil {
		t.Fatal("completion loss not injected")
	}
	if err := f.service.DrainSessionCommandsForTest(t.Context()); err != nil {
		t.Fatal(err)
	}
	view, err := f.service.SessionCommand(t.Context(), command.RequestID)
	if err != nil || view.Command.Outcome != store.IdeationCommandRefused {
		t.Fatalf("lost refusal became %#v,%v", view, err)
	}
}
