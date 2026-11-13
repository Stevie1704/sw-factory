package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Stevie1704/sw-factory/internal/localissues"
)

// ErrLocalIssueIdentityConflict rejects aliases between IDs and correlations.
var ErrLocalIssueIdentityConflict = errors.New("local issue identity or publication correlation conflicts with durable mapping")

// ErrLocalIssueIdentityNotFound distinguishes an unallocated identity from a failed read.
var ErrLocalIssueIdentityNotFound = errors.New("local issue identity not found")

// LocalIssueIdentity is the durable, collision-checked integer tracker mapping.
// Allocation and import do not confer approval, authorization, or eligibility.
type LocalIssueIdentity struct {
	RepositoryPath string
	ID             string
	Number         int
	Correlation    string
}

const localIssueIdentitySchema = `CREATE TABLE IF NOT EXISTS local_issue_identities (
    number INTEGER PRIMARY KEY AUTOINCREMENT CHECK(number > 0 AND number <= 2147483647),
    repository_path TEXT NOT NULL,
    issue_id TEXT NOT NULL,
    correlation TEXT NOT NULL,
    UNIQUE(repository_path, issue_id),
    UNIQUE(repository_path, correlation)
)`

// AllocateLocalIssueIdentity reserves an opaque identity and tracker number
// before preview. The coordinator calls this explicit write once per stable
// publication correlation; a retry returns the original reservation.
func (s *Store) AllocateLocalIssueIdentity(ctx context.Context, repositoryPath, correlation string) (LocalIssueIdentity, error) {
	if err := validateLocalIdentityInput(repositoryPath, correlation); err != nil {
		return LocalIssueIdentity{}, err
	}
	existing, err := s.LocalIssueIdentityByCorrelation(ctx, repositoryPath, correlation)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, ErrLocalIssueIdentityNotFound) {
		return LocalIssueIdentity{}, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return LocalIssueIdentity{}, err
		}
		result, err := s.RegisterLocalIssueIdentity(ctx, repositoryPath, "local-"+hex.EncodeToString(random[:]), correlation)
		if err == nil {
			return result, nil
		}
		if !errors.Is(err, ErrLocalIssueIdentityConflict) {
			return LocalIssueIdentity{}, err
		}
		existing, readErr := s.LocalIssueIdentityByCorrelation(ctx, repositoryPath, correlation)
		if readErr == nil {
			return existing, nil
		}
		if !errors.Is(readErr, ErrLocalIssueIdentityNotFound) {
			return LocalIssueIdentity{}, readErr
		}
	}
	return LocalIssueIdentity{}, ErrLocalIssueIdentityConflict
}

// RegisterLocalIssueIdentity imports content identity only, assigning a durable
// sequential number without hashing. Unique constraints reject all collisions;
// an existing identity can never be rebound to another correlation.
func (s *Store) RegisterLocalIssueIdentity(ctx context.Context, repositoryPath, id, correlation string) (LocalIssueIdentity, error) {
	if err := validateLocalIdentityInput(repositoryPath, correlation); err != nil {
		return LocalIssueIdentity{}, err
	}
	if !localissues.ValidID(id) {
		return LocalIssueIdentity{}, errors.New("invalid local issue identity")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO local_issue_identities(repository_path,issue_id,correlation) VALUES(?,?,?) ON CONFLICT DO NOTHING`, repositoryPath, id, correlation)
	if err != nil {
		return LocalIssueIdentity{}, fmt.Errorf("reserve local issue identity: %w", err)
	}
	existing, err := s.LocalIssueIdentity(ctx, repositoryPath, id)
	if errors.Is(err, ErrLocalIssueIdentityNotFound) {
		return LocalIssueIdentity{}, ErrLocalIssueIdentityConflict
	}
	if err != nil {
		return LocalIssueIdentity{}, err
	}
	if existing.Correlation != correlation {
		return LocalIssueIdentity{}, ErrLocalIssueIdentityConflict
	}
	return existing, nil
}

// LocalIssueIdentity reads an existing mapping without allocation or approval.
func (s *Store) LocalIssueIdentity(ctx context.Context, repositoryPath, id string) (LocalIssueIdentity, error) {
	return scanLocalIdentity(s.db.QueryRowContext(ctx, `SELECT repository_path,issue_id,number,correlation FROM local_issue_identities WHERE repository_path=? AND issue_id=?`, repositoryPath, id))
}

// LocalIssueIdentityByNumber resolves a persisted tracker number without hashing.
func (s *Store) LocalIssueIdentityByNumber(ctx context.Context, repositoryPath string, number int) (LocalIssueIdentity, error) {
	return scanLocalIdentity(s.db.QueryRowContext(ctx, `SELECT repository_path,issue_id,number,correlation FROM local_issue_identities WHERE repository_path=? AND number=?`, repositoryPath, number))
}

// LocalIssueIdentityByCorrelation reads the durable preview reservation.
func (s *Store) LocalIssueIdentityByCorrelation(ctx context.Context, repositoryPath, correlation string) (LocalIssueIdentity, error) {
	return scanLocalIdentity(s.db.QueryRowContext(ctx, `SELECT repository_path,issue_id,number,correlation FROM local_issue_identities WHERE repository_path=? AND correlation=?`, repositoryPath, correlation))
}

// scanLocalIdentity maps a missing row to the public allocation diagnostic.
func scanLocalIdentity(row *sql.Row) (LocalIssueIdentity, error) {
	var result LocalIssueIdentity
	err := row.Scan(&result.RepositoryPath, &result.ID, &result.Number, &result.Correlation)
	if errors.Is(err, sql.ErrNoRows) {
		return LocalIssueIdentity{}, ErrLocalIssueIdentityNotFound
	}
	if err != nil {
		return LocalIssueIdentity{}, fmt.Errorf("read local issue identity: %w", err)
	}
	return result, nil
}

// validateLocalIdentityInput prevents ambiguous repository and correlation keys.
func validateLocalIdentityInput(repositoryPath, correlation string) error {
	if !filepath.IsAbs(repositoryPath) || filepath.Clean(repositoryPath) != repositoryPath {
		return errors.New("local issue mapping requires a canonical absolute repository path")
	}
	if !utf8.ValidString(correlation) || correlation == "" || len(correlation) > 512 || strings.TrimSpace(correlation) != correlation || strings.IndexFunc(correlation, unicode.IsControl) >= 0 {
		return errors.New("invalid local issue publication correlation")
	}
	return nil
}
