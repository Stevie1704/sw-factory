package git

import (
	"context"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/doctor"
)

// TestRemoteDiagnosisPreservesTheSpecificFailureCategory verifies the Git
// contributor does not collapse independent remote failures into one action.
func TestRemoteDiagnosisPreservesTheSpecificFailureCategory(t *testing.T) {
	for _, test := range []struct {
		kind    remoteFailureKind
		problem string
	}{
		{kind: remoteFailureFetchIdentity, problem: "fetch remote"},
		{kind: remoteFailurePushIdentity, problem: "push remote"},
		{kind: remoteFailureBranchNoCommit, problem: "has no commit"},
	} {
		t.Run(string(test.kind), func(t *testing.T) {
			report := doctor.Run(context.Background(), StartupChecks(&remoteFailureChecker{err: remoteFailure(test.kind)}, DoctorRequest{})[0])
			result := report.Results[0]
			if result.Status != doctor.StatusFailed || !strings.Contains(result.Problem, test.problem) || result.Action == "" {
				t.Fatalf("remote result = %#v, want specific problem/action", result)
			}
		})
	}
}

// remoteFailureChecker supplies a categorized remote result without invoking
// the host Git executable.
type remoteFailureChecker struct {
	err error
}

// CheckRemote implements the Git remote diagnosis seam for category tests.
func (c *remoteFailureChecker) CheckRemote(context.Context, DoctorRequest) error {
	return c.err
}

// CheckHooks implements the required Git diagnosis seam for category tests.
func (*remoteFailureChecker) CheckHooks(context.Context, DoctorRequest) error {
	return nil
}

// CheckWorktree implements the required Git diagnosis seam for category tests.
func (*remoteFailureChecker) CheckWorktree(context.Context, DoctorRequest) error {
	return nil
}

var _ DoctorChecker = (*remoteFailureChecker)(nil)
