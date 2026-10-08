package git

import (
	"slices"
	"testing"
	"time"
)

// TestHostCommandSetsTheDeadlineForEachSubcommand verifies that remote
// transfers get the long deadline and every other host Git command gets the
// short one.
func TestHostCommandSetsTheDeadlineForEachSubcommand(t *testing.T) {
	cases := map[string]time.Duration{
		"fetch":     10 * time.Minute,
		"push":      10 * time.Minute,
		"status":    2 * time.Minute,
		"ls-remote": 2 * time.Minute,
		"rev-parse": 2 * time.Minute,
	}
	for subcommand, want := range cases {
		command := HostCommand("/repository", []string{subcommand, "origin"})
		if command.Timeout != want {
			t.Errorf("HostCommand(%q) timeout = %s, want %s", subcommand, command.Timeout, want)
		}
		if command.Operation != "git "+subcommand {
			t.Errorf("HostCommand(%q) operation = %q, want %q", subcommand, command.Operation, "git "+subcommand)
		}
	}
}

// TestHostCommandRunsGitNonInteractivelyUnderTheHookPolicy verifies that a
// host Git command can neither prompt on the terminal nor start an askpass
// program, and that it keeps the factory hook policy.
func TestHostCommandRunsGitNonInteractivelyUnderTheHookPolicy(t *testing.T) {
	command := HostCommand("/repository", []string{"fetch", "origin"})

	if command.Name != "git" || command.Dir != "/repository" {
		t.Fatalf("HostCommand() = %#v, want git in the repository directory", command)
	}
	if !slices.Equal(command.Args, HostCommandArgs("fetch", "origin")) {
		t.Fatalf("HostCommand() args = %q, want the hook policy before the subcommand", command.Args)
	}
	for _, entry := range []string{"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS="} {
		if !slices.Contains(command.Env, entry) {
			t.Errorf("HostCommand() env = %q, want %q", command.Env, entry)
		}
	}
}
