package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Stevie1704/sw-factory/internal/localissues"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// ErrLocalIssueSourceNotFound means the requested issue is absent at a snapshot.
var ErrLocalIssueSourceNotFound = errors.New("local issue source not found at committed snapshot")

// LocalIssueSource couples exact committed content to its durable tracker number.
type LocalIssueSource struct {
	Issue  localissues.Issue
	Number int
}

// LocalIssueProjection is mutable lifecycle evidence, separate from Markdown.
type LocalIssueProjection struct {
	State  string
	Labels []string
}

// migrateLocalIssueProjections creates revision-addressed source and local history.
func migrateLocalIssueProjections(ctx context.Context, tx *sql.Tx) error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS local_issue_snapshots(repository_path TEXT NOT NULL, revision TEXT NOT NULL, graph_digest TEXT NOT NULL, PRIMARY KEY(repository_path,revision))`,
		`CREATE TABLE IF NOT EXISTS local_issue_sources(repository_path TEXT NOT NULL, revision TEXT NOT NULL, issue_id TEXT NOT NULL, source_path TEXT NOT NULL, content_digest TEXT NOT NULL, content BLOB NOT NULL, PRIMARY KEY(repository_path,revision,issue_id))`,
		`CREATE TABLE IF NOT EXISTS local_issue_projections(repository_path TEXT NOT NULL, issue_id TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'open', labels TEXT NOT NULL DEFAULT '[]', PRIMARY KEY(repository_path,issue_id))`,
		`CREATE TABLE IF NOT EXISTS local_issue_comments(sequence INTEGER PRIMARY KEY AUTOINCREMENT, repository_path TEXT NOT NULL, issue_id TEXT NOT NULL, author TEXT NOT NULL, body TEXT NOT NULL, creation_key TEXT NOT NULL, updated_at TEXT NOT NULL, UNIQUE(repository_path,issue_id,creation_key))`,
		`CREATE INDEX IF NOT EXISTS local_issue_comments_by_issue ON local_issue_comments(repository_path,issue_id,sequence)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

