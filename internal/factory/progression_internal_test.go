package factory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// TestProgressionStepPlansReportAcceptanceFromObservedReadiness verifies the
// planner consumes readiness observed by the coordinator without inspecting
// the invocation result directory itself.
func TestProgressionStepPlansReportAcceptanceFromObservedReadiness(t *testing.T) {
	run := store.Run{ID: "run-1", Stage: store.StageImplementation, Status: store.StatusActive}
	invocation := &store.Invocation{ID: "inv-1"}

	action, stop := progressionStep(progressionState{
		Run:                &run,
		ActiveInvocations:  []*store.Invocation{invocation},
		ReadyInvocationIDs: map[string]bool{"inv-1": true},
	}, workflow.DefaultRegistry())
	if stop != nil {
		t.Fatalf("progressionStep() stopped with %#v, want report acceptance", stop)
	}
	if action.kind != progressionActionAcceptAgentReport {
		t.Fatalf("planned action kind = %q, want %q", action.kind, progressionActionAcceptAgentReport)
	}
	if action.name != "accept agent report" {
		t.Fatalf("planned action name = %q, want unchanged operator-facing name", action.name)
	}
	if action.runID != run.ID || action.invocationID != invocation.ID {
		t.Fatalf("planned action operands = %#v, want run %q and invocation %q", action, run.ID, invocation.ID)
	}
}

// TestDriveRunPausesAnExpiredActiveInvocation verifies unattended progression
// pauses a report-less invocation after the frozen agent deadline instead of
// continuing to publish that the invocation is executing.
func TestDriveRunPausesAnExpiredActiveInvocation(t *testing.T) {
	now := time.Date(2026, 9, 3, 13, 30, 0, 0, time.UTC)
	createdAt := now.Add(-2 * time.Hour)
	packetData, err := json.Marshal(SpecificationPacket{
		Version: specificationPacketVersion,
		RepositoryConfig: config.RepositoryConfig{
			Timeouts: config.TimeoutConfig{Agent: "1h"},
		},
	})
	if err != nil {
		t.Fatalf("marshal deadline packet: %v", err)
	}
	invocation := store.Invocation{
		ID:              "inv-expired",
		RunID:           "run-expired",
		Role:            workflow.RoleImplementation,
		Stage:           store.StageImplementation,
		Status:          store.InvocationStatusActive,
		ResultDirectory: t.TempDir(),
		CreatedAt:       createdAt,
		UpdatedAt:       createdAt,
	}
	run := store.Run{
		ID:                  invocation.RunID,
		Stage:               store.StageImplementation,
		Status:              store.StatusActive,
		ActiveInvocationIDs: []string{invocation.ID},
		SpecificationPacket: string(packetData),
		CreatedAt:           createdAt,
		UpdatedAt:           createdAt,
	}
	runStore := &progressionDispatchStore{run: run, active: []store.Invocation{invocation}}
	registration := config.RepositoryRegistration{OperationalDataPath: "/state/factory.db"}
	service := &Service{
		deps: Dependencies{
			OpenStore: func(context.Context, string) (OperationalStore, error) {
				return runStore, nil
			},
			GitHub: progressionDispatchGitHub{},
			Now: func() time.Time {
				return now
			},
		},
	}

	result, err := service.driveRun(context.Background(), registration)
	if err != nil {
		t.Fatalf("driveRun() error = %v", err)
	}
	if result.Outcome != progressionWaiting {
		t.Fatalf("driveRun() outcome = %q, want waiting after the deadline", result.Outcome)
	}
	if result.Run.Status != store.StatusWaitingForHuman {
		t.Fatalf("driveRun() status = %q, want waiting_for_human", result.Run.Status)
	}
	for _, reason := range []string{result.Reason, result.Run.LifecycleReason} {
		if !strings.Contains(reason, invocation.ID) || !strings.Contains(reason, "2h0m0s") {
			t.Fatalf("pause reason = %q, want invocation %q and elapsed time", reason, invocation.ID)
		}
	}
}

