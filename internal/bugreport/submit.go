package bugreport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// UpstreamRepository is the only destination of Factory defect reports. It
// never comes from the current repository or from worker instructions.
const UpstreamRepository = "Stevie1704/sw-factory"

// TriageLabel is the only label a new report receives.
const TriageLabel = "needs-triage"

// maxDraftBytes bounds the draft file Submit reads.
const maxDraftBytes = 256 << 10

// Issue identifies one upstream issue.
type Issue struct {
	// Number is the upstream issue number.
	Number int `json:"number"`
	// Title is the upstream issue title.
	Title string `json:"title"`
	// URL is the upstream issue web address.
	URL string `json:"url"`
}

// Upstream is the port to Factory's upstream issue tracker. An adapter binds
// it to UpstreamRepository and returns an *UpstreamError for a failure whose
// kind it knows.
type Upstream interface {
	// FindReport returns the issue whose body carries the report identity,
	// in any state, or nil when there is none.
	FindReport(ctx context.Context, reportID string) (*Issue, error)
	// FindCandidates returns open issues that are likely duplicates of a
	// report with this title.
	FindCandidates(ctx context.Context, title string) ([]Issue, error)
	// LabelExists reports whether the label exists upstream.
	LabelExists(ctx context.Context, label string) (bool, error)
	// CreateIssue creates one issue and returns it.
	CreateIssue(ctx context.Context, title, body string, labels []string) (Issue, error)
}

// FailureKind classifies a failed submission.
type FailureKind string

const (
	// FailureInvalidDraft means the draft is incomplete or malformed.
	FailureInvalidDraft FailureKind = "invalid_draft"
	// FailureAuthentication means the host GitHub authentication is missing or invalid.
	FailureAuthentication FailureKind = "authentication"
	// FailurePermission means the authenticated account may not create the issue.
	FailurePermission FailureKind = "permission"
	// FailureRateLimited means the upstream rate limit stopped the request.
	FailureRateLimited FailureKind = "rate_limited"
	// FailureMissingLabel means the upstream repository has no triage label.
	FailureMissingLabel FailureKind = "missing_label"
	// FailureUnavailable means the upstream could not be reached before any
	// creation request.
	FailureUnavailable FailureKind = "unavailable"
	// FailureAmbiguous means a creation request failed after it may have
	// reached the upstream.
	FailureAmbiguous FailureKind = "ambiguous"
	// FailureUnconfirmed means an earlier creation has an unknown outcome and
	// the upstream does not show the report.
	FailureUnconfirmed FailureKind = "unconfirmed"
	// FailureLocalState means the local submission state could not be used.
	FailureLocalState FailureKind = "local_state"
)

// UpstreamError is an adapter failure of a known kind. A creation that fails
// with an UpstreamError did not create an issue.
type UpstreamError struct {
	// Kind is the failure class.
	Kind FailureKind
	// Err is the adapter failure.
	Err error
}

// Error describes the failure.
func (e *UpstreamError) Error() string { return fmt.Sprintf("%s: %v", e.Kind, e.Err) }

// Unwrap returns the adapter failure.
func (e *UpstreamError) Unwrap() error { return e.Err }

// SubmitError is a failed submission. The draft is always retained.
type SubmitError struct {
	// Kind is the failure class.
	Kind FailureKind
	// Action tells the operator what to do next.
	Action string
	// Err is the underlying failure.
	Err error
}

// Error describes the failure and the next action.
func (e *SubmitError) Error() string {
	return fmt.Sprintf("bug report submission failed (%s): %v; %s", e.Kind, e.Err, e.Action)
}

// Unwrap returns the underlying failure.
func (e *SubmitError) Unwrap() error { return e.Err }

// Outcome is the result of a submission that did not fail.
type Outcome string

const (
	// OutcomeCreated means Submit created a new upstream issue.
	OutcomeCreated Outcome = "created"
	// OutcomeAlreadySubmitted means the report identity already has an issue.
	OutcomeAlreadySubmitted Outcome = "already_submitted"
	// OutcomeDuplicateCandidate means likely duplicates need the operator's
	// disposition; Submit created nothing.
	OutcomeDuplicateCandidate Outcome = "duplicate_candidate"
)

