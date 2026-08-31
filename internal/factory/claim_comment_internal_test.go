package factory

import (
	"context"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/github"
)

// TestSafeStatusCommentValueReplacesC1Controls verifies that every Unicode
// control character is made safe before untrusted text reaches a status comment.
func TestSafeStatusCommentValueReplacesC1Controls(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		value string
	}{
		{name: "next line", value: "before\u0085after"},
		{name: "control sequence introducer", value: "before\u009bafter"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got, want := safeStatusCommentValue(test.value), "before after"; got != want {
				t.Fatalf("safeStatusCommentValue(%q) = %q, want %q", test.value, got, want)
			}
		})
	}
}

// TestClaimCommentWatermarkFailsClosedWithoutCommentReader verifies a claim
// cannot proceed without the command cutoff reader required by every run.
func TestClaimCommentWatermarkFailsClosedWithoutCommentReader(t *testing.T) {
	t.Parallel()

	_, err := (&Service{}).claimCommentWatermark(context.Background(), github.Repository{}, 42)
	if err == nil || !strings.Contains(err.Error(), "comment reader is required") {
		t.Fatalf("claimCommentWatermark() error = %v, want missing comment-reader failure", err)
	}
}
