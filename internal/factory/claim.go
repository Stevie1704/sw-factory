package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// specificationPacketVersion is the first instance version assigned at claim.
const specificationPacketVersion = 1

// stateTransitionRetryDelay bounds the pause before the one persistence retry.
const stateTransitionRetryDelay = 10 * time.Millisecond

// Clarification is one accepted human answer retained in a specification
// packet so later invocations receive the resolved product intent.
type Clarification struct {
	// QuestionID identifies the question answered by the maintainer.
	QuestionID string `json:"question_id"`
	// Question preserves the prompt that the answer resolves.
	Question string `json:"question"`
	// Answer is the authorized maintainer's response.
	Answer string `json:"answer"`
}

// SpecificationPacket is the versioned issue snapshot and resolved repository
// configuration captured when a run is claimed or intentionally refreshed.
type SpecificationPacket struct {
	Version          int                     `json:"version"`
	Issue            github.Issue            `json:"issue"`
	RepositoryConfig config.RepositoryConfig `json:"repository_config"`
	// Clarifications contains the authorized answers resolved after claim.
	Clarifications []Clarification `json:"clarifications,omitempty"`
}

// BootstrapLabelsResult reports the labels explicitly created for a repository.
type BootstrapLabelsResult struct {
	Repository github.Repository
	Labels     []string
}

// IssueResult contains the claimed run and its decoded frozen packet.
type IssueResult struct {
	Run    store.Run
	Packet SpecificationPacket
}

// TransitionRequest selects the next orthogonal stage and status for the
// active run. RunID is optional when the repository has one active run.
type TransitionRequest struct {
	RunID  string
	Stage  store.Stage
	Status store.Status
}

// factoryLabels is the sole factory-owned label definition set.
var factoryLabels = []github.Label{
	{Name: github.LabelAgentReady, Description: "Issue is explicitly authorized for factory work", Color: "0e8a16"},
	{Name: github.LabelAgentRunning, Description: "Factory run is active", Color: "1d76db"},
	{Name: github.LabelAgentNeedsInput, Description: "Factory run is waiting for human input", Color: "fbca04"},
	{Name: github.LabelAgentFailed, Description: "Factory run failed and needs attention", Color: "b60205"},
	{Name: github.LabelAgentCancelled, Description: "Factory run was cancelled", Color: "6f42c1"},
	{Name: github.LabelAgentComplete, Description: "Factory run completed", Color: "5319e7"},
}

// factoryLabelByStatus maps orthogonal status values to product labels.
var factoryLabelByStatus = map[store.Status]string{
	store.StatusActive:            github.LabelAgentRunning,
	store.StatusWaitingForHarness: github.LabelAgentRunning,
	store.StatusWaitingForHuman:   github.LabelAgentNeedsInput,
	store.StatusFailed:            github.LabelAgentFailed,
	store.StatusCancelled:         github.LabelAgentCancelled,
	store.StatusComplete:          github.LabelAgentComplete,
}

// validStatuses contains the independent statuses accepted by transitions.
var validStatuses = map[store.Status]struct{}{
	store.StatusActive: {}, store.StatusWaitingForHuman: {}, store.StatusWaitingForHarness: {},
	store.StatusFailed: {}, store.StatusCancelled: {}, store.StatusComplete: {},
}

// stateTransition is the shared state-machine input used by initial claims
// and later transitions. The only difference is whether a status comment is
// created or edited.
type stateTransition struct {
	Repository    github.Repository
	Issue         github.Issue
	Previous      store.Run
	Next          store.Run
	CreateComment bool
	// StopWorker makes terminal transitions keep worker shutdown inside the
	// same durable effect as the GitHub and run projections.
	StopWorker bool
	// PersistBeforeEffects is used by replay-sensitive commands so the
	// processed-comment watermark is durable before GitHub mutation.
	PersistBeforeEffects bool
	// InvalidateResults makes a packet revision boundary invalidate prior
	// invocation and gate projections atomically with the run update.
	InvalidateResults bool
}

// BootstrapLabels explicitly creates or updates the factory-owned labels for
// the registered repository. No claim path calls this operation implicitly.
func (s *Service) BootstrapLabels(ctx context.Context) (BootstrapLabelsResult, error) {
	registration, err := s.registration()
	if err != nil {
		return BootstrapLabelsResult{}, err
	}
	repository := github.Repository{Owner: registration.GitHub.Owner, Name: registration.GitHub.Repository}
	for _, label := range factoryLabels {
		if err := s.deps.GitHub.CreateLabel(ctx, repository, label); err != nil {
			return BootstrapLabelsResult{}, err
		}
	}
	labels := make([]string, 0, len(factoryLabels))
	for _, label := range factoryLabels {
		labels = append(labels, label.Name)
	}
	return BootstrapLabelsResult{Repository: repository, Labels: labels}, nil
}

