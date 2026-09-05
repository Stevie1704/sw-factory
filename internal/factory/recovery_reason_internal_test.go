package factory

import (
	"context"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestRecoveryPauseReasonConvergesAcrossGenerations verifies a pause reason
// stops growing when it observes the comment that carries the previous pause
// reason. Recording both comment bodies verbatim made every generation quote
// the one before it, and one run reached a 872 MB lifecycle reason that no
// longer fit the bounded pending-effect payload.
func TestRecoveryPauseReasonConvergesAcrossGenerations(t *testing.T) {
	t.Parallel()

	run := reasonLoopRun()
	sizes := make([]int, 0, 12)
	for generation := 0; generation < 12; generation++ {
		diagnosis := newRecoveryDiagnosis(run.ID)
		addRecoveryDiscrepancy(&diagnosis, RecoveryDiscrepancy{
			Source:   "github",
			Field:    "status comment",
			Expected: statusCommentBody(run),
			Observed: statusCommentBody(run) + "drift",
		})
		run.LifecycleReason = recoveryDiscrepancyReason(diagnosis)
		sizes = append(sizes, len(run.LifecycleReason))
	}

	final := sizes[len(sizes)-1]
	if final > maxRecoveryReasonBytes {
		t.Fatalf("pause reason = %d bytes after %d generations, want at most %d", final, len(sizes), maxRecoveryReasonBytes)
	}
	if final != sizes[1] {
		t.Fatalf("pause reason sizes = %v, want a stable size after the first generation", sizes)
	}
}

// TestStatusCommentDiscrepancyRecordsIdentityNotBody verifies the projection
// comparison records what the bodies are rather than what they contain, so a
// pause reason cannot carry the comment that carries it.
func TestStatusCommentDiscrepancyRecordsIdentityNotBody(t *testing.T) {
	t.Parallel()

	run := reasonLoopRun()
	client := &reasonLoopGitHub{
		issue:   github.Issue{Number: run.IssueNumber, Labels: []string{github.LabelAgentNeedsInput}},
		comment: github.Comment{ID: run.StatusCommentID, Body: statusCommentMarker(run.ID) + "\ndrifted body\n"},
	}
	diagnosis := newRecoveryDiagnosis(run.ID)

	inspectGitHubProjection(context.Background(), &diagnosis, client, nil, github.Repository{Owner: "owner", Name: "repository"}, run)

	discrepancy, found := reasonLoopDiscrepancy(diagnosis, "status comment")
	if !found {
		t.Fatalf("discrepancies = %#v, want a status comment discrepancy", diagnosis.Discrepancies)
	}
	for _, value := range []string{discrepancy.Expected, discrepancy.Observed} {
		if !strings.Contains(value, "sha256 ") {
			t.Fatalf("status comment discrepancy value = %q, want a content identity", value)
		}
		if strings.Contains(value, statusCommentMarker(run.ID)) {
			t.Fatalf("status comment discrepancy value = %q, want no comment body", value)
		}
	}
}

// TestStatusCommentComparisonIgnoresTheLifecycleReason verifies two comments
// that differ only in the reason line agree. That line states the result of
// this comparison, so comparing it feeds each pause into the next one.
func TestStatusCommentComparisonIgnoresTheLifecycleReason(t *testing.T) {
	t.Parallel()

	run := reasonLoopRun()
	run.LifecycleReason = "restart reconciliation paused: an earlier observation"
	published := run
	published.LifecycleReason = "restart reconciliation paused: a different earlier observation"
	if statusCommentBody(run) == statusCommentBody(published) {
		t.Fatal("status comment bodies are identical, want the reason line to differ")
	}

	if statusCommentComparable(statusCommentBody(run)) != statusCommentComparable(statusCommentBody(published)) {
		t.Fatalf("comparable bodies differ:\n%q\n%q", statusCommentComparable(statusCommentBody(run)), statusCommentComparable(statusCommentBody(published)))
	}

	client := &reasonLoopGitHub{
		issue:   github.Issue{Number: run.IssueNumber, Labels: []string{github.LabelAgentNeedsInput}},
		comment: github.Comment{ID: run.StatusCommentID, Body: statusCommentBody(published)},
	}
	diagnosis := newRecoveryDiagnosis(run.ID)
	inspectGitHubProjection(context.Background(), &diagnosis, client, nil, github.Repository{Owner: "owner", Name: "repository"}, run)
	if _, found := reasonLoopDiscrepancy(diagnosis, "status comment"); found {
		t.Fatalf("discrepancies = %#v, want no status comment drift from the reason line alone", diagnosis.Discrepancies)
	}
}

// reasonLoopRun returns the waiting run the reason-loop tests project.
func reasonLoopRun() store.Run {
	return store.Run{
		ID:              "run-reason-loop",
		IssueNumber:     141,
		Status:          store.StatusWaitingForHuman,
		Stage:           store.StageReview,
		StatusCommentID: "comment-1",
	}
}

// reasonLoopDiscrepancy returns the first discrepancy recorded for a field.
func reasonLoopDiscrepancy(diagnosis RecoveryDiagnosis, field string) (RecoveryDiscrepancy, bool) {
	for _, discrepancy := range diagnosis.Discrepancies {
		if discrepancy.Field == field {
			return discrepancy, true
		}
	}
	return RecoveryDiscrepancy{}, false
}

// reasonLoopGitHub is the read-only projection the inspection reads.
type reasonLoopGitHub struct {
	issue   github.Issue
	comment github.Comment
}

// Issue returns the fixture issue.
func (c *reasonLoopGitHub) Issue(context.Context, github.Repository, int) (github.Issue, error) {
	return c.issue, nil
}

// FindStatusComment returns the fixture status comment.
func (c *reasonLoopGitHub) FindStatusComment(context.Context, github.Repository, int, string) (github.Comment, error) {
	return c.comment, nil
}

// CreateLabel is never called by a read-only inspection.
func (*reasonLoopGitHub) CreateLabel(context.Context, github.Repository, github.Label) error {
	return nil
}

// ReplaceIssueLabels is never called by a read-only inspection.
func (*reasonLoopGitHub) ReplaceIssueLabels(context.Context, github.Repository, int, []string) error {
	return nil
}

// CreateIssueComment is never called by a read-only inspection.
func (*reasonLoopGitHub) CreateIssueComment(context.Context, github.Repository, int, string) (github.Comment, error) {
	return github.Comment{}, nil
}

// EditIssueComment is never called by a read-only inspection.
func (*reasonLoopGitHub) EditIssueComment(context.Context, github.Repository, string, string) error {
	return nil
}
