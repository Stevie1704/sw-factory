package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// CurrentSchemaVersion is the latest operational-store schema understood by
// this binary.
const CurrentSchemaVersion = 16

// ErrRevisionConflict reports that another coordinator revision was persisted
// after a command read the run and before it attempted its compare-and-set.
var ErrRevisionConflict = errors.New("run revision conflict")

// ErrRevisionNotAdvanced reports that a compare-and-set update did not move
// the run to a strictly newer revision.
var ErrRevisionNotAdvanced = errors.New("run revision did not advance")

// runTimestampLayout keeps serialized run timestamps fixed-width so SQLite
// text ordering matches chronological ordering for sub-second timestamps.
const runTimestampLayout = "2006-01-02T15:04:05.000000000Z07:00"

type Stage string

const (
	StageClaim          Stage = "claim"
	StagePreflight      Stage = "preflight"
	StageTest           Stage = "test"
	StageImplementation Stage = "implementation"
	StageCheck          Stage = "check"
	StageDraftPR        Stage = "draft_pr"
	StageReview         Stage = "review"
	StageReady          Stage = "ready"
)

type Status string

const (
	StatusActive            Status = "active"
	StatusWaitingForHuman   Status = "waiting_for_human"
	StatusWaitingForHarness Status = "waiting_for_harness"
	StatusFailed            Status = "failed"
	StatusCancelled         Status = "cancelled"
	StatusComplete          Status = "complete"
)

// IsTerminalStatus reports whether a run has no further coordinator
// progression state to reconcile.
func IsTerminalStatus(status Status) bool {
	return status == StatusComplete || status == StatusCancelled || status == StatusFailed
}

// InvocationStatus describes the lifecycle of one visible harness invocation.
type InvocationStatus string

const (
	// InvocationStatusActive means the harness may still produce a report.
	InvocationStatusActive InvocationStatus = "active"
	// InvocationStatusCompleted means the coordinator accepted a completed report.
	InvocationStatusCompleted InvocationStatus = "completed"
	// InvocationStatusWaitingForHuman means the report requested clarification.
	InvocationStatusWaitingForHuman InvocationStatus = "waiting_for_human"
	// InvocationStatusCannotProceed means the report contained blocking evidence.
	InvocationStatusCannotProceed InvocationStatus = "cannot_proceed"
)

// GatePhase identifies why a deterministic gate result was recorded.
type GatePhase string

const (
	// GatePhaseBaseline identifies the pre-edit health evaluation.
	GatePhaseBaseline GatePhase = "baseline"
	// GatePhaseCheckpoint identifies an implementation checkpoint evaluation.
	GatePhaseCheckpoint GatePhase = "checkpoint"
)

// GateOutcome identifies the coordinator-owned semantic result of one gate.
type GateOutcome string

const (
	// GateOutcomePassed records a successful declared command.
	GateOutcomePassed GateOutcome = "passed"
	// GateOutcomeFailed records a declared command failure or timeout.
	GateOutcomeFailed GateOutcome = "failed"
	// GateOutcomeError records a worker/runtime execution error.
	GateOutcomeError GateOutcome = "error"
	// GateOutcomeSkipped records a dependency that was not evaluated.
	GateOutcomeSkipped GateOutcome = "skipped"
	// GateOutcomeSetupFailed records a gate blocked by setup failure.
	GateOutcomeSetupFailed GateOutcome = "setup_failed"
)

type UnknownSchemaVersionError struct {
	Version   int
	Supported int
}

func (e *UnknownSchemaVersionError) Error() string {
	return fmt.Sprintf("operational store schema version %d is newer than supported version %d", e.Version, e.Supported)
}

type UnversionedDatabaseError struct {
	Path string
	Err  error
}

func (e *UnversionedDatabaseError) Error() string {
	return fmt.Sprintf("operational store %q has no readable schema version: %v", e.Path, e.Err)
}

func (e *UnversionedDatabaseError) Unwrap() error { return e.Err }

// Run is the persisted operational identity and current state of one run.
type Run struct {
	ID              string
	RepositoryPath  string
	IssueNumber     int
	Stage           Stage
	Status          Status
	Branch          string
	Worktree        string
	CheckpointSHA   string
	ImageDigest     string
	Coordinator     string
	StatusCommentID string
	// PullRequestNumber is the persisted idempotency identity of the draft PR.
	PullRequestNumber int
	// PullRequestURL is the operator-facing URL of the draft PR.
	PullRequestURL string
	// MergeCommitSHA is the immutable commit recorded when the tracked PR merges.
	MergeCommitSHA string
	// LifecycleReason explains an automatic or authorized terminal transition.
	LifecycleReason string
	// LifecycleNotificationSent records successful cmux notification delivery.
	LifecycleNotificationSent bool
	// Revision is the monotonic coordinator revision used by command replay
	// prevention and persisted supervision updates.
	Revision int64
	// ProcessedCommentID is the highest processed GitHub comment watermark for
	// this run. Editing that comment must never execute it again.
	ProcessedCommentID string
	// ProcessedCommentRevision records the run revision at which the watermark
	// was persisted, making the watermark tuple restart-safe and auditable.
	ProcessedCommentRevision int64
	// LastCommandName is the last recognized structured command kind.
	LastCommandName string
	// LastCommandOutcome is accepted, rejected, or replayed for the last command.
	LastCommandOutcome string
	// LastCommandMessage is the operator-facing result detail rendered in the
	// single editable status comment.
	LastCommandMessage string
	// HarnessOverride is the authorized harness choice for a later invocation.
	HarnessOverride string
	// CheckRepairAttempts is the number of deterministic check-repair sessions
	// successfully launched for this run.
	CheckRepairAttempts int
	// CheckRepairBudget is the frozen maximum number of check-repair sessions
	// allowed for this run.
	CheckRepairBudget int
	// CheckRepairPendingAttempt is a durable reservation between worker setup
	// and native resume. It lets reconciliation distinguish an interrupted
	// launch from a successfully consumed attempt.
	CheckRepairPendingAttempt int
	SpecificationPacket       string
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
}

// Invocation is the persisted recoverable identity of one harness session.
type Invocation struct {
	// ID is the immutable invocation identifier used by the report protocol.
	ID string
	// RunID binds the invocation to one factory run.
	RunID string
	// Harness identifies the selected role harness.
	Harness string
	// Role identifies the workflow role owning the invocation.
	Role string
	// Stage identifies the workflow stage owning the invocation.
	Stage Stage
	// Model is the validated model policy selection.
	Model string
	// ReasoningEffort is the validated reasoning policy selection.
	ReasoningEffort string
	// CredentialStoreID identifies the factory-managed credential volume used
	// by this invocation, without retaining the host auth path or its contents.
	CredentialStoreID string
	// NativeSessionID is the harness-native continuation identity when known.
	NativeSessionID string
	// WorkspaceID is the opaque terminal workspace handle.
	WorkspaceID string
	// StatusSurfaceID is the opaque supervision status surface handle.
	StatusSurfaceID string
	// ImplementationSurfaceID is the opaque implementation surface handle.
	ImplementationSurfaceID string
	// ChecksSurfaceID is the opaque deterministic-check surface handle.
	ChecksSurfaceID string
	// InvocationDirectory is the host directory containing the frozen packet.
	InvocationDirectory string
	// ResultDirectory is the host directory containing report.json.
	ResultDirectory string
	// PermittedPaths contains repository-relative prefixes authorized for the
	// completed handoff and is persisted for coordinator restart validation.
	PermittedPaths []string
	// PromptVersion identifies the versioned core role prompt.
	PromptVersion string
	// Status is the invocation lifecycle state.
	Status InvocationStatus
	// CreatedAt is the immutable invocation creation time.
	CreatedAt time.Time
	// UpdatedAt is the latest coordinator update time.
	UpdatedAt time.Time
}

