package github

import (
	"slices"
	"testing"
	"time"
)

// TestHostCommandRunsGhNonInteractivelyWithADeadline verifies that every gh
// call has the two-minute deadline, cannot prompt, and receives its input.
func TestHostCommandRunsGhNonInteractivelyWithADeadline(t *testing.T) {
	command := hostCommand([]string{"api", "repos/example/project"}, []byte(`{"body":"x"}`))

	if command.Name != "gh" || command.Operation != "gh api" {
		t.Fatalf("hostCommand() = %#v, want gh with operation %q", command, "gh api")
	}
	if !slices.Equal(command.Args, []string{"api", "repos/example/project"}) {
		t.Fatalf("hostCommand() args = %q, want the gh arguments unchanged", command.Args)
	}
	if string(command.Stdin) != `{"body":"x"}` {
		t.Fatalf("hostCommand() stdin = %q, want the request body", command.Stdin)
	}
	if command.Timeout != 2*time.Minute {
		t.Fatalf("hostCommand() timeout = %s, want 2m0s", command.Timeout)
	}
	if !slices.Contains(command.Env, "GH_PROMPT_DISABLED=1") {
		t.Fatalf("hostCommand() env = %q, want GH_PROMPT_DISABLED=1", command.Env)
	}
}
