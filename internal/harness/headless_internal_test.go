package harness

import (
	"errors"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestClassifyHeadlessEventsRecognizesTurnFailedMessages verifies the event
// shape documented for non-interactive Codex failures, where the stable
// category is carried by error.message rather than an error code field.
func TestClassifyHeadlessEventsRecognizesTurnFailedMessages(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want error
	}{
		{name: "rate limit", body: `{"type":"turn.failed","error":{"message":"request exceeded the rate limit"}}`, want: ErrRateLimited},
		{name: "authentication", body: `{"type":"turn.failed","error":{"message":"authentication token expired"}}`, want: ErrAuthenticationExpired},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := classifyHeadlessEvents(test.body)
			if failure == nil || !errors.Is(failure, test.want) {
				t.Fatalf("classifyHeadlessEvents() = %v, want %v", failure, test.want)
			}
		})
	}
}

// TestClassifyHeadlessInspectionIncludesStderr verifies adapter-owned stream
// classification does not lose a structured failure emitted on stderr.
func TestClassifyHeadlessInspectionIncludesStderr(t *testing.T) {
	failure := classifyHeadlessInspection(HeadlessInspection{
		Status: worker.HeadlessStatusExited,
		Stderr: `{"type":"turn.failed","error":{"message":"authentication token expired"}}`,
	})
	if failure == nil || !errors.Is(failure, ErrAuthenticationExpired) {
		t.Fatalf("classifyHeadlessInspection() = %v, want authentication-expired outcome", failure)
	}
}
