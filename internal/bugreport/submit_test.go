package bugreport_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/bugreport"
)

// fakeUpstream records every upstream call and returns scripted results.
type fakeUpstream struct {
	issues       []bugreport.Issue
	bodies       map[int]string
	candidates   []bugreport.Issue
	labelMissing bool
	findErr      error
	createErrs   []error
	created      []createCall
	calls        int
	// entered, when set, receives a signal when CreateIssue starts, and
	// release then holds the creation until the test closes it.
	entered chan struct{}
	release chan struct{}
}

// createCall is one recorded issue creation.
type createCall struct {
	title, body string
	labels      []string
}

// FindReport returns the issue whose body carries the report identity.
func (f *fakeUpstream) FindReport(_ context.Context, reportID string) (*bugreport.Issue, error) {
	f.calls++
	if f.findErr != nil {
		return nil, f.findErr
	}
	for _, issue := range f.issues {
		if marker := "id=" + reportID + " "; strings.Contains(f.bodies[issue.Number], marker) {
			found := issue
			return &found, nil
		}
	}
	return nil, nil
}

// FindCandidates returns the scripted likely duplicates.
func (f *fakeUpstream) FindCandidates(context.Context, string) ([]bugreport.Issue, error) {
	f.calls++
	return f.candidates, nil
}

// LabelExists reports the scripted triage label state.
func (f *fakeUpstream) LabelExists(context.Context, string) (bool, error) {
	f.calls++
	return !f.labelMissing, nil
}

// CreateIssue records the creation, or fails with the next scripted error.
// A scripted ambiguous error still creates the issue, like a timeout after
// the upstream accepted the request.
func (f *fakeUpstream) CreateIssue(_ context.Context, title, body string, labels []string) (bugreport.Issue, error) {
	f.calls++
	if f.entered != nil {
		f.entered <- struct{}{}
		<-f.release
	}
	if len(f.createErrs) > 0 {
		err := f.createErrs[0]
		f.createErrs = f.createErrs[1:]
		var upstream *bugreport.UpstreamError
		if errors.As(err, &upstream) {
			return bugreport.Issue{}, err
		}
		f.record(title, body, labels)
		return bugreport.Issue{}, err
	}
	return f.record(title, body, labels), nil
}

// record stores one created issue.
func (f *fakeUpstream) record(title, body string, labels []string) bugreport.Issue {
	f.created = append(f.created, createCall{title: title, body: body, labels: labels})
	issue := bugreport.Issue{Number: 100 + len(f.created), Title: title, URL: "https://github.com/Stevie1704/sw-factory/issues/" + strconv.Itoa(100+len(f.created))}
	if f.bodies == nil {
		f.bodies = map[int]string{}
	}
	f.bodies[issue.Number] = body
	f.issues = append(f.issues, issue)
	return issue
}