// TestDriveRunKeepsAnActiveInvocationInsideItsDeadline verifies a report-less
// invocation remains active while its frozen agent deadline has not elapsed.
func TestDriveRunKeepsAnActiveInvocationInsideItsDeadline(t *testing.T) {
	now := time.Date(2026, 9, 3, 13, 30, 0, 0, time.UTC)
	createdAt := now.Add(-30 * time.Minute)
	packetData, err := json.Marshal(SpecificationPacket{
		Version: specificationPacketVersion,
		RepositoryConfig: config.RepositoryConfig{
			Timeouts: config.TimeoutConfig{Agent: "1h"},
		},
	})
	if err != nil {
		t.Fatalf("marshal deadline packet: %v", err)
	}
	invocation := store.Invocation{
		ID:              "inv-within-deadline",
		RunID:           "run-within-deadline",
		Role:            workflow.RoleImplementation,
		Stage:           store.StageImplementation,
		Status:          store.InvocationStatusActive,
		ResultDirectory: t.TempDir(),
		CreatedAt:       createdAt,
		UpdatedAt:       createdAt,
	}
	run := store.Run{
		ID:                  invocation.RunID,
		Stage:               store.StageImplementation,
		Status:              store.StatusActive,
		ActiveInvocationIDs: []string{invocation.ID},
		SpecificationPacket: string(packetData),
		CreatedAt:           createdAt,
		UpdatedAt:           createdAt,
	}
	runStore := &progressionDispatchStore{run: run, active: []store.Invocation{invocation}}
	registration := config.RepositoryRegistration{OperationalDataPath: "/state/factory.db"}
	service := &Service{
		deps: Dependencies{
			OpenStore: func(context.Context, string) (OperationalStore, error) {
				return runStore, nil
			},
			GitHub: progressionDispatchGitHub{},
			Now: func() time.Time {
				return now
			},
		},
	}

	result, err := service.driveRun(context.Background(), registration)
	if err != nil {
		t.Fatalf("driveRun() error = %v", err)
	}
	if result.Outcome != progressionInvocationActive {
		t.Fatalf("driveRun() outcome = %q, want invocation_active before the deadline", result.Outcome)
	}
	if result.Run.Status != store.StatusActive {
		t.Fatalf("driveRun() status = %q, want active before the deadline", result.Run.Status)
	}
}

