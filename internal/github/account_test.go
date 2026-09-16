package github_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/github"
)

// accountRunner answers one gh account probe with a fixed response.
type accountRunner struct {
	output []byte
	err    error
	args   []string
}

// Run records the requested gh arguments and returns the fixed response.
func (r *accountRunner) Run(_ context.Context, args []string, _ []byte) ([]byte, error) {
	r.args = args
	return r.output, r.err
}

// TestAuthenticatedLoginReadsTheAuthenticatedAccount verifies the login comes
// from the authenticated GitHub CLI account without reading its credential.
func TestAuthenticatedLoginReadsTheAuthenticatedAccount(t *testing.T) {
	t.Parallel()

	runner := &accountRunner{output: []byte(`{"login":"alice","id":42}`)}
	client := &github.GhClient{Runner: runner}
	login, err := client.AuthenticatedLogin(context.Background())
	if err != nil {
		t.Fatalf("AuthenticatedLogin() error = %v", err)
	}
	if login != "alice" {
		t.Fatalf("login = %q, want alice", login)
	}
	if strings.Join(runner.args, " ") != "api user" {
		t.Fatalf("gh args = %v, want api user", runner.args)
	}
}

// TestAuthenticatedLoginFailsClosed verifies an unavailable or unsafe account
// response is rejected with an actionable diagnosis that omits gh output.
func TestAuthenticatedLoginFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		runner *accountRunner
	}{
		{name: "unauthenticated", runner: &accountRunner{err: errors.New("gh auth token secret")}},
		{name: "malformed response", runner: &accountRunner{output: []byte(`{"login":`)}},
		{name: "empty login", runner: &accountRunner{output: []byte(`{"login":"  "}`)}},
		{name: "unsafe login", runner: &accountRunner{output: []byte(`{"login":"example/alice"}`)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			client := &github.GhClient{Runner: tc.runner}
			_, err := client.AuthenticatedLogin(context.Background())
			if err == nil {
				t.Fatal("AuthenticatedLogin() error = nil, want a fail-closed diagnosis")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error = %q, want it to omit gh output", err.Error())
			}
			if !strings.Contains(err.Error(), "gh auth login") {
				t.Fatalf("error = %q, want an actionable gh auth login instruction", err.Error())
			}
		})
	}
}
