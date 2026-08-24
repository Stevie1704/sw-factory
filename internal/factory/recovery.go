package factory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// RecoveryRequiredCode is the stable code for the pre-reconciliation safety
// boundary. Issue #21 supersedes this refusal with complete reconciliation.
const RecoveryRequiredCode = "recovery-required"

// RecoveryDiscrepancy describes one observed disagreement between persisted
// run identity and an external projection.
type RecoveryDiscrepancy struct {
	// Source identifies the projection that disagrees with persisted state.
	Source string
	// Field identifies the identity field that differs.
	Field string
	// Expected is the persisted or coordinator-derived value.
	Expected string
	// Observed is the value read from the external projection.
	Observed string
}

// RecoveryDiagnosis is the read-only result of checking an interrupted run.
// SourcesAgree may be true while recovery is still required: before issue #21,
// agreement is diagnosed but never treated as permission to resume.
type RecoveryDiagnosis struct {
	// Code is always RecoveryRequiredCode for a non-terminal run.
	Code string
	// RunID identifies the interrupted run being diagnosed.
	RunID string
	// SourcesAgree reports whether any discovered projection disagreed.
	SourcesAgree bool
	// Discrepancies contains every disagreement found during the read-only check.
	Discrepancies []RecoveryDiscrepancy
	// InvocationExists reports persisted invocation history for the diagnosed run.
	InvocationExists bool
	// SafeActions identifies operator actions that do not claim recovery.
	SafeActions []string
}

// RecoveryRequiredError refuses progression for a persisted non-terminal run.
// It carries the same diagnosis exposed by Status and performs no recovery.
type RecoveryRequiredError struct {
	// Diagnosis is the complete read-only startup diagnosis.
	Diagnosis RecoveryDiagnosis
}

// Error returns a stable recovery-required message without implying that the
// coordinator repaired, resumed, or otherwise recovered the run.
func (e *RecoveryRequiredError) Error() string {
	if e == nil {
		return RecoveryRequiredCode
	}
	agreement := "sources disagree"
	if e.Diagnosis.SourcesAgree {
		agreement = "sources agree"
	}
	discrepancyCount := len(e.Diagnosis.Discrepancies)
	message := fmt.Sprintf("%s: run %q is interrupted; %s; %d discrepancies; no recovery occurred", RecoveryRequiredCode, e.Diagnosis.RunID, agreement, discrepancyCount)
	if discrepancyCount != 0 {
		details := make([]string, 0, discrepancyCount)
		for _, discrepancy := range e.Diagnosis.Discrepancies {
			details = append(details, fmt.Sprintf("%s.%s expected=%q observed=%q", discrepancy.Source, discrepancy.Field, discrepancy.Expected, discrepancy.Observed))
		}
		message += "; discrepancies: " + strings.Join(details, ", ")
	}
	if e.Diagnosis.InvocationExists {
		message += "; persisted invocation history exists"
	}
	if len(e.Diagnosis.SafeActions) != 0 {
		message += "; safe actions: " + strings.Join(e.Diagnosis.SafeActions, "; ")
	}
	return message
}

// Code returns the machine-readable refusal code.
func (e *RecoveryRequiredError) Code() string { return RecoveryRequiredCode }

// diagnoseInterruptedRun compares all available persisted and external
// identity projections without mutating Git, GitHub, the worker, the terminal,
// the harness, or operational workflow state.
func (s *Service) diagnoseInterruptedRun(ctx context.Context, registration config.RepositoryRegistration, run store.Run) RecoveryDiagnosis {
	diagnosis := newRecoveryDiagnosis(run.ID)
	comparePersistedRunIdentity(&diagnosis, registration.Path, run)

	inspector := s.worktreeInspector()
	inspectWorktreeProjection(ctx, &diagnosis, inspector, registration.Path, run)

	repository := github.Repository{Owner: registration.GitHub.Owner, Name: registration.GitHub.Repository}
	inspectGitHubProjection(ctx, &diagnosis, s.deps.GitHub, s.pullRequestClient(), repository, run)
	diagnosis.SourcesAgree = len(diagnosis.Discrepancies) == 0
	return diagnosis
}

// newRecoveryDiagnosis creates the stable result envelope and operator actions.
func newRecoveryDiagnosis(runID string) RecoveryDiagnosis {
	return RecoveryDiagnosis{
		Code:  RecoveryRequiredCode,
		RunID: runID,
		SafeActions: []string{
			"inspect the diagnosis with `factory status`",
			"verify the repository, worktree, branch, checkpoint, issue label, status comment, and pull request",
			"wait for issue #21 before attempting automatic recovery",
		},
	}
}

