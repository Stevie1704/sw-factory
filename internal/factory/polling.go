package factory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// PollOutcome identifies the result of one queue observation.
type PollOutcome string

const (
	// PollNoWork means the repository has no eligible issue at this instant.
	PollNoWork PollOutcome = "no_work"
	// PollActiveRun means an existing non-terminal run suppressed claiming.
	PollActiveRun PollOutcome = "active_run"
	// PollClaimed means the oldest eligible issue was claimed successfully.
	PollClaimed PollOutcome = "claimed"
)

const (
	// maxConsecutiveLeaseFailures limits lease-renewal retry attempts before
	// returning a persistent error to the caller.
	maxConsecutiveLeaseFailures = 5
	// maxConsecutiveProgressionFailures limits how long the supervisor backs
	// off an unattended progression pass whose durable state it cannot read
	// before it reports a persistent coordinator failure.
	maxConsecutiveProgressionFailures = 5
)

// PollResult reports one deterministic polling observation and any run it
// found or claimed.
type PollResult struct {
	// Outcome identifies whether work was claimed or suppressed.
	Outcome PollOutcome
	// IssueNumber is the issue claimed during this observation, or zero.
	IssueNumber int
	// Run is the active or newly claimed run, when one exists.
	Run store.Run
}

// StartupBlockedError reports that a blocking startup diagnosis prevented the
// coordinator from acquiring its host lock or contacting the issue queue.
type StartupBlockedError struct {
	// Diagnosis contains every startup check result, including warnings.
	Diagnosis DoctorResult
}

// pollingTransportError marks a read-only queue transport failure that can be
// retried without entering the claim state machine.
type pollingTransportError struct {
	err error
}

// Error returns the bounded polling transport failure.
func (e *pollingTransportError) Error() string { return e.err.Error() }

// Unwrap exposes the underlying queue adapter failure to callers and tests.
func (e *pollingTransportError) Unwrap() error { return e.err }

// Error returns a bounded startup refusal while retaining the full diagnosis
// for callers that need to render individual corrective actions.
func (e *StartupBlockedError) Error() string {
	if e == nil {
		return "startup diagnosis blocked"
	}
	return fmt.Sprintf("startup diagnosis blocked: %d blocking checks", len(e.Diagnosis.Report.Failures()))
}

// Start runs the supervised polling loop until its context is cancelled. It
// diagnoses the full host before taking the lock, acquires exclusive ownership
// before reconciliation, renews a visible GitHub lease, and backs off read-only
// queue or lease transport failures without changing run state or retry
// budgets. After every observation that found or claimed a run it drives that
// run toward its draft pull request, so the routine path needs no
// stage-driving CLI command. Claim failures are returned because the claim
// state machine may already have performed compensating effects.
func (s *Service) Start(ctx context.Context) error {
	diagnosis, err := s.startupDiagnosis(ctx)
	if err != nil {
		return fmt.Errorf("run startup diagnosis: %w", err)
	}
	if !diagnosis.Ready() {
		return &StartupBlockedError{Diagnosis: diagnosis}
	}
	registration, repositoryConfig, err := s.pollConfiguration()
	if err != nil {
		return err
	}
	if s.issuePoller() == nil {
		return errors.New("GitHub issue poller is required to start the coordinator")
	}
	if s.deps.Lease == nil {
		return errors.New("GitHub lease client is required to start the coordinator")
	}
	interval, backoff, err := pollingDurations(registration.Polling)
	if err != nil {
		return err
	}
	lock, err := acquireCoordinatorLock(coordinatorLockPath(registration))
	if err != nil {
		return err
	}
	defer func() { _ = lock.release() }()
	if err := s.reconcileRegisteredRun(ctx, registration); err != nil {
		return fmt.Errorf("reconcile persisted run at startup: %w", err)
	}

	pollContext, cancel := context.WithCancel(ctx)
	if !s.setPollCancel(cancel) {
		cancel()
		return ErrCoordinatorAlreadyRunning
	}
	defer s.clearPollCancel()
	defer cancel()

	repository := github.Repository{Owner: registration.GitHub.Owner, Name: registration.GitHub.Repository}
	leaseRunID := ""
	delay := time.Duration(0)
	consecutiveLeaseFailures := 0
	consecutiveProgressionFailures := 0
	for {
		if err := waitPolling(pollContext, delay); err != nil {
			if pollingContextDone(err) {
				return nil
			}
			return err
		}
		if err := pollContext.Err(); err != nil {
			if pollingContextDone(err) {
				return nil
			}
			return err
		}
		now := s.deps.Now().UTC()
		if err := s.deps.Lease.RenewLease(pollContext, repository, github.Lease{
			TargetBranch: repositoryConfig.TargetBranch,
			Coordinator:  s.deps.Coordinator,
			RunID:        leaseRunID,
			RenewedAt:    now,
			ExpiresAt:    now.Add(interval + backoff),
		}); err != nil {
			if pollingContextDone(err) {
				return nil
			}
			consecutiveLeaseFailures++
			if consecutiveLeaseFailures > maxConsecutiveLeaseFailures {
				return fmt.Errorf("lease renewal failed after %d consecutive attempts: %w", maxConsecutiveLeaseFailures, err)
			}
			delay = backoff
			continue
		}
		consecutiveLeaseFailures = 0

		if err := s.retryWaitingForHarness(pollContext, registration); err != nil {
			if pollingContextDone(err) {
				return nil
			}
			return err
		}
		result, err := s.pollOnce(pollContext, registration)
		if err != nil {
			if pollingContextDone(err) {
				return nil
			}
			var transportErr *pollingTransportError
			if errors.As(err, &transportErr) {
				delay = backoff
				continue
			}
			return err
		}
		leaseRunID = result.Run.ID
		if result.Outcome == PollClaimed || result.Outcome == PollActiveRun {
			if _, err := s.driveRun(pollContext, registration); err != nil {
				if pollingContextDone(err) {
					return nil
				}
				consecutiveProgressionFailures++
				if consecutiveProgressionFailures > maxConsecutiveProgressionFailures {
					return fmt.Errorf("drive run %s after %d consecutive attempts: %w", result.Run.ID, maxConsecutiveProgressionFailures, err)
				}
				delay = backoff
				continue
			}
			consecutiveProgressionFailures = 0
		}
		delay = interval
	}
}

