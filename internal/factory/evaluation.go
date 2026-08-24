package factory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// EvaluationReadStore is the local, network-free reporting seam for retained
// evaluation summaries and their aggregate projection.
type EvaluationReadStore interface {
	OperationalStore
	ListEvaluationSummaries(context.Context, string) ([]store.EvaluationSummary, error)
	AggregateEvaluation(context.Context) (store.EvaluationAggregate, error)
	DeleteEvaluationSummariesBefore(context.Context, time.Time) (store.EvaluationDeletionResult, error)
}

// EvaluationDispositionStore is the local mutation seam for human
// classifications of already-recorded escalations.
type EvaluationDispositionStore interface {
	EvaluationReadStore
	SaveEvaluationDisposition(context.Context, store.EvaluationDispositionRequest) (store.EvaluationDispositionRecord, error)
}

// EvaluationReportRequest selects all retained summaries or one opaque run.
type EvaluationReportRequest struct {
	RunID string
}

// EvaluationReportResult contains per-run summaries, aggregate values, and the
// configured retention declaration used by the operator.
type EvaluationReportResult struct {
	Summaries []store.EvaluationSummary
	Aggregate store.EvaluationAggregate
	Retention string
}

// EvaluationDeleteRequest selects a deliberate terminal-summary cutoff.
type EvaluationDeleteRequest struct {
	Before time.Time
}

// EvaluationDeleteResult reports only the local rows removed by explicit
// evaluation-summary deletion.
type EvaluationDeleteResult struct {
	Deleted store.EvaluationDeletionResult
	Before  time.Time
}

// EvaluationDispositionRequest selects one opaque escalated event and its
// human classification.
type EvaluationDispositionRequest struct {
	RunID       string
	EventID     string
	Category    store.EvaluationEscalationCategory
	Disposition store.EvaluationDisposition
	DecidedAt   time.Time
}

// EvaluationDispositionResult contains the hashed, persisted disposition.
type EvaluationDispositionResult struct {
	Disposition store.EvaluationDispositionRecord
}

// EvaluationReport reads summaries and aggregates without contacting GitHub or
// any other network service.
func (s *Service) EvaluationReport(ctx context.Context, request EvaluationReportRequest) (EvaluationReportResult, error) {
	registration, reader, closeStore, err := s.openEvaluationReader(ctx)
	if err != nil {
		return EvaluationReportResult{}, err
	}
	defer func() { _ = closeStore() }()
	summaries, err := reader.ListEvaluationSummaries(ctx, request.RunID)
	if err != nil {
		return EvaluationReportResult{}, err
	}
	aggregate, err := reader.AggregateEvaluation(ctx)
	if err != nil {
		return EvaluationReportResult{}, err
	}
	retention := ""
	if registration.RepositoryConfigPath != "" {
		repositoryConfig, loadErr := s.deps.LoadRepository(registration.RepositoryConfigPath)
		if loadErr != nil {
			return EvaluationReportResult{}, fmt.Errorf("load repository evaluation retention: %w", loadErr)
		}
		retention = repositoryConfig.Evaluation.Retention
	}
	return EvaluationReportResult{Summaries: summaries, Aggregate: aggregate, Retention: retention}, nil
}

// DeleteEvaluation removes only terminal evaluation summaries before an
// explicit cutoff. It is intentionally separate from ordinary run-artifact
// cleanup and does not infer a cutoff from configuration.
func (s *Service) DeleteEvaluation(ctx context.Context, request EvaluationDeleteRequest) (EvaluationDeleteResult, error) {
	if request.Before.IsZero() {
		return EvaluationDeleteResult{}, errors.New("evaluation deletion requires an explicit cutoff")
	}
	_, reader, closeStore, err := s.openEvaluationReader(ctx)
	if err != nil {
		return EvaluationDeleteResult{}, err
	}
	defer func() { _ = closeStore() }()
	deleted, err := reader.DeleteEvaluationSummariesBefore(ctx, request.Before)
	if err != nil {
		return EvaluationDeleteResult{}, err
	}
	return EvaluationDeleteResult{Deleted: deleted, Before: request.Before.UTC()}, nil
}

// AttachEvaluationDisposition adds an explicit human disposition while
// persisting only a one-way event identity and fixed vocabulary.
func (s *Service) AttachEvaluationDisposition(ctx context.Context, request EvaluationDispositionRequest) (EvaluationDispositionResult, error) {
	_, reader, closeStore, err := s.openEvaluationReader(ctx)
	if err != nil {
		return EvaluationDispositionResult{}, err
	}
	defer func() { _ = closeStore() }()
	writer, ok := reader.(EvaluationDispositionStore)
	if !ok {
		return EvaluationDispositionResult{}, errors.New("operational store does not support evaluation dispositions")
	}
	record, err := writer.SaveEvaluationDisposition(ctx, store.EvaluationDispositionRequest{
		RunID:       request.RunID,
		EventID:     request.EventID,
		Category:    request.Category,
		Disposition: request.Disposition,
		DecidedAt:   request.DecidedAt,
	})
	if err != nil {
		return EvaluationDispositionResult{}, err
	}
	return EvaluationDispositionResult{Disposition: record}, nil
}