// ClaimIssue claims exactly one eligible issue and runs the same claim transition
// that a future poller will call. It freezes the issue and resolved repository
// configuration before changing the issue's factory state.
func (s *Service) ClaimIssue(ctx context.Context, issueNumber int) (IssueResult, error) {
	if issueNumber <= 0 {
		return IssueResult{}, errors.New("issue number must be positive")
	}
	registration, runStore, current, err := s.openActiveRunStore(ctx)
	if err != nil {
		return IssueResult{}, err
	}
	defer func() { _ = runStore.Close() }()
	repositoryConfig, err := s.deps.LoadRepository(registration.RepositoryConfigPath)
	if err != nil {
		return IssueResult{}, fmt.Errorf("load repository configuration: %w", err)
	}
	if err := harness.ValidateInteractiveResumeCapabilities(repositoryConfig, s.deps.HarnessCapabilities); err != nil {
		return IssueResult{}, fmt.Errorf("validate harness capabilities before claim: %w", err)
	}
	repository := github.Repository{Owner: registration.GitHub.Owner, Name: registration.GitHub.Repository}
	if current != nil {
		return IssueResult{}, fmt.Errorf("an active run already exists: %s", current.ID)
	}

	issue, err := s.deps.GitHub.Issue(ctx, repository, issueNumber)
	if err != nil {
		return IssueResult{}, err
	}
	if issue.Number == 0 {
		issue.Number = issueNumber
	}
	if issue.Number != issueNumber {
		return IssueResult{}, fmt.Errorf("GitHub returned issue #%d while claiming #%d", issue.Number, issueNumber)
	}
	if issue.IsPullRequest {
		return IssueResult{}, fmt.Errorf("issue #%d is a pull request; pull requests cannot be claimed", issueNumber)
	}
	if !strings.EqualFold(strings.TrimSpace(issue.State), "open") {
		return IssueResult{}, fmt.Errorf("issue #%d is %s; only open issues can be claimed", issueNumber, defaultString(issue.State, "not open"))
	}
	if !hasLabel(issue.Labels, github.LabelAgentReady) {
		return IssueResult{}, fmt.Errorf("issue #%d is not labeled %q", issueNumber, github.LabelAgentReady)
	}
	if conflictingFactoryLabel(issue.Labels) {
		return IssueResult{}, fmt.Errorf("issue #%d has another factory state label", issueNumber)
	}

	runID, err := s.deps.NewRunID()
	if err != nil {
		return IssueResult{}, fmt.Errorf("generate run identifier: %w", err)
	}
	if strings.TrimSpace(runID) == "" {
		return IssueResult{}, errors.New("run identifier must not be empty")
	}
	startedAt := s.deps.Now().UTC()
	packet := SpecificationPacket{
		Version:          specificationPacketVersion,
		Issue:            cloneIssue(issue),
		RepositoryConfig: repositoryConfig,
	}
	packetData, err := json.Marshal(packet)
	if err != nil {
		return IssueResult{}, fmt.Errorf("freeze specification packet: %w", err)
	}
	worktreeManager := s.worktreeManager()
	if worktreeManager == nil {
		return IssueResult{}, errors.New("GitWorkspace is required to create a run worktree")
	}
	workspace, err := worktreeManager.Create(ctx, registration.Path, repositoryConfig.TargetBranch, runID)
	if err != nil {
		return IssueResult{}, err
	}
	cleanupWorkspace := func(cause error) error {
		if cleanupErr := worktreeManager.Remove(ctx, registration.Path, workspace); cleanupErr != nil {
			return errors.Join(cause, fmt.Errorf("remove claim workspace: %w", cleanupErr))
		}
		return cause
	}
	run := store.Run{
		ID:                  runID,
		RepositoryPath:      registration.Path,
		IssueNumber:         issueNumber,
		Stage:               store.StageClaim,
		Status:              store.StatusActive,
		Branch:              workspace.Branch,
		Worktree:            workspace.Worktree,
		CheckpointSHA:       workspace.BaseSHA,
		BaseCheckpointSHA:   workspace.BaseSHA,
		ImageDigest:         repositoryConfig.WorkerBuild.Digest,
		Coordinator:         s.deps.Coordinator,
		CheckRepairBudget:   repositoryConfig.RetryLimits.CheckRepair,
		SpecificationPacket: string(packetData),
		CreatedAt:           startedAt,
		UpdatedAt:           startedAt,
	}
	if err := runStore.SaveRun(ctx, run); err != nil {
		return IssueResult{}, cleanupWorkspace(fmt.Errorf("persist frozen specification packet: %w", err))
	}
	if recorder, ok := runStore.(evaluationRecorder); ok {
		if err := recorder.EnsureEvaluationSummary(ctx, run); err != nil {
			_, failureErr := s.failClaim(ctx, runStore, run, repository, issue, fmt.Errorf("persist local evaluation summary: %w", err))
			return IssueResult{}, cleanupWorkspace(failureErr)
		}
	}

	run, err = s.applyStateTransition(ctx, runStore, stateTransition{
		Repository:    repository,
		Issue:         issue,
		Next:          run,
		CreateComment: true,
	})
	if err != nil {
		_, failureErr := s.failClaim(ctx, runStore, run, repository, issue, err)
		return IssueResult{}, cleanupWorkspace(failureErr)
	}
	return IssueResult{Run: run, Packet: packet}, nil
}

// Transition edits the existing status comment and updates the orthogonal
// stage/status values in one persisted run record. It never creates a second
// status comment.
func (s *Service) Transition(ctx context.Context, request TransitionRequest) (store.Run, error) {
	if err := validateStage(request.Stage); err != nil {
		return store.Run{}, err
	}
	if err := validateStatus(request.Status); err != nil {
		return store.Run{}, err
	}
	registration, runStore, run, err := s.openActiveRunStore(ctx)
	if err != nil {
		return store.Run{}, err
	}
	defer func() { _ = runStore.Close() }()
	if run == nil {
		return store.Run{}, errors.New("no active run")
	}
	if request.RunID != "" && request.RunID != run.ID {
		return store.Run{}, fmt.Errorf("active run is %s, not %s", run.ID, request.RunID)
	}
	if err := s.ensureInvocationAttached(ctx, runStore, *run); err != nil {
		return store.Run{}, err
	}
	if request.Stage == store.StageTest || request.Stage == store.StageImplementation {
		if strings.TrimSpace(run.SpecificationPacket) == "" {
			return store.Run{}, errors.New("stage transition requires a frozen specification packet")
		}
		packet, err := decodeSpecificationPacket(run.SpecificationPacket)
		if err != nil {
			return store.Run{}, fmt.Errorf("decode specification packet for stage transition: %w", err)
		}
		if err := validateTestStageTransition(*run, packet, request.Stage); err != nil {
			return store.Run{}, err
		}
	}
	if err := s.ensureTransitionBaseline(ctx, runStore, *run, request); err != nil {
		return store.Run{}, err
	}
	repository := github.Repository{Owner: registration.GitHub.Owner, Name: registration.GitHub.Repository}
	issue, err := s.deps.GitHub.Issue(ctx, repository, run.IssueNumber)
	if err != nil {
		return store.Run{}, err
	}
	if run.StatusCommentID == "" {
		comment, err := s.deps.GitHub.FindStatusComment(ctx, repository, run.IssueNumber, statusCommentMarker(run.ID))
		if err != nil {
			return store.Run{}, err
		}
		if strings.TrimSpace(comment.ID) == "" {
			return store.Run{}, errors.New("active run has no recoverable status comment")
		}
		run.StatusCommentID = comment.ID
	}
	previous := *run
	next := previous
	next.Stage = request.Stage
	next.Status = request.Status
	next.UpdatedAt = s.deps.Now().UTC()
	updated, err := s.applyStateTransition(ctx, runStore, stateTransition{
		Repository: repository,
		Issue:      issue,
		Previous:   previous,
		Next:       next,
	})
	if err != nil {
		return store.Run{}, err
	}
	return updated, nil
}

