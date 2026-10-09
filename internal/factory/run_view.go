package factory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// RunReadStore is the read-only store seam for the run list, run detail, and
// run evaluation.
type RunReadStore interface {
	OperationalStore
	SupervisorHeartbeatReader
	ListRuns(context.Context) ([]store.Run, error)
	Run(context.Context, string) (*store.Run, error)
	Invocations(context.Context, string) ([]store.Invocation, error)
	RunGateResults(context.Context, string) ([]store.GateResult, error)
	PendingEffect(context.Context, string) (*store.PendingEffect, error)
	EvaluationSummary(context.Context, string) (*store.EvaluationSummary, error)
}

// ErrRunNotFound reports that no persisted run has the requested identity.
var ErrRunNotFound = errors.New("run not found")

// SupervisorView is the coordinator liveness shown on every page. It reads
// only the persisted heartbeat: probing the coordinator lock would take an
// exclusive lock for a moment and could race a starting coordinator.
type SupervisorView struct {
	// Heartbeat is the persisted heartbeat, or nil when none was recorded.
	Heartbeat *store.SupervisorHeartbeat
	// Live reports an unexpired heartbeat at ObservedAt.
	Live bool
	// ObservedAt is when the page read the store.
	ObservedAt time.Time
	// ActiveRunID names the newest non-terminal run, or is empty when no run
	// is active. With Live false it means a run waits for a coordinator.
	ActiveRunID string
}

// supervisorReader is the store seam the supervisor view reads.
type supervisorReader interface {
	OperationalStore
	SupervisorHeartbeatReader
}

// RunListEntry is one row of the run list.
type RunListEntry struct {
	Run store.Run
	// IssueTitle comes from the frozen packet; it is empty when unreadable.
	IssueTitle string
	// Route is the frozen workflow route, or unknown when unreadable.
	Route    workflow.Route
	Activity RunActivity
}

// RunOverview is the run list page model.
type RunOverview struct {
	RepositoryPath string
	Supervisor     SupervisorView
	// Runs holds every persisted run, newest update first.
	Runs []RunListEntry
}

// GateCheckpointResults groups the gate results of one phase at one exact
// checkpoint.
type GateCheckpointResults struct {
	Phase         store.GatePhase
	CheckpointSHA string
	// Results are in repository declaration order.
	Results []store.GateResult
}

// GateFailureDiagnostic is the host-side diagnostic of the latest failure of
// one gate phase. Each failure replaces the file, so it can belong to an
// older checkpoint than the retained gate results.
type GateFailureDiagnostic struct {
	Phase store.GatePhase
	// Content is a bounded read of repository command output. It stays on
	// the coordinator host.
	Content string
	// Truncated reports that the file is longer than the read bound, so
	// Content holds only its leading part.
	Truncated bool
	// ModifiedAt is when the diagnostic file was last written.
	ModifiedAt time.Time
}

// RunDetail is the run detail page model.
type RunDetail struct {
	Supervisor     SupervisorView
	Run            store.Run
	IssueTitle     string
	Route          workflow.Route
	TestPolicyMode config.TestMode
	Activity       RunActivity
	// Invocations are in update order, oldest first.
	Invocations []store.Invocation
	// Gates are grouped by phase, then checkpoint.
	Gates []GateCheckpointResults
	// PendingEffect is the in-flight external effect, or nil. Its Payload
	// holds replay intent and is not for display.
	PendingEffect *store.PendingEffect
	// Diagnostics hold the retained gate failure diagnostics; absent files
	// are omitted.
	Diagnostics []GateFailureDiagnostic
}

// RunEvaluation is the evaluation page model of one run. A summary outlives
// cleanup, so either the run or the summary can be absent, never both.
type RunEvaluation struct {
	Supervisor SupervisorView
	RunID      string
	// Run is the persisted run, or nil when cleanup removed its row.
	Run *store.Run
	// Summary is the content-free evaluation summary, or nil when none is
	// retained.
	Summary *store.EvaluationSummary
}

// RunOverview reads the run list without contacting any external service or
// changing the store.
func (s *Service) RunOverview(ctx context.Context) (RunOverview, error) {
	return readRunView(ctx, s, func(view runViewRead) (RunOverview, error) {
		runs, err := view.reader.ListRuns(ctx)
		if err != nil {
			return RunOverview{}, err
		}
		overview := RunOverview{RepositoryPath: view.repositoryPath, Supervisor: view.supervisor, Runs: make([]RunListEntry, 0, len(runs))}
		for _, run := range runs {
			overview.Runs = append(overview.Runs, RunListEntry{
				Run:        run,
				IssueTitle: issueTitleForRun(run),
				Route:      statusRouteForRun(run),
				Activity:   RunActivityFor(run),
			})
		}
		return overview, nil
	})
}