// reconcileRegisteredRun opens the operational store once after the polling
// loop owns its host lock. This makes a process restart converge and resume an
// active run before queue work is considered.
func (s *Service) reconcileRegisteredRun(ctx context.Context, registration config.RepositoryRegistration) error {
	opened, err := s.deps.OpenStore(ctx, registration.OperationalDataPath)
	if err != nil {
		return fmt.Errorf("open operational store for startup reconciliation: %w", err)
	}
	runStore, ok := opened.(RunStore)
	if !ok {
		_ = opened.Close()
		return errors.New("operational store does not support startup reconciliation")
	}
	defer func() { _ = runStore.Close() }()
	run, err := readReconciliationRun(ctx, runStore)
	if err != nil {
		return fmt.Errorf("read persisted run for startup reconciliation: %w", err)
	}
	if journal, journaled := runStore.(PendingEffectStore); journaled && run != nil {
		pending, pendingErr := journal.PendingEffect(ctx, run.ID)
		if pendingErr != nil {
			return fmt.Errorf("read pending effect before startup lifecycle observation: %w", pendingErr)
		}
		if pending == nil {
			lifecycle, lifecycleErr := s.observeLifecycle(ctx, registration, runStore, run)
			if lifecycleErr != nil {
				return fmt.Errorf("observe lifecycle before startup reconciliation: %w", lifecycleErr)
			}
			if lifecycle.Outcome != LifecycleUnchanged || store.IsTerminalStatus(lifecycle.Run.Status) {
				return nil
			}
		}
	}
	return s.ensureAgentStartup(ctx, registration, runStore, run, AgentRequest{})
}

// Stop asks the current coordinator process to stop polling. A same-process
// stop cancels the loop directly; a separate CLI process signals the PID
// recorded by the kernel-backed host lock. Neither path changes an active
// run's workflow status or removes its artifacts.
func (s *Service) Stop(ctx context.Context) (StopResult, error) {
	if err := ctx.Err(); err != nil {
		return StopResult{}, err
	}
	s.pollMu.Lock()
	cancel := s.pollCancel
	s.pollMu.Unlock()
	if cancel != nil {
		cancel()
		return StopResult{Running: true, PID: os.Getpid()}, nil
	}
	registration, err := s.registration()
	if err != nil {
		return StopResult{}, err
	}
	result, err := requestCoordinatorStop(coordinatorLockPath(registration))
	if errors.Is(err, ErrCoordinatorNotRunning) {
		return StopResult{}, nil
	}
	return result, err
}

// PollOnce performs one queue observation using the same active-run check and
// claim path as Start. It is useful for a supervised one-shot poll and for
// testing the high-level coordinator seam without waiting on a timer.
func (s *Service) PollOnce(ctx context.Context) (PollResult, error) {
	registration, _, err := s.pollConfiguration()
	if err != nil {
		return PollResult{}, err
	}
	if err := s.retryWaitingForHarness(ctx, registration); err != nil {
		return PollResult{}, err
	}
	return s.pollOnce(ctx, registration)
}