// applyStateTransition performs the shared label/comment/store transition for
// both the claim path and later state changes. It retries a transient store
// failure once and compensates external changes when an edit transition cannot
// be persisted.
func (s *Service) applyStateTransition(ctx context.Context, runStore RunStore, transition stateTransition) (store.Run, error) {
	next := transition.Next
	if next.Revision <= transition.Previous.Revision {
		next.Revision = transition.Previous.Revision + 1
	}
	if store.IsTerminalStatus(next.Status) && !store.IsTerminalStatus(transition.Previous.Status) && next.TerminalAt.IsZero() {
		next.TerminalAt = next.UpdatedAt
		if next.TerminalAt.IsZero() {
			next.TerminalAt = s.deps.Now().UTC()
		}
	}
	if !store.IsTerminalStatus(next.Status) {
		next.TerminalAt = time.Time{}
	}
	if _, journaled := runStore.(PendingEffectStore); journaled {
		return s.applyJournaledStateTransition(ctx, runStore, transition, next)
	}
	return s.applyLegacyStateTransition(ctx, runStore, transition, next)
}

// applyJournaledStateTransition reserves the complete label/comment transition
// before crossing GitHub's mutation boundary. The reservation remains until
// both the projection and durable run state are complete.
func (s *Service) applyJournaledStateTransition(ctx context.Context, runStore RunStore, transition stateTransition, next store.Run) (store.Run, error) {
	payload := stateTransitionEffectPayload{
		Repository:        transition.Repository,
		Issue:             transition.Issue,
		Previous:          transition.Previous,
		Next:              next,
		CreateComment:     transition.CreateComment,
		StopWorker:        transition.StopWorker,
		InvalidateResults: transition.InvalidateResults,
	}
	effect, err := s.newPendingEffect(next.ID, store.PendingEffectKindStateTransition, fmt.Sprintf("revision=%d", next.Revision), payload)
	if err != nil {
		return next, err
	}
	apply := func() error {
		if transition.StopWorker {
			if err := s.stopRunWorker(ctx, transition.Previous.ID); err != nil {
				return err
			}
		}
		if transition.PersistBeforeEffects {
			next.UpdatedAt = s.deps.Now().UTC()
			if transition.CreateComment {
				return errors.New("cannot persist a command before creating its status comment")
			}
			if err := saveCommandRun(ctx, runStore, transition.Previous.Revision, next); err != nil {
				return fmt.Errorf("persist state transition before GitHub effects: %w", err)
			}
			if recorder, ok := runStore.(evaluationRecorder); ok {
				if err := recordEvaluationTransition(ctx, recorder, transition.Previous, next, next.UpdatedAt); err != nil {
					return fmt.Errorf("record evaluation state transition: %w", err)
				}
			}
		}
		if err := s.applyStateTransitionEffects(ctx, &next, transition); err != nil {
			return err
		}
		next.UpdatedAt = s.deps.Now().UTC()
		if !transition.PersistBeforeEffects {
			if transition.InvalidateResults {
				if atomicStore, ok := runStore.(atomicPacketTransitionStore); ok {
					if err := atomicStore.SaveRunAndInvalidateResults(ctx, transition.Previous.Revision, next); err != nil {
						return fmt.Errorf("persist state transition and invalidate results: %w", err)
					}
				} else {
					if err := saveCommandRun(ctx, runStore, transition.Previous.Revision, next); err != nil {
						return fmt.Errorf("persist state transition: %w", err)
					}
					if err := invalidateRunResults(ctx, runStore, next.ID); err != nil {
						return err
					}
				}
			} else if err := saveRunWithRetry(ctx, runStore, next); err != nil {
				return fmt.Errorf("persist state transition: %w", err)
			}
			if recorder, ok := runStore.(evaluationRecorder); ok {
				if err := recordEvaluationTransition(ctx, recorder, transition.Previous, next, next.UpdatedAt); err != nil {
					return fmt.Errorf("record evaluation state transition: %w", err)
				}
			}
		}
		return nil
	}
	if err := s.withPendingEffect(ctx, runStore, effect, apply); err != nil {
		return next, err
	}
	return next, nil
}