// comparePersistedRunIdentity validates the values that can be checked before
// consulting external projections.
func comparePersistedRunIdentity(diagnosis *RecoveryDiagnosis, repositoryPath string, run store.Run) {
	if strings.TrimSpace(run.ID) == "" {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "run", Field: "identifier", Expected: "non-empty run identifier", Observed: "empty"})
	}
	if strings.TrimSpace(run.RepositoryPath) == "" {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "run", Field: "repository", Expected: repositoryPath, Observed: "empty"})
	} else if strings.TrimSpace(repositoryPath) == "" || resolvePath(run.RepositoryPath) != resolvePath(repositoryPath) {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "run", Field: "repository", Expected: repositoryPath, Observed: run.RepositoryPath})
	}
	if strings.TrimSpace(run.Worktree) == "" {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "run", Field: "worktree", Expected: "non-empty worktree path", Observed: "empty"})
	} else if !filepath.IsAbs(run.Worktree) {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "run", Field: "worktree", Expected: "absolute worktree path", Observed: run.Worktree})
	}
	if strings.TrimSpace(run.Branch) == "" {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "run", Field: "branch", Expected: "non-empty branch", Observed: "empty"})
	} else if strings.TrimSpace(run.ID) != "" {
		expectedBranch := "factory/" + run.ID
		if run.Branch != expectedBranch {
			addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "run", Field: "branch identity", Expected: expectedBranch, Observed: run.Branch})
		}
	}
	if strings.TrimSpace(run.CheckpointSHA) == "" {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "run", Field: "checkpoint", Expected: "non-empty checkpoint SHA", Observed: "empty"})
	}
	if run.IssueNumber <= 0 {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "run", Field: "issue", Expected: "positive issue number", Observed: fmt.Sprintf("%d", run.IssueNumber)})
	}
	if run.PullRequestNumber < 0 {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "run", Field: "pull request", Expected: "zero or positive pull-request number", Observed: fmt.Sprintf("%d", run.PullRequestNumber)})
	}
	if run.PullRequestNumber == 0 && strings.TrimSpace(run.PullRequestURL) != "" {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "run", Field: "pull request", Expected: "number for persisted pull-request URL", Observed: run.PullRequestURL})
	}
}

// inspectWorktreeProjection compares the persisted checkout identity with a
// read-only Git inspection and records observation failures as discrepancies.
func inspectWorktreeProjection(ctx context.Context, diagnosis *RecoveryDiagnosis, inspector gitadapter.WorktreeInspector, repositoryPath string, run store.Run) {
	if strings.TrimSpace(run.Worktree) == "" || !filepath.IsAbs(run.Worktree) {
		return
	}
	info, err := os.Stat(run.Worktree)
	if err != nil {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "worktree", Field: "path", Expected: "existing directory", Observed: err.Error()})
		return
	}
	if !info.IsDir() {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "worktree", Field: "path", Expected: "directory", Observed: "not a directory"})
		return
	}
	if inspector == nil {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "worktree", Field: "inspection", Expected: "read-only Git inspection", Observed: "inspector unavailable"})
		return
	}
	state, err := inspector.Inspect(ctx, run.Worktree)
	if err != nil {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "worktree", Field: "inspection", Expected: "read-only Git inspection", Observed: err.Error()})
		return
	}
	if strings.TrimSpace(state.RepositoryPath) == "" {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "worktree", Field: "repository", Expected: repositoryPath, Observed: "empty"})
	} else if resolvePath(state.RepositoryPath) != resolvePath(repositoryPath) {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "worktree", Field: "repository", Expected: repositoryPath, Observed: state.RepositoryPath})
	}
	if state.Branch != run.Branch {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "worktree", Field: "branch", Expected: run.Branch, Observed: state.Branch})
	}
	if state.HeadSHA != run.CheckpointSHA {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "worktree", Field: "checkpoint", Expected: run.CheckpointSHA, Observed: state.HeadSHA})
	}
}

// inspectGitHubProjection compares the issue, state label, status comment, and
// optional pull request through read-only GitHub adapter methods.
func inspectGitHubProjection(ctx context.Context, diagnosis *RecoveryDiagnosis, client github.Client, pullRequests github.PullRequestClient, repository github.Repository, run store.Run) {
	if client == nil {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "client", Expected: "read-only GitHub client", Observed: "client unavailable"})
		return
	}
	issue, err := client.Issue(ctx, repository, run.IssueNumber)
	if err != nil {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "issue", Expected: fmt.Sprintf("issue #%d", run.IssueNumber), Observed: err.Error()})
	} else {
		if issue.Number != run.IssueNumber {
			addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "issue", Expected: fmt.Sprintf("#%d", run.IssueNumber), Observed: fmt.Sprintf("#%d", issue.Number)})
		}
		if issue.IsPullRequest {
			addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "issue", Expected: "issue, not pull request", Observed: "pull request"})
		}
		expectedLabel := factoryLabelForStatus(run.Status)
		actualLabels := factoryStateLabels(issue.Labels)
		if len(actualLabels) != 1 || actualLabels[0] != expectedLabel {
			addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "state label", Expected: expectedLabel, Observed: strings.Join(actualLabels, ", ")})
		}
	}

	marker := statusCommentMarker(run.ID)
	comment, commentErr := client.FindStatusComment(ctx, repository, run.IssueNumber, marker)
	if commentErr != nil {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "status comment", Expected: run.StatusCommentID, Observed: commentErr.Error()})
	} else if strings.TrimSpace(comment.ID) == "" {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "status comment", Expected: run.StatusCommentID, Observed: "missing"})
	} else if !strings.Contains(comment.Body, marker) {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "status comment marker", Expected: marker, Observed: "marker missing"})
	} else if strings.TrimSpace(run.StatusCommentID) == "" {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "status comment", Expected: "persisted comment " + comment.ID, Observed: "persisted identity missing"})
	} else if comment.ID != run.StatusCommentID {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "status comment", Expected: run.StatusCommentID, Observed: comment.ID})
	}

	inspectPullRequestProjection(ctx, diagnosis, pullRequests, repository, run)
}

