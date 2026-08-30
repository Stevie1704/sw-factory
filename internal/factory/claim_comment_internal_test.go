package factory

import (
	"context"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/github"
)

// TestClaimCommentWatermarkFailsClosedWithoutCommentReader verifies a claim
// cannot proceed without the command cutoff reader required by every run.
func TestClaimCommentWatermarkFailsClosedWithoutCommentReader(t *testing.T) {
	t.Parallel()

	_, err := (&Service{}).claimCommentWatermark(context.Background(), github.Repository{}, 42)
	if err == nil || !strings.Contains(err.Error(), "comment reader is required") {
		t.Fatalf("claimCommentWatermark() error = %v, want missing comment-reader failure", err)
	}
}
