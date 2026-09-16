package git_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
)

// discoveryRunner answers the three read-only Git probes used by repository
// discovery and records the directory each probe ran in.
type discoveryRunner struct {
	root        string
	fetchURL    string
	pushURL     string
	fetchErr    error
	pushErr     error
	rootErr     error
	directories []string
	commands    []string
}

// Run returns the configured discovery answers without touching a checkout.
func (r *discoveryRunner) Run(_ context.Context, directory string, args []string) ([]byte, error) {
	r.directories = append(r.directories, directory)
	r.commands = append(r.commands, strings.Join(args, " "))
	switch strings.Join(args, " ") {
	case "rev-parse --show-toplevel":
		if r.rootErr != nil {
			return nil, r.rootErr
		}
		return []byte(r.root + "\n"), nil
	case "remote get-url origin":
		if r.fetchErr != nil {
			return nil, r.fetchErr
		}
		return []byte(r.fetchURL + "\n"), nil
	case "remote get-url --push origin":
		if r.pushErr != nil {
			return nil, r.pushErr
		}
		return []byte(r.pushURL + "\n"), nil
	}
	return nil, fmt.Errorf("unexpected git command %q", strings.Join(args, " "))
}

// TestDiscoverRepositoryAcceptsSupportedRemoteForms verifies discovery reads
// the checkout root and GitHub identity from every remote URL form Git emits.
func TestDiscoverRepositoryAcceptsSupportedRemoteForms(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	tests := []struct {
		name   string
		remote string
	}{
		{name: "https", remote: "https://github.com/example/project.git"},
		{name: "https without suffix", remote: "https://github.com/example/project"},
		{name: "scp-like ssh", remote: "git@github.com:example/project.git"},
		{name: "ssh url", remote: "ssh://git@github.com/example/project.git"},
		{name: "mixed case host", remote: "https://GitHub.com/example/project.git"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			runner := &discoveryRunner{root: root, fetchURL: tc.remote, pushURL: tc.remote}
			manager := &gitadapter.LocalWorktreeManager{Runner: runner}
			discovery, err := manager.DiscoverRepository(context.Background(), root)
			if err != nil {
				t.Fatalf("DiscoverRepository() error = %v", err)
			}
			if discovery.Root != root {
				t.Fatalf("Root = %q, want %q", discovery.Root, root)
			}
			if discovery.Owner != "example" || discovery.Repository != "project" {
				t.Fatalf("identity = %q/%q, want example/project", discovery.Owner, discovery.Repository)
			}
		})
	}
}

// TestDiscoverRepositoryResolvesTheRootFromASubdirectory verifies discovery
// probes Git in the requested directory and returns the checkout root.
func TestDiscoverRepositoryResolvesTheRootFromASubdirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	subdirectory := filepath.Join(root, "internal", "cli")
	runner := &discoveryRunner{root: root, fetchURL: "https://github.com/example/project.git", pushURL: "https://github.com/example/project.git"}
	manager := &gitadapter.LocalWorktreeManager{Runner: runner}
	discovery, err := manager.DiscoverRepository(context.Background(), subdirectory)
	if err != nil {
		t.Fatalf("DiscoverRepository() error = %v", err)
	}
	if discovery.Root != root {
		t.Fatalf("Root = %q, want %q", discovery.Root, root)
	}
	for _, directory := range runner.directories {
		if directory != subdirectory {
			t.Fatalf("git ran in %q, want %q", directory, subdirectory)
		}
	}
}

// TestDiscoverRepositoryFailsClosed verifies every unsafe or missing Git and
// GitHub identity is rejected with a bounded, actionable diagnosis.
func TestDiscoverRepositoryFailsClosed(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	tests := []struct {
		name   string
		runner *discoveryRunner
		want   string
	}{
		{
			name:   "outside a checkout",
			runner: &discoveryRunner{rootErr: errors.New("not a git repository")},
			want:   "not inside a Git checkout",
		},
		{
			name:   "empty root",
			runner: &discoveryRunner{root: "  "},
			want:   "not inside a Git checkout",
		},
		{
			name:   "missing remote",
			runner: &discoveryRunner{root: root, fetchErr: errors.New("no such remote")},
			want:   "has no origin remote",
		},
		{
			name:   "non-github remote",
			runner: &discoveryRunner{root: root, fetchURL: "https://gitlab.com/example/project.git", pushURL: "https://gitlab.com/example/project.git"},
			want:   "does not identify a GitHub repository",
		},
		{
			name:   "malformed remote",
			runner: &discoveryRunner{root: root, fetchURL: "github.com-example-project", pushURL: "github.com-example-project"},
			want:   "does not identify a GitHub repository",
		},
		{
			name:   "unreadable push remote",
			runner: &discoveryRunner{root: root, fetchURL: "https://github.com/example/project.git", pushErr: errors.New("no push url")},
			want:   "push URL cannot be read",
		},
		{
			name:   "mismatched fetch and push identities",
			runner: &discoveryRunner{root: root, fetchURL: "https://github.com/example/project.git", pushURL: "https://github.com/other/project.git"},
			want:   "fetch and push URLs identify different GitHub repositories",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			manager := &gitadapter.LocalWorktreeManager{Runner: tc.runner}
			_, err := manager.DiscoverRepository(context.Background(), root)
			if err == nil {
				t.Fatal("DiscoverRepository() error = nil, want a fail-closed diagnosis")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestDiscoverRepositoryRequiresAWorkingDirectory verifies discovery refuses to
// run Git without an explicit directory.
func TestDiscoverRepositoryRequiresAWorkingDirectory(t *testing.T) {
	t.Parallel()

	runner := &discoveryRunner{}
	manager := &gitadapter.LocalWorktreeManager{Runner: runner}
	if _, err := manager.DiscoverRepository(context.Background(), "  "); err == nil {
		t.Fatal("DiscoverRepository() error = nil, want a rejected working directory")
	}
	if len(runner.commands) != 0 {
		t.Fatalf("git commands = %v, want none", runner.commands)
	}
}