// inspectPullRequestProjection checks an existing or unexpectedly discovered
// pull request without creating, updating, or otherwise mutating it.
func inspectPullRequestProjection(ctx context.Context, diagnosis *RecoveryDiagnosis, pullRequests github.PullRequestClient, repository github.Repository, run store.Run) {
	hasPersistedIdentity := run.PullRequestNumber > 0 || strings.TrimSpace(run.PullRequestURL) != ""
	if pullRequests == nil {
		if hasPersistedIdentity {
			addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "pull request", Expected: "read-only pull-request client", Observed: "client unavailable"})
		}
		return
	}
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "run", Field: "specification packet", Expected: "decodable packet for pull-request target", Observed: err.Error()})
		return
	}
	baseBranch := packet.RepositoryConfig.TargetBranch
	if strings.TrimSpace(baseBranch) == "" {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "run", Field: "target branch", Expected: "non-empty target branch", Observed: "empty"})
		return
	}
	pullRequest, err := pullRequests.FindPullRequest(ctx, repository, run.Branch, baseBranch)
	if err != nil {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "pull request", Expected: pullRequestIdentity(run), Observed: err.Error()})
		return
	}
	if pullRequest.Number == 0 {
		if hasPersistedIdentity {
			addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "pull request", Expected: pullRequestIdentity(run), Observed: "missing"})
		}
		return
	}
	if !hasPersistedIdentity {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "pull request", Expected: "no persisted pull-request identity", Observed: fmt.Sprintf("#%d", pullRequest.Number)})
		return
	}
	if pullRequest.Number != run.PullRequestNumber {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "pull request number", Expected: fmt.Sprintf("#%d", run.PullRequestNumber), Observed: fmt.Sprintf("#%d", pullRequest.Number)})
	}
	if strings.TrimSpace(run.PullRequestURL) != "" && pullRequest.URL != run.PullRequestURL {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "pull request URL", Expected: run.PullRequestURL, Observed: pullRequest.URL})
	}
	if pullRequest.HeadBranch != "" && pullRequest.HeadBranch != run.Branch {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "pull request head", Expected: run.Branch, Observed: pullRequest.HeadBranch})
	}
	if pullRequest.BaseBranch != "" && pullRequest.BaseBranch != baseBranch {
		addRecoveryDiscrepancy(diagnosis, RecoveryDiscrepancy{Source: "github", Field: "pull request base", Expected: baseBranch, Observed: pullRequest.BaseBranch})
	}
}

// pullRequestIdentity formats the persisted PR identity for diagnostics.
func pullRequestIdentity(run store.Run) string {
	if run.PullRequestNumber > 0 && strings.TrimSpace(run.PullRequestURL) != "" {
		return fmt.Sprintf("#%d %s", run.PullRequestNumber, run.PullRequestURL)
	}
	if run.PullRequestNumber > 0 {
		return fmt.Sprintf("#%d", run.PullRequestNumber)
	}
	return run.PullRequestURL
}

// factoryStateLabels extracts and sorts only labels owned by the factory so a
// stale or duplicated lifecycle label is visible in a deterministic message.
func factoryStateLabels(labels []string) []string {
	result := make([]string, 0, len(labels))
	for _, label := range labels {
		if hasLabel(github.FactoryStateLabels, label) {
			result = append(result, label)
		}
	}
	sort.Strings(result)
	return result
}

// addRecoveryDiscrepancy appends one non-empty discrepancy to a diagnosis.
func addRecoveryDiscrepancy(diagnosis *RecoveryDiagnosis, discrepancy RecoveryDiscrepancy) {
	if diagnosis == nil {
		return
	}
	if strings.TrimSpace(discrepancy.Source) == "" {
		discrepancy.Source = "unknown"
	}
	diagnosis.Discrepancies = append(diagnosis.Discrepancies, discrepancy)
}

// recoveryRequiredError wraps a diagnosis in the typed progression refusal.
func recoveryRequiredError(diagnosis RecoveryDiagnosis) error {
	return &RecoveryRequiredError{Diagnosis: diagnosis}
}