// RunDetail reads one run without contacting any external service or
// changing the store. It returns ErrRunNotFound for an unknown identity.
func (s *Service) RunDetail(ctx context.Context, runID string) (RunDetail, error) {
	return readRunView(ctx, s, func(view runViewRead) (RunDetail, error) {
		run, err := view.reader.Run(ctx, runID)
		if err != nil {
			return RunDetail{}, err
		}
		if run == nil {
			return RunDetail{}, fmt.Errorf("%w: %s", ErrRunNotFound, runID)
		}
		invocations, err := view.reader.Invocations(ctx, runID)
		if err != nil {
			return RunDetail{}, err
		}
		gateResults, err := view.reader.RunGateResults(ctx, runID)
		if err != nil {
			return RunDetail{}, err
		}
		pendingEffect, err := view.reader.PendingEffect(ctx, runID)
		if err != nil {
			return RunDetail{}, fmt.Errorf("read pending effect: %w", err)
		}
		diagnostics, err := readGateFailureDiagnostics(*run)
		if err != nil {
			return RunDetail{}, err
		}
		return RunDetail{
			Supervisor:     view.supervisor,
			Run:            *run,
			IssueTitle:     issueTitleForRun(*run),
			Route:          statusRouteForRun(*run),
			TestPolicyMode: testPolicyModeForRun(*run),
			Activity:       RunActivityFor(*run),
			Invocations:    invocations,
			Gates:          groupGateResults(gateResults),
			PendingEffect:  pendingEffect,
			Diagnostics:    diagnostics,
		}, nil
	})
}

// RunEvaluation reads one run's evaluation summary without contacting any
// external service or changing the store. It returns ErrRunNotFound when
// neither the run nor a summary exists.
func (s *Service) RunEvaluation(ctx context.Context, runID string) (RunEvaluation, error) {
	return readRunView(ctx, s, func(view runViewRead) (RunEvaluation, error) {
		run, err := view.reader.Run(ctx, runID)
		if err != nil {
			return RunEvaluation{}, err
		}
		summary, err := view.reader.EvaluationSummary(ctx, runID)
		if err != nil {
			return RunEvaluation{}, fmt.Errorf("read evaluation summary: %w", err)
		}
		if run == nil && summary == nil {
			return RunEvaluation{}, fmt.Errorf("%w: %s", ErrRunNotFound, runID)
		}
		return RunEvaluation{Supervisor: view.supervisor, RunID: runID, Run: run, Summary: summary}, nil
	})
}

// runViewRead is what every run view page reads first: the registered
// repository, the open read-only store, and the supervisor view.
type runViewRead struct {
	repositoryPath string
	reader         RunReadStore
	supervisor     SupervisorView
}

// readRunView opens the store read-only, reads the supervisor view, and
// passes both to read. It closes the store when read returns, so no page
// model keeps a store handle.
func readRunView[T any](ctx context.Context, s *Service, read func(runViewRead) (T, error)) (T, error) {
	var none T
	repositoryPath, reader, err := s.openRunReader(ctx)
	if err != nil {
		return none, err
	}
	defer func() { _ = reader.Close() }()
	supervisor, err := s.supervisorView(ctx, reader)
	if err != nil {
		return none, err
	}
	return read(runViewRead{repositoryPath: repositoryPath, reader: reader, supervisor: supervisor})
}

// groupGateResults splits results ordered by phase, checkpoint, and ordinal
// into one group per phase and checkpoint.
func groupGateResults(results []store.GateResult) []GateCheckpointResults {
	groups := []GateCheckpointResults{}
	for _, result := range results {
		last := len(groups) - 1
		if last < 0 || groups[last].Phase != result.Phase || groups[last].CheckpointSHA != result.CheckpointSHA {
			groups = append(groups, GateCheckpointResults{Phase: result.Phase, CheckpointSHA: result.CheckpointSHA})
			last++
		}
		groups[last].Results = append(groups[last].Results, result)
	}
	return groups
}

// openRunReader opens the registered operational store read-only. It never
// uses the read-write opener, so a page view cannot create, migrate, or back
// up the store. It returns the registered repository path with the reader.
func (s *Service) openRunReader(ctx context.Context) (string, RunReadStore, error) {
	registration, err := s.registration()
	if err != nil {
		return "", nil, err
	}
	if err := validateOperationalPath(registration.Path, registration.OperationalDataPath); err != nil {
		return "", nil, err
	}
	opened, err := s.deps.OpenStoreReadOnly(ctx, registration.OperationalDataPath)
	if err != nil {
		return "", nil, fmt.Errorf("open operational store read-only: %w", err)
	}
	reader, ok := opened.(RunReadStore)
	if !ok {
		_ = opened.Close()
		return "", nil, errors.New("operational store does not support run views")
	}
	return registration.Path, reader, nil
}

// supervisorView reads the persisted heartbeat and the active run, and
// judges the heartbeat at the service clock.
func (s *Service) supervisorView(ctx context.Context, reader supervisorReader) (SupervisorView, error) {
	view := SupervisorView{ObservedAt: s.deps.Now().UTC()}
	heartbeat, err := reader.ReadSupervisorHeartbeat(ctx)
	if err != nil {
		return SupervisorView{}, fmt.Errorf("read supervisor heartbeat: %w", err)
	}
	view.Heartbeat = heartbeat
	view.Live = heartbeat != nil && heartbeat.Live(view.ObservedAt)
	active, err := reader.CurrentRun(ctx)
	if err != nil {
		return SupervisorView{}, fmt.Errorf("read active run: %w", err)
	}
	if active != nil {
		view.ActiveRunID = active.ID
	}
	return view, nil
}

// issueTitleForRun returns the issue title frozen in the run's packet, or an
// empty title when the packet is unreadable.
func issueTitleForRun(run store.Run) string {
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return ""
	}
	return packet.Issue.Title
}