// GateResult is the content-limited durable projection of one deterministic
// gate outcome. Its primary identity includes the exact checkpoint and phase,
// so a later code change cannot reuse an earlier result.
type GateResult struct {
	// RunID identifies the factory run.
	RunID string
	// CheckpointSHA identifies the exact evaluated commit.
	CheckpointSHA string
	// Phase distinguishes baseline from checkpoint evaluation.
	Phase GatePhase
	// Ordinal preserves repository declaration order.
	Ordinal int
	// GateName identifies the repository-declared gate.
	GateName string
	// Outcome is the coordinator-owned semantic result.
	Outcome GateOutcome
	// Status is the published exact-SHA status state.
	Status string
	// Blocking records the repository policy for the gate.
	Blocking bool
	// SkipReason explains a dependency or setup skip.
	SkipReason string
	// SetupFingerprint identifies the configured dependency inputs used by setup.
	SetupFingerprint string
	// CreatedAt is the first persistence time for this result identity.
	CreatedAt time.Time
	// UpdatedAt is the latest persistence time for this result identity.
	UpdatedAt time.Time
}

// GateResultTransaction atomically persists gate results and the corresponding
// content-free evaluation gate-run count.
type GateResultTransaction interface {
	SaveGateResults(context.Context, []GateResult) error
	RecordEvaluationGateRun(context.Context, string, int) error
	Commit() error
	Rollback() error
}

type Store struct {
	db            *sql.DB
	path          string
	schemaVersion int
}

// Open opens the operational store at path, creating or migrating its database as needed.
// Open opens or creates an operational store at path, applying supported schema migrations as needed.
// It requires a private parent directory and rejects empty databases, unsupported schema versions,
// and databases with missing or unreadable schema metadata. Existing databases are backed up before migration.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("operational store path is required")
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve operational store path: %w", err)
	}
	storeDirectory := filepath.Dir(absolutePath)
	if err := os.MkdirAll(storeDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create operational store directory: %w", err)
	}
	if info, err := os.Stat(storeDirectory); err != nil {
		return nil, fmt.Errorf("inspect operational store directory: %w", err)
	} else if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("operational store directory %q is not private; expected no group or other permissions", storeDirectory)
	}
	existed, empty, err := databaseState(absolutePath)
	if err != nil {
		return nil, err
	}
	if existed && empty {
		return nil, &UnversionedDatabaseError{Path: absolutePath, Err: errors.New("database file is empty")}
	}
	database, err := openConfiguredDatabase(ctx, absolutePath)
	if err != nil {
		return nil, err
	}
	closeOnError := true
	defer func() {
		if closeOnError && database != nil {
			_ = database.Close()
		}
	}()
	if !existed {
		if err := initializeMetadata(ctx, database); err != nil {
			return nil, err
		}
	}
	version, err := readSchemaVersion(ctx, database, absolutePath)
	if err != nil {
		return nil, err
	}
	if version > CurrentSchemaVersion {
		return nil, &UnknownSchemaVersionError{Version: version, Supported: CurrentSchemaVersion}
	}
	if version < CurrentSchemaVersion {
		if existed {
			if err := database.Close(); err != nil {
				return nil, fmt.Errorf("close store before migration: %w", err)
			}
			if _, err := backupDatabase(absolutePath); err != nil {
				return nil, err
			}
			database, err = openConfiguredDatabase(ctx, absolutePath)
			if err != nil {
				return nil, fmt.Errorf("reopen operational store for migration: %w", err)
			}
		}
		if err := migrate(ctx, database, version); err != nil {
			return nil, err
		}
		version = CurrentSchemaVersion
	}
	closeOnError = false
	return &Store{db: database, path: absolutePath, schemaVersion: version}, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Path() string { return s.path }

func (s *Store) SchemaVersion() int { return s.schemaVersion }

// CurrentRun returns the newest non-terminal run, if one is active.
func (s *Store) CurrentRun(ctx context.Context) (*Run, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, repository_path, issue_number, stage, status, branch, worktree,
		       checkpoint_sha, image_digest, coordinator, status_comment_id,
		       pull_request_number, pull_request_url, merge_commit_sha,
		       lifecycle_reason, lifecycle_notification_sent,
		       revision, processed_comment_id, processed_comment_revision,
		       last_command_name, last_command_outcome, last_command_message,
		       harness_override, check_repair_attempts, check_repair_budget,
		       check_repair_pending_attempt,
		       specification_packet, created_at, updated_at
		FROM operational_runs
		WHERE status NOT IN (?, ?, ?)
		ORDER BY updated_at DESC
		LIMIT 1`, StatusComplete, StatusCancelled, StatusFailed)
	return scanRun(row)
}

// LatestRun returns the most recently updated claimed run, including terminal
// runs so status can show the last known branch and worktree.
func (s *Store) LatestRun(ctx context.Context) (*Run, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, repository_path, issue_number, stage, status, branch, worktree,
		       checkpoint_sha, image_digest, coordinator, status_comment_id,
		       pull_request_number, pull_request_url, merge_commit_sha,
		       lifecycle_reason, lifecycle_notification_sent,
		       revision, processed_comment_id, processed_comment_revision,
		       last_command_name, last_command_outcome, last_command_message,
		       harness_override, check_repair_attempts, check_repair_budget,
		       check_repair_pending_attempt,
		       specification_packet, created_at, updated_at
		FROM operational_runs
		ORDER BY updated_at DESC
		LIMIT 1`)
	return scanRun(row)
}

// scanRun decodes one operational run row and its RFC3339 timestamps.
func scanRun(row *sql.Row) (*Run, error) {
	var run Run
	var createdAt, updatedAt string
	if err := row.Scan(
		&run.ID,
		&run.RepositoryPath,
		&run.IssueNumber,
		&run.Stage,
		&run.Status,
		&run.Branch,
		&run.Worktree,
		&run.CheckpointSHA,
		&run.ImageDigest,
		&run.Coordinator,
		&run.StatusCommentID,
		&run.PullRequestNumber,
		&run.PullRequestURL,
		&run.MergeCommitSHA,
		&run.LifecycleReason,
		&run.LifecycleNotificationSent,
		&run.Revision,
		&run.ProcessedCommentID,
		&run.ProcessedCommentRevision,
		&run.LastCommandName,
		&run.LastCommandOutcome,
		&run.LastCommandMessage,
		&run.HarnessOverride,
		&run.CheckRepairAttempts,
		&run.CheckRepairBudget,
		&run.CheckRepairPendingAttempt,
		&run.SpecificationPacket,
		&createdAt,
		&updatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read run row: %w", err)
	}
	var err error
	run.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse run created_at: %w", err)
	}
	run.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return nil, fmt.Errorf("parse run updated_at: %w", err)
	}
	return &run, nil
}

