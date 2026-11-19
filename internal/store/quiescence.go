package store

import (
	"context"
	"fmt"
	"strings"
)

// FullQuiescenceFindings returns one finding for each full-quiescence
// condition of ADR 0032 that the store does not meet. Full quiescence is the
// migration gate (ActivityFindings) plus the publication condition: no
// ideation session is publishing or has an incomplete publication. The
// coordinator-lock condition belongs to the caller, which holds that lock.
//
// view is the connection that holds the snapshot write lock, so the findings
// describe exactly the state that a snapshot then captures. A new condition
// added to the migration gate is a full-quiescence condition too.
func FullQuiescenceFindings(ctx context.Context, view Querier) ([]UpgradeFinding, error) {
	var inspection UpgradeInspection
	if err := readActiveWork(ctx, view, &inspection); err != nil {
		return nil, err
	}
	findings := inspection.ActivityFindings()
	publishing, err := publishingSessions(ctx, view)
	if err != nil {
		return nil, err
	}
	if len(publishing) != 0 {
		findings = append(findings, UpgradeFinding{Code: "publication.in_progress", Detail: "sessions that publish or have an incomplete publication: " + strings.Join(publishing, ", ")})
	}
	return findings, nil
}

// publishingSessions lists the ideation sessions whose publication runs or
// stopped before it completed.
func publishingSessions(ctx context.Context, view Querier) ([]string, error) {
	rows, err := view.QueryContext(ctx, "SELECT id FROM ideation_sessions WHERE state IN (?, ?) ORDER BY id", IdeationStatePublishing, IdeationStatePublicationIncomplete)
	if err != nil {
		return nil, fmt.Errorf("read publishing sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var sessions []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read publishing sessions: %w", err)
		}
		sessions = append(sessions, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read publishing sessions: %w", err)
	}
	return sessions, nil
}
