package github_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/github"
)

// TestStartupChecksReportMissingFactoryLabelsAlongsideSuccessfulAccessChecks
// verifies the GitHub contributor checks every required read-only prerequisite.
func TestStartupChecksReportMissingFactoryLabelsAlongsideSuccessfulAccessChecks(t *testing.T) {
	runner := &doctorRunner{responses: []doctorResponse{
		{output: []byte("authenticated\n")},
		{output: []byte(`{"permissions":{"pull":true,"push":true,"triage":true}}`)},
		{output: []byte(`[[{"name":"agent-ready"}]]`)},
	}}
	client := &github.GhClient{Runner: runner}
	report := doctor.Run(context.Background(), github.StartupChecks(client, github.Repository{Owner: "example", Name: "project"})...)

	if report.Ready() {
		t.Fatal("GitHub report is ready despite missing factory labels")
	}
	if got := len(report.Results); got != 3 {
		t.Fatalf("GitHub check count = %d, want authentication, permissions, and labels", got)
	}
	if got := report.Results[2]; got.Status != doctor.StatusFailed || !strings.Contains(got.Name, "labels") || got.Problem == "" || got.Action == "" {
		t.Fatalf("label result = %#v, want actionable failure", got)
	}
}

// TestStartupChecksDistinguishLabelReadFailuresFromMissingLabels verifies the
// report preserves the operator-relevant cause of a label diagnosis failure.
func TestStartupChecksDistinguishLabelReadFailuresFromMissingLabels(t *testing.T) {
	for _, test := range []struct {
		name    string
		output  []byte
		err     error
		problem string
	}{
		{name: "api failure", err: errors.New("network secret"), problem: "could not be read"},
		{name: "malformed response", output: []byte(`{"labels":`), problem: "invalid factory-label response"},
		{name: "missing label", output: []byte(`[[{"name":"agent-ready"}]]`), problem: `required factory label "agent-running" is missing`},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &doctorRunner{responses: []doctorResponse{
				{output: []byte("authenticated\n")},
				{output: []byte(`{"permissions":{"pull":true,"push":true,"triage":true}}`)},
				{output: test.output, err: test.err},
			}}
			client := &github.GhClient{Runner: runner}
			report := doctor.Run(context.Background(), github.StartupChecks(client, github.Repository{Owner: "example", Name: "project"})...)
			result := report.Results[2]
			if !strings.Contains(result.Problem, test.problem) {
				t.Fatalf("label problem = %q, want substring %q", result.Problem, test.problem)
			}
			if result.Action == "" {
				t.Fatal("label action is empty")
			}
		})
	}
}

// TestStartupChecksNeverRenderAnAuthenticationError verifies diagnosis output
// remains content-free even when the command runner reports a sensitive error.
func TestStartupChecksNeverRenderAnAuthenticationError(t *testing.T) {
	secret := "credential-value-must-not-appear"
	runner := &doctorRunner{responses: []doctorResponse{{err: errors.New(secret)}}}
	client := &github.GhClient{Runner: runner}
	report := doctor.Run(context.Background(), github.StartupChecks(client, github.Repository{Owner: "example", Name: "project"})...)
	for _, result := range report.Results {
		if strings.Contains(result.Problem+result.Action, secret) {
			t.Fatalf("diagnosis result exposed authentication error: %#v", result)
		}
	}
}

// TestGhClientDoctorUsesReadOnlyGitHubOperations verifies diagnosis does not
// create labels or mutate repository state.
func TestGhClientDoctorUsesReadOnlyGitHubOperations(t *testing.T) {
	runner := &doctorRunner{responses: []doctorResponse{
		{output: []byte("authenticated\n")},
		{output: []byte(`{"permissions":{"pull":true,"push":true,"triage":true}}`)},
		{output: []byte(`[[{"name":"agent-ready"},{"name":"agent-running"},{"name":"agent-needs-input"},{"name":"agent-failed"},{"name":"agent-cancelled"},{"name":"agent-complete"}]]`)},
	}}
	client := &github.GhClient{Runner: runner}
	if err := client.CheckAuthentication(context.Background()); err != nil {
		t.Fatalf("CheckAuthentication() error = %v", err)
	}
	if err := client.CheckRepositoryAccess(context.Background(), github.Repository{Owner: "example", Name: "project"}); err != nil {
		t.Fatalf("CheckRepositoryAccess() error = %v", err)
	}
	if err := client.CheckFactoryLabels(context.Background(), github.Repository{Owner: "example", Name: "project"}); err != nil {
		t.Fatalf("CheckFactoryLabels() error = %v", err)
	}
	joined := ""
	for _, call := range runner.calls {
		joined += strings.Join(call, " ") + "\n"
	}
	if strings.Contains(joined, "label create") || strings.Contains(joined, "--method POST") || strings.Contains(joined, "--method PUT") {
		t.Fatalf("doctor used a mutating GitHub operation: %q", joined)
	}
}

// doctorResponse is one deterministic GitHub command result.
type doctorResponse struct {
	output []byte
	err    error
}

// doctorRunner records GitHub diagnosis calls and returns queued responses.
type doctorRunner struct {
	responses []doctorResponse
	calls     [][]string
}

// Run implements the GitHub command runner for diagnosis tests.
func (r *doctorRunner) Run(_ context.Context, args []string, _ []byte) ([]byte, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(r.responses) == 0 {
		return nil, errors.New("unexpected GitHub command")
	}
	response := r.responses[0]
	r.responses = r.responses[1:]
	return response.output, response.err
}

var _ github.CommandRunner = (*doctorRunner)(nil)