// SaveRun validates and upserts one operational run record.
func (s *Store) SaveRun(ctx context.Context, run Run) error {
	run, err := normalizeRun(run)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, saveRunStatement, runValues(run)...); err != nil {
		return fmt.Errorf("save run: %w", err)
	}
	return nil
}

// SaveRunIfRevision persists one command result only when the run still has
// expectedRevision. It prevents concurrent coordinators from executing one
// comment against the same stale run snapshot.
func (s *Store) SaveRunIfRevision(ctx context.Context, expectedRevision int64, run Run) error {
	run, err := normalizeRun(run)
	if err != nil {
		return err
	}
	if run.Revision <= expectedRevision {
		return fmt.Errorf("%w: got %d, expected greater than %d", ErrRevisionNotAdvanced, run.Revision, expectedRevision)
	}
	result, err := s.db.ExecContext(ctx, saveRunIfRevisionStatement, append(runValues(run)[1:], run.ID, expectedRevision)...)
	if err != nil {
		return fmt.Errorf("save run at revision %d: %w", expectedRevision, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect run revision %d update: %w", expectedRevision, err)
	}
	if changed != 1 {
		return fmt.Errorf("%w: run %q no longer has revision %d", ErrRevisionConflict, run.ID, expectedRevision)
	}
	return nil
}

// normalizeRun validates a run and fills absent timestamps before persistence.
func normalizeRun(run Run) (Run, error) {
	if run.ID == "" {
		return Run{}, errors.New("run id is required")
	}
	if run.RepositoryPath == "" {
		return Run{}, errors.New("run repository path is required")
	}
	if run.Status == "" {
		return Run{}, errors.New("run status is required")
	}
	if run.CheckRepairAttempts < 0 {
		return Run{}, errors.New("run check-repair attempts must not be negative")
	}
	if run.CheckRepairBudget < 0 {
		return Run{}, errors.New("run check-repair budget must not be negative")
	}
	if run.CheckRepairAttempts > run.CheckRepairBudget {
		return Run{}, errors.New("run check-repair attempts must not exceed budget")
	}
	if run.CheckRepairPendingAttempt < 0 {
		return Run{}, errors.New("run pending check-repair attempt must not be negative")
	}
	if run.CheckRepairPendingAttempt > run.CheckRepairBudget {
		return Run{}, errors.New("run pending check-repair attempt must not exceed budget")
	}
	if run.CheckRepairPendingAttempt != 0 && run.CheckRepairPendingAttempt != run.CheckRepairAttempts+1 {
		return Run{}, errors.New("run pending check-repair attempt must be the next attempt")
	}
	if run.CreatedAt.IsZero() {
		run.CreatedAt = time.Now().UTC()
	}
	if run.UpdatedAt.IsZero() {
		run.UpdatedAt = run.CreatedAt
	}
	return run, nil
}

// runValues returns the ordered SQL values shared by normal and revision-safe
// run persistence.
func runValues(run Run) []any {
	return []any{
		run.ID,
		run.RepositoryPath,
		run.IssueNumber,
		run.Stage,
		run.Status,
		run.Branch,
		run.Worktree,
		run.CheckpointSHA,
		run.ImageDigest,
		run.Coordinator,
		run.StatusCommentID,
		run.PullRequestNumber,
		run.PullRequestURL,
		run.MergeCommitSHA,
		run.LifecycleReason,
		run.LifecycleNotificationSent,
		run.Revision,
		run.ProcessedCommentID,
		run.ProcessedCommentRevision,
		run.LastCommandName,
		run.LastCommandOutcome,
		run.LastCommandMessage,
		run.HarnessOverride,
		run.CheckRepairAttempts,
		run.CheckRepairBudget,
		run.CheckRepairPendingAttempt,
		run.SpecificationPacket,
		run.CreatedAt.UTC().Format(runTimestampLayout),
		run.UpdatedAt.UTC().Format(runTimestampLayout),
	}
}

const saveRunStatement = `
		INSERT INTO operational_runs (
			id, repository_path, issue_number, stage, status, branch, worktree,
			checkpoint_sha, image_digest, coordinator, status_comment_id,
			pull_request_number, pull_request_url, merge_commit_sha, lifecycle_reason,
			lifecycle_notification_sent,
			revision, processed_comment_id,
			processed_comment_revision, last_command_name, last_command_outcome,
			last_command_message, harness_override, check_repair_attempts,
			check_repair_budget, check_repair_pending_attempt, specification_packet,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			repository_path = excluded.repository_path,
			issue_number = excluded.issue_number,
			stage = excluded.stage,
			status = excluded.status,
			branch = excluded.branch,
			worktree = excluded.worktree,
			checkpoint_sha = excluded.checkpoint_sha,
			image_digest = excluded.image_digest,
			coordinator = excluded.coordinator,
			status_comment_id = excluded.status_comment_id,
			pull_request_number = excluded.pull_request_number,
			pull_request_url = excluded.pull_request_url,
			merge_commit_sha = excluded.merge_commit_sha,
			lifecycle_reason = excluded.lifecycle_reason,
			lifecycle_notification_sent = excluded.lifecycle_notification_sent,
			revision = excluded.revision,
			processed_comment_id = excluded.processed_comment_id,
			processed_comment_revision = excluded.processed_comment_revision,
			last_command_name = excluded.last_command_name,
			last_command_outcome = excluded.last_command_outcome,
			last_command_message = excluded.last_command_message,
			harness_override = excluded.harness_override,
			check_repair_attempts = excluded.check_repair_attempts,
			check_repair_budget = excluded.check_repair_budget,
			check_repair_pending_attempt = excluded.check_repair_pending_attempt,
			specification_packet = excluded.specification_packet,
			updated_at = excluded.updated_at`

const saveRunIfRevisionStatement = `
		UPDATE operational_runs SET
			repository_path = ?, issue_number = ?, stage = ?, status = ?, branch = ?, worktree = ?,
			checkpoint_sha = ?, image_digest = ?, coordinator = ?, status_comment_id = ?,
			pull_request_number = ?, pull_request_url = ?, merge_commit_sha = ?, lifecycle_reason = ?,
			lifecycle_notification_sent = ?,
			revision = ?, processed_comment_id = ?,
			processed_comment_revision = ?, last_command_name = ?, last_command_outcome = ?,
			last_command_message = ?, harness_override = ?, check_repair_attempts = ?,
			check_repair_budget = ?, check_repair_pending_attempt = ?, specification_packet = ?,
			created_at = ?, updated_at = ?
		WHERE id = ? AND revision = ?`

// SaveInvocation validates and upserts one recoverable harness invocation.
func (s *Store) SaveInvocation(ctx context.Context, invocation Invocation) error {
	if invocation.ID == "" {
		return errors.New("invocation id is required")
	}
	if invocation.RunID == "" {
		return errors.New("invocation run id is required")
	}
	if invocation.Harness == "" {
		return errors.New("invocation harness is required")
	}
	if invocation.Role == "" {
		return errors.New("invocation role is required")
	}
	if invocation.Stage == "" {
		return errors.New("invocation stage is required")
	}
	if invocation.Status == "" {
		return errors.New("invocation status is required")
	}
	if strings.ContainsAny(invocation.CredentialStoreID, "\x00\r\n") {
		return errors.New("invocation credential store id contains control characters")
	}
	if invocation.CreatedAt.IsZero() {
		invocation.CreatedAt = time.Now().UTC()
	}
	if invocation.UpdatedAt.IsZero() {
		invocation.UpdatedAt = invocation.CreatedAt
	}
	permittedPathJSON, err := json.Marshal(invocation.PermittedPaths)
	if err != nil {
		return fmt.Errorf("encode invocation permitted paths: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO invocations (
			id, run_id, harness, role, stage, model, reasoning_effort, credential_store_id,
			native_session_id, workspace_id, status_surface_id,
			implementation_surface_id, checks_surface_id, invocation_directory,
			result_directory, permitted_paths, prompt_version, status, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			run_id = excluded.run_id,
			harness = excluded.harness,
			role = excluded.role,
			stage = excluded.stage,
			model = excluded.model,
			reasoning_effort = excluded.reasoning_effort,
			credential_store_id = excluded.credential_store_id,
			native_session_id = excluded.native_session_id,
			workspace_id = excluded.workspace_id,
			status_surface_id = excluded.status_surface_id,
			implementation_surface_id = excluded.implementation_surface_id,
			checks_surface_id = excluded.checks_surface_id,
			invocation_directory = excluded.invocation_directory,
			result_directory = excluded.result_directory,
			permitted_paths = excluded.permitted_paths,
			prompt_version = excluded.prompt_version,
			status = excluded.status,
			updated_at = excluded.updated_at`,
		invocation.ID,
		invocation.RunID,
		invocation.Harness,
		invocation.Role,
		invocation.Stage,
		invocation.Model,
		invocation.ReasoningEffort,
		invocation.CredentialStoreID,
		invocation.NativeSessionID,
		invocation.WorkspaceID,
		invocation.StatusSurfaceID,
		invocation.ImplementationSurfaceID,
		invocation.ChecksSurfaceID,
		invocation.InvocationDirectory,
		invocation.ResultDirectory,
		string(permittedPathJSON),
		invocation.PromptVersion,
		invocation.Status,
		invocation.CreatedAt.UTC().Format(runTimestampLayout),
		invocation.UpdatedAt.UTC().Format(runTimestampLayout),
	)
	if err != nil {
		if isActiveInvocationConflict(err) {
			return fmt.Errorf("save invocation: active invocation already exists for run %q: %w", invocation.RunID, err)
		}
		return fmt.Errorf("save invocation: %w", err)
	}
	return nil
}

// Invocation loads one persisted invocation only when both identifiers match.
func (s *Store) Invocation(ctx context.Context, runID, invocationID string) (*Invocation, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, run_id, harness, role, stage, model, reasoning_effort, credential_store_id,
		       native_session_id, workspace_id, status_surface_id,
		       implementation_surface_id, checks_surface_id, invocation_directory,
		       result_directory, permitted_paths, prompt_version, status, created_at, updated_at
		FROM invocations
		WHERE run_id = ? AND id = ?`, runID, invocationID)
	return scanInvocation(row)
}

// LatestInvocation returns the most recently updated invocation for one run,
// including terminal history needed to resume the implementation session that
// produced a failed checkpoint.
func (s *Store) LatestInvocation(ctx context.Context, runID string) (*Invocation, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, errors.New("invocation run id is required")
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT id, run_id, harness, role, stage, model, reasoning_effort, credential_store_id,
		       native_session_id, workspace_id, status_surface_id,
		       implementation_surface_id, checks_surface_id, invocation_directory,
		       result_directory, permitted_paths, prompt_version, status, created_at, updated_at
		FROM invocations
		WHERE run_id = ?
		ORDER BY updated_at DESC, id DESC
		LIMIT 1`, runID)
	return scanInvocation(row)
}

// HasInvocation reports whether any invocation, including terminal history,
// has been persisted for one run.
func (s *Store) HasInvocation(ctx context.Context, runID string) (bool, error) {
	if strings.TrimSpace(runID) == "" {
		return false, errors.New("invocation run id is required")
	}
	var found bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM invocations WHERE run_id = ?)`, runID).Scan(&found); err != nil {
		return false, fmt.Errorf("check invocation history: %w", err)
	}
	return found, nil
}

// ActiveInvocation returns the newest active visible invocation for one run.
// The coordinator uses it to avoid launching duplicate harness sessions after
// a process restart or a repeated operator command.
func (s *Store) ActiveInvocation(ctx context.Context, runID string) (*Invocation, error) {
	if runID == "" {
		return nil, errors.New("invocation run id is required")
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT id, run_id, harness, role, stage, model, reasoning_effort, credential_store_id,
		       native_session_id, workspace_id, status_surface_id,
		       implementation_surface_id, checks_surface_id, invocation_directory,
		       result_directory, permitted_paths, prompt_version, status, created_at, updated_at
		FROM invocations
		WHERE run_id = ? AND status = ?
		ORDER BY updated_at DESC
		LIMIT 1`, runID, InvocationStatusActive)
	return scanInvocation(row)
}

// scanInvocation decodes one invocation row and its RFC3339 timestamps.
func scanInvocation(row *sql.Row) (*Invocation, error) {
	var invocation Invocation
	var permittedPathJSON, createdAt, updatedAt string
	if err := row.Scan(
		&invocation.ID,
		&invocation.RunID,
		&invocation.Harness,
		&invocation.Role,
		&invocation.Stage,
		&invocation.Model,
		&invocation.ReasoningEffort,
		&invocation.CredentialStoreID,
		&invocation.NativeSessionID,
		&invocation.WorkspaceID,
		&invocation.StatusSurfaceID,
		&invocation.ImplementationSurfaceID,
		&invocation.ChecksSurfaceID,
		&invocation.InvocationDirectory,
		&invocation.ResultDirectory,
		&permittedPathJSON,
		&invocation.PromptVersion,
		&invocation.Status,
		&createdAt,
		&updatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read invocation row: %w", err)
	}
	if permittedPathJSON != "" {
		if err := json.Unmarshal([]byte(permittedPathJSON), &invocation.PermittedPaths); err != nil {
			return nil, fmt.Errorf("decode invocation permitted paths: %w", err)
		}
	}
	var err error
	invocation.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse invocation created_at: %w", err)
	}
	invocation.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return nil, fmt.Errorf("parse invocation updated_at: %w", err)
	}
	return &invocation, nil
}

// validateGateResults validates the complete batch before a transaction is
// opened, keeping invalid input from creating any partial durable state.
func validateGateResults(results []GateResult) error {
	if len(results) == 0 {
		return errors.New("at least one gate result is required")
	}
	for _, result := range results {
		if err := validateGateResult(result); err != nil {
			return err
		}
	}
	return nil
}

// saveGateResults writes one validated batch through the supplied transaction.
func saveGateResults(ctx context.Context, tx *sql.Tx, results []GateResult) error {
	for _, result := range results {
		createdAt := result.CreatedAt
		if createdAt.IsZero() {
			createdAt = time.Now().UTC()
		}
		updatedAt := result.UpdatedAt
		if updatedAt.IsZero() {
			updatedAt = createdAt
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO gate_results (
				run_id, checkpoint_sha, phase, ordinal, gate_name, outcome, status,
				blocking, skip_reason, setup_fingerprint, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(run_id, checkpoint_sha, phase, gate_name) DO UPDATE SET
				ordinal = excluded.ordinal,
				outcome = excluded.outcome,
				status = excluded.status,
				blocking = excluded.blocking,
				skip_reason = excluded.skip_reason,
				setup_fingerprint = excluded.setup_fingerprint,
				updated_at = excluded.updated_at`,
			result.RunID,
			result.CheckpointSHA,
			result.Phase,
			result.Ordinal,
			result.GateName,
			result.Outcome,
			result.Status,
			result.Blocking,
			result.SkipReason,
			result.SetupFingerprint,
			createdAt.UTC().Format(runTimestampLayout),
			updatedAt.UTC().Format(runTimestampLayout),
		); err != nil {
			return fmt.Errorf("save gate result %q: %w", result.GateName, err)
		}
	}
	return nil
}