// writeCompletedDraft writes a submission-ready draft and returns its path.
func writeCompletedDraft(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "report.md")
	if err := os.WriteFile(path, []byte(fillDraft(t, string(bugreport.Render(testDraft())))), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSubmitCreatesOneTriageIssueFromTheReviewedDraft verifies a new
// submission and its exact replay.
func TestSubmitCreatesOneTriageIssueFromTheReviewedDraft(t *testing.T) {
	path := writeCompletedDraft(t)
	content, _ := os.ReadFile(path)
	want, err := bugreport.Parse(content)
	if err != nil {
		t.Fatal(err)
	}
	upstream := &fakeUpstream{}

	result, err := bugreport.Submit(context.Background(), path, upstream, bugreport.SubmitOptions{})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if result.Outcome != bugreport.OutcomeCreated || result.Issue == nil || result.Issue.URL != "https://github.com/Stevie1704/sw-factory/issues/101" {
		t.Fatalf("Submit() = %#v, want a created issue URL", result)
	}
	if len(upstream.created) != 1 || upstream.created[0].title != want.Title || upstream.created[0].body != want.Body || !slices.Equal(upstream.created[0].labels, []string{"needs-triage"}) {
		t.Fatalf("created = %#v, want the exact reviewed draft with needs-triage only", upstream.created)
	}

	replay, err := bugreport.Submit(context.Background(), path, upstream, bugreport.SubmitOptions{})
	if err != nil {
		t.Fatalf("Submit(replay) error = %v", err)
	}
	if replay.Outcome != bugreport.OutcomeAlreadySubmitted || replay.Issue == nil || replay.Issue.URL != result.Issue.URL || len(upstream.created) != 1 {
		t.Fatalf("Submit(replay) = %#v with %d creations, want the existing URL and no new issue", replay, len(upstream.created))
	}
}

// TestSubmitReturnsAnUpstreamReportWithoutLocalState verifies that a report
// already upstream is found by its identity even when local state is lost.
func TestSubmitReturnsAnUpstreamReportWithoutLocalState(t *testing.T) {
	path := writeCompletedDraft(t)
	upstream := &fakeUpstream{}
	upstream.record("earlier", "<!-- factory-bug-report id=fbr-0123456789abcdef0123456789abcdef schema=1 -->\n", nil)
	upstream.created = nil

	result, err := bugreport.Submit(context.Background(), path, upstream, bugreport.SubmitOptions{})
	if err != nil || result.Outcome != bugreport.OutcomeAlreadySubmitted || result.Issue.Number != 101 || len(upstream.created) != 0 {
		t.Fatalf("Submit() = %#v, %v; want already_submitted #101 without a creation", result, err)
	}
}

// TestSubmitShowsLikelyDuplicatesForDisposition verifies that a likely
// duplicate stops the creation until the operator declares the report
// distinct from every candidate.
func TestSubmitShowsLikelyDuplicatesForDisposition(t *testing.T) {
	path := writeCompletedDraft(t)
	candidate := bugreport.Issue{Number: 42, Title: "Suspected Factory defect: status exits 1", URL: "https://github.com/Stevie1704/sw-factory/issues/42"}
	upstream := &fakeUpstream{candidates: []bugreport.Issue{candidate}}

	result, err := bugreport.Submit(context.Background(), path, upstream, bugreport.SubmitOptions{})
	if err != nil || result.Outcome != bugreport.OutcomeDuplicateCandidate || len(result.Candidates) != 1 || result.Candidates[0] != candidate || len(upstream.created) != 0 {
		t.Fatalf("Submit() = %#v, %v; want duplicate_candidate #42 without a creation", result, err)
	}

	result, err = bugreport.Submit(context.Background(), path, upstream, bugreport.SubmitOptions{DistinctFrom: []int{42}})
	if err != nil || result.Outcome != bugreport.OutcomeCreated || len(upstream.created) != 1 {
		t.Fatalf("Submit(distinct from #42) = %#v, %v; want a created issue", result, err)
	}
}

// TestSubmitFailuresRetainTheDraftAndCreateNothing verifies the definite
// failures: each returns its kind, creates no issue, keeps the draft, and
// leaves a later retry free to submit.
func TestSubmitFailuresRetainTheDraftAndCreateNothing(t *testing.T) {
	tests := []struct {
		name     string
		upstream *fakeUpstream
		want     bugreport.FailureKind
	}{
		{"missing label", &fakeUpstream{labelMissing: true}, bugreport.FailureMissingLabel},
		{"authentication", &fakeUpstream{findErr: &bugreport.UpstreamError{Kind: bugreport.FailureAuthentication, Err: errors.New("gh auth login required")}}, bugreport.FailureAuthentication},
		{"rate limited", &fakeUpstream{createErrs: []error{&bugreport.UpstreamError{Kind: bugreport.FailureRateLimited, Err: errors.New("API rate limit exceeded")}}}, bugreport.FailureRateLimited},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeCompletedDraft(t)
			before, _ := os.ReadFile(path)
			_, err := bugreport.Submit(context.Background(), path, test.upstream, bugreport.SubmitOptions{})
			var failure *bugreport.SubmitError
			if !errors.As(err, &failure) || failure.Kind != test.want {
				t.Fatalf("Submit() error = %v, want %s failure", err, test.want)
			}
			if len(test.upstream.created) != 0 {
				t.Fatalf("Submit() created %d issues, want none", len(test.upstream.created))
			}
			if after, _ := os.ReadFile(path); string(after) != string(before) {
				t.Fatal("Submit() changed the draft")
			}
			test.upstream.labelMissing, test.upstream.findErr = false, nil
			if result, err := bugreport.Submit(context.Background(), path, test.upstream, bugreport.SubmitOptions{}); err != nil || result.Outcome != bugreport.OutcomeCreated {
				t.Fatalf("Submit(retry) = %#v, %v; want a created issue", result, err)
			}
		})
	}
}

