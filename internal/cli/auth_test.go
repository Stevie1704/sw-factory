package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestAuthRefreshResumeFlagPassesThroughAndReportsNativeContinuation
// verifies both the opt-in request field and its dedicated CLI output line.
func TestAuthRefreshResumeFlagPassesThroughAndReportsNativeContinuation(t *testing.T) {
	original := authRefreshForCLI
	t.Cleanup(func() { authRefreshForCLI = original })
	var received factory.AuthRefreshRequest
	authRefreshForCLI = func(_ context.Context, _ string, request factory.AuthRefreshRequest) (factory.AuthRefreshResult, error) {
		received = request
		return factory.AuthRefreshResult{
			Run:        store.Run{ID: "run-cli-resume"},
			Invocation: store.Invocation{ID: "inv-cli-resume"},
			Harness:    config.HarnessCodex,
			Resumed:    true,
		}, nil
	}

	var output bytes.Buffer
	code := runAuth(context.Background(), []string{"refresh", "--config", "/tmp/factory.yaml", "--run-id", "run-cli-resume", "--resume"}, "", &output, &output)
	if code != 0 {
		t.Fatalf("runAuth() exit code = %d, output = %q", code, output.String())
	}
	if !received.Resume {
		t.Fatalf("auth refresh request = %#v, want Resume true", received)
	}
	if !strings.Contains(output.String(), "resumed: true") {
		t.Fatalf("auth refresh output = %q, want resumed result", output.String())
	}
}
