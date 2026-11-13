package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/localissues"
)

// ErrLocalClaimConflict refuses a rebound, consumed, or competing claim.
var ErrLocalClaimConflict = errors.New("local claim conflicts with durable execution evidence")

// LocalClaim is one durable claim intent. Its exact packet and base are fixed
// before Git creation; State and retained terminal proof are projections only.
type LocalClaim struct {
	CodeHostProvider, CodeHostRepository, TargetBranch                       string
	Key, RepositoryPath, InstallationID, IssueID, ContentDigest, GraphDigest string
	AuthorizationID, RunID, BaseSHA, SpecificationPacket                     string
	Number                                                                   int
	CreatedAt                                                                time.Time
	State                                                                    string
	TerminalStatus                                                           Status
	MergeCommitSHA                                                           string
}

// LocalBacklogItem is bounded retained evidence for a local issue page.
type LocalBacklogItem struct {
	QueueState, QueueReason, RunState, RunReason, FrozenRevision, FrozenContentDigest string
	Issue                                                                             LocalExecutionIssue
	Title, State, Reason, AuthorizationID, SessionID, RunID, PRURL                    string
	PublicationBranch, PublicationCommit, PublicationPolicy                           string
}

// LocalBacklogProjection is the coordinator's last complete queue observation.
// It is readable while stopped and never grants authority by itself.
type LocalBacklogProjection struct {
	Revision, GraphDigest, Error string
	ObservedAt                   time.Time
	Items                        []LocalBacklogItem
}

