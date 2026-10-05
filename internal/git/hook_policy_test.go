package git_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
)

// hookedCommandNames are the client hooks reachable through the factory's
// worktree, checkpoint, base synchronization, push, and inspection operations,
// plus the fsmonitor command that status reads would run.
var hookedCommandNames = []string{
	"post-checkout",
	"pre-commit",
	"prepare-commit-msg",
	"commit-msg",
	"post-commit",
	"pre-merge-commit",
	"post-merge",
	"pre-push",
	"reference-transaction",
	"fsmonitor",
}

// TestFactoryGitOperationsNeverRunRepositoryHooks verifies that hooks and the
// fsmonitor command selected through repository, global, or ambient Git
// configuration do not run through any factory operation, while a developer's
// manual Git usage in the same checkout still runs them.
func TestFactoryGitOperationsNeverRunRepositoryHooks(t *testing.T) {
	scopes := map[string]func(t *testing.T, repository string){
		// The relative path addresses the tracked, worker-editable directory.
		"repository relative": func(t *testing.T, repository string) {
			runGit(t, repository, "config", "core.hooksPath", ".hooks")
			runGit(t, repository, "config", "core.fsmonitor", ".hooks/fsmonitor")
		},
		"global absolute": func(t *testing.T, repository string) {
			global := filepath.Join(filepath.Dir(repository), "global.gitconfig")
			hooks := filepath.Join(repository, ".hooks")
			content := "[core]\n\thooksPath = " + hooks + "\n\tfsmonitor = " + filepath.Join(hooks, "fsmonitor") + "\n"
			if err := os.WriteFile(global, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GIT_CONFIG_GLOBAL", global)
		},
		"ambient environment": func(t *testing.T, repository string) {
			hooks := filepath.Join(repository, ".hooks")
			t.Setenv("GIT_CONFIG_COUNT", "2")
			t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
			t.Setenv("GIT_CONFIG_VALUE_0", hooks)
			t.Setenv("GIT_CONFIG_KEY_1", "core.fsmonitor")
			t.Setenv("GIT_CONFIG_VALUE_1", filepath.Join(hooks, "fsmonitor"))
		},
		"ambient parameters": func(t *testing.T, repository string) {
			hooks := filepath.Join(repository, ".hooks")
			t.Setenv("GIT_CONFIG_PARAMETERS", "'core.hooksPath'='"+hooks+"' 'core.fsmonitor'='"+filepath.Join(hooks, "fsmonitor")+"'")
		},
	}
	for name, enableHooks := range scopes {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			repository := filepath.Join(root, "project")
			markers := filepath.Join(root, "markers")
			initializeHookedRepository(t, repository, filepath.Join(root, "project-origin.git"), markers)
			enableHooks(t, repository)

			exerciseFactoryGitOperations(t, repository, filepath.Join(root, "worktrees"))

			if ran := ranHooks(t, markers); len(ran) != 0 {
				t.Fatalf("factory Git operations ran repository hooks %v", ran)
			}
			runGit(t, repository, "commit", "--allow-empty", "-m", "manual developer commit")
			if ran := ranHooks(t, markers); !slices.Contains(ran, "pre-commit") {
				t.Fatalf("manual commit ran hooks %v, want the developer's pre-commit hook", ran)
			}
		})
	}
}