// applyLegacyStateTransition retains the pre-journal behavior for embedders
// that supply an older RunStore implementation. The production SQLite store
// implements PendingEffectStore and always uses the restart-safe path.
func (s *Service) applyLegacyStateTransition(ctx context.Context, runStore RunStore, transition stateTransition, next store.Run) (store.Run, error) {
	if transition.PersistBeforeEffects {
		next.UpdatedAt = s.deps.Now().UTC()
		if transition.CreateComment {
			return next, errors.New("cannot persist a command before creating its status comment")
		}
		if err := saveCommandRun(ctx, runStore, transition.Previous.Revision, next); err != nil {
			return next, fmt.Errorf("persist state transition before GitHub effects: %w", err)
		}
		if recorder, ok := runStore.(evaluationRecorder); ok {
			if err := recordEvaluationTransition(ctx, recorder, transition.Previous, next, next.UpdatedAt); err != nil {
				return next, fmt.Errorf("record evaluation state transition: %w", err)
			}
		}
	}
	oldLabels := append([]string(nil), transition.Issue.Labels...)
	newLabels := replaceFactoryState(oldLabels, factoryLabelForStatus(next.Status))
	if err := s.deps.GitHub.ReplaceIssueLabels(ctx, transition.Repository, next.IssueNumber, newLabels); err != nil {
		return next, fmt.Errorf("set issue #%d state: %w", next.IssueNumber, err)
	}
	if transition.CreateComment {
		comment, err := s.deps.GitHub.CreateIssueComment(ctx, transition.Repository, next.IssueNumber, statusCommentBody(next))
		if err != nil {
			return next, fmt.Errorf("create status comment: %w", err)
		}
		if strings.TrimSpace(comment.ID) == "" {
			return next, errors.New("create status comment returned an empty comment id")
		}
		next.StatusCommentID = comment.ID
	} else {
		if err := s.deps.GitHub.EditIssueComment(ctx, transition.Repository, next.StatusCommentID, statusCommentBody(next)); err != nil {
			_ = s.deps.GitHub.ReplaceIssueLabels(ctx, transition.Repository, next.IssueNumber, oldLabels)
			return next, fmt.Errorf("edit status comment: %w", err)
		}
	}
	next.UpdatedAt = s.deps.Now().UTC()
	if transition.PersistBeforeEffects {
		return next, nil
	}
	if err := saveRunWithRetry(ctx, runStore, next); err != nil {
		if !transition.CreateComment {
			compensationErrors := []error{
				s.deps.GitHub.EditIssueComment(ctx, transition.Repository, transition.Previous.StatusCommentID, statusCommentBody(transition.Previous)),
				s.deps.GitHub.ReplaceIssueLabels(ctx, transition.Repository, next.IssueNumber, oldLabels),
			}
			return next, errors.Join(append([]error{fmt.Errorf("persist state transition: %w", err)}, compensationErrors...)...)
		}
		return next, fmt.Errorf("persist claim state: %w", err)
	}
	if recorder, ok := runStore.(evaluationRecorder); ok {
		if err := recordEvaluationTransition(ctx, recorder, transition.Previous, next, next.UpdatedAt); err != nil {
			return next, fmt.Errorf("record evaluation state transition: %w", err)
		}
	}
	return next, nil
}

// saveRunWithRetry retries one failed persistence operation after a short,
// cancellable delay so transient SQLite contention can clear without delaying
// a cancelled transition.
func saveRunWithRetry(ctx context.Context, runStore RunStore, run store.Run) error {
	err := runStore.SaveRun(ctx, run)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return err
	}
	timer := time.NewTimer(stateTransitionRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return err
	case <-timer.C:
	}
	if ctx.Err() != nil {
		return err
	}
	if retryErr := runStore.SaveRun(ctx, run); retryErr == nil {
		return nil
	}
	return err
}

// registration loads the one configured repository registration.
func (s *Service) registration() (config.RepositoryRegistration, error) {
	if s.configPath == "" {
		return config.RepositoryRegistration{}, errors.New("host configuration path is required")
	}
	host, err := s.deps.Config.Load(s.configPath)
	if err != nil {
		return config.RepositoryRegistration{}, err
	}
	if len(host.Repositories) == 0 {
		return config.RepositoryRegistration{}, errors.New("no repository is registered")
	}
	return host.Repositories[0], nil
}

// openRunStore loads the registration and opens the claim-capable store once.
func (s *Service) openRunStore(ctx context.Context) (config.RepositoryRegistration, RunStore, error) {
	registration, err := s.registration()
	if err != nil {
		return config.RepositoryRegistration{}, nil, err
	}
	opened, err := s.deps.OpenStore(ctx, registration.OperationalDataPath)
	if err != nil {
		return config.RepositoryRegistration{}, nil, err
	}
	runStore, ok := opened.(RunStore)
	if !ok {
		_ = opened.Close()
		return config.RepositoryRegistration{}, nil, errors.New("operational store does not support run coordination")
	}
	return registration, runStore, nil
}

// openActiveRunStore opens one claim-capable store and reads its active run
// once, so claim and transition share the same state-selection logic.
func (s *Service) openActiveRunStore(ctx context.Context) (config.RepositoryRegistration, RunStore, *store.Run, error) {
	registration, runStore, err := s.openRunStore(ctx)
	if err != nil {
		return config.RepositoryRegistration{}, nil, nil, err
	}
	run, err := runStore.CurrentRun(ctx)
	if err != nil {
		_ = runStore.Close()
		return config.RepositoryRegistration{}, nil, nil, err
	}
	if err := s.ensureProgressionStartup(ctx, registration, runStore, run); err != nil {
		_ = runStore.Close()
		return config.RepositoryRegistration{}, nil, nil, err
	}
	if err := s.drainPendingEffect(ctx, registration, runStore, run); err != nil {
		_ = runStore.Close()
		return config.RepositoryRegistration{}, nil, nil, err
	}
	return registration, runStore, run, nil
}

// openReportRunStore opens the active run for report acceptance and also
// exposes the latest terminal run so a retried report can be recognized as an
// already-completed idempotent operation.
func (s *Service) openReportRunStore(ctx context.Context) (config.RepositoryRegistration, RunStore, *store.Run, error) {
	registration, runStore, err := s.openRunStore(ctx)
	if err != nil {
		return config.RepositoryRegistration{}, nil, nil, err
	}
	run, err := readReconciliationRun(ctx, runStore)
	if err != nil {
		_ = runStore.Close()
		return config.RepositoryRegistration{}, nil, nil, err
	}
	if run != nil {
		if err := s.ensureProgressionStartup(ctx, registration, runStore, run); err != nil {
			_ = runStore.Close()
			return config.RepositoryRegistration{}, nil, nil, err
		}
		if err := s.drainPendingEffect(ctx, registration, runStore, run); err != nil {
			_ = runStore.Close()
			return config.RepositoryRegistration{}, nil, nil, err
		}
	}
	return registration, runStore, run, nil
}

