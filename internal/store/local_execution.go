package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Stevie1704/sw-factory/internal/localissues"
)

// ErrLocalExecutionNotFound means no explicit preview or authorization exists.
var ErrLocalExecutionNotFound = errors.New("local execution evidence not found")

// ErrLocalIssueNotRegistered means an execution binding names an issue that
// this store never indexed, for example after a reset or a stale preview.
var ErrLocalIssueNotRegistered = errors.New("local issue is not registered in this store; observe the backlog and preview again")

// ErrLocalExecutionConflict refuses stale or rebound execution authority.
var ErrLocalExecutionConflict = errors.New("local execution preview or authorization conflict")

// LocalExecutionIssue freezes one issue's exact source and durable mapping.
type LocalExecutionIssue struct {
	ID            string
	Number        int
	Path          string
	ContentDigest string
	Bytes         []byte
	Kind          localissues.Kind
	Parent        string
	Dependencies  []string
}

// LocalExecutionBinding is host-owned authority over exact content, graph,
// publication scope and selected executable issues; it grants no merge rights.
type LocalExecutionBinding struct {
	// SourceKind is publication or target, set only by the coordinator.
	SourceKind                string
	RepositoryPath            string
	InstallationID            string
	OperatorName              string
	CodeHostProvider          string
	CodeHostRepository        string
	TargetBranch              string
	PublicationPolicy         string
	SourceRevision            string
	GraphDigest               string
	PayloadSHA256             string
	RelationshipMappingSHA256 string
	Payload                   []byte
	RelationshipMapping       []byte
	Issues                    []LocalExecutionIssue
	SelectedIssueIDs          []string
	SessionID                 string
	ApprovalID                string
}

// LocalExecutionPreview is an immutable coordinator-rendered exact preview.
// Revision is a monotonic repository-independent store sequence for stale tabs.
type LocalExecutionPreview struct {
	ID        string
	Revision  int64
	Binding   LocalExecutionBinding
	CreatedAt time.Time
}

// LocalExecutionAuthorization is a separate explicit operator decision.
// Claim reservation/consumption is deliberately separate from this append-only row.
type LocalExecutionAuthorization struct {
	Sequence  int64
	RequestID string
	PreviewID string
	Binding   LocalExecutionBinding
	CreatedAt time.Time
}