// migrateLocalClaims creates claim recovery and read-only backlog projections.
func migrateLocalClaims(ctx context.Context, tx *sql.Tx) error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS local_claims(claim_key TEXT PRIMARY KEY,repository_path TEXT NOT NULL,run_id TEXT NOT NULL UNIQUE,state TEXT NOT NULL,claim BLOB NOT NULL,terminal_status TEXT NOT NULL DEFAULT '',merge_sha TEXT NOT NULL DEFAULT '')`,
		`CREATE UNIQUE INDEX IF NOT EXISTS one_reserved_local_claim ON local_claims(repository_path) WHERE state='reserved'`,
		`CREATE TABLE IF NOT EXISTS local_backlog(repository_path TEXT PRIMARY KEY,projection BLOB NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS local_execution_invalidations(request_id TEXT PRIMARY KEY,repository_path TEXT NOT NULL,reason TEXT NOT NULL,observed_at TEXT NOT NULL)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

// LocalClaims reads intents and consumption receipts, including cleaned Runs.
func (s *Store) LocalClaims(ctx context.Context, repo string) ([]LocalClaim, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT claim_key,repository_path,run_id,state,claim,terminal_status,merge_sha FROM local_claims WHERE repository_path=? ORDER BY claim_key`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []LocalClaim{}
	for rows.Next() {
		claim, err := scanLocalClaim(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, claim)
	}
	return result, rows.Err()
}

// scanLocalClaim validates immutable intent identity against indexed columns.
func scanLocalClaim(row localExecutionScanner) (LocalClaim, error) {
	var key, repo, id, state, status, merge string
	var data []byte
	if err := row.Scan(&key, &repo, &id, &state, &data, &status, &merge); err != nil {
		return LocalClaim{}, err
	}
	var claim LocalClaim
	if err := json.Unmarshal(data, &claim); err != nil {
		return claim, err
	}
	if claim.Key != key || claim.RepositoryPath != repo || claim.RunID != id {
		return claim, ErrLocalClaimConflict
	}
	claim.State, claim.TerminalStatus, claim.MergeCommitSHA = state, Status(status), merge
	return claim, nil
}

// ReserveLocalClaim atomically reserves one exact authorized revision before
// workspace creation. New intents require the latest selected authorization;
// already admitted intents remain adoptable after a newer decision.
func (s *Store) ReserveLocalClaim(ctx context.Context, claim LocalClaim) (LocalClaim, error) {
	if claim.Key == "" || claim.RunID == "" || !validLocalRevision(claim.BaseSHA) || claim.SpecificationPacket == "" || len(claim.SpecificationPacket) > 32<<20 {
		return claim, ErrLocalClaimConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return claim, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE local_execution_authorizations SET request_id=request_id WHERE request_id=?`, claim.AuthorizationID); err != nil {
		return claim, err
	}
	auth, err := scanLocalExecutionAuthorization(tx.QueryRowContext(ctx, `SELECT sequence,request_id,preview_id,binding,created_at FROM local_execution_authorizations WHERE request_id=?`, claim.AuthorizationID))
	if err != nil {
		return claim, err
	}
	if auth.Binding.RepositoryPath != claim.RepositoryPath || auth.Binding.InstallationID != claim.InstallationID || auth.Binding.GraphDigest != claim.GraphDigest || !slices.Contains(auth.Binding.SelectedIssueIDs, claim.IssueID) {
		return claim, ErrLocalClaimConflict
	}
	if auth.Binding.CodeHostProvider != claim.CodeHostProvider || auth.Binding.CodeHostRepository != claim.CodeHostRepository || auth.Binding.TargetBranch != claim.TargetBranch {
		return claim, ErrLocalClaimConflict
	}
	expectedKey := localissues.Digest([]byte(strings.Join([]string{claim.InstallationID, claim.RepositoryPath, claim.CodeHostProvider, claim.CodeHostRepository, claim.TargetBranch, claim.IssueID, claim.ContentDigest, claim.GraphDigest}, "\x00")))
	if claim.Key != expectedKey || claim.RunID != "local-"+strings.TrimPrefix(expectedKey, "sha256:") {
		return claim, ErrLocalClaimConflict
	}
	found := false
	for _, issue := range auth.Binding.Issues {
		if issue.ID == claim.IssueID && issue.Number == claim.Number && issue.ContentDigest == claim.ContentDigest && issue.Kind == localissues.KindTask {
			found = true
		}
	}
	if !found {
		return claim, ErrLocalClaimConflict
	}
	existing, err := scanLocalClaim(tx.QueryRowContext(ctx, `SELECT claim_key,repository_path,run_id,state,claim,terminal_status,merge_sha FROM local_claims WHERE claim_key=?`, claim.Key))
	if err == nil {
		if existing.CodeHostProvider != claim.CodeHostProvider || existing.CodeHostRepository != claim.CodeHostRepository || existing.TargetBranch != claim.TargetBranch || existing.RepositoryPath != claim.RepositoryPath || existing.InstallationID != claim.InstallationID || existing.IssueID != claim.IssueID || existing.ContentDigest != claim.ContentDigest || existing.GraphDigest != claim.GraphDigest || existing.RunID != claim.RunID {
			return claim, ErrLocalClaimConflict
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return claim, err
	}
	// The write lock acquired above serializes this observation with authority
	// writers. A queue observation cannot reserve an older decision after a newer
	// decision selecting this issue committed, even if its graph is different.
	var newer string
	if err := tx.QueryRowContext(ctx, `SELECT request_id FROM local_execution_authorizations AS authority
		WHERE repository_path=? AND sequence>? AND EXISTS (
			SELECT 1 FROM json_each(authority.binding, '$.SelectedIssueIDs') WHERE value=?
		) ORDER BY sequence DESC LIMIT 1`, claim.RepositoryPath, auth.Sequence, claim.IssueID).Scan(&newer); err == nil {
		return claim, fmt.Errorf("%w: selected issue has newer execution authorization %s; observe the queue again", ErrLocalClaimConflict, newer)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return claim, err
	}
	var invalid string
	if err := tx.QueryRowContext(ctx, `SELECT reason FROM local_execution_invalidations WHERE request_id=?`, claim.AuthorizationID).Scan(&invalid); err == nil {
		return claim, fmt.Errorf("%w: authorization was invalidated: %s", ErrLocalClaimConflict, invalid)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return claim, err
	}
	var active string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM operational_runs WHERE repository_path=? AND status NOT IN (?,?,?) LIMIT 1`, claim.RepositoryPath, StatusFailed, StatusCancelled, StatusComplete).Scan(&active); err == nil {
		return claim, fmt.Errorf("%w: active Run %s", ErrLocalClaimConflict, active)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return claim, err
	}
	claim.State = "reserved"
	if claim.CreatedAt.IsZero() {
		claim.CreatedAt = time.Now().UTC()
	}
	data, err := json.Marshal(claim)
	if err != nil {
		return claim, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO local_claims(claim_key,repository_path,run_id,state,claim) VALUES(?,?,?,?,?)`, claim.Key, claim.RepositoryPath, claim.RunID, claim.State, data); err != nil {
		return claim, err
	}
	if err := tx.Commit(); err != nil {
		return claim, err
	}
	return claim, nil
}