// SaveGateResults atomically upserts the ordered deterministic results from
// one suite while preserving their exact run, phase, and checkpoint identity.
func (s *Store) SaveGateResults(ctx context.Context, results []GateResult) error {
	if err := validateGateResults(results); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin gate-result save: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := saveGateResults(ctx, tx, results); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit gate-result save: %w", err)
	}
	return nil
}

// BeginGateResultTransaction starts the transaction used to persist gate
// results together with their evaluation execution count.
func (s *Store) BeginGateResultTransaction(ctx context.Context) (GateResultTransaction, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin gate-result save: %w", err)
	}
	return &gateResultTransaction{tx: tx}, nil
}

// gateResultTransaction adapts a SQLite transaction to the durable gate
// persistence seam used by the coordinator.
type gateResultTransaction struct {
	tx *sql.Tx
}

// SaveGateResults persists validated gate results within the open transaction.
func (t *gateResultTransaction) SaveGateResults(ctx context.Context, results []GateResult) error {
	if err := validateGateResults(results); err != nil {
		return err
	}
	return saveGateResults(ctx, t.tx, results)
}

// RecordEvaluationGateRun persists the execution count within the open
// transaction so it commits or rolls back with the gate results.
func (t *gateResultTransaction) RecordEvaluationGateRun(ctx context.Context, runID string, count int) error {
	return recordEvaluationGateRun(ctx, t.tx, runID, count)
}