// TestProgressionStepPlansEveryCoordinatorCommand verifies each legal planner
// branch returns a validated typed command while preserving its transition
// name and required operands.
func TestProgressionStepPlansEveryCoordinatorCommand(t *testing.T) {
	registry := workflow.DefaultRegistry()
	runWithReviews := progressionRunWithConcurrentReviews(t)
	runWithMissingReview := runWithReviews
	runWithMissingReview.ID = "run-missing-review"
	runWithMissingReview.Stage = store.StageReview
	runWithMissingReview.CheckpointSHA = "checkpoint"
	runWithMissingReview.SpecificationReview = &store.SpecificationReview{CheckpointSHA: "checkpoint"}
	tests := []struct {
		name           string
		state          progressionState
		wantKind       progressionActionKind
		wantName       string
		wantInvocation string
		wantRole       string
		wantStage      store.Stage
	}{
		{
			name:     "baseline",
			state:    progressionState{Run: &store.Run{ID: "run-baseline", Stage: store.StageClaim, Status: store.StatusActive}},
			wantKind: progressionActionRunBaseline,
			wantName: "run baseline",
		},
		{
			name:     "start agent",
			state:    progressionState{Run: &store.Run{ID: "run-agent", Stage: store.StageImplementation, Status: store.StatusActive}},
			wantKind: progressionActionStartAgent,
			wantName: "start agent",
		},
		{
			name:      "start missing review",
			state:     progressionState{Run: &runWithMissingReview},
			wantKind:  progressionActionStartAgent,
			wantName:  "start missing standards_review review",
			wantRole:  workflow.RoleStandardsReview,
			wantStage: workflow.StageStandardsReview,
		},
		{
			name: "accept report",
			state: progressionState{
				Run:                &store.Run{ID: "run-report", Stage: store.StageImplementation, Status: store.StatusActive},
				ActiveInvocations:  []*store.Invocation{{ID: "inv-report"}},
				ReadyInvocationIDs: map[string]bool{"inv-report": true},
			},
			wantKind:       progressionActionAcceptAgentReport,
			wantName:       "accept agent report",
			wantInvocation: "inv-report",
		},
		{
			name: "create draft pull request",
			state: progressionState{
				Run: &store.Run{ID: "run-draft", Stage: store.StageCheck, Status: store.StatusActive},
			},
			wantKind: progressionActionCreateDraftPullRequest,
			wantName: "create draft pull request",
		},
		{
			name:     "start review round",
			state:    progressionState{Run: &runWithReviews},
			wantKind: progressionActionStartReviewRound,
			wantName: "start concurrent reviews",
		},
		{
			name:     "retry review readiness",
			state:    progressionState{Run: &store.Run{ID: "run-readiness", Stage: store.StageReview, Status: store.StatusActive}},
			wantKind: progressionActionRetryReviewReadiness,
			wantName: "finalize pull-request readiness",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			action, stop := progressionStep(test.state, registry)
			if stop != nil {
				t.Fatalf("progressionStep() stopped with %#v, want %s", stop, test.wantKind)
			}
			if action.kind != test.wantKind || action.name != test.wantName {
				t.Fatalf("planned action = %#v, want kind=%q name=%q", action, test.wantKind, test.wantName)
			}
			if action.runID == "" {
				t.Fatal("planned action has no run id")
			}
			if action.invocationID != test.wantInvocation {
				t.Fatalf("planned invocation id = %q, want %q", action.invocationID, test.wantInvocation)
			}
			if action.role != test.wantRole || action.stage != test.wantStage {
				t.Fatalf("planned role/stage = %q/%q, want %q/%q", action.role, action.stage, test.wantRole, test.wantStage)
			}
			if err := action.validate(); err != nil {
				t.Fatalf("planned action validation error = %v", err)
			}
		})
	}
}

// TestProgressionActionValidationRejectsInvalidKindAndOperands verifies the
// closed command set rejects malformed commands before coordinator dispatch.
func TestProgressionActionValidationRejectsInvalidKindAndOperands(t *testing.T) {
	tests := []progressionAction{
		{kind: progressionActionKind("invented"), name: "invented", runID: "run-1"},
		{kind: progressionActionAcceptAgentReport, name: "accept agent report", runID: "run-1", role: workflow.RoleTest, invocationID: "inv-1"},
		{kind: progressionActionStartAgent, name: "start agent", runID: "run-1", role: workflow.RoleTest},
	}

	for index, action := range tests {
		if err := action.validate(); err == nil {
			t.Fatalf("action %d validation error = nil, want typed invalid-action error", index)
		} else if _, ok := err.(*progressionActionError); !ok {
			t.Fatalf("action %d validation error type = %T, want *progressionActionError", index, err)
		}
	}
}