// openAgentStartRunStore opens the active run for the one progression seam
// allowed to cross a clean claim-to-first-invocation process boundary.
func (s *Service) openAgentStartRunStore(ctx context.Context, request AgentRequest) (config.RepositoryRegistration, RunStore, *store.Run, error) {
	registration, runStore, err := s.openRunStore(ctx)
	if err != nil {
		return config.RepositoryRegistration{}, nil, nil, err
	}
	run, err := runStore.CurrentRun(ctx)
	if err != nil {
		_ = runStore.Close()
		return config.RepositoryRegistration{}, nil, nil, err
	}
	if err := s.ensureAgentStartup(ctx, registration, runStore, run, request); err != nil {
		_ = runStore.Close()
		return config.RepositoryRegistration{}, nil, nil, err
	}
	return registration, runStore, run, nil
}

// drainPendingEffect consumes a durable effect left behind by an earlier
// failed attempt in this same process. The one-time startup reconciliation
// cannot cover it, because a reservation may outlive any operation that failed
// after startup. Without this drain the next reservation would collide with the
// stale one and block every later progression until the process restarts.
func (s *Service) drainPendingEffect(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run) error {
	if run == nil {
		return nil
	}
	journal, journaled := runStore.(PendingEffectStore)
	if !journaled {
		return nil
	}
	pending, err := journal.PendingEffect(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("read pending effect before progression: %w", err)
	}
	if pending == nil {
		return nil
	}
	updated, _, _, reconcileErr := s.reconcileInterruptedRun(ctx, registration, runStore, *run)
	*run = updated
	return reconcileErr
}

// ensureProgressionStartup performs the restart reconciliation shared by all
// progression seams. A production store consumes one pending effect, verifies
// the external projections, and permits continuation only after they agree.
func (s *Service) ensureProgressionStartup(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run) error {
	s.startupMu.Lock()
	defer s.startupMu.Unlock()
	if s.startupChecked {
		return s.startupErr
	}
	s.startupChecked = true
	if run == nil {
		latest, err := readReconciliationRun(ctx, runStore)
		if err != nil {
			s.startupErr = fmt.Errorf("read latest run for startup reconciliation: %w", err)
			return s.startupErr
		}
		run = latest
	}
	if run == nil {
		return nil
	}
	updated, _, _, err := s.reconcileInterruptedRun(ctx, registration, runStore, *run)
	if err != nil {
		*run = updated
		s.startupErr = err
		return s.startupErr
	}
	*run = updated
	if err := s.ensureInvocationAttached(ctx, runStore, *run); err != nil {
		s.startupErr = err
		return s.startupErr
	}
	return s.startupErr
}

// ensureAgentStartup permits only clean claim/test or explicitly skipped-test
// states to cross into their first visible invocation. A clean draft checkpoint
// may also cross into its independent immutable review. Journaled interrupted
// runs have already passed reconciliation before this seam is reached; legacy
// stores retain the typed #24 refusal.
func (s *Service) ensureAgentStartup(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run, request AgentRequest) error {
	s.startupMu.Lock()
	defer s.startupMu.Unlock()
	if s.startupChecked {
		return s.startupErr
	}
	s.startupChecked = true
	if run == nil || store.IsTerminalStatus(run.Status) {
		return nil
	}
	if _, journaled := runStore.(PendingEffectStore); !journaled {
		return s.ensureLegacyAgentStartup(ctx, registration, runStore, run, request)
	}
	updated, diagnosis, _, err := s.reconcileInterruptedRun(ctx, registration, runStore, *run)
	*run = updated
	if err != nil {
		s.startupErr = err
		return s.startupErr
	}
	if run.CheckRepairPendingAttempt != 0 {
		updated, completionErr := s.completePendingCheckRepair(ctx, registration, runStore, *run)
		if completionErr != nil {
			diagnosis.SourcesAgree = false
			addRecoveryDiscrepancy(&diagnosis, RecoveryDiscrepancy{
				Kind:     RecoveryDiscrepancyInfrastructure,
				Source:   "check-repair",
				Field:    "attempt reservation",
				Expected: "completed resumed invocation",
				Observed: completionErr.Error(),
			})
			s.startupErr = &InfrastructureDiscrepancyError{Diagnosis: diagnosis}
			return s.pauseAgentStartup(ctx, registration, runStore, run, diagnosis)
		}
		*run = updated
	}
	if run.Status == store.StatusActive && run.Stage == store.StageImplementation &&
		(run.LastCommandName == "answer" || run.LastCommandName == "refresh") &&
		run.LastCommandOutcome == string(CommandAccepted) {
		return nil
	}
	reviewStart := run.Stage == store.StageDraftPR
	if request.Role != "" {
		definition, declared := workflow.DefaultRegistry().Role(request.Role)
		reviewStart = declared && definition.Kind == workflow.RoleKindReview
	}
	cleanStart := run.Stage == store.StageClaim || run.Stage == store.StageTest || (run.Stage == store.StageImplementation && implementationStartIsClean(*run))
	if reviewStart && run.Stage == store.StageDraftPR {
		return nil
	}
	if !cleanStart || run.Status != store.StatusActive {
		return nil
	}
	if latestStore, ok := runStore.(LatestInvocationStore); ok {
		latest, latestErr := latestStore.LatestInvocation(ctx, run.ID)
		if latestErr != nil {
			addRecoveryDiscrepancy(&diagnosis, RecoveryDiscrepancy{
				Kind:     RecoveryDiscrepancyInfrastructure,
				Source:   "operational store",
				Field:    "latest invocation",
				Expected: "read latest invocation",
				Observed: latestErr.Error(),
			})
			diagnosis.SourcesAgree = false
			return s.pauseAgentStartup(ctx, registration, runStore, run, diagnosis)
		}
		resumedActiveInvocation := latest != nil && latest.Status == store.InvocationStatusActive && latest.RecoveryResumeCount > 0
		if latest != nil && latest.Status != store.InvocationStatusSuperseded && !resumedActiveInvocation && invocationBlocksCleanStart(*run, *latest) {
			diagnosis.InvocationExists = true
			addRecoveryDiscrepancy(&diagnosis, RecoveryDiscrepancy{
				Kind:     RecoveryDiscrepancyWorkflow,
				Source:   "run",
				Field:    "invocation history",
				Expected: "no unfinished invocation for the clean stage boundary",
				Observed: fmt.Sprintf("invocation %q is %s", latest.ID, latest.Status),
			})
			diagnosis.SourcesAgree = false
			return s.pauseAgentStartup(ctx, registration, runStore, run, diagnosis)
		}
	} else if run.Stage == store.StageClaim {
		if historyStore, ok := runStore.(InvocationHistoryStore); ok {
			hasInvocation, historyErr := historyStore.HasInvocation(ctx, run.ID)
			if historyErr != nil {
				addRecoveryDiscrepancy(&diagnosis, RecoveryDiscrepancy{
					Kind:     RecoveryDiscrepancyInfrastructure,
					Source:   "operational store",
					Field:    "invocation history",
					Expected: "read invocation history",
					Observed: historyErr.Error(),
				})
				diagnosis.SourcesAgree = false
				return s.pauseAgentStartup(ctx, registration, runStore, run, diagnosis)
			}
			if hasInvocation {
				diagnosis.InvocationExists = true
				addRecoveryDiscrepancy(&diagnosis, RecoveryDiscrepancy{
					Kind:     RecoveryDiscrepancyWorkflow,
					Source:   "run",
					Field:    "invocation history",
					Expected: "no invocation history for a clean claim stage",
					Observed: "persisted invocation exists",
				})
				diagnosis.SourcesAgree = false
				return s.pauseAgentStartup(ctx, registration, runStore, run, diagnosis)
			}
		}
	}
	return nil
}

