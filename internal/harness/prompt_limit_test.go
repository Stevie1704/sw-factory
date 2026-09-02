package harness_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/terminal"
)

// TestLaunchRefusesAPromptLargerThanOneCommandArgument verifies both adapters
// refuse a prompt the kernel would reject at exec. Linux caps one argument at
// 32 pages, and the resulting E2BIG reaches the coordinator only as an opaque
// exec failure followed by an expired native-session deadline.
func TestLaunchRefusesAPromptLargerThanOneCommandArgument(t *testing.T) {
	oversized := strings.Repeat("x", 200<<10)
	for _, name := range []string{"codex", "claude"} {
		for _, resume := range []bool{false, true} {
			mode := "start"
			if resume {
				mode = "resume"
			}
			t.Run(name+"/"+mode, func(t *testing.T) {
				workerRuntime := &fakeWorker{}
				terminalRuntime := &fakeTerminal{surface: terminal.Surface{ID: "surface-review", WorkspaceID: "workspace-run", Name: name}}
				request := harness.StartRequest{
					InvocationID: "inv-review",
					RunID:        "run-review",
					Role:         "spec_review",
					Stage:        "review",
					WorkspaceID:  "workspace-run",
					Surface:      terminalRuntime.surface,
					Prompt:       oversized,
				}
				var err error
				switch {
				case name == "codex" && resume:
					request.ResumeSessionID = "session-1"
					_, err = harness.NewCodex(workerRuntime, terminalRuntime).Resume(context.Background(), request)
				case name == "codex":
					_, err = harness.NewCodex(workerRuntime, terminalRuntime).Start(context.Background(), request)
				case resume:
					request.ResumeSessionID = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
					_, err = harness.NewClaude(workerRuntime, terminalRuntime).Resume(context.Background(), request)
				default:
					_, err = harness.NewClaude(workerRuntime, terminalRuntime).Start(context.Background(), request)
				}
				var tooLarge *harness.PromptTooLargeError
				if !errors.As(err, &tooLarge) {
					t.Fatalf("launch error = %v, want a typed prompt-size refusal", err)
				}
				if tooLarge.Bytes != len(oversized) || tooLarge.Limit >= tooLarge.Bytes {
					t.Fatalf("refusal = %#v, want the observed size above the named limit", tooLarge)
				}
				if !strings.Contains(tooLarge.Error(), "launch limit") {
					t.Fatalf("refusal message = %q, want the limit named", tooLarge.Error())
				}
				// The refusal happens before any worker command, so no surface
				// or session is left behind for a human to clean up.
				if len(workerRuntime.requests) != 0 {
					t.Fatalf("worker requests = %#v, want the launch refused before execution", workerRuntime.requests)
				}
			})
		}
	}
}

// TestLaunchAcceptsAPromptWithinTheArgumentLimit verifies the guard bounds only
// the prompts a harness genuinely cannot receive.
func TestLaunchAcceptsAPromptWithinTheArgumentLimit(t *testing.T) {
	workerRuntime := &fakeWorker{}
	terminalRuntime := &fakeTerminal{surface: terminal.Surface{ID: "surface-implementation", WorkspaceID: "workspace-run", Name: "implementation"}}
	if _, err := harness.NewCodex(workerRuntime, terminalRuntime).Start(context.Background(), harness.StartRequest{
		InvocationID: "inv-1",
		RunID:        "run-1",
		Role:         "implementation",
		Stage:        "implementation",
		WorkspaceID:  "workspace-run",
		Surface:      terminalRuntime.surface,
		Prompt:       strings.Repeat("y", 64<<10),
	}); err != nil {
		t.Fatalf("Start() error = %v, want a prompt of ordinary size accepted", err)
	}
}