// migrateLocalExecution creates exact previews and explicit authority, and
// extends the existing inbox without changing old session-command semantics.
func migrateLocalExecution(ctx context.Context, tx *sql.Tx) error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS local_execution_previews(revision INTEGER PRIMARY KEY AUTOINCREMENT,preview_id TEXT NOT NULL UNIQUE,repository_path TEXT NOT NULL,binding BLOB NOT NULL,created_at TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS local_execution_previews_by_repository ON local_execution_previews(repository_path,revision)`,
		`CREATE TABLE IF NOT EXISTS local_execution_authorizations(sequence INTEGER PRIMARY KEY AUTOINCREMENT,request_id TEXT NOT NULL UNIQUE,preview_id TEXT NOT NULL,repository_path TEXT NOT NULL,binding BLOB NOT NULL,created_at TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS local_execution_authorizations_by_repository ON local_execution_authorizations(repository_path,created_at,request_id)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	for _, field := range []string{"scope", "subject_id"} {
		if err := addCommandScopeColumn(ctx, tx, field); err != nil {
			return err
		}
	}
	return nil
}

// RecordLocalExecutionPreview persists an exact coordinator preview once.
// It never creates authorization, modifies source, or changes a session.
func (s *Store) RecordLocalExecutionPreview(ctx context.Context, preview LocalExecutionPreview) (LocalExecutionPreview, error) {
	if err := validateIdeationIdentifier("execution preview ID", preview.ID); err != nil {
		return preview, err
	}
	data, err := encodeLocalExecutionBinding(preview.Binding)
	if err != nil {
		return preview, err
	}
	if err := validateLocalExecutionMappings(ctx, s.db, preview.Binding); err != nil {
		return preview, err
	}
	if preview.CreatedAt.IsZero() {
		preview.CreatedAt = time.Now().UTC()
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO local_execution_previews(preview_id,repository_path,binding,created_at) VALUES(?,?,?,?) ON CONFLICT(preview_id) DO NOTHING`, preview.ID, preview.Binding.RepositoryPath, data, formatIdeationTime(preview.CreatedAt))
	if err != nil {
		return preview, err
	}
	existing, err := s.LocalExecutionPreview(ctx, preview.Binding.RepositoryPath, preview.ID)
	if err != nil {
		return preview, err
	}
	actual, _ := encodeLocalExecutionBinding(existing.Binding)
	if !bytes.Equal(data, actual) {
		return preview, ErrLocalExecutionConflict
	}
	return existing, nil
}

// LocalExecutionPreview reads exact preview evidence with no state changes.
func (s *Store) LocalExecutionPreview(ctx context.Context, repo, id string) (LocalExecutionPreview, error) {
	return scanLocalExecutionPreview(s.db.QueryRowContext(ctx, `SELECT preview_id,revision,binding,created_at FROM local_execution_previews WHERE repository_path=? AND preview_id=?`, repo, id))
}

// LatestLocalExecutionPreview returns the most recently rendered repository preview.
func (s *Store) LatestLocalExecutionPreview(ctx context.Context, repo string) (LocalExecutionPreview, error) {
	return scanLocalExecutionPreview(s.db.QueryRowContext(ctx, `SELECT preview_id,revision,binding,created_at FROM local_execution_previews WHERE repository_path=? ORDER BY revision DESC LIMIT 1`, repo))
}

// AuthorizeLocalExecution records one explicit exact decision idempotently.
// If completion is supplied, authority and final inbox outcome commit atomically.
// The caller is the coordinator and must revalidate current host/source policy.
func (s *Store) AuthorizeLocalExecution(ctx context.Context, auth LocalExecutionAuthorization, completion *IdeationCommandResult) (LocalExecutionAuthorization, error) {
	if err := validateIdeationIdentifier("execution authorization request ID", auth.RequestID); err != nil {
		return auth, err
	}
	data, err := encodeLocalExecutionBinding(auth.Binding)
	if err != nil {
		return auth, err
	}
	if auth.PreviewID == "" {
		return auth, errors.New("execution authorization needs exact preview evidence")
	}
	if auth.CreatedAt.IsZero() {
		auth.CreatedAt = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return auth, err
	}
	defer tx.Rollback()
	if err := validateLocalExecutionMappings(ctx, tx, auth.Binding); err != nil {
		return auth, err
	}
	var previewData []byte
	var previewRevision int64
	if err := tx.QueryRowContext(ctx, `SELECT binding,revision FROM local_execution_previews WHERE preview_id=? AND repository_path=?`, auth.PreviewID, auth.Binding.RepositoryPath).Scan(&previewData, &previewRevision); err != nil {
		return auth, ErrLocalExecutionNotFound
	}
	if !bytes.Equal(data, previewData) {
		return auth, ErrLocalExecutionConflict
	}
	insertResult, err := tx.ExecContext(ctx, `INSERT INTO local_execution_authorizations(request_id,preview_id,repository_path,binding,created_at) VALUES(?,?,?,?,?) ON CONFLICT(request_id) DO NOTHING`, auth.RequestID, auth.PreviewID, auth.Binding.RepositoryPath, data, formatIdeationTime(auth.CreatedAt))
	if err != nil {
		return auth, err
	}
	inserted, err := insertResult.RowsAffected()
	if err != nil {
		return auth, err
	}
	var storedData []byte
	var storedPreview, created string
	if err := tx.QueryRowContext(ctx, `SELECT preview_id,binding,created_at FROM local_execution_authorizations WHERE request_id=?`, auth.RequestID).Scan(&storedPreview, &storedData, &created); err != nil {
		return auth, err
	}
	if storedPreview != auth.PreviewID || !bytes.Equal(storedData, data) {
		return auth, ErrLocalExecutionConflict
	}
	if completion != nil {
		if completion.RequestID != auth.RequestID || completion.Outcome != IdeationCommandApplied || completion.SessionRevision != previewRevision {
			return auth, errors.New("authorization completion must apply its exact preview request")
		}
		if err := validateIdeationCommandResult(*completion); err != nil {
			return auth, err
		}
		command, err := scanIdeationCommand(tx.QueryRowContext(ctx, `SELECT `+ideationCommandColumns+` FROM ideation_commands WHERE request_id=?`, auth.RequestID), auth.RequestID)
		if err != nil {
			return auth, err
		}
		if command.Scope != "backlog" || command.SubjectID != "repository" || command.Kind != "authorize_execution" || command.ExpectedRevision != previewRevision {
			return auth, ErrLocalExecutionConflict
		}
		var payload struct {
			PreviewID                 string   `json:"preview_id"`
			PayloadSHA256             string   `json:"payload_sha256"`
			RelationshipMappingSHA256 string   `json:"relationship_mapping_sha256"`
			GraphDigest               string   `json:"graph_digest"`
			SelectedIssueIDs          []string `json:"selected_issue_ids"`
		}
		if err := json.Unmarshal(command.Payload, &payload); err != nil {
			return auth, err
		}
		slices.Sort(payload.SelectedIssueIDs)
		if payload.PreviewID != auth.PreviewID || payload.PayloadSHA256 != auth.Binding.PayloadSHA256 || payload.RelationshipMappingSHA256 != auth.Binding.RelationshipMappingSHA256 || payload.GraphDigest != auth.Binding.GraphDigest || !slices.Equal(payload.SelectedIssueIDs, auth.Binding.SelectedIssueIDs) {
			return auth, ErrLocalExecutionConflict
		}
		if command.State == IdeationCommandCompleted {
			if inserted != 0 || command.Outcome != completion.Outcome || command.Detail != completion.Detail || command.SessionRevision != completion.SessionRevision || !bytes.Equal(command.Result, completion.Result) {
				return auth, ErrLocalExecutionConflict
			}
		} else {
			if command.State != IdeationCommandApplying {
				return auth, ErrLocalExecutionConflict
			}
			updated, err := tx.ExecContext(ctx, completeIdeationCommandStatement, completeIdeationCommandArguments(*completion)...)
			if err != nil {
				return auth, err
			}
			changed, err := updated.RowsAffected()
			if err != nil {
				return auth, err
			}
			if changed != 1 {
				return auth, ErrLocalExecutionConflict
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return auth, err
	}
	return s.LocalExecutionAuthorization(ctx, auth.RequestID)
}

// LocalExecutionAuthorization reads the explicit decision for a request ID.
func (s *Store) LocalExecutionAuthorization(ctx context.Context, id string) (LocalExecutionAuthorization, error) {
	return scanLocalExecutionAuthorization(s.db.QueryRowContext(ctx, `SELECT sequence,request_id,preview_id,binding,created_at FROM local_execution_authorizations WHERE request_id=?`, id))
}

// LocalExecutionAuthorizations lists durable decisions in deterministic order.
func (s *Store) LocalExecutionAuthorizations(ctx context.Context, repo string) ([]LocalExecutionAuthorization, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sequence,request_id,preview_id,binding,created_at FROM local_execution_authorizations WHERE repository_path=? ORDER BY sequence`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []LocalExecutionAuthorization{}
	for rows.Next() {
		auth, err := scanLocalExecutionAuthorization(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, auth)
	}
	return result, rows.Err()
}

// localExecutionScanner is shared by SQLite rows and cursors.
type localExecutionScanner interface{ Scan(...any) error }

// scanLocalExecutionPreview validates stored evidence before exposing it.
func scanLocalExecutionPreview(row localExecutionScanner) (LocalExecutionPreview, error) {
	var result LocalExecutionPreview
	var data []byte
	var created string
	if err := row.Scan(&result.ID, &result.Revision, &data, &created); errors.Is(err, sql.ErrNoRows) {
		return result, ErrLocalExecutionNotFound
	} else if err != nil {
		return result, err
	}
	if err := json.Unmarshal(data, &result.Binding); err != nil {
		return result, err
	}
	if _, err := encodeLocalExecutionBinding(result.Binding); err != nil {
		return result, err
	}
	var err error
	result.CreatedAt, err = parseIdeationTime(created)
	return result, err
}

// scanLocalExecutionAuthorization validates stored authority before use.
func scanLocalExecutionAuthorization(row localExecutionScanner) (LocalExecutionAuthorization, error) {
	var result LocalExecutionAuthorization
	var data []byte
	var created string
	if err := row.Scan(&result.Sequence, &result.RequestID, &result.PreviewID, &data, &created); errors.Is(err, sql.ErrNoRows) {
		return result, ErrLocalExecutionNotFound
	} else if err != nil {
		return result, err
	}
	if err := json.Unmarshal(data, &result.Binding); err != nil {
		return result, err
	}
	if _, err := encodeLocalExecutionBinding(result.Binding); err != nil {
		return result, err
	}
	var err error
	result.CreatedAt, err = parseIdeationTime(created)
	return result, err
}

// encodeLocalExecutionBinding rejects incomplete, forged or inconsistent bindings.
func encodeLocalExecutionBinding(binding LocalExecutionBinding) ([]byte, error) {
	if binding.SourceKind != "publication" && binding.SourceKind != "target" {
		return nil, errors.New("execution binding needs explicit source kind")
	}
	if !filepath.IsAbs(binding.RepositoryPath) || filepath.Clean(binding.RepositoryPath) != binding.RepositoryPath || !strings.HasPrefix(binding.InstallationID, "install-") || !localissues.ValidID("local-"+strings.TrimPrefix(binding.InstallationID, "install-")) || !utf8.ValidString(binding.OperatorName) || len(binding.OperatorName) > 128 || strings.IndexFunc(binding.OperatorName, unicode.IsControl) >= 0 || binding.OperatorName == "" || binding.CodeHostRepository == "" || binding.TargetBranch == "" {
		return nil, errors.New("execution binding requires explicit installation/repository/operator/code-host provenance")
	}
	if binding.SourceKind == "publication" {
		if err := validateIdeationIdentifier("publication source session", binding.SessionID); err != nil {
			return nil, err
		}
		if err := validateIdeationIdentifier("publication source approval", binding.ApprovalID); err != nil {
			return nil, err
		}
		if binding.PublicationPolicy == "target_visible" {
			return nil, errors.New("publication authority cannot skip its publication ledger")
		}
	} else if binding.SessionID != "" || binding.ApprovalID != "" || binding.PublicationPolicy != "target_visible" {
		return nil, errors.New("committed-target authority must use fresh independent target preview")
	}
	if binding.CodeHostProvider != "github" && binding.CodeHostProvider != "azure_devops" {
		return nil, errors.New("unsupported execution code host")
	}
	if binding.PublicationPolicy != "local_commit" && binding.PublicationPolicy != "push" && binding.PublicationPolicy != "pull_request" && binding.PublicationPolicy != "target_visible" {
		return nil, errors.New("unsupported execution publication policy")
	}
	if !validLocalRevision(binding.SourceRevision) {
		return nil, errors.New("execution binding requires immutable source revision")
	}
	if localissues.Digest(binding.Payload) != binding.PayloadSHA256 || localissues.Digest(binding.RelationshipMapping) != binding.RelationshipMappingSHA256 {
		return nil, errors.New("execution preview digests differ from displayed bytes")
	}
	if len(binding.Payload) > MaxIdeationCommandResultBytes || len(binding.RelationshipMapping) > MaxIdeationCommandResultBytes || !json.Valid(binding.Payload) || !json.Valid(binding.RelationshipMapping) {
		return nil, errors.New("execution preview payload is invalid or oversized")
	}
	files := make([]localissues.File, 0, len(binding.Issues))
	byID := map[string]LocalExecutionIssue{}
	for _, issue := range binding.Issues {
		source, err := localissues.Parse(issue.Path, issue.Bytes)
		if err != nil {
			return nil, err
		}
		if issue.Number < 1 || source.ID != issue.ID || source.ContentDigest != issue.ContentDigest || source.Kind != issue.Kind || source.Parent != issue.Parent || !slices.Equal(source.Dependencies, issue.Dependencies) {
			return nil, errors.New("execution issue evidence differs from exact source")
		}
		files = append(files, localissues.File{Path: issue.Path, Data: issue.Bytes})
		byID[issue.ID] = issue
	}
	graph, err := localissues.Validate(files)
	if err != nil {
		return nil, err
	}
	if graph.Digest != binding.GraphDigest {
		return nil, errors.New("execution graph digest differs from exact source")
	}
	selected := map[string]bool{}
	if len(binding.SelectedIssueIDs) == 0 {
		return nil, errors.New("execution authorization needs an explicit executable subset")
	}
	for _, id := range binding.SelectedIssueIDs {
		issue, ok := byID[id]
		if !ok || issue.Kind != localissues.KindTask || selected[id] {
			return nil, errors.New("execution selection must contain unique executable tasks from the displayed graph")
		}
		selected[id] = true
	}
	data, err := json.Marshal(binding)
	if err != nil {
		return nil, err
	}
	if len(data) > 32<<20 {
		return nil, fmt.Errorf("execution evidence exceeds its byte limit")
	}
	return data, nil
}

// addCommandScopeColumn tolerates schema fixtures whose metadata was rolled back.
func addCommandScopeColumn(ctx context.Context, tx *sql.Tx, field string) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info(ideation_commands)")
	if err != nil {
		return err
	}
	exists := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notnull, &defaultValue, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == field {
			exists = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if exists {
		return nil
	}
	statement := ""
	switch field {
	case "scope":
		statement = "ALTER TABLE ideation_commands ADD COLUMN scope TEXT NOT NULL DEFAULT 'session'"
	case "subject_id":
		statement = "ALTER TABLE ideation_commands ADD COLUMN subject_id TEXT NOT NULL DEFAULT ''"
	default:
		return errors.New("unsupported command scope column")
	}
	_, err = tx.ExecContext(ctx, statement)
	return err
}

// localIdentityQuery reads mappings without allocation at preview/approval time.
type localIdentityQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// validateLocalExecutionMappings rejects a source that impersonates another number.
func validateLocalExecutionMappings(ctx context.Context, query localIdentityQuery, binding LocalExecutionBinding) error {
	for _, issue := range binding.Issues {
		var number int
		var correlation string
		if err := query.QueryRowContext(ctx, `SELECT number,correlation FROM local_issue_identities WHERE repository_path=? AND issue_id=?`, binding.RepositoryPath, issue.ID).Scan(&number, &correlation); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrLocalIssueNotRegistered, issue.ID)
		} else if err != nil {
			return err
		}
		source, err := localissues.Parse(issue.Path, issue.Bytes)
		if err != nil {
			return err
		}
		if number != issue.Number || correlation != source.Correlation {
			return ErrLocalIssueIdentityConflict
		}
	}
	return nil
}