// IndexLocalIssueSnapshot validates all content before atomically importing
// mappings and an immutable projection. Import never writes approval or labels.
func (s *Store) IndexLocalIssueSnapshot(ctx context.Context, repositoryPath, revision string, graph localissues.Graph) ([]LocalIssueSource, error) {
	if !filepath.IsAbs(repositoryPath) || filepath.Clean(repositoryPath) != repositoryPath {
		return nil, errors.New("local source index requires a canonical absolute repository path")
	}
	if !validLocalRevision(revision) {
		return nil, errors.New("local snapshot requires a full immutable commit identity")
	}
	files := make([]localissues.File, 0, len(graph.Issues))
	for _, issue := range graph.Issues {
		files = append(files, localissues.File{Path: issue.Path, Data: issue.Bytes})
	}
	validated, err := localissues.Validate(files)
	if err != nil {
		return nil, err
	}
	if validated.Digest != graph.Digest {
		return nil, errors.New("local snapshot graph identity differs from its source")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT graph_digest FROM local_issue_snapshots WHERE repository_path=? AND revision=?`, repositoryPath, revision).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && existing != validated.Digest {
		return nil, errors.New("immutable local snapshot conflicts with indexed evidence")
	}
	for _, issue := range validated.Issues {
		if err := validateLocalIdentityInput(repositoryPath, issue.Correlation); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO local_issue_identities(repository_path,issue_id,correlation) VALUES(?,?,?) ON CONFLICT DO NOTHING`, repositoryPath, issue.ID, issue.Correlation); err != nil {
			return nil, err
		}
		var correlation string
		if err := tx.QueryRowContext(ctx, `SELECT correlation FROM local_issue_identities WHERE repository_path=? AND issue_id=?`, repositoryPath, issue.ID).Scan(&correlation); errors.Is(err, sql.ErrNoRows) {
			return nil, ErrLocalIssueIdentityConflict
		} else if err != nil {
			return nil, err
		}
		if correlation != issue.Correlation {
			return nil, ErrLocalIssueIdentityConflict
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO local_issue_sources(repository_path,revision,issue_id,source_path,content_digest,content) VALUES(?,?,?,?,?,?) ON CONFLICT DO NOTHING`, repositoryPath, revision, issue.ID, issue.Path, issue.ContentDigest, issue.Bytes); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO local_issue_projections(repository_path,issue_id) VALUES(?,?) ON CONFLICT DO NOTHING`, repositoryPath, issue.ID); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO local_issue_snapshots(repository_path,revision,graph_digest) VALUES(?,?,?) ON CONFLICT DO NOTHING`, repositoryPath, revision, validated.Digest); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.LocalIssueSources(ctx, repositoryPath, revision)
}

// LocalIssueSources reads a previously indexed immutable revision without Git or writes.
func (s *Store) LocalIssueSources(ctx context.Context, repositoryPath, revision string) ([]LocalIssueSource, error) {
	var expectedDigest string
	if err := s.db.QueryRowContext(ctx, `SELECT graph_digest FROM local_issue_snapshots WHERE repository_path=? AND revision=?`, repositoryPath, revision).Scan(&expectedDigest); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrLocalIssueSourceNotFound
	} else if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT source_path,content,number FROM local_issue_sources JOIN local_issue_identities USING(repository_path,issue_id) WHERE repository_path=? AND revision=? ORDER BY issue_id`, repositoryPath, revision)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []LocalIssueSource{}
	for rows.Next() {
		var name string
		var data []byte
		var number int
		if err := rows.Scan(&name, &data, &number); err != nil {
			return nil, err
		}
		issue, err := localissues.Parse(name, data)
		if err != nil {
			return nil, err
		}
		result = append(result, LocalIssueSource{Issue: issue, Number: number})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	files := make([]localissues.File, 0, len(result))
	for _, source := range result {
		files = append(files, localissues.File{Path: source.Issue.Path, Data: source.Issue.Bytes})
	}
	graph, err := localissues.Validate(files)
	if err != nil {
		return nil, err
	}
	if graph.Digest != expectedDigest {
		return nil, errors.New("indexed local source no longer matches immutable snapshot evidence")
	}
	return result, nil
}

// LocalIssueProjection reads local status; accepted content remains in Git.
func (s *Store) LocalIssueProjection(ctx context.Context, repositoryPath, id string) (LocalIssueProjection, error) {
	var result LocalIssueProjection
	var labels string
	err := s.db.QueryRowContext(ctx, `SELECT state,labels FROM local_issue_projections WHERE repository_path=? AND issue_id=?`, repositoryPath, id).Scan(&result.State, &labels)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrLocalIssueSourceNotFound
	}
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal([]byte(labels), &result.Labels); err != nil {
		return result, err
	}
	return result, nil
}

// ReplaceLocalIssueLabels persists the coordinator's lifecycle projection only.
// An agent-ready label is never sufficient execution authority for local issues.
func (s *Store) ReplaceLocalIssueLabels(ctx context.Context, repositoryPath, id string, labels []string) error {
	if len(labels) > 64 {
		return errors.New("local label limit exceeded")
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 128 || strings.ContainsAny(label, "\x00\r\n") {
			return errors.New("invalid local label")
		}
	}
	labels = slices.Clone(labels)
	slices.Sort(labels)
	labels = slices.Compact(labels)
	if labels == nil {
		labels = []string{}
	}
	data, err := json.Marshal(labels)
	if err != nil {
		return err
	}
	state := "open"
	if slices.Contains(labels, tracker.LabelAgentComplete) {
		state = "closed"
	}
	result, err := s.db.ExecContext(ctx, `UPDATE local_issue_projections SET state=?,labels=? WHERE repository_path=? AND issue_id=?`, state, string(data), repositoryPath, id)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrLocalIssueSourceNotFound
	}
	return nil
}