// TestDriveRunDispatchesEveryProgressionCommand verifies the real coordinator
// dispatch reaches the existing seam for every declared progression command.
// Each seam returns context.Canceled after recording its call, which makes the
// test stop before any unrelated effect can obscure the dispatch assertion.
func TestDriveRunDispatchesEveryProgressionCommand(t *testing.T) {
	const checkpoint = "0123456789abcdef0123456789abcdef01234567"
	reportDirectory := t.TempDir()
	if err := os.WriteFile(filepath.Join(reportDirectory, report.ReportFileName), nil, 0o600); err != nil {
		t.Fatalf("write ready report marker: %v", err)
	}

	tests := []struct {
		name               string
		run                store.Run
		active             []store.Invocation
		pullRequestErrors  []error
		wantInspectCalls   int
		wantGateCalls      int
		wantInvocationCall int
		wantFindCalls      int
	}{
		{
			name: "run baseline",
			run: store.Run{
				ID:                  "run-baseline-dispatch",
				Stage:               store.StageClaim,
				Status:              store.StatusActive,
				SpecificationPacket: progressionDispatchPacket(t, false),
				Worktree:            "/worktree",
			},
			wantInspectCalls: 1,
		},
		{
			name: "start agent",
			run: store.Run{
				ID:                  "run-agent-dispatch",
				Stage:               store.StageImplementation,
				Status:              store.StatusActive,
				CheckpointSHA:       checkpoint,
				SpecificationPacket: progressionDispatchPacket(t, false),
				Worktree:            "/worktree",
			},
			wantGateCalls: 1,
		},
		{
			name: "accept agent report",
			run: store.Run{
				ID:                  "run-report-dispatch",
				Stage:               store.StageImplementation,
				Status:              store.StatusActive,
				ActiveInvocationIDs: []string{"inv-report-dispatch"},
				SpecificationPacket: progressionDispatchPacket(t, false),
				Worktree:            "/worktree",
			},
			active: []store.Invocation{{
				ID:              "inv-report-dispatch",
				RunID:           "run-report-dispatch",
				Role:            workflow.RoleImplementation,
				Stage:           store.StageImplementation,
				Status:          store.InvocationStatusActive,
				ResultDirectory: reportDirectory,
			}},
			wantInvocationCall: 1,
		},
		{
			name: "create draft pull request",
			run: store.Run{
				ID:                  "run-draft-dispatch",
				Stage:               store.StageCheck,
				Status:              store.StatusActive,
				CheckpointSHA:       checkpoint,
				SpecificationPacket: progressionDispatchPacket(t, false),
				Worktree:            "/worktree",
			},
			wantInspectCalls: 1,
		},
		{
			name: "start review round",
			run: store.Run{
				ID:                  "run-review-dispatch",
				Stage:               store.StageDraftPR,
				Status:              store.StatusActive,
				PullRequestNumber:   17,
				CheckpointSHA:       checkpoint,
				BaseCheckpointSHA:   checkpoint,
				SpecificationPacket: progressionDispatchPacket(t, true),
				Worktree:            "/worktree",
			},
			pullRequestErrors: []error{nil, context.Canceled},
			wantInspectCalls:  1,
			wantFindCalls:     1,
		},
		{
			name: "retry review readiness",
			run: store.Run{
				ID:                  "run-readiness-dispatch",
				Stage:               store.StageReview,
				Status:              store.StatusActive,
				CheckpointSHA:       checkpoint,
				BaseCheckpointSHA:   checkpoint,
				SpecificationPacket: progressionDispatchPacket(t, true),
				SpecificationReview: &store.SpecificationReview{CheckpointSHA: checkpoint},
				StandardsReview:     &store.StandardsReview{CheckpointSHA: checkpoint},
			},
			pullRequestErrors: []error{context.Canceled},
			wantFindCalls:     1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runStore := &progressionDispatchStore{
				run:            test.run,
				active:         append([]store.Invocation(nil), test.active...),
				gateResultsErr: context.Canceled,
				invocationErr:  context.Canceled,
			}
			workspace := &progressionDispatchWorkspace{inspectErr: context.Canceled}
			pullRequests := &progressionDispatchPullRequests{
				findErrors: append([]error(nil), test.pullRequestErrors...),
			}
			registration := config.RepositoryRegistration{
				Path:                 "/repository",
				OperationalDataPath:  "/state/factory.db",
				RepositoryConfigPath: "/repository/factory.yaml",
				GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
			}
			service := &Service{
				configPath: "/host/config.yaml",
				deps: Dependencies{
					Config: progressionDispatchConfig{registration: registration},
					OpenStore: func(context.Context, string) (OperationalStore, error) {
						return runStore, nil
					},
					GitHub:       progressionDispatchGitHub{},
					GitWorkspace: workspace,
					PullRequests: pullRequests,
					Now: func() time.Time {
						return time.Unix(1, 0).UTC()
					},
					NewRunID: func() (string, error) {
						return "dispatch-generated", nil
					},
				},
				startupChecked: true,
			}

			result, err := service.driveRun(context.Background(), registration)
			if err != nil {
				t.Fatalf("driveRun() error = %v", err)
			}
			if result.Outcome != progressionWaiting || result.Reason != "coordinator stopped" {
				t.Fatalf("driveRun() result = %#v, want context-canceled dispatch stop", result)
			}
			if result.Steps != 0 {
				t.Fatalf("dispatch steps = %d, want zero after the controlled seam stop", result.Steps)
			}
			if workspace.inspectCalls != test.wantInspectCalls {
				t.Fatalf("worktree inspection calls = %d, want %d", workspace.inspectCalls, test.wantInspectCalls)
			}
			if runStore.gateResultsCalls != test.wantGateCalls {
				t.Fatalf("gate-result calls = %d, want %d", runStore.gateResultsCalls, test.wantGateCalls)
			}
			if runStore.invocationCalls != test.wantInvocationCall {
				t.Fatalf("invocation lookup calls = %d, want %d", runStore.invocationCalls, test.wantInvocationCall)
			}
			if pullRequests.findCalls != test.wantFindCalls {
				t.Fatalf("pull-request lookup calls = %d, want %d", pullRequests.findCalls, test.wantFindCalls)
			}
		})
	}
}