// SaveLocalClaimRun atomically saves the Run and consumes its exact revision.
// A lost response adopts an advanced existing Run instead of overwriting it.
func (s *Store) SaveLocalClaimRun(ctx context.Context, claim LocalClaim, run Run) (Run, error) {
	if run.ID != claim.RunID || run.RepositoryPath != claim.RepositoryPath || run.IssueNumber != claim.Number || run.BaseCheckpointSHA != claim.BaseSHA || run.SpecificationPacket != claim.SpecificationPacket {
		return Run{}, ErrLocalClaimConflict
	}
	normalized, err := normalizeRun(run)
	if err != nil {
		return Run{}, err
	}
	values, err := runValues(normalized)
	if err != nil {
		return Run{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE local_claims SET state=state WHERE claim_key=?`, claim.Key); err != nil {
		return Run{}, err
	}
	stored, err := scanLocalClaim(tx.QueryRowContext(ctx, `SELECT claim_key,repository_path,run_id,state,claim,terminal_status,merge_sha FROM local_claims WHERE claim_key=?`, claim.Key))
	if err != nil {
		return Run{}, err
	}
	if stored.RunID != run.ID || stored.BaseSHA != claim.BaseSHA || stored.SpecificationPacket != claim.SpecificationPacket {
		return Run{}, ErrLocalClaimConflict
	}
	existing, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM operational_runs WHERE id=?`, run.ID))
	if err != nil {
		return Run{}, err
	}
	if existing != nil {
		return *existing, nil
	}
	if stored.State != "reserved" {
		return Run{}, fmt.Errorf("%w: revision already consumed", ErrLocalClaimConflict)
	}
	if _, err := tx.ExecContext(ctx, saveRunStatement, values...); err != nil {
		return Run{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE local_claims SET state='claimed' WHERE claim_key=?`, claim.Key); err != nil {
		return Run{}, err
	}
	if err := tx.Commit(); err != nil {
		return Run{}, err
	}
	return normalized, nil
}

// SaveLocalBacklog records a bounded non-authoritative coordinator projection.
func (s *Store) SaveLocalBacklog(ctx context.Context, repo string, projection LocalBacklogProjection) error {
	data, err := json.Marshal(projection)
	if err != nil {
		return err
	}
	if len(data) > 32<<20 || len(projection.Items) > localissues.MaxIssues {
		return errors.New("local backlog projection exceeds limits")
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO local_backlog(repository_path,projection) VALUES(?,?) ON CONFLICT(repository_path) DO UPDATE SET projection=excluded.projection`, repo, data)
	return err
}

// LocalBacklog reads retained source/queue evidence without Git or writes.
func (s *Store) LocalBacklog(ctx context.Context, repo string) (LocalBacklogProjection, error) {
	var data []byte
	if err := s.db.QueryRowContext(ctx, `SELECT projection FROM local_backlog WHERE repository_path=?`, repo).Scan(&data); errors.Is(err, sql.ErrNoRows) {
		return LocalBacklogProjection{Items: []LocalBacklogItem{}}, nil
	} else if err != nil {
		return LocalBacklogProjection{}, err
	}
	var result LocalBacklogProjection
	err := json.Unmarshal(data, &result)
	return result, err
}

// InvalidateLocalExecution permanently revokes an unclaimed authority observed
// stale by the coordinator. Restoring old bytes never restores that decision.
func (s *Store) InvalidateLocalExecution(ctx context.Context, repo, id, reason string, at time.Time) error {
	if len(reason) > 4000 {
		reason = TruncateUTF8(reason, 4000)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO local_execution_invalidations(request_id,repository_path,reason,observed_at) SELECT request_id,repository_path,?,? FROM local_execution_authorizations WHERE request_id=? AND repository_path=? ON CONFLICT(request_id) DO NOTHING`, reason, at.UTC().Format(time.RFC3339Nano), id, repo)
	return err
}

// LocalExecutionInvalidations reads durable reapproval requirements.
func (s *Store) LocalExecutionInvalidations(ctx context.Context, repo string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT request_id,reason FROM local_execution_invalidations WHERE repository_path=?`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]string{}
	for rows.Next() {
		var id, reason string
		if err := rows.Scan(&id, &reason); err != nil {
			return nil, err
		}
		result[id] = reason
	}
	return result, rows.Err()
}
