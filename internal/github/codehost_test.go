package github_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// TestCodeHostDiagnosisNeverRequiresIssues checks the independent host seam.
func TestCodeHostDiagnosisNeverRequiresIssues(t *testing.T) {
	runner := &fakeCommandRunner{outputs: [][]byte{
		[]byte("authenticated"),
		[]byte(`{"private":false,"default_branch":"main","permissions":{"pull":true,"push":true}}`),
		[]byte(`{"hosts":{"github.com":[{"active":true,"state":"success","scopes":"public_repo"}]}}`),
		[]byte(`[]`),
		[]byte(`{"commit":{"sha":"1111111111111111111111111111111111111111"}}`),
		[]byte(`{"statuses":[]}`),
	}}
	client := &github.GhClient{Runner: runner}
	report := doctor.Run(context.Background(), client.CodeHostStartupChecks(tracker.Repository{Owner: "example", Name: "project"})...)
	if !report.Ready() {
		t.Fatalf("code-host diagnosis = %#v", report)
	}
	for _, call := range runner.calls {
		joined := strings.Join(call.args, " ")
		if strings.Contains(joined, "issues") || strings.Contains(joined, "labels") {
			t.Fatalf("code host required issue operations: %s", joined)
		}
	}
}