// pollOnce checks the operational store before reading GitHub, ensuring an
// active run suppresses queue work and that the oldest eligible issue wins.
func (s *Service) pollOnce(ctx context.Context, registration config.RepositoryRegistration) (PollResult, error) {
	if err := ctx.Err(); err != nil {
		return PollResult{}, err
	}
	opened, err := s.deps.OpenStore(ctx, registration.OperationalDataPath)
	if err != nil {
		return PollResult{}, fmt.Errorf("open operational store for polling: %w", err)
	}
	run, currentErr := opened.CurrentRun(ctx)
	closeErr := opened.Close()
	if currentErr != nil {
		return PollResult{}, fmt.Errorf("read active run for polling: %w", currentErr)
	}
	if closeErr != nil {
		return PollResult{}, fmt.Errorf("close operational store after polling: %w", closeErr)
	}
	if run != nil {
		return PollResult{Outcome: PollActiveRun, Run: *run}, nil
	}

	poller := s.issuePoller()
	if poller == nil {
		return PollResult{}, errors.New("GitHub issue poller is required for polling")
	}
	issues, err := poller.ListEligibleIssues(ctx, github.Repository{Owner: registration.GitHub.Owner, Name: registration.GitHub.Repository})
	if err != nil {
		return PollResult{}, &pollingTransportError{err: fmt.Errorf("poll eligible issues: %w", err)}
	}
	issues = eligibleIssueQueue(issues)
	if len(issues) == 0 {
		return PollResult{Outcome: PollNoWork}, nil
	}
	claimed, err := s.ClaimIssue(ctx, issues[0].Number)
	if err != nil {
		return PollResult{}, fmt.Errorf("claim oldest eligible issue #%d: %w", issues[0].Number, err)
	}
	return PollResult{Outcome: PollClaimed, IssueNumber: claimed.Run.IssueNumber, Run: claimed.Run}, nil
}

// pollConfiguration loads the one registered repository and its checked-in
// policy after startup diagnosis has confirmed both are readable.
func (s *Service) pollConfiguration() (config.RepositoryRegistration, config.RepositoryConfig, error) {
	registration, err := s.registration()
	if err != nil {
		return config.RepositoryRegistration{}, config.RepositoryConfig{}, err
	}
	repositoryConfig, err := s.deps.LoadRepository(registration.RepositoryConfigPath)
	if err != nil {
		return config.RepositoryRegistration{}, config.RepositoryConfig{}, fmt.Errorf("load repository configuration for polling: %w", err)
	}
	return registration, repositoryConfig, nil
}

// startupDiagnosis resolves the injected diagnosis seam or the complete
// service-owned Doctor implementation.
func (s *Service) startupDiagnosis(ctx context.Context) (DoctorResult, error) {
	if s.deps.StartupDiagnosis != nil {
		return s.deps.StartupDiagnosis(ctx)
	}
	return s.Doctor(ctx)
}

// issuePoller resolves the configured queue adapter without broadening the
// existing GitHub mutation client seam.
func (s *Service) issuePoller() github.IssuePoller {
	if s.deps.IssuePoller != nil {
		return s.deps.IssuePoller
	}
	if poller, ok := s.deps.GitHub.(github.IssuePoller); ok {
		return poller
	}
	return nil
}

// eligibleIssueQueue defensively applies the queue contract even when a test
// or alternate GitHub adapter does not filter its endpoint response.
func eligibleIssueQueue(issues []github.Issue) []github.Issue {
	eligible := make([]github.Issue, 0, len(issues))
	for _, issue := range issues {
		if issue.Number <= 0 || issue.IsPullRequest || !strings.EqualFold(strings.TrimSpace(issue.State), "open") || !hasLabel(issue.Labels, github.LabelAgentReady) {
			continue
		}
		eligible = append(eligible, issue)
	}
	sort.SliceStable(eligible, func(left, right int) bool {
		return eligible[left].Number < eligible[right].Number
	})
	return eligible
}

// pollingDurations parses the configured normal interval and transport
// backoff, preserving the configuration validator's positive-duration rule at
// the runtime boundary as well.
func pollingDurations(polling config.PollingConfig) (time.Duration, time.Duration, error) {
	interval, err := time.ParseDuration(strings.TrimSpace(polling.Interval))
	if err != nil || interval <= 0 {
		return 0, 0, fmt.Errorf("invalid polling interval %q", polling.Interval)
	}
	backoff, err := time.ParseDuration(strings.TrimSpace(polling.Backoff))
	if err != nil || backoff <= 0 {
		return 0, 0, fmt.Errorf("invalid polling backoff %q", polling.Backoff)
	}
	return interval, backoff, nil
}

// waitPolling waits for one configured delay without making context
// cancellation depend on a wall-clock sleep.
func waitPolling(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// pollingContextDone reports the two context endings that are normal for a
// long-running coordinator controlled by start/stop.
func pollingContextDone(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// setPollCancel records a same-process stop handle while rejecting duplicate
// loops before they can poll the same repository.
func (s *Service) setPollCancel(cancel context.CancelFunc) bool {
	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	if s.pollCancel != nil {
		return false
	}
	s.pollCancel = cancel
	return true
}

// clearPollCancel removes the stop handle when the polling loop completes.
func (s *Service) clearPollCancel() {
	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	if s.pollCancel != nil {
		s.pollCancel = nil
	}
}