// progressionDispatchConfig supplies the registration that coordinator seams
// reload while the dispatch test is driving a run.
type progressionDispatchConfig struct {
	registration config.RepositoryRegistration
}

// Load returns the controlled repository registration.
func (c progressionDispatchConfig) Load(string) (config.HostConfig, error) {
	return config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{c.registration}}, nil
}

// Save satisfies the configuration repository seam.
func (progressionDispatchConfig) Save(string, config.HostConfig) error { return nil }

// Create satisfies the configuration repository seam.
func (progressionDispatchConfig) Create(string) (config.HostConfig, error) {
	return config.HostConfig{}, nil
}

// progressionDispatchPacket builds the smallest frozen packet accepted by the
// coordinator seams exercised by TestDriveRunDispatchesEveryProgressionCommand.
func progressionDispatchPacket(t *testing.T, reviews bool) string {
	t.Helper()
	packet := SpecificationPacket{
		Version: specificationPacketVersion,
		RepositoryConfig: config.RepositoryConfig{
			TargetBranch: "main",
			TestPolicy:   config.TestPolicy{Mode: config.TestModeAdvisory},
			RoleHarnessDefaults: map[string]config.Harness{
				workflow.RoleImplementation: config.HarnessCodex,
			},
			ModelOptions: map[string][]string{
				workflow.RoleImplementation: {"gpt-5"},
			},
		},
	}
	if reviews {
		packet.RepositoryConfig.RoleHarnessDefaults[workflow.RoleSpecificationReview] = config.HarnessCodex
		packet.RepositoryConfig.RoleHarnessDefaults[workflow.RoleStandardsReview] = config.HarnessCodex
		packet.RepositoryConfig.ModelOptions[workflow.RoleSpecificationReview] = []string{"gpt-5"}
		packet.RepositoryConfig.ModelOptions[workflow.RoleStandardsReview] = []string{"gpt-5"}
	}
	data, err := json.Marshal(packet)
	if err != nil {
		t.Fatalf("marshal dispatch packet: %v", err)
	}
	return string(data)
}

// progressionDispatchStore is an in-memory operational store that exposes the
// read projections driveRun needs and records seam-specific observations.
type progressionDispatchStore struct {
	run              store.Run
	active           []store.Invocation
	latest           *store.Invocation
	gateResultsErr   error
	invocationErr    error
	gateResultsCalls int
	invocationCalls  int
}