// openEvaluationReader loads the registered repository and opens the local
// operational database without invoking any external adapter.
func (s *Service) openEvaluationReader(ctx context.Context) (config.RepositoryRegistration, EvaluationReadStore, func() error, error) {
	registration, err := s.registration()
	if err != nil {
		return config.RepositoryRegistration{}, nil, func() error { return nil }, err
	}
	opened, err := s.deps.OpenStore(ctx, registration.OperationalDataPath)
	if err != nil {
		return config.RepositoryRegistration{}, nil, func() error { return nil }, err
	}
	reader, ok := opened.(EvaluationReadStore)
	if !ok {
		_ = opened.Close()
		return config.RepositoryRegistration{}, nil, func() error { return nil }, errors.New("operational store does not support local evaluation summaries")
	}
	return registration, reader, opened.Close, nil
}

// evaluationRecorder is the optional coordinator seam implemented by the real
// SQLite store. Existing focused test doubles remain valid when they omit it.
type evaluationRecorder interface {
	EnsureEvaluationSummary(context.Context, store.Run) error
	RecordEvaluationStageTransition(context.Context, string, store.Stage, store.Stage, time.Time) error
	RecordEvaluationInvocation(context.Context, string, store.Invocation, string, int) error
	RecordEvaluationGateRun(context.Context, string, int) error
	RecordEvaluationAttempt(context.Context, string, store.EvaluationAttempt) error
	RecordEvaluationExemption(context.Context, string, store.EvaluationExemption) error
	RecordEvaluationEscalation(context.Context, string, store.EvaluationEscalationCategory) error
	RecordEvaluationBlocker(context.Context, string, store.EvaluationBlockerCategory) error
	RecordEvaluationBudgetExhaustion(context.Context, string) error
	RecordEvaluationUsage(context.Context, string, string, store.EvaluationUsage) error
	RefreshEvaluationCounts(context.Context, string) error
	SetEvaluationOutcome(context.Context, string, store.EvaluationOutcome) error
	FinalizeEvaluation(context.Context, string, store.EvaluationOutcome, time.Time) error
	FinalizeEvaluationWithBudget(context.Context, string, store.EvaluationOutcome, time.Time, bool) error
	ReopenEvaluation(context.Context, string, store.Stage, time.Time) error
}

// recordEvaluationTransition keeps the optional evaluation projection aligned
// with a persisted run transition without making fake coordinator stores carry
// the production SQLite surface.
func recordEvaluationTransition(ctx context.Context, recorder evaluationRecorder, previous, next store.Run, at time.Time) error {
	if recorder == nil {
		return nil
	}
	if err := recorder.EnsureEvaluationSummary(ctx, next); err != nil {
		return err
	}
	if store.IsTerminalStatus(previous.Status) && !store.IsTerminalStatus(next.Status) {
		if err := recorder.ReopenEvaluation(ctx, next.ID, next.Stage, at); err != nil {
			return err
		}
		if attempt := evaluationAttemptForRetry(next.Stage); attempt != "" {
			if err := recorder.RecordEvaluationAttempt(ctx, next.ID, attempt); err != nil {
				return err
			}
		}
	}
	if previous.Stage != "" && previous.Stage != next.Stage {
		if err := recorder.RecordEvaluationStageTransition(ctx, next.ID, previous.Stage, next.Stage, at); err != nil {
			return err
		}
	}
	if !store.IsTerminalStatus(next.Status) {
		if err := recorder.SetEvaluationOutcome(ctx, next.ID, evaluationOutcome(next.Status)); err != nil {
			return err
		}
	}
	if store.IsTerminalStatus(next.Status) {
		if err := recorder.FinalizeEvaluation(ctx, next.ID, evaluationOutcome(next.Status), at); err != nil {
			return err
		}
	}
	return nil
}

// evaluationOutcome converts the operational status vocabulary to the local
// evaluation vocabulary without retaining any additional state.
func evaluationOutcome(status store.Status) store.EvaluationOutcome {
	return store.EvaluationOutcome(status)
}

// evaluationAttemptForRetry maps an authorized terminal-run retry to the
// workflow effort bucket it is revising. Preflight and implementation retries
// have no more specific policy bucket and therefore remain unclassified.
func evaluationAttemptForRetry(stage store.Stage) store.EvaluationAttempt {
	switch stage {
	case store.StageCheck:
		return store.EvaluationAttemptCheckRepair
	case store.StageTest:
		return store.EvaluationAttemptTestRevision
	case store.StageReview:
		return store.EvaluationAttemptReviewRevision
	default:
		return ""
	}
}