// Result is the outcome of one submission.
type Result struct {
	// Outcome is the submission result.
	Outcome Outcome
	// ReportID is the report identity.
	ReportID string
	// Issue is the created or existing issue.
	Issue *Issue
	// Candidates are the likely duplicates for OutcomeDuplicateCandidate.
	Candidates []Issue
}

// SubmitOptions are the operator's explicit decisions for one submission.
type SubmitOptions struct {
	// DistinctFrom lists reviewed candidate issue numbers the operator
	// declares to be different defects.
	DistinctFrom []int
	// ConfirmNotCreated tells that the operator checked upstream after an
	// ambiguous creation and found no issue for the report.
	ConfirmNotCreated bool
}

// Submission states persisted beside the draft.
const (
	statePending = "pending"
	stateCreated = "created"
)

// submissionState is the local record that reconciles retries.
type submissionState struct {
	SchemaVersion int       `json:"schema_version"`
	ReportID      string    `json:"report_id"`
	State         string    `json:"state"`
	Issue         *Issue    `json:"issue,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Submit validates the draft at path and publishes exactly that content to
// the upstream tracker. It gathers no further evidence. A replay of an
// already submitted report returns the existing issue, likely duplicates stop
// the creation for the operator's disposition, and a creation with an
// unknown outcome is never repeated until it is reconciled.
func Submit(ctx context.Context, path string, upstream Upstream, options SubmitOptions) (Result, error) {
	content, err := readDraft(path)
	if err != nil {
		return Result{}, &SubmitError{Kind: FailureInvalidDraft, Action: "check the draft path", Err: err}
	}
	submission, err := Parse(content)
	if err != nil {
		return Result{}, &SubmitError{Kind: FailureInvalidDraft, Action: "complete the draft and submit again", Err: err}
	}
	statePath := submissionStatePath(path, submission.ID)
	state, err := readState(statePath)
	if err != nil {
		return Result{}, &SubmitError{Kind: FailureLocalState, Action: "inspect or remove " + statePath, Err: err}
	}
	result := Result{ReportID: submission.ID}
	if state.State == stateCreated && state.Issue != nil {
		result.Outcome, result.Issue = OutcomeAlreadySubmitted, state.Issue
		return result, nil
	}
	existing, err := upstream.FindReport(ctx, submission.ID)
	if err != nil {
		return Result{}, upstreamFailure(err, FailureUnavailable)
	}
	if existing != nil {
		if err := writeState(statePath, submissionState{ReportID: submission.ID, State: stateCreated, Issue: existing}); err != nil {
			return Result{}, &SubmitError{Kind: FailureLocalState, Action: "the report is upstream at " + existing.URL, Err: err}
		}
		result.Outcome, result.Issue = OutcomeAlreadySubmitted, existing
		return result, nil
	}
	if state.State == statePending && !options.ConfirmNotCreated {
		return Result{}, &SubmitError{
			Kind:   FailureUnconfirmed,
			Action: fmt.Sprintf("an earlier creation of report %s has an unknown outcome; search https://github.com/%s/issues for the report identity, wait for the search to update, and submit again, or add --confirm-not-created when no issue exists", submission.ID, UpstreamRepository),
			Err:    errors.New("the upstream does not show the report yet"),
		}
	}
	candidates, err := upstream.FindCandidates(ctx, submission.Title)
	if err != nil {
		return Result{}, upstreamFailure(err, FailureUnavailable)
	}
	candidates = slices.DeleteFunc(candidates, func(issue Issue) bool { return slices.Contains(options.DistinctFrom, issue.Number) })
	if len(candidates) > 0 {
		result.Outcome, result.Candidates = OutcomeDuplicateCandidate, candidates
		return result, nil
	}
	exists, err := upstream.LabelExists(ctx, TriageLabel)
	if err != nil {
		return Result{}, upstreamFailure(err, FailureUnavailable)
	}
	if !exists {
		return Result{}, &SubmitError{Kind: FailureMissingLabel, Action: "ask a Factory maintainer to create the label; Factory never creates it", Err: fmt.Errorf("label %q does not exist in %s", TriageLabel, UpstreamRepository)}
	}
	return create(ctx, upstream, submission, statePath)
}

// create records the pending creation, creates the issue, and records its
// result. A creation failure of a known kind clears the pending record; any
// other failure keeps it, because the upstream may have created the issue.
func create(ctx context.Context, upstream Upstream, submission Submission, statePath string) (Result, error) {
	if err := writeState(statePath, submissionState{ReportID: submission.ID, State: statePending}); err != nil {
		return Result{}, &SubmitError{Kind: FailureLocalState, Action: "make the draft directory writable and submit again", Err: err}
	}
	issue, err := upstream.CreateIssue(ctx, submission.Title, submission.Body, []string{TriageLabel})
	if err != nil {
		var known *UpstreamError
		if errors.As(err, &known) {
			if removeErr := os.Remove(statePath); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
				return Result{}, &SubmitError{Kind: FailureLocalState, Action: "remove " + statePath + " and submit again", Err: removeErr}
			}
			return Result{}, upstreamFailure(err, known.Kind)
		}
		return Result{}, &SubmitError{Kind: FailureAmbiguous, Action: "submit the same draft again later; Factory checks for the report before it creates another issue", Err: err}
	}
	if err := writeState(statePath, submissionState{ReportID: submission.ID, State: stateCreated, Issue: &issue}); err != nil {
		return Result{}, &SubmitError{Kind: FailureLocalState, Action: "the report is upstream at " + issue.URL, Err: err}
	}
	return Result{Outcome: OutcomeCreated, ReportID: submission.ID, Issue: &issue}, nil
}

// failureActions tells the operator how to recover from each upstream failure.
var failureActions = map[FailureKind]string{
	FailureAuthentication: "run `gh auth login` on the host and submit again",
	FailurePermission:     "use a GitHub account that may open issues in " + UpstreamRepository,
	FailureRateLimited:    "wait for the GitHub rate limit to reset and submit again",
	FailureMissingLabel:   "ask a Factory maintainer to create the label; Factory never creates it",
	FailureUnavailable:    "check the network connection and submit again",
}

// upstreamFailure wraps an upstream error with its kind and recovery action.
// An error of unknown kind gets the fallback kind.
func upstreamFailure(err error, fallback FailureKind) *SubmitError {
	kind := fallback
	var known *UpstreamError
	if errors.As(err, &known) {
		kind = known.Kind
	}
	action, ok := failureActions[kind]
	if !ok {
		action = failureActions[FailureUnavailable]
	}
	return &SubmitError{Kind: kind, Action: action + "; the draft is unchanged", Err: err}
}

// readDraft reads a bounded regular draft file.
func readDraft(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect draft: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("the draft must be a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open draft: %w", err)
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(io.LimitReader(file, maxDraftBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read draft: %w", err)
	}
	if len(content) > maxDraftBytes {
		return nil, fmt.Errorf("the draft exceeds %d bytes", maxDraftBytes)
	}
	return content, nil
}

// submissionStatePath names the local submission record of one report
// identity beside its draft.
func submissionStatePath(draftPath, reportID string) string {
	return filepath.Join(filepath.Dir(draftPath), "."+filepath.Base(draftPath)+"."+reportID+".submission.json")
}

// readState returns the local submission record, or an empty record.
func readState(path string) (submissionState, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return submissionState{}, nil
	}
	if err != nil {
		return submissionState{}, fmt.Errorf("read submission state: %w", err)
	}
	var state submissionState
	if err := json.Unmarshal(content, &state); err != nil {
		return submissionState{}, fmt.Errorf("decode submission state: %w", err)
	}
	return state, nil
}

// writeState atomically replaces the local submission record with private
// permissions.
func writeState(path string, state submissionState) error {
	state.SchemaVersion = 1
	state.UpdatedAt = time.Now().UTC()
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".submission-*.tmp")
	if err != nil {
		return fmt.Errorf("create submission state: %w", err)
	}
	defer func() { _ = os.Remove(temporary.Name()) }()
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write submission state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync submission state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close submission state: %w", err)
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return fmt.Errorf("publish submission state: %w", err)
	}
	return nil
}