// invocationBlocksCleanStart identifies terminal invocation history that means
// a clean-stage launch may be the second attempt at an already accepted report.
func invocationBlocksCleanStart(run store.Run, invocation store.Invocation) bool {
	switch run.Stage {
	case store.StageClaim:
		return true
	case store.StageTest:
		return invocation.Stage == store.StageTest
	case store.StageImplementation:
		return invocation.Stage == store.StageImplementation
	case store.StageDraftPR:
		return invocation.Stage == store.StageReview
	default:
		return false
	}
}

// pauseAgentStartup records a startup disagreement before returning its typed
// category. Journaled stores use the ordinary waiting transition when the
// external surface is available and fall back to a local waiting projection
// while preserving any pending effect that still needs operator resolution.
func (s *Service) pauseAgentStartup(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run, diagnosis RecoveryDiagnosis) error {
	paused, pauseErr := s.pauseForRecovery(ctx, registration, runStore, *run, diagnosis)
	*run = paused
	if pauseErr != nil {
		local, localErr := s.pauseRunLocally(ctx, runStore, *run, diagnosis)
		*run = local
		if localErr != nil {
			pauseErr = errors.Join(pauseErr, localErr)
		}
	}
	if hasWorkflowDiscrepancy(diagnosis) {
		// Keep the legacy RecoveryRequiredError discoverable for callers that
		// used it as the broad startup refusal while exposing workflow failure
		// as the primary category for new callers.
		s.startupErr = &WorkflowFailureError{RunID: run.ID, Cause: recoveryRequiredError(diagnosis)}
	} else {
		s.startupErr = &InfrastructureDiscrepancyError{Diagnosis: diagnosis}
	}
	if pauseErr != nil {
		s.startupErr = errors.Join(s.startupErr, pauseErr)
	}
	return s.startupErr
}

// ensureLegacyAgentStartup preserves the first-invocation compatibility seam
// for embedders that have not upgraded their operational store to the durable
// pending-effect journal.
func (s *Service) ensureLegacyAgentStartup(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run, request AgentRequest) error {
	diagnosis := s.diagnoseInterruptedRun(ctx, registration, *run)
	if !diagnosis.SourcesAgree {
		s.startupErr = recoveryDiscrepancyError(diagnosis)
		return s.startupErr
	}
	if run.Status == store.StatusActive && run.Stage == store.StageImplementation &&
		(run.LastCommandName == "answer" || run.LastCommandName == "refresh") &&
		run.LastCommandOutcome == string(CommandAccepted) {
		return nil
	}
	reviewStart := run.Stage == store.StageDraftPR
	if request.Role != "" {
		definition, declared := workflow.DefaultRegistry().Role(request.Role)
		reviewStart = declared && definition.Kind == workflow.RoleKindReview
	}
	cleanStart := run.Stage == store.StageClaim || run.Stage == store.StageTest || (run.Stage == store.StageImplementation && implementationStartIsClean(*run))
	if reviewStart && run.Stage == store.StageDraftPR {
		return nil
	}
	if !cleanStart || run.Status != store.StatusActive {
		s.startupErr = recoveryDiscrepancyError(diagnosis)
		return s.startupErr
	}
	historyStore, ok := runStore.(InvocationHistoryStore)
	if !ok {
		addRecoveryDiscrepancy(&diagnosis, RecoveryDiscrepancy{
			Source:   "operational store",
			Field:    "invocation history",
			Expected: "read-only invocation history lookup",
			Observed: "store does not support invocation history",
		})
		diagnosis.SourcesAgree = false
		s.startupErr = recoveryDiscrepancyError(diagnosis)
		return s.startupErr
	}
	hasInvocation, err := historyStore.HasInvocation(ctx, run.ID)
	if err != nil {
		addRecoveryDiscrepancy(&diagnosis, RecoveryDiscrepancy{
			Source:   "operational store",
			Field:    "invocation history",
			Expected: "read invocation history",
			Observed: err.Error(),
		})
		diagnosis.SourcesAgree = false
		s.startupErr = recoveryDiscrepancyError(diagnosis)
		return s.startupErr
	}
	if hasInvocation {
		diagnosis.InvocationExists = true
		s.startupErr = recoveryDiscrepancyError(diagnosis)
		return s.startupErr
	}
	return nil
}