// Commit commits all writes made through the transaction.
func (t *gateResultTransaction) Commit() error {
	return t.tx.Commit()
}

// Rollback discards all writes made through the transaction.
func (t *gateResultTransaction) Rollback() error {
	return t.tx.Rollback()
}

// GateResults returns results only for the requested exact checkpoint and
// phase, ordered as declared in the frozen repository configuration.
func (s *Store) GateResults(ctx context.Context, runID string, phase GatePhase, checkpointSHA string) ([]GateResult, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, errors.New("gate result run id is required")
	}
	if err := validateGatePhase(phase); err != nil {
		return nil, err
	}
	if strings.TrimSpace(checkpointSHA) == "" {
		return nil, errors.New("gate result checkpoint SHA is required")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT run_id, checkpoint_sha, phase, ordinal, gate_name, outcome, status,
		       blocking, skip_reason, setup_fingerprint, created_at, updated_at
		FROM gate_results
		WHERE run_id = ? AND phase = ? AND checkpoint_sha = ?
		ORDER BY ordinal, gate_name`, runID, phase, checkpointSHA)
	if err != nil {
		return nil, fmt.Errorf("read gate results: %w", err)
	}
	defer func() { _ = rows.Close() }()
	results := []GateResult{}
	for rows.Next() {
		var result GateResult
		var createdAt, updatedAt string
		if err := rows.Scan(
			&result.RunID,
			&result.CheckpointSHA,
			&result.Phase,
			&result.Ordinal,
			&result.GateName,
			&result.Outcome,
			&result.Status,
			&result.Blocking,
			&result.SkipReason,
			&result.SetupFingerprint,
			&createdAt,
			&updatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan gate result: %w", err)
		}
		var parseErr error
		result.CreatedAt, parseErr = time.Parse(time.RFC3339Nano, createdAt)
		if parseErr != nil {
			return nil, fmt.Errorf("parse gate result created_at: %w", parseErr)
		}
		result.UpdatedAt, parseErr = time.Parse(time.RFC3339Nano, updatedAt)
		if parseErr != nil {
			return nil, fmt.Errorf("parse gate result updated_at: %w", parseErr)
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read gate results: %w", err)
	}
	return results, nil
}

// validateGateResult validates the durable result identity and vocabulary.
func validateGateResult(result GateResult) error {
	if strings.TrimSpace(result.RunID) == "" {
		return errors.New("gate result run id is required")
	}
	if !validGateCheckpointSHA(result.CheckpointSHA) {
		return errors.New("gate result checkpoint SHA must contain exactly 40 or 64 lowercase hexadecimal characters")
	}
	if err := validateGatePhase(result.Phase); err != nil {
		return err
	}
	if result.Ordinal < 0 {
		return errors.New("gate result ordinal must not be negative")
	}
	if strings.TrimSpace(result.GateName) == "" {
		return errors.New("gate result gate name is required")
	}
	switch result.Outcome {
	case GateOutcomePassed, GateOutcomeFailed, GateOutcomeError, GateOutcomeSkipped, GateOutcomeSetupFailed:
	default:
		return fmt.Errorf("unsupported gate result outcome %q", result.Outcome)
	}
	if strings.TrimSpace(result.Status) == "" {
		return errors.New("gate result status is required")
	}
	return nil
}

// validGateCheckpointSHA validates the exact commit identity retained by a
// durable gate result without importing the host-side GitHub adapter.
func validGateCheckpointSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') {
			continue
		}
		return false
	}
	return true
}

// validateGatePhase validates the durable baseline/checkpoint phase vocabulary.
func validateGatePhase(phase GatePhase) error {
	if phase != GatePhaseBaseline && phase != GatePhaseCheckpoint {
		return fmt.Errorf("unsupported gate result phase %q", phase)
	}
	return nil
}

// ClaimLifecycleNotification atomically claims the right to send one terminal
// notification, returning true if the claim succeeded (caller should send),
// false if the claim already exists (notification already sent or being sent).
func (s *Store) ClaimLifecycleNotification(ctx context.Context, runID string, terminalStatus Status) (bool, error) {
	if runID == "" {
		return false, errors.New("run id is required for lifecycle notification claim")
	}
	if !IsTerminalStatus(terminalStatus) {
		return false, fmt.Errorf("cannot claim lifecycle notification for non-terminal status %q", terminalStatus)
	}
	now := time.Now().UTC().Format(runTimestampLayout)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO lifecycle_notifications (run_id, terminal_status, created_at)
		VALUES (?, ?, ?)`, runID, terminalStatus, now)
	if err != nil {
		if isLifecycleNotificationClaimConflict(err) {
			return false, nil
		}
		return false, fmt.Errorf("claim lifecycle notification for run %q status %q: %w", runID, terminalStatus, err)
	}
	return true, nil
}