// CreateLocalIssueComment records a coordinator-authored status or question.
// Repeating the same creation content adopts its original immutable identity.
func (s *Store) CreateLocalIssueComment(ctx context.Context, repositoryPath, id, author, body string, at time.Time) (tracker.Comment, error) {
	if author == "" || len(author) > 128 || !utf8.ValidString(author) || !utf8.ValidString(body) || strings.ContainsAny(author, "\x00\r\n") || len(body) == 0 || len(body) > 128<<10 || strings.ContainsRune(body, 0) {
		return tracker.Comment{}, errors.New("invalid local comment")
	}
	if _, err := s.LocalIssueIdentity(ctx, repositoryPath, id); err != nil {
		return tracker.Comment{}, err
	}
	key := localissues.Digest([]byte(author + "\x00" + body))
	_, err := s.db.ExecContext(ctx, `INSERT INTO local_issue_comments(repository_path,issue_id,author,body,creation_key,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT DO NOTHING`, repositoryPath, id, author, body, key, at.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return tracker.Comment{}, err
	}
	return scanLocalComment(s.db.QueryRowContext(ctx, `SELECT sequence,author,body,updated_at FROM local_issue_comments WHERE repository_path=? AND issue_id=? AND creation_key=?`, repositoryPath, id, key))
}

// LocalIssueComments returns durable status/question/answer history in creation order.
func (s *Store) LocalIssueComments(ctx context.Context, repositoryPath, id string) ([]tracker.Comment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sequence,author,body,updated_at FROM local_issue_comments WHERE repository_path=? AND issue_id=? ORDER BY sequence`, repositoryPath, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []tracker.Comment{}
	for rows.Next() {
		var sequence int64
		var comment tracker.Comment
		var stamp string
		if err := rows.Scan(&sequence, &comment.Author, &comment.Body, &stamp); err != nil {
			return nil, err
		}
		comment.ID = localCommentID(sequence)
		comment.UpdatedAt, err = time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			return nil, err
		}
		result = append(result, comment)
	}
	return result, rows.Err()
}

// EditLocalIssueComment updates only the named local operational record.
func (s *Store) EditLocalIssueComment(ctx context.Context, repositoryPath, commentID, body string, at time.Time) error {
	sequence, err := strconv.ParseInt(strings.TrimPrefix(commentID, "local-comment-"), 10, 64)
	if err != nil || sequence < 1 || !strings.HasPrefix(commentID, "local-comment-") || !utf8.ValidString(body) || len(body) == 0 || len(body) > 128<<10 || strings.ContainsRune(body, 0) {
		return errors.New("invalid local comment update")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE local_issue_comments SET body=?,updated_at=CASE WHEN body=? THEN updated_at ELSE ? END WHERE sequence=? AND repository_path=?`, body, body, at.UTC().Format(time.RFC3339Nano), sequence, repositoryPath)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return errors.New("local comment not found")
	}
	return nil
}

// scanLocalComment decodes a durable local comment.
func scanLocalComment(row *sql.Row) (tracker.Comment, error) {
	var sequence int64
	var result tracker.Comment
	var stamp string
	if err := row.Scan(&sequence, &result.Author, &result.Body, &stamp); err != nil {
		return result, err
	}
	result.ID = localCommentID(sequence)
	var err error
	result.UpdatedAt, err = time.Parse(time.RFC3339Nano, stamp)
	return result, err
}

// localCommentID keeps SQLite identities sortable without sharing host IDs.
func localCommentID(sequence int64) string { return fmt.Sprintf("local-comment-%020d", sequence) }

// validLocalRevision accepts only a full SHA, never a mutable ref or option.
func validLocalRevision(revision string) bool {
	if len(revision) != 40 && len(revision) != 64 {
		return false
	}
	for _, c := range revision {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