// initializeHookedRepository creates a pushed repository whose tracked .hooks
// directory records every hook invocation in markers, plus a remote target
// branch that has advanced past main so base synchronization merges.
func initializeHookedRepository(t *testing.T, repository, remote, markers string) {
	t.Helper()
	initializeGitRepository(t, repository, remote)
	// The local remote stands in for GitHub, whose server-side hooks belong to
	// the remote rather than to the factory's checkout. Git starts a local
	// remote's receive-pack without the caller's command-line configuration, so
	// the remote keeps its own hook setting instead of the test's global one.
	runGit(t, remote, "config", "core.hooksPath", os.DevNull)
	if err := os.MkdirAll(markers, 0o755); err != nil {
		t.Fatal(err)
	}
	hooks := filepath.Join(repository, ".hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range hookedCommandNames {
		script := "#!/bin/sh\ntouch '" + filepath.Join(markers, name) + "'\n"
		if err := os.WriteFile(filepath.Join(hooks, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, repository, "add", ".hooks")
	runGit(t, repository, "commit", "-m", "add repository hooks")
	runGit(t, repository, "push", "origin", "main")
	runGit(t, repository, "switch", "-c", "next")
	if err := os.WriteFile(filepath.Join(repository, "target.txt"), []byte("target update\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "target.txt")
	runGit(t, repository, "commit", "-m", "advance target")
	runGit(t, repository, "push", "origin", "next")
	runGit(t, repository, "switch", "main")
}

// exerciseFactoryGitOperations runs every hook-relevant factory operation once
// and checks each still produces its ordinary result under the hook policy.
func exerciseFactoryGitOperations(t *testing.T, repository, worktrees string) {
	t.Helper()
	ctx := context.Background()
	manager := &gitadapter.LocalWorktreeManager{WorktreeDir: worktrees}
	workspace, err := manager.Create(ctx, repository, "main", "run-hooks")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Worktree, "test.txt"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testCheckpoint, err := manager.CreateCheckpoint(ctx, gitadapter.CheckpointRequest{
		RunID: "run-hooks", WorktreePath: workspace.Worktree, ParentSHA: workspace.BaseSHA,
		Kind: gitadapter.CheckpointKindTest, Paths: []string{"test.txt"},
	})
	if err != nil {
		t.Fatalf("CreateCheckpoint(test) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Worktree, "implementation.txt"), []byte("implementation\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	implementation := gitadapter.CheckpointRequest{RunID: "run-hooks", WorktreePath: workspace.Worktree, ParentSHA: testCheckpoint.SHA}
	created, err := manager.CreateCheckpoint(ctx, implementation)
	if err != nil || !created.Created {
		t.Fatalf("CreateCheckpoint(implementation) = %#v, %v, want a created checkpoint", created, err)
	}
	recovered, err := manager.CreateCheckpoint(ctx, implementation)
	if err != nil || recovered.Created || recovered.SHA != created.SHA {
		t.Fatalf("repeated CreateCheckpoint() = %#v, %v, want recovery of %s", recovered, err, created.SHA)
	}
	if err := manager.SynchronizeBase(ctx, gitadapter.BaseSyncRequest{WorktreePath: workspace.Worktree, TargetBranch: "next"}); err != nil {
		t.Fatalf("SynchronizeBase() error = %v", err)
	}
	state, err := manager.Inspect(ctx, workspace.Worktree)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if err := manager.Push(ctx, gitadapter.PushRequest{WorktreePath: workspace.Worktree, Branch: workspace.Branch}); err != nil {
		t.Fatalf("Push() error = %v", err)
	}
	remoteHead, err := manager.RemoteBranchHead(ctx, gitadapter.PushRequest{WorktreePath: workspace.Worktree, Branch: workspace.Branch})
	if err != nil || remoteHead != state.HeadSHA {
		t.Fatalf("RemoteBranchHead() = %q, %v, want pushed merge %q", remoteHead, err, state.HeadSHA)
	}
}

// ranHooks lists the hooks that have left a marker so far.
func ranHooks(t *testing.T, markers string) []string {
	t.Helper()
	entries, err := os.ReadDir(markers)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// TestFactoryGitPushKeepsTransportAuthenticationWithoutLeakingCredentials
// verifies the hook policy leaves the operator's transport command in place
// and that a failed push does not report a credential embedded in the URL.
func TestFactoryGitPushKeepsTransportAuthenticationWithoutLeakingCredentials(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	repository := filepath.Join(root, "project")
	initializeGitRepository(t, repository, filepath.Join(root, "project-origin.git"))
	transportMarker := filepath.Join(root, "ssh-command-ran")
	sshCommand := filepath.Join(root, "ssh-command")
	if err := os.WriteFile(sshCommand, []byte("#!/bin/sh\ntouch '"+transportMarker+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "config", "core.sshCommand", sshCommand)
	manager := &gitadapter.LocalWorktreeManager{}
	push := gitadapter.PushRequest{WorktreePath: repository, Branch: "main"}

	runGit(t, repository, "remote", "set-url", "origin", "ssh://example.invalid/project.git")
	if err := manager.Push(context.Background(), push); err == nil {
		t.Fatal("Push() through the failing transport command error = nil")
	}
	if _, err := os.Stat(transportMarker); err != nil {
		t.Fatalf("configured core.sshCommand did not run under the hook policy: %v", err)
	}

	runGit(t, repository, "remote", "set-url", "origin", "https://factory:secret-token@127.0.0.1:1/project.git")
	err := manager.Push(context.Background(), push)
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("Push() to an unreachable authenticated remote error = %v, want a failure without the credential", err)
	}
}