// TestSubmitNeverRepeatsAnAmbiguousCreationBlindly verifies the reconciliation
// of a creation whose outcome is unknown: a found report is returned, an
// unconfirmed one blocks until the operator confirms it was not created.
func TestSubmitNeverRepeatsAnAmbiguousCreationBlindly(t *testing.T) {
	path := writeCompletedDraft(t)
	upstream := &fakeUpstream{createErrs: []error{errors.New("gh issue create did not finish within 1m0s")}}

	_, err := bugreport.Submit(context.Background(), path, upstream, bugreport.SubmitOptions{})
	var failure *bugreport.SubmitError
	if !errors.As(err, &failure) || failure.Kind != bugreport.FailureAmbiguous {
		t.Fatalf("Submit() error = %v, want an ambiguous failure", err)
	}

	hidden := upstream.issues
	upstream.issues = nil // the upstream search does not show the issue yet
	_, err = bugreport.Submit(context.Background(), path, upstream, bugreport.SubmitOptions{})
	if !errors.As(err, &failure) || failure.Kind != bugreport.FailureUnconfirmed || len(upstream.created) != 1 {
		t.Fatalf("Submit(unconfirmed) error = %v with %d creations, want unconfirmed and no new creation", err, len(upstream.created))
	}

	upstream.issues = hidden // the search now shows the accepted issue
	result, err := bugreport.Submit(context.Background(), path, upstream, bugreport.SubmitOptions{})
	if err != nil || result.Outcome != bugreport.OutcomeAlreadySubmitted || len(upstream.created) != 1 {
		t.Fatalf("Submit(reconciled) = %#v, %v; want already_submitted without a new creation", result, err)
	}
}

// TestSubmitCreatesAfterTheOperatorConfirmsNoIssueExists verifies the
// explicit recovery path of an unconfirmed creation.
func TestSubmitCreatesAfterTheOperatorConfirmsNoIssueExists(t *testing.T) {
	path := writeCompletedDraft(t)
	upstream := &fakeUpstream{createErrs: []error{errors.New("connection reset")}}
	if _, err := bugreport.Submit(context.Background(), path, upstream, bugreport.SubmitOptions{}); err == nil {
		t.Fatal("Submit() error = nil, want ambiguous failure")
	}
	upstream.issues, upstream.created = nil, nil // the upstream really did not create it

	result, err := bugreport.Submit(context.Background(), path, upstream, bugreport.SubmitOptions{ConfirmNotCreated: true})
	if err != nil || result.Outcome != bugreport.OutcomeCreated || len(upstream.created) != 1 {
		t.Fatalf("Submit(confirmed) = %#v, %v; want one created issue", result, err)
	}
}

// TestSubmitRejectsAnIncompleteDraftBeforeAnyUpstreamCall verifies that
// validation happens offline.
func TestSubmitRejectsAnIncompleteDraftBeforeAnyUpstreamCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.md")
	if err := os.WriteFile(path, bugreport.Render(testDraft()), 0o600); err != nil {
		t.Fatal(err)
	}
	upstream := &fakeUpstream{}
	_, err := bugreport.Submit(context.Background(), path, upstream, bugreport.SubmitOptions{})
	var failure *bugreport.SubmitError
	if !errors.As(err, &failure) || failure.Kind != bugreport.FailureInvalidDraft || upstream.calls != 0 {
		t.Fatalf("Submit() error = %v after %d upstream calls, want invalid_draft before any call", err, upstream.calls)
	}
}

// TestSubmitRefusesAConcurrentSubmissionOfTheSameReport verifies that a
// second submission of a report cannot start while the first one is
// creating the issue, so two commands never both create it.
func TestSubmitRefusesAConcurrentSubmissionOfTheSameReport(t *testing.T) {
	path := writeCompletedDraft(t)
	first := &fakeUpstream{entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := bugreport.Submit(context.Background(), path, first, bugreport.SubmitOptions{})
		done <- err
	}()
	<-first.entered

	second := &fakeUpstream{}
	_, err := bugreport.Submit(context.Background(), path, second, bugreport.SubmitOptions{})
	var failure *bugreport.SubmitError
	if !errors.As(err, &failure) || failure.Kind != bugreport.FailureInProgress || second.calls != 0 {
		t.Fatalf("Submit(concurrent) error = %v after %d upstream calls, want in_progress before any call", err, second.calls)
	}

	close(first.release)
	if err := <-done; err != nil {
		t.Fatalf("Submit(first) error = %v", err)
	}
	if len(first.created) != 1 {
		t.Fatalf("first submission created %d issues, want 1", len(first.created))
	}
}