// ReleaseLifecycleNotification removes a notification claim, allowing a future
// retry to attempt delivery again. This is called when notification delivery
// fails after the claim was successfully recorded.
func (s *Store) ReleaseLifecycleNotification(ctx context.Context, runID string, terminalStatus Status) error {
	if runID == "" {
		return errors.New("run id is required for lifecycle notification release")
	}
	if !IsTerminalStatus(terminalStatus) {
		return fmt.Errorf("cannot release lifecycle notification for non-terminal status %q", terminalStatus)
	}
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM lifecycle_notifications
		WHERE run_id = ? AND terminal_status = ?`, runID, terminalStatus)
	if err != nil {
		return fmt.Errorf("release lifecycle notification claim for run %q status %q: %w", runID, terminalStatus, err)
	}
	return nil
}

// isLifecycleNotificationClaimConflict recognizes the primary key violation
// without depending on a driver-specific SQLite error type.
func isLifecycleNotificationClaimConflict(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "primary key") || strings.Contains(message, "unique constraint failed: lifecycle_notifications")
}

// databaseState reports whether the path identifies an existing database file and whether that file is empty.
func databaseState(path string) (bool, bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("inspect operational store: %w", err)
	}
	if info.IsDir() {
		return false, false, fmt.Errorf("operational store path %q is a directory", path)
	}
	return true, info.Size() == 0, nil
}

// openConfiguredDatabase opens the SQLite database at path, restricts its file permissions,
// limits it to a single connection, and applies the store configuration. It closes the
// database and returns an error if setup fails.
func openConfiguredDatabase(ctx context.Context, path string) (*sql.DB, error) {
	database, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open operational store: %w", err)
	}
	if err := database.PingContext(ctx); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("open operational store: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("protect operational store: %w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	if err := configure(ctx, database); err != nil {
		_ = database.Close()
		return nil, err
	}
	return database, nil
}

// configure applies the SQLite settings required by the operational store.
func configure(ctx context.Context, database *sql.DB) error {
	for _, statement := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = DELETE",
	} {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure SQLite: %w", err)
		}
	}
	return nil
}

// initializeMetadata creates the schema metadata table and initializes its version to zero.
func initializeMetadata(ctx context.Context, database *sql.DB) error {
	if _, err := database.ExecContext(ctx, `CREATE TABLE schema_metadata (
		singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
		version INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema metadata: %w", err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO schema_metadata(singleton, version) VALUES (1, 0)`); err != nil {
		return fmt.Errorf("initialize schema metadata: %w", err)
	}
	return nil
}

// readSchemaVersion reads the schema version recorded in the database metadata.
// It returns an UnversionedDatabaseError if the metadata cannot be read.
func readSchemaVersion(ctx context.Context, database *sql.DB, path string) (int, error) {
	var version int
	if err := database.QueryRowContext(ctx, "SELECT version FROM schema_metadata WHERE singleton = 1").Scan(&version); err != nil {
		return 0, &UnversionedDatabaseError{Path: path, Err: err}
	}
	return version, nil
}