// CurrentRun returns the controlled active run projection.
func (s *progressionDispatchStore) CurrentRun(context.Context) (*store.Run, error) {
	run := s.run
	return &run, nil
}

// SaveRun records a run save if a future dispatch path reaches one.
func (s *progressionDispatchStore) SaveRun(_ context.Context, run store.Run) error {
	s.run = run
	return nil
}

// ClaimLifecycleNotification satisfies the run-store notification seam.
func (*progressionDispatchStore) ClaimLifecycleNotification(context.Context, string, store.Status) (bool, error) {
	return true, nil
}

// ReleaseLifecycleNotification satisfies the run-store notification seam.
func (*progressionDispatchStore) ReleaseLifecycleNotification(context.Context, string, store.Status) error {
	return nil
}

// Close satisfies the operational-store seam.
func (*progressionDispatchStore) Close() error { return nil }

// ActiveInvocations returns the controlled active-invocation projection.
func (s *progressionDispatchStore) ActiveInvocations(context.Context, string) ([]store.Invocation, error) {
	return append([]store.Invocation(nil), s.active...), nil
}

// LatestInvocation returns the controlled latest invocation projection.
func (s *progressionDispatchStore) LatestInvocation(context.Context, string) (*store.Invocation, error) {
	if s.latest == nil {
		return nil, nil
	}
	latest := *s.latest
	return &latest, nil
}

// SaveGateResults satisfies the gate-result persistence seam.
func (*progressionDispatchStore) SaveGateResults(context.Context, []store.GateResult) error {
	return nil
}

// GateResults records the gate-readiness observation used by agent launch.
func (s *progressionDispatchStore) GateResults(context.Context, string, store.GatePhase, string) ([]store.GateResult, error) {
	s.gateResultsCalls++
	return nil, s.gateResultsErr
}

// SaveInvocation satisfies the visible-invocation persistence seam.
func (*progressionDispatchStore) SaveInvocation(context.Context, store.Invocation) error {
	return nil
}

// Invocation records the report-acceptance invocation lookup.
func (s *progressionDispatchStore) Invocation(context.Context, string, string) (*store.Invocation, error) {
	s.invocationCalls++
	return nil, s.invocationErr
}

// progressionDispatchWorkspace is a controlled Git workspace whose inspection
// error identifies the baseline, draft, or review launch seam being exercised.
type progressionDispatchWorkspace struct {
	inspectErr   error
	inspectCalls int
}

// Create creates no workspace because dispatch tests stop at inspection.
func (*progressionDispatchWorkspace) Create(context.Context, string, string, string) (gitadapter.Workspace, error) {
	return gitadapter.Workspace{}, context.Canceled
}

// Remove satisfies the worktree-management seam.
func (*progressionDispatchWorkspace) Remove(context.Context, string, gitadapter.Workspace) error {
	return context.Canceled
}

// Inspect records the read-only seam reached by the dispatched command.
func (w *progressionDispatchWorkspace) Inspect(context.Context, string) (gitadapter.WorktreeState, error) {
	w.inspectCalls++
	return gitadapter.WorktreeState{}, w.inspectErr
}

// CreateCheckpoint satisfies the checkpoint seam.
func (*progressionDispatchWorkspace) CreateCheckpoint(context.Context, gitadapter.CheckpointRequest) (gitadapter.CheckpointResult, error) {
	return gitadapter.CheckpointResult{}, context.Canceled
}

// Push satisfies the branch-push seam.
func (*progressionDispatchWorkspace) Push(context.Context, gitadapter.PushRequest) error {
	return context.Canceled
}

// SynchronizeBase satisfies the target-branch synchronization seam.
func (*progressionDispatchWorkspace) SynchronizeBase(context.Context, gitadapter.BaseSyncRequest) error {
	return context.Canceled
}

// progressionDispatchPullRequests is a controlled pull-request client whose
// ordered errors distinguish lifecycle observation from readiness retry.
type progressionDispatchPullRequests struct {
	findErrors []error
	findCalls  int
}