// failClaim records a terminal failure and best-effort moves the issue label
// away from running so a partial claim is visible and retryable.
func (s *Service) failClaim(ctx context.Context, runStore RunStore, run store.Run, repository github.Repository, issue github.Issue, cause error) (IssueResult, error) {
	if journal, journaled := runStore.(PendingEffectStore); journaled {
		// Claim failure owns the partially created run. Abandon the in-flight
		// success transition before recording failure, otherwise a restart
		// could replay an obsolete agent-running projection over the failed run.
		if pending, err := journal.PendingEffect(ctx, run.ID); err != nil {
			cause = errors.Join(cause, fmt.Errorf("inspect failed-claim pending effect: %w", err))
		} else if pending != nil {
			if err := journal.ClearPendingEffect(ctx, run.ID, pending.ID); err != nil {
				cause = errors.Join(cause, fmt.Errorf("abandon failed-claim pending effect: %w", err))
			}
		}
		previous := run
		next := run
		next.Status = store.StatusFailed
		next.Revision = previous.Revision + 1
		next.UpdatedAt = s.deps.Now().UTC()
		updated, transitionErr := s.applyStateTransition(ctx, runStore, stateTransition{
			Repository:    repository,
			Issue:         issue,
			Previous:      previous,
			Next:          next,
			CreateComment: strings.TrimSpace(previous.StatusCommentID) == "",
		})
		if transitionErr != nil {
			cause = errors.Join(cause, fmt.Errorf("persist failed claim transition: %w", transitionErr))
			if pending, pendingErr := journal.PendingEffect(ctx, run.ID); pendingErr == nil && pending != nil {
				_ = journal.ClearPendingEffect(ctx, run.ID, pending.ID)
			}
			if saveErr := runStore.SaveRun(ctx, next); saveErr != nil {
				cause = errors.Join(cause, fmt.Errorf("persist failed claim fallback: %w", saveErr))
			}
			run = next
		} else {
			run = updated
		}
		if recorder, ok := runStore.(evaluationRecorder); ok {
			if err := recorder.EnsureEvaluationSummary(ctx, run); err != nil {
				cause = errors.Join(cause, fmt.Errorf("ensure failed-claim evaluation summary: %w", err))
			} else if err := recorder.FinalizeEvaluation(ctx, run.ID, store.EvaluationOutcomeFailed, run.UpdatedAt); err != nil {
				cause = errors.Join(cause, fmt.Errorf("finalize failed-claim evaluation summary: %w", err))
			}
		}
		return IssueResult{}, cause
	}
	run.Status = store.StatusFailed
	run.UpdatedAt = s.deps.Now().UTC()
	compensationErrors := []error{
		s.deps.GitHub.ReplaceIssueLabels(ctx, repository, run.IssueNumber, replaceFactoryState(issue.Labels, github.LabelAgentFailed)),
	}
	if run.StatusCommentID != "" {
		compensationErrors = append(compensationErrors, s.deps.GitHub.EditIssueComment(ctx, repository, run.StatusCommentID, statusCommentBody(run)))
	}
	if err := runStore.SaveRun(ctx, run); err != nil {
		compensationErrors = append(compensationErrors, fmt.Errorf("persist failed claim: %w", err))
	}
	if recorder, ok := runStore.(evaluationRecorder); ok {
		if err := recorder.EnsureEvaluationSummary(ctx, run); err != nil {
			compensationErrors = append(compensationErrors, fmt.Errorf("ensure failed-claim evaluation summary: %w", err))
		} else if err := recorder.FinalizeEvaluation(ctx, run.ID, store.EvaluationOutcomeFailed, run.UpdatedAt); err != nil {
			compensationErrors = append(compensationErrors, fmt.Errorf("finalize failed-claim evaluation summary: %w", err))
		}
	}
	for _, compensationErr := range compensationErrors {
		if compensationErr != nil {
			cause = errors.Join(cause, compensationErr)
		}
	}
	return IssueResult{}, cause
}

// statusCommentBody renders the single editable supervision comment.
func statusCommentBody(run store.Run) string {
	started := run.CreatedAt.UTC().Format(time.RFC3339)
	pullRequest := ""
	if run.PullRequestNumber > 0 {
		pullRequest = fmt.Sprintf("- pull request: #%d %s\n", run.PullRequestNumber, run.PullRequestURL)
	}
	lifecycle := ""
	if run.MergeCommitSHA != "" {
		lifecycle += fmt.Sprintf("- merge commit: `%s`\n", safeStatusCommentValue(run.MergeCommitSHA))
	}
	if run.LifecycleReason != "" {
		lifecycle += fmt.Sprintf("- lifecycle reason: %s\n", safeStatusCommentValue(run.LifecycleReason))
	}
	commandFeedback := ""
	if run.LastCommandName != "" {
		commandFeedback = fmt.Sprintf("\n### Last command\n\n- comment: `%s`\n- revision: `%d`\n- command: `%s`\n- outcome: `%s`\n- message: %s\n", safeStatusCommentValue(run.ProcessedCommentID), run.ProcessedCommentRevision, safeStatusCommentValue(run.LastCommandName), safeStatusCommentValue(run.LastCommandOutcome), safeStatusCommentValue(run.LastCommandMessage))
	}
	harness := ""
	if run.HarnessOverride != "" {
		harness = fmt.Sprintf("\n- harness override: `%s`\n", safeStatusCommentValue(run.HarnessOverride))
	}
	checkRepair := ""
	if run.CheckRepairBudget > 0 {
		remaining := remainingCheckRepairBudget(run.CheckRepairAttempts, run.CheckRepairBudget)
		checkRepair = fmt.Sprintf("\n- check-repair attempts: %d/%d\n- check-repair remaining: %d\n", run.CheckRepairAttempts, run.CheckRepairBudget, remaining)
		if run.CheckRepairPendingAttempt > 0 {
			checkRepair += fmt.Sprintf("- check-repair pending: attempt %d\n", run.CheckRepairPendingAttempt)
		}
	}
	questions := ""
	if len(run.PendingQuestions) > 0 {
		questions = "\n### Pending clarification\n\n"
		for _, question := range run.PendingQuestions {
			questions += fmt.Sprintf("- `%s`: %s\n", safeStatusCommentValue(question.ID), safeStatusCommentValue(question.Prompt))
		}
	}
	review := specificationReviewStatusComment(run)
	return fmt.Sprintf("%s\n## Factory run\n\n- run identifier: `%s`\n- issue: #%d\n- branch: `%s`\n- worktree: `%s`\n- coordinator: `%s`\n- start time: `%s`\n- checkpoint: `%s`\n- stage: `%s`\n- status: `%s`\n- test policy: `%s`\n%s%s%s%s%s%s%s", statusCommentMarker(run.ID), run.ID, run.IssueNumber, run.Branch, run.Worktree, run.Coordinator, started, run.CheckpointSHA, run.Stage, run.Status, testPolicyDescription(testPolicyModeForRun(run)), checkRepair, pullRequest, lifecycle, harness, review, questions, commandFeedback)
}

