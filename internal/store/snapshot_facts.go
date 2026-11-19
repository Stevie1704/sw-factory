package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// Watermark is the extent of one kind of record in a store: how many exist
// and the highest identity or time among them. A record beyond it is one
// that a snapshot does not know.
type Watermark struct {
	// Count is the number of records.
	Count int `json:"count"`
	// Latest is the highest creation time or number, as stored text; it is
	// empty when no record exists.
	Latest string `json:"latest"`
}

// Watermarks are the highest identities that a store knows, as a
// checkpoint manifest records them and a restore compares them.
type Watermarks struct {
	// Runs covers the operational runs, by creation time.
	Runs Watermark `json:"runs"`
	// Invocations covers the run harness invocations, by creation time.
	Invocations Watermark `json:"invocations"`
	// SessionInvocations covers the session-role invocations, by creation
	// time.
	SessionInvocations Watermark `json:"session_invocations"`
	// PublicationLedger covers the publication ledger operations, by update
	// time.
	PublicationLedger Watermark `json:"publication_ledger"`
	// LocalIssues covers the local issue identities, by number.
	LocalIssues Watermark `json:"local_issues"`
}

// KnownRun names the external objects that one stored run identifies: its
// issue, run branch, pull request, and status comment. A zero or empty
// field means the run does not identify that object.
type KnownRun struct {
	RunID             string
	IssueNumber       int
	Branch            string
	PullRequestNumber int
	StatusCommentID   string
}

// watermarkQueries reads each watermark with one COUNT and one MAX.
var watermarkQueries = []struct {
	query  string
	target func(*Watermarks) *Watermark
}{
	{"SELECT COUNT(*), COALESCE(MAX(created_at), '') FROM operational_runs", func(w *Watermarks) *Watermark { return &w.Runs }},
	{"SELECT COUNT(*), COALESCE(MAX(created_at), '') FROM invocations", func(w *Watermarks) *Watermark { return &w.Invocations }},
	{"SELECT COUNT(*), COALESCE(MAX(created_at), '') FROM ideation_invocations", func(w *Watermarks) *Watermark { return &w.SessionInvocations }},
	{"SELECT COUNT(*), COALESCE(MAX(updated_at), '') FROM ideation_publication_operations", func(w *Watermarks) *Watermark { return &w.PublicationLedger }},
	{"SELECT COUNT(*), COALESCE(CAST(MAX(number) AS TEXT), '') FROM local_issue_identities", func(w *Watermarks) *Watermark { return &w.LocalIssues }},
}

// ReadWatermarks reads the watermarks of a store through any Querier, so a
// restore can compare a live store with the watermarks of a snapshot.
func ReadWatermarks(ctx context.Context, view Querier) (Watermarks, error) {
	var watermarks Watermarks
	for _, item := range watermarkQueries {
		target := item.target(&watermarks)
		if err := view.QueryRowContext(ctx, item.query).Scan(&target.Count, &target.Latest); err != nil {
			return Watermarks{}, fmt.Errorf("read store watermarks: %w", err)
		}
	}
	return watermarks, nil
}

// ReadKnownRuns lists, ordered by run ID, the external objects that each
// stored run identifies.
func ReadKnownRuns(ctx context.Context, view Querier) ([]KnownRun, error) {
	rows, err := view.QueryContext(ctx, "SELECT id, issue_number, branch, pull_request_number, status_comment_id FROM operational_runs ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("read known runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var runs []KnownRun
	for rows.Next() {
		var run KnownRun
		if err := rows.Scan(&run.RunID, &run.IssueNumber, &run.Branch, &run.PullRequestNumber, &run.StatusCommentID); err != nil {
			return nil, fmt.Errorf("read known runs: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read known runs: %w", err)
	}
	return runs, nil
}

// KnownPublication names the external objects that one publication ledger
// operation records: the tracker issue of a created item, or the branch and
// the pull request of an atomic Git batch. A zero or empty field means the
// operation does not identify that object. A publication can record them
// before any run exists.
type KnownPublication struct {
	SessionID         string
	ApprovalID        string
	OperationID       string
	IssueNumber       int
	Branch            string
	PullRequestNumber int
}

// gitBatchEvidence is the part of a Git batch ledger detail that names
// external objects. The publication package writes the detail.
type gitBatchEvidence struct {
	Evidence struct {
		Branch            string `json:"branch"`
		PullRequestNumber int    `json:"pull_request_number"`
	} `json:"evidence"`
}

// ReadKnownPublications lists, in ledger order, each publication ledger
// operation that identifies an external object. A recorded identity that it
// cannot interpret is an error, never an omission, so a checkpoint cannot
// miss an object.
func ReadKnownPublications(ctx context.Context, view Querier) ([]KnownPublication, error) {
	rows, err := view.QueryContext(ctx, "SELECT session_id, approval_id, id, kind, external_id, detail FROM ideation_publication_operations WHERE kind IN (?, ?) ORDER BY session_id, approval_id, sequence",
		PublicationOperationCreateItem, PublicationOperationGitBatch)
	if err != nil {
		return nil, fmt.Errorf("read known publications: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var publications []KnownPublication
	for rows.Next() {
		var publication KnownPublication
		var kind PublicationOperationKind
		var externalID, detail string
		if err := rows.Scan(&publication.SessionID, &publication.ApprovalID, &publication.OperationID, &kind, &externalID, &detail); err != nil {
			return nil, fmt.Errorf("read known publications: %w", err)
		}
		if err := publication.identify(kind, externalID, detail); err != nil {
			return nil, err
		}
		if publication.IssueNumber != 0 || publication.Branch != "" || publication.PullRequestNumber != 0 {
			publications = append(publications, publication)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read known publications: %w", err)
	}
	return publications, nil
}

// identify fills the external objects of one ledger operation: the issue
// number of a created item, or the branch and pull request of a Git batch.
func (p *KnownPublication) identify(kind PublicationOperationKind, externalID, detail string) error {
	if kind == PublicationOperationCreateItem {
		if externalID == "" {
			return nil
		}
		number, err := strconv.Atoi(externalID)
		if err != nil || number <= 0 {
			return fmt.Errorf("publication operation %s records the item %q, which is not an issue number", p.OperationID, externalID)
		}
		p.IssueNumber = number
		return nil
	}
	if detail == "" {
		return nil
	}
	var evidence gitBatchEvidence
	if err := json.Unmarshal([]byte(detail), &evidence); err != nil {
		return fmt.Errorf("publication operation %s has unreadable Git batch evidence: %w", p.OperationID, err)
	}
	p.Branch = evidence.Evidence.Branch
	p.PullRequestNumber = evidence.Evidence.PullRequestNumber
	return nil
}