// FindPullRequest records a pull-request lookup and returns its configured error.
func (p *progressionDispatchPullRequests) FindPullRequest(context.Context, github.Repository, string, string) (github.PullRequest, error) {
	p.findCalls++
	if len(p.findErrors) == 0 {
		return github.PullRequest{}, nil
	}
	err := p.findErrors[0]
	p.findErrors = p.findErrors[1:]
	return github.PullRequest{}, err
}

// CreatePullRequest satisfies the pull-request creation seam.
func (*progressionDispatchPullRequests) CreatePullRequest(context.Context, github.Repository, github.PullRequestRequest) (github.PullRequest, error) {
	return github.PullRequest{}, context.Canceled
}

// UpdatePullRequest satisfies the pull-request update seam.
func (*progressionDispatchPullRequests) UpdatePullRequest(context.Context, github.Repository, int, github.PullRequestRequest) (github.PullRequest, error) {
	return github.PullRequest{}, context.Canceled
}

// progressionDispatchGitHub is the minimal issue client needed to let a
// tracked draft pull request reach the review-round dispatch in driveRun.
type progressionDispatchGitHub struct{}

// Issue reports an open issue during lifecycle observation.
func (progressionDispatchGitHub) Issue(context.Context, github.Repository, int) (github.Issue, error) {
	return github.Issue{State: "open"}, nil
}

// CreateLabel satisfies the issue client seam.
func (progressionDispatchGitHub) CreateLabel(context.Context, github.Repository, github.Label) error {
	return context.Canceled
}

// ReplaceIssueLabels satisfies the issue client seam.
func (progressionDispatchGitHub) ReplaceIssueLabels(context.Context, github.Repository, int, []string) error {
	return context.Canceled
}

// CreateIssueComment satisfies the issue client seam.
func (progressionDispatchGitHub) CreateIssueComment(context.Context, github.Repository, int, string) (github.Comment, error) {
	return github.Comment{}, context.Canceled
}

// FindStatusComment satisfies the issue client seam.
func (progressionDispatchGitHub) FindStatusComment(context.Context, github.Repository, int, string) (github.Comment, error) {
	return github.Comment{}, context.Canceled
}

// EditIssueComment satisfies the issue client seam.
func (progressionDispatchGitHub) EditIssueComment(context.Context, github.Repository, string, string) error {
	return context.Canceled
}

var (
	_ RunStore                 = (*progressionDispatchStore)(nil)
	_ ActiveInvocationsStore   = (*progressionDispatchStore)(nil)
	_ LatestInvocationStore    = (*progressionDispatchStore)(nil)
	_ GateResultStore          = (*progressionDispatchStore)(nil)
	_ InvocationStore          = (*progressionDispatchStore)(nil)
	_ gitadapter.GitWorkspace  = (*progressionDispatchWorkspace)(nil)
	_ github.Client            = progressionDispatchGitHub{}
	_ github.PullRequestClient = (*progressionDispatchPullRequests)(nil)
)

// progressionRunWithConcurrentReviews builds a frozen packet that enables
// both factory-owned review roles for planner branch coverage.
func progressionRunWithConcurrentReviews(t *testing.T) store.Run {
	t.Helper()
	packetData, err := json.Marshal(SpecificationPacket{
		Version: specificationPacketVersion,
		RepositoryConfig: config.RepositoryConfig{
			RoleHarnessDefaults: map[string]config.Harness{
				workflow.RoleSpecificationReview: config.HarnessCodex,
				workflow.RoleStandardsReview:     config.HarnessCodex,
			},
			ModelOptions: map[string][]string{
				workflow.RoleSpecificationReview: {"gpt-5"},
				workflow.RoleStandardsReview:     {"gpt-5"},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal concurrent review packet: %v", err)
	}
	return store.Run{
		ID:                  "run-reviews",
		Stage:               store.StageDraftPR,
		Status:              store.StatusActive,
		SpecificationPacket: string(packetData),
	}
}