// StatusCommentBody renders the coordinator-owned status projection for one
// run. It is exported for adapters and integration tests that need to compare
// a recovered GitHub comment without treating its marker alone as agreement.
func StatusCommentBody(run store.Run) string { return statusCommentBody(run) }

// specificationReviewStatusComment renders the complete accepted review
// finding contract in the single editable issue supervision comment.
func specificationReviewStatusComment(run store.Run) string {
	if run.SpecificationReview == nil {
		if run.Stage != store.StageReview {
			return ""
		}
		return fmt.Sprintf("\n### Specification review\n\n- status: pending for checkpoint `%s`\n", safeStatusCommentValue(run.CheckpointSHA))
	}
	blocking, advisory := reviewFindingCounts(run.SpecificationReview.Findings)
	result := fmt.Sprintf("\n### Specification review\n\n- reviewed checkpoint: `%s`\n- outcome: %d blocking findings, %d advisory findings\n", safeStatusCommentValue(run.SpecificationReview.CheckpointSHA), blocking, advisory)
	for index, finding := range run.SpecificationReview.Findings {
		result += fmt.Sprintf("\n#### Finding %d — %s/%s\n\n- location: `%s`\n- claim: %s\n- evidence: %s\n- suggested resolution: %s\n- suggested owner: `%s`\n", index+1, safeStatusCommentValue(finding.Severity), safeStatusCommentValue(finding.Category), safeStatusCommentValue(finding.Location), safeStatusCommentValue(finding.Claim), safeStatusCommentValue(finding.Evidence), safeStatusCommentValue(finding.SuggestedResolution), safeStatusCommentValue(finding.SuggestedOwner))
	}
	return result
}

// safeStatusCommentValue keeps command feedback single-line and prevents
// untrusted GitHub login or parser text from changing the status-comment
// structure.
func safeStatusCommentValue(value string) string {
	value = strings.NewReplacer("\r", " ", "\n", " ", "`", "'", "\x00", " ").Replace(value)
	return strings.TrimSpace(value)
}

// statusCommentMarker identifies the one editable status comment for a run.
func statusCommentMarker(runID string) string {
	return fmt.Sprintf("<!-- factory-status: %s -->", runID)
}

// factoryLabelForStatus maps a status to its product state label.
func factoryLabelForStatus(status store.Status) string {
	if label, ok := factoryLabelByStatus[status]; ok {
		return label
	}
	return github.LabelAgentRunning
}

// validateStage rejects values outside the factory-owned workflow registry.
func validateStage(stage store.Stage) error {
	if _, ok := workflow.DefaultRegistry().Stage(stage); ok {
		return nil
	}
	return fmt.Errorf("unknown run stage %q", stage)
}

// validateStatus rejects values outside the independent workflow statuses.
func validateStatus(status store.Status) error {
	if _, ok := validStatuses[status]; ok {
		return nil
	}
	return fmt.Errorf("unknown run status %q", status)
}

// hasLabel reports whether a label is present in an issue snapshot.
func hasLabel(labels []string, wanted string) bool {
	for _, label := range labels {
		if label == wanted {
			return true
		}
	}
	return false
}

// conflictingFactoryLabel rejects an issue already owned by another run state.
func conflictingFactoryLabel(labels []string) bool {
	for _, label := range labels {
		if label != github.LabelAgentReady && hasLabel(github.FactoryStateLabels, label) {
			return true
		}
	}
	return false
}

// replaceFactoryState removes every factory label and appends exactly one new
// state while preserving ordinary labels and their first-seen order.
func replaceFactoryState(labels []string, state string) []string {
	result := make([]string, 0, len(labels)+1)
	seen := make(map[string]struct{}, len(labels)+1)
	for _, label := range labels {
		if hasLabel(github.FactoryStateLabels, label) {
			continue
		}
		if _, exists := seen[label]; exists {
			continue
		}
		seen[label] = struct{}{}
		result = append(result, label)
	}
	if state != "" {
		result = append(result, state)
	}
	return result
}

// cloneIssue copies the mutable label slice before freezing the packet.
func cloneIssue(issue github.Issue) github.Issue {
	issue.Labels = append([]string(nil), issue.Labels...)
	return issue
}

// defaultCoordinator identifies the host without reading credentials.
func defaultCoordinator() string {
	if hostname, err := os.Hostname(); err == nil && strings.TrimSpace(hostname) != "" {
		return hostname
	}
	return "factory"
}
