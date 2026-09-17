package factory

import (
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestResumeAdmissionReasonCoversEveryPolicyBranch verifies the complete
// resume admission table, including every recoverable pause and every refusal.
func TestResumeAdmissionReasonCoversEveryPolicyBranch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  store.Run
		want bool
	}{
		{
			name: "terminal status",
			run:  store.Run{Status: store.StatusComplete},
			want: false,
		},
		{
			name: "harness capacity pause",
			run: store.Run{
				Status:          store.StatusWaitingForHarness,
				LifecycleReason: LifecycleReasonHarnessCapacityUnavailable + " (codex); waiting for capacity",
			},
			want: true,
		},
		{
			name: "unrelated harness pause",
			run: store.Run{
				Status:          store.StatusWaitingForHarness,
				LifecycleReason: "worker stopped for maintenance",
			},
			want: false,
		},
		{
			name: "pending clarification",
			run: store.Run{
				Status:           store.StatusWaitingForHuman,
				LifecycleReason:  LifecycleReasonHarnessAuthenticationExpired + " (codex); refresh required",
				PendingQuestions: []store.PendingQuestion{{ID: "question-1", Prompt: "choose one"}},
			},
			want: false,
		},
		{
			name: "authentication pause",
			run: store.Run{
				Status:          store.StatusWaitingForHuman,
				LifecycleReason: LifecycleReasonHarnessAuthenticationExpired + " (codex); refresh required",
			},
			want: true,
		},
		{
			name: "automatic recovery pause",
			run: store.Run{
				Status:          store.StatusWaitingForHuman,
				LifecycleReason: LifecycleReasonAutomaticHarnessRecoveryExhausted + " (codex); manual native resume required",
			},
			want: true,
		},
		{
			name: "other status",
			run:  store.Run{Status: store.StatusActive},
			want: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			admitted := resumeAdmissionReason(test.run) == ""
			if admitted != test.want {
				t.Fatalf("resumeAdmissionReason(%#v) admitted = %t, want %t", test.run, admitted, test.want)
			}
		})
	}
}

// TestResumeCommandStatusCommentNamesTheRecoverablePause verifies the
// supervision comment closes the discovery loop for explicit recovery.
func TestResumeCommandStatusCommentNamesTheRecoverablePause(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		run    store.Run
		wants  []string
		absent string
	}{
		{
			name: "capacity",
			run: store.Run{
				Status:          store.StatusWaitingForHarness,
				LifecycleReason: LifecycleReasonHarnessCapacityUnavailable + " (codex)",
			},
			wants: []string{"/factory resume"},
		},
		{
			name: "authentication",
			run: store.Run{
				Status:          store.StatusWaitingForHuman,
				LifecycleReason: LifecycleReasonHarnessAuthenticationExpired + " (codex)",
			},
			wants: []string{"factory auth refresh --resume", "/factory resume"},
		},
		{
			name: "clarification remains answer-only",
			run: store.Run{
				Status:           store.StatusWaitingForHuman,
				LifecycleReason:  LifecycleReasonHarnessAuthenticationExpired + " (codex)",
				PendingQuestions: []store.PendingQuestion{{ID: "question-1", Prompt: "choose one"}},
			},
			wants:  nil,
			absent: "/factory resume",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := statusCommentBody(test.run)
			for _, want := range test.wants {
				if !strings.Contains(body, want) {
					t.Fatalf("status comment = %q, want %q", body, want)
				}
			}
			if test.absent != "" && strings.Contains(body, test.absent) {
				t.Fatalf("status comment = %q, does not want %q", body, test.absent)
			}
		})
	}
}