// migrate applies supported schema migrations from the specified version through the current schema version in a single transaction. It records each completed migration and leaves the database unchanged if a migration fails.
func migrate(ctx context.Context, database *sql.DB, from int) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin store migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	version := from
	for version < CurrentSchemaVersion {
		switch version + 1 {
		case 1:
			if _, err := tx.ExecContext(ctx, `CREATE TABLE operational_runs (
				id TEXT PRIMARY KEY,
				repository_path TEXT NOT NULL,
				issue_number INTEGER NOT NULL,
				stage TEXT NOT NULL,
				status TEXT NOT NULL,
				branch TEXT NOT NULL DEFAULT '',
				worktree TEXT NOT NULL DEFAULT '',
				checkpoint_sha TEXT NOT NULL DEFAULT '',
				image_digest TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL
			)`); err != nil {
				return fmt.Errorf("apply store migration 1: %w", err)
			}
		case 2:
			for _, statement := range []string{
				"ALTER TABLE operational_runs ADD COLUMN coordinator TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN status_comment_id TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN specification_packet TEXT NOT NULL DEFAULT ''",
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("apply store migration 2: %w", err)
				}
			}
		case 3:
			if err := reconcileDuplicateRuns(ctx, tx); err != nil {
				return fmt.Errorf("reconcile runs before store migration 3: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX one_active_run_per_repository
				ON operational_runs (repository_path)
				WHERE status NOT IN ('complete', 'cancelled', 'failed')`); err != nil {
				return fmt.Errorf("apply store migration 3: %w", err)
			}
		case 4:
			if _, err := tx.ExecContext(ctx, `CREATE TABLE invocations (
					id TEXT PRIMARY KEY,
					run_id TEXT NOT NULL,
					harness TEXT NOT NULL,
					role TEXT NOT NULL,
					stage TEXT NOT NULL,
					model TEXT NOT NULL DEFAULT '',
					reasoning_effort TEXT NOT NULL DEFAULT '',
					native_session_id TEXT NOT NULL DEFAULT '',
					workspace_id TEXT NOT NULL DEFAULT '',
					status_surface_id TEXT NOT NULL DEFAULT '',
					implementation_surface_id TEXT NOT NULL DEFAULT '',
					checks_surface_id TEXT NOT NULL DEFAULT '',
					invocation_directory TEXT NOT NULL DEFAULT '',
					result_directory TEXT NOT NULL DEFAULT '',
					prompt_version TEXT NOT NULL DEFAULT '',
					status TEXT NOT NULL,
					created_at TEXT NOT NULL,
					updated_at TEXT NOT NULL
				)`); err != nil {
				return fmt.Errorf("apply store migration 4: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `CREATE INDEX invocations_by_run
					ON invocations (run_id, updated_at)`); err != nil {
				return fmt.Errorf("apply store migration 4 index: %w", err)
			}
		case 5:
			if _, err := tx.ExecContext(ctx, `ALTER TABLE invocations ADD COLUMN permitted_paths TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("apply store migration 5: %w", err)
			}
		case 6:
			if err := reconcileDuplicateInvocations(ctx, tx); err != nil {
				return fmt.Errorf("reconcile invocations before store migration 6: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX one_active_invocation_per_run
				ON invocations (run_id)
				WHERE status = 'active'`); err != nil {
				return fmt.Errorf("apply store migration 6: %w", err)
			}
		case 7:
			for _, statement := range []string{
				"ALTER TABLE operational_runs ADD COLUMN pull_request_number INTEGER NOT NULL DEFAULT 0",
				"ALTER TABLE operational_runs ADD COLUMN pull_request_url TEXT NOT NULL DEFAULT ''",
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("apply store migration 7: %w", err)
				}
			}
		case 8:
			for _, statement := range []string{
				"ALTER TABLE operational_runs ADD COLUMN revision INTEGER NOT NULL DEFAULT 0",
				"ALTER TABLE operational_runs ADD COLUMN processed_comment_id TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN processed_comment_revision INTEGER NOT NULL DEFAULT 0",
				"ALTER TABLE operational_runs ADD COLUMN last_command_name TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN last_command_outcome TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN last_command_message TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN harness_override TEXT NOT NULL DEFAULT ''",
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("apply store migration 8: %w", err)
				}
			}
		case 9:
			for _, statement := range []string{
				"ALTER TABLE operational_runs ADD COLUMN merge_commit_sha TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN lifecycle_reason TEXT NOT NULL DEFAULT ''",
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("apply store migration 9: %w", err)
				}
			}
		case 10:
			if _, err := tx.ExecContext(ctx, "ALTER TABLE operational_runs ADD COLUMN lifecycle_notification_sent INTEGER NOT NULL DEFAULT 0"); err != nil {
				return fmt.Errorf("apply store migration 10: %w", err)
			}
		case 11:
			if _, err := tx.ExecContext(ctx, `CREATE TABLE lifecycle_notifications (
					run_id TEXT NOT NULL,
					terminal_status TEXT NOT NULL,
					created_at TEXT NOT NULL,
					PRIMARY KEY (run_id, terminal_status)
				)`); err != nil {
				return fmt.Errorf("apply store migration 11: %w", err)
			}
		case 12:
			if _, err := tx.ExecContext(ctx, `CREATE TABLE gate_results (
					run_id TEXT NOT NULL,
					checkpoint_sha TEXT NOT NULL,
					phase TEXT NOT NULL,
					ordinal INTEGER NOT NULL,
					gate_name TEXT NOT NULL,
					outcome TEXT NOT NULL,
					status TEXT NOT NULL,
					blocking INTEGER NOT NULL,
					skip_reason TEXT NOT NULL DEFAULT '',
					setup_fingerprint TEXT NOT NULL DEFAULT '',
					created_at TEXT NOT NULL,
					updated_at TEXT NOT NULL,
					PRIMARY KEY (run_id, checkpoint_sha, phase, gate_name)
				)`); err != nil {
				return fmt.Errorf("apply store migration 12: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `CREATE INDEX gate_results_by_run
					ON gate_results (run_id, phase, checkpoint_sha, ordinal)`); err != nil {
				return fmt.Errorf("apply store migration 12 index: %w", err)
			}
		case 13:
			if _, err := tx.ExecContext(ctx, `CREATE TABLE evaluation_summaries (
				run_id TEXT PRIMARY KEY,
				outcome TEXT NOT NULL,
				started_at TEXT NOT NULL,
				completed_at TEXT NOT NULL DEFAULT '',
				total_wall_nanos INTEGER NOT NULL DEFAULT 0,
				stage_durations TEXT NOT NULL DEFAULT '[]',
				invocation_versions TEXT NOT NULL DEFAULT '[]',
				invocation_count INTEGER NOT NULL DEFAULT 0,
				gate_count INTEGER NOT NULL DEFAULT 0,
				check_repair_count INTEGER NOT NULL DEFAULT 0,
				test_revision_count INTEGER NOT NULL DEFAULT 0,
				review_revision_count INTEGER NOT NULL DEFAULT 0,
				budget_exhausted INTEGER NOT NULL DEFAULT 0,
				exemptions TEXT NOT NULL DEFAULT '[]',
				escalation_categories TEXT NOT NULL DEFAULT '[]',
				repeated_blockers TEXT NOT NULL DEFAULT '[]',
				usage_available INTEGER NOT NULL DEFAULT 0,
				usage_unavailable_reason TEXT NOT NULL DEFAULT 'not_reported',
				usage_input_tokens INTEGER NOT NULL DEFAULT 0,
				usage_output_tokens INTEGER NOT NULL DEFAULT 0,
				usage_total_tokens INTEGER NOT NULL DEFAULT 0,
				usage_cost_reported INTEGER NOT NULL DEFAULT 0,
				usage_cost_micros INTEGER NOT NULL DEFAULT 0,
				usage_currency TEXT NOT NULL DEFAULT '',
				active_stage TEXT NOT NULL DEFAULT '',
				active_stage_started_at TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL
			)`); err != nil {
				return fmt.Errorf("apply store migration 13 evaluation summaries: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `CREATE TABLE evaluation_usage (
				run_id TEXT NOT NULL,
				invocation_id TEXT NOT NULL,
				input_tokens INTEGER NOT NULL DEFAULT 0,
				output_tokens INTEGER NOT NULL DEFAULT 0,
				total_tokens INTEGER NOT NULL DEFAULT 0,
				cost_reported INTEGER NOT NULL DEFAULT 0,
				cost_micros INTEGER NOT NULL DEFAULT 0,
				currency TEXT NOT NULL DEFAULT '',
				recorded_at TEXT NOT NULL,
				PRIMARY KEY (run_id, invocation_id)
			)`); err != nil {
				return fmt.Errorf("apply store migration 13 evaluation usage: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `CREATE INDEX evaluation_usage_by_run
					ON evaluation_usage (run_id, recorded_at)`); err != nil {
				return fmt.Errorf("apply store migration 13 evaluation usage index: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `CREATE TABLE evaluation_dispositions (
				run_id TEXT NOT NULL,
				event_id_hash TEXT NOT NULL,
				category TEXT NOT NULL,
				disposition TEXT NOT NULL,
				decided_at TEXT NOT NULL,
				PRIMARY KEY (run_id, event_id_hash)
			)`); err != nil {
				return fmt.Errorf("apply store migration 13 evaluation dispositions: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `CREATE INDEX evaluation_dispositions_by_run
					ON evaluation_dispositions (run_id, decided_at)`); err != nil {
				return fmt.Errorf("apply store migration 13 evaluation dispositions index: %w", err)
			}
		case 14:
			for _, statement := range []string{
				"ALTER TABLE operational_runs ADD COLUMN check_repair_attempts INTEGER NOT NULL DEFAULT 0",
				"ALTER TABLE operational_runs ADD COLUMN check_repair_budget INTEGER NOT NULL DEFAULT 0",
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("apply store migration 14: %w", err)
				}
			}
		case 15:
			if _, err := tx.ExecContext(ctx, "ALTER TABLE invocations ADD COLUMN credential_store_id TEXT NOT NULL DEFAULT ''"); err != nil {
				return fmt.Errorf("apply store migration 15: %w", err)
			}
		case 16:
			if _, err := tx.ExecContext(ctx, "ALTER TABLE operational_runs ADD COLUMN check_repair_pending_attempt INTEGER NOT NULL DEFAULT 0"); err != nil {
				return fmt.Errorf("apply store migration 16: %w", err)
			}
		default:
			return fmt.Errorf("no migration registered for schema version %d", version+1)
		}
		version++
		if _, err := tx.ExecContext(ctx, "UPDATE schema_metadata SET version = ? WHERE singleton = 1", version); err != nil {
			return fmt.Errorf("record store schema version %d: %w", version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit store migration: %w", err)
	}
	return nil
}

// reconcileDuplicateRuns keeps the newest non-terminal run per repository
// while migrating legacy rows whose timestamp text may have variable-width
// fractional seconds. Go's RFC3339Nano parser provides chronological ordering
// at nanosecond precision before older duplicates are marked failed.
func reconcileDuplicateRuns(ctx context.Context, tx *sql.Tx) error {
	type runCandidate struct {
		id        string
		updatedAt time.Time
	}
	// First, normalize all legacy timestamps to the canonical fixed-width format
	// so that subsequent SQL ORDER BY operations on created_at/updated_at produce
	// correct chronological results via lexical string comparison.
	allRows, err := tx.QueryContext(ctx, `SELECT id, created_at, updated_at FROM operational_runs`)
	if err != nil {
		return fmt.Errorf("list runs for timestamp normalization: %w", err)
	}
	type timestampUpdate struct {
		id        string
		createdAt string
		updatedAt string
	}
	updates := make([]timestampUpdate, 0)
	for allRows.Next() {
		var id, createdAt, updatedAt string
		if err := allRows.Scan(&id, &createdAt, &updatedAt); err != nil {
			_ = allRows.Close()
			return fmt.Errorf("scan run for timestamp normalization: %w", err)
		}
		parsedCreatedAt, err := time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			_ = allRows.Close()
			return fmt.Errorf("parse run created_at for normalization: %w", err)
		}
		parsedUpdatedAt, err := time.Parse(time.RFC3339Nano, updatedAt)
		if err != nil {
			_ = allRows.Close()
			return fmt.Errorf("parse run updated_at for normalization: %w", err)
		}
		normalizedCreatedAt := parsedCreatedAt.Format(runTimestampLayout)
		normalizedUpdatedAt := parsedUpdatedAt.Format(runTimestampLayout)
		if createdAt != normalizedCreatedAt || updatedAt != normalizedUpdatedAt {
			updates = append(updates, timestampUpdate{
				id:        id,
				createdAt: normalizedCreatedAt,
				updatedAt: normalizedUpdatedAt,
			})
		}
	}
	if err := allRows.Err(); err != nil {
		_ = allRows.Close()
		return fmt.Errorf("read runs for timestamp normalization: %w", err)
	}
	if err := allRows.Close(); err != nil {
		return fmt.Errorf("close runs for timestamp normalization: %w", err)
	}
	for _, update := range updates {
		if _, err := tx.ExecContext(ctx, `UPDATE operational_runs SET created_at = ?, updated_at = ? WHERE id = ?`,
			update.createdAt, update.updatedAt, update.id); err != nil {
			return fmt.Errorf("normalize timestamps for run %q: %w", update.id, err)
		}
	}
	// Now reconcile duplicates using the normalized timestamps
	rows, err := tx.QueryContext(ctx, `
		SELECT id, repository_path, updated_at
		FROM operational_runs
		WHERE status NOT IN (?, ?, ?)`, StatusComplete, StatusCancelled, StatusFailed)
	if err != nil {
		return fmt.Errorf("list non-terminal runs: %w", err)
	}
	newestByRepository := make(map[string]runCandidate)
	duplicates := make([]string, 0)
	for rows.Next() {
		var id, repositoryPath, updatedAt string
		if err := rows.Scan(&id, &repositoryPath, &updatedAt); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan run for reconciliation: %w", err)
		}
		parsedUpdatedAt, err := time.Parse(time.RFC3339Nano, updatedAt)
		if err != nil {
			_ = rows.Close()
			return fmt.Errorf("parse run updated_at for reconciliation: %w", err)
		}
		candidate := runCandidate{id: id, updatedAt: parsedUpdatedAt}
		current, exists := newestByRepository[repositoryPath]
		if !exists || parsedUpdatedAt.After(current.updatedAt) || (parsedUpdatedAt.Equal(current.updatedAt) && id > current.id) {
			if exists {
				duplicates = append(duplicates, current.id)
			}
			newestByRepository[repositoryPath] = candidate
		} else {
			duplicates = append(duplicates, candidate.id)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read runs for reconciliation: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close runs for reconciliation: %w", err)
	}
	for _, id := range duplicates {
		if _, err := tx.ExecContext(ctx, "UPDATE operational_runs SET status = ? WHERE id = ?", StatusFailed, id); err != nil {
			return fmt.Errorf("mark duplicate run %q failed: %w", id, err)
		}
	}
	return nil
}

// reconcileDuplicateInvocations keeps the newest active invocation per run
// before the partial unique index is created for migrated stores.
func reconcileDuplicateInvocations(ctx context.Context, tx *sql.Tx) error {
	type invocationCandidate struct {
		id        string
		updatedAt time.Time
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, run_id, updated_at
		FROM invocations
		WHERE status = ?`, InvocationStatusActive)
	if err != nil {
		return fmt.Errorf("list active invocations: %w", err)
	}
	newestByRun := make(map[string]invocationCandidate)
	duplicates := make([]string, 0)
	for rows.Next() {
		var id, runID, updatedAt string
		if err := rows.Scan(&id, &runID, &updatedAt); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan invocation for reconciliation: %w", err)
		}
		parsedUpdatedAt, err := time.Parse(time.RFC3339Nano, updatedAt)
		if err != nil {
			_ = rows.Close()
			return fmt.Errorf("parse invocation updated_at for reconciliation: %w", err)
		}
		candidate := invocationCandidate{id: id, updatedAt: parsedUpdatedAt}
		current, exists := newestByRun[runID]
		if !exists || parsedUpdatedAt.After(current.updatedAt) || (parsedUpdatedAt.Equal(current.updatedAt) && id > current.id) {
			if exists {
				duplicates = append(duplicates, current.id)
			}
			newestByRun[runID] = candidate
		} else {
			duplicates = append(duplicates, candidate.id)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read invocations for reconciliation: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close invocations for reconciliation: %w", err)
	}
	for _, id := range duplicates {
		if _, err := tx.ExecContext(ctx, "UPDATE invocations SET status = ? WHERE id = ?", InvocationStatusCannotProceed, id); err != nil {
			return fmt.Errorf("mark duplicate invocation %q unavailable: %w", id, err)
		}
	}
	return nil
}

// isActiveInvocationConflict recognizes the migration-created unique index
// without depending on a driver-specific SQLite error type.
func isActiveInvocationConflict(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "one_active_invocation_per_run") || strings.Contains(message, "unique constraint failed: invocations.run_id")
}

// backupDatabase creates a private 0600 backup of the database and returns its path.
// It removes an incomplete backup when copying fails.
func backupDatabase(path string) (string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		suffix := time.Now().UTC().Format("20060102T150405.000000000Z07:00")
		if attempt > 0 {
			suffix += "-" + strconv.Itoa(attempt)
		}
		backupPath := path + ".bak-" + suffix
		if _, err := os.Stat(backupPath); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect store backup path: %w", err)
		}
		input, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("open store for backup: %w", err)
		}
		output, err := os.OpenFile(backupPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			_ = input.Close()
			return "", fmt.Errorf("create store backup: %w", err)
		}
		_, copyErr := io.Copy(output, input)
		if copyErr == nil {
			copyErr = output.Sync()
		}
		closeOutputErr := output.Close()
		closeInputErr := input.Close()
		if copyErr != nil {
			_ = os.Remove(backupPath)
			return "", fmt.Errorf("copy store backup: %w", copyErr)
		}
		if closeOutputErr != nil {
			return "", fmt.Errorf("close store backup: %w", closeOutputErr)
		}
		if closeInputErr != nil {
			return "", fmt.Errorf("close source store after backup: %w", closeInputErr)
		}
		return backupPath, nil
	}
	return "", errors.New("could not choose a unique store backup path")
}
