package worker_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestDockerRuntimeMountsInvocationAndResultDirectories verifies that the
// read-only packet and writable result directory are explicit worker mounts.
func TestDockerRuntimeMountsInvocationAndResultDirectories(t *testing.T) {
	stub, logPath, _ := writeDockerStub(t)
	worktree := makeDirectory(t, "worktree")
	gitMetadata := makeDirectory(t, "git-metadata")
	invocation := makeDirectory(t, "invocation")
	results := makeDirectory(t, "results")
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	if err := runtime.Start(context.Background(), worker.StartRequest{
		RunID: "run-report-mounts", WorktreePath: worktree, GitMetadataPath: gitMetadata,
		InvocationPath: invocation, ResultPath: results, CredentialStoreID: "registration",
		Role: "implementation", Image: "ghcr.io/example/factory-worker", ImageDigest: testWorkerDigest,
	}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	runLine := findLogLine(t, readStubLog(t, logPath), " run ")
	if !strings.Contains(runLine, "dst=/invocation,readonly") || !strings.Contains(runLine, "dst=/results") {
		t.Fatalf("worker mounts = %q, want read-only invocation and writable results", runLine)
	}
	if !strings.Contains(runLine, "factory-role-implementation") || !strings.Contains(runLine, "dst="+worker.CredentialPath) {
		t.Fatalf("worker mounts = %q, want role session and separate credential volumes", runLine)
	}
}

// TestDockerRuntimeRecreatesAnExistingWorkerForInvocationMounts verifies that
// a gate-created worker is safely rebuilt before a harness invocation starts.
func TestDockerRuntimeRecreatesAnExistingWorkerForInvocationMounts(t *testing.T) {
	stub, logPath, _ := writeDockerStub(t)
	worktree := makeDirectory(t, "worktree")
	gitMetadata := makeDirectory(t, "git-metadata")
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	base := worker.StartRequest{
		RunID: "run-reconfigure", WorktreePath: worktree, GitMetadataPath: gitMetadata,
		CredentialStoreID: "registration", Role: "gate",
		Image: "ghcr.io/example/factory-worker", ImageDigest: testWorkerDigest,
	}
	if err := runtime.Start(context.Background(), base); err != nil {
		t.Fatalf("base Start() error = %v", err)
	}
	inspectionPath := filepath.Join(t.TempDir(), "inspection.json")
	inspection, err := json.Marshal(map[string]any{
		"State":  map[string]bool{"Running": true},
		"Config": map[string]string{"Image": "ghcr.io/example/factory-worker@" + testWorkerDigest},
		"Mounts": []map[string]string{
			{"Type": "bind", "Source": worktree, "Destination": worker.WorktreePath},
			{"Type": "bind", "Source": gitMetadata, "Destination": worker.GitMetadataPath},
			{"Type": "volume", "Name": testRoleVolumeName(base.RunID, base.Role), "Destination": "/home/factory"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inspectionPath, inspection, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORKER_DOCKER_INSPECT_JSON_FILE", inspectionPath)
	if err := runtime.Start(context.Background(), worker.StartRequest{
		RunID: base.RunID, WorktreePath: worktree, GitMetadataPath: gitMetadata,
		InvocationPath: makeDirectory(t, "invocation"), ResultPath: makeDirectory(t, "results"),
		CredentialStoreID: base.CredentialStoreID, Role: "implementation",
		Image: base.Image, ImageDigest: base.ImageDigest,
	}); err != nil {
		t.Fatalf("invocation Start() error = %v", err)
	}
	lines := readStubLog(t, logPath)
	if countLogLines(lines, " run ") != 2 || countLogLines(lines, " rm ") != 1 {
		t.Fatalf("Docker reconfiguration calls = %#v, want stop/rm and replacement run", lines)
	}
	for _, line := range findLogLines(lines, " rm ") {
		if strings.Contains(line, " -v ") {
			t.Fatalf("worker recreation removed named volumes: %q", line)
		}
	}
	if replacement := findLogLines(lines, " run ")[1]; !strings.Contains(replacement, "dst="+worker.CredentialPath) {
		t.Fatalf("replacement worker mounts = %q, want the factory credential projection", replacement)
	}
}

// TestDockerRuntimeSeedsOnlyTheCodexCredentialFile verifies that seeding reads
// one explicit host file and never exposes the host harness directory.
func TestDockerRuntimeSeedsOnlyTheCodexCredentialFile(t *testing.T) {
	stub, logPath, _ := writeDockerStub(t)
	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"access_token":"test-only"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	if err := runtime.Start(context.Background(), worker.StartRequest{
		RunID: "run-auth", WorktreePath: makeDirectory(t, "worktree"), GitMetadataPath: makeDirectory(t, "git-metadata"),
		Role: "implementation", Image: "ghcr.io/example/factory-worker", ImageDigest: testWorkerDigest,
	}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := runtime.SeedCodexCredentials(context.Background(), worker.CredentialSeedRequest{RunID: "run-auth", AuthPath: authPath}); err != nil {
		t.Fatalf("SeedCodexCredentials() error = %v", err)
	}
	joined := strings.Join(readStubLog(t, logPath), "\n")
	if strings.Contains(joined, filepath.Dir(authPath)) || strings.Contains(joined, "docker.sock") || !strings.Contains(joined, worker.CredentialPath+"/auth.json") {
		t.Fatalf("credential seed leaked host harness path or Docker socket: %q", joined)
	}
}

// TestDockerRuntimeSeedsCredentialsWithoutPrivilegedOperations verifies that
// credential seeding writes as the owning users without chown.
func TestDockerRuntimeSeedsCredentialsWithoutPrivilegedOperations(t *testing.T) {
	stub, logPath, _ := writeDockerStub(t)
	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"access_token":"test-only"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	if err := runtime.Start(context.Background(), worker.StartRequest{
		RunID: "run-auth-privileges", WorktreePath: makeDirectory(t, "worktree"), GitMetadataPath: makeDirectory(t, "git-metadata"),
		Role: "implementation", Image: "ghcr.io/example/factory-worker", ImageDigest: testWorkerDigest,
	}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := runtime.SeedCodexCredentials(context.Background(), worker.CredentialSeedRequest{RunID: "run-auth-privileges", AuthPath: authPath}); err != nil {
		t.Fatalf("SeedCodexCredentials() error = %v", err)
	}
	var wroteCredential, linkedRoleHome bool
	for _, line := range readStubLog(t, logPath) {
		if !strings.HasPrefix(line, "exec ") {
			continue
		}
		if strings.Contains(line, "chown") {
			t.Fatalf("credential seed requires a dropped capability: %q", line)
		}
		switch {
		case strings.Contains(line, "--user 0:0"):
			if strings.Contains(line, "$HOME/.codex") {
				t.Fatalf("uid 0 cannot write the factory-owned role home: %q", line)
			}
			wroteCredential = strings.Contains(line, "mv -f \""+worker.CredentialPath+"/auth.json.new\" \""+worker.CredentialPath+"/auth.json\"") || wroteCredential
		case strings.Contains(line, "--user "+worker.WorkerUser):
			linkedRoleHome = strings.Contains(line, "ln -s") && strings.Contains(line, "$HOME/.codex/auth.json") || linkedRoleHome
		}
	}
	if !wroteCredential || !linkedRoleHome {
		t.Fatalf("credential projection ownership = wrote:%t linked:%t, want both", wroteCredential, linkedRoleHome)
	}
}

// TestDockerRuntimeSeedsOnlyTheClaudeCredentialFile verifies Claude is seeded
// from one explicit host file without write-back or host-directory exposure.
func TestDockerRuntimeSeedsOnlyTheClaudeCredentialFile(t *testing.T) {
	stub, logPath, _ := writeDockerStub(t)
	authPath := filepath.Join(t.TempDir(), ".credentials.json")
	contents := []byte(`{"claudeAiOauth":{"accessToken":"test-only"}}`)
	if err := os.WriteFile(authPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	if err := runtime.Start(context.Background(), worker.StartRequest{
		RunID: "run-claude-auth", WorktreePath: makeDirectory(t, "worktree"), GitMetadataPath: makeDirectory(t, "git-metadata"),
		Role: "implementation", Image: "ghcr.io/example/factory-worker", ImageDigest: testWorkerDigest,
	}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := runtime.SeedClaudeCredentials(context.Background(), worker.CredentialSeedRequest{RunID: "run-claude-auth", AuthPath: authPath}); err != nil {
		t.Fatalf("SeedClaudeCredentials() error = %v", err)
	}
	lines := readStubLog(t, logPath)
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, filepath.Dir(authPath)) || strings.Contains(joined, "docker.sock") || !strings.Contains(joined, worker.CredentialPath+"/claude-credentials.json") {
		t.Fatalf("credential seed escaped the factory-managed volume: %q", joined)
	}
	after, err := os.ReadFile(authPath)
	if err != nil || string(after) != string(contents) {
		t.Fatal("credential seed wrote back to the host source")
	}
	var wroteCredential, linkedRoleHome bool
	for _, line := range lines {
		if !strings.HasPrefix(line, "exec ") {
			continue
		}
		if strings.Contains(line, "chown") {
			t.Fatalf("credential seed requires a dropped capability: %q", line)
		}
		wroteCredential = wroteCredential || strings.Contains(line, "--user 0:0") && strings.Contains(line, "mv -f \""+worker.CredentialPath+"/claude-credentials.json.new\" \""+worker.CredentialPath+"/claude-credentials.json\"")
		linkedRoleHome = linkedRoleHome || strings.Contains(line, "--user "+worker.WorkerUser) && strings.Contains(line, "ln -s") && strings.Contains(line, "$HOME/.claude/.credentials.json")
	}
	if !wroteCredential || !linkedRoleHome {
		t.Fatalf("credential projection ownership = wrote:%t linked:%t, want both", wroteCredential, linkedRoleHome)
	}
}

// TestDockerRuntimeRefusesAnUnsafeClaudeCredentialSource verifies Claude
// applies the same source checks as Codex.
func TestDockerRuntimeRefusesAnUnsafeClaudeCredentialSource(t *testing.T) {
	stub, _, _ := writeDockerStub(t)
	root := t.TempDir()
	regular := filepath.Join(root, "regular.json")
	if err := os.WriteFile(regular, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.json")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	for _, test := range []struct{ name, path, problem string }{
		{name: "relative", path: ".credentials.json", problem: "absolute safe path"},
		{name: "symlink", path: link, problem: "symbolic link"},
		{name: "empty", path: regular, problem: "empty"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := runtime.SeedClaudeCredentials(context.Background(), worker.CredentialSeedRequest{RunID: "run-claude-auth", AuthPath: test.path})
			if err == nil || !strings.Contains(err.Error(), test.problem) {
				t.Fatalf("SeedClaudeCredentials() error = %v, want %q refusal", err, test.problem)
			}
		})
	}
}

// TestDockerRuntimeRedactsCredentialSourceAndDockerErrors verifies credential
// paths and bytes cannot escape through worker errors.
func TestDockerRuntimeRedactsCredentialSourceAndDockerErrors(t *testing.T) {
	stub, _, _ := writeDockerStub(t)
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	authPath := filepath.Join(t.TempDir(), "auth.json")
	missingErr := runtime.SeedCodexCredentials(context.Background(), worker.CredentialSeedRequest{RunID: "run-redacted-source", AuthPath: authPath})
	if missingErr == nil || strings.Contains(missingErr.Error(), authPath) {
		t.Fatalf("missing credential error was absent or leaked its path: %v", missingErr)
	}
	if err := os.WriteFile(authPath, []byte("credential-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORKER_DOCKER_EXEC_ERROR", authPath+": credential-secret")
	dockerErr := runtime.SeedCodexCredentials(context.Background(), worker.CredentialSeedRequest{RunID: "run-redacted-docker", AuthPath: authPath})
	if dockerErr == nil || strings.Contains(dockerErr.Error(), authPath) || strings.Contains(dockerErr.Error(), "credential-secret") {
		t.Fatalf("Docker credential projection error was absent or leaked details: %v", dockerErr)
	}
}

// TestDockerRuntimeKeepsACaptureLimitFailureThroughCredentialSeeding verifies
// that an overflow at either projection step stays identifiable instead of
// being reported as a generic projection failure, and still publishes no
// credential path or content.
func TestDockerRuntimeKeepsACaptureLimitFailureThroughCredentialSeeding(t *testing.T) {
	stub, _, _ := writeDockerStub(t)
	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"access_token":"test-only"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	for _, test := range []struct{ name, match, step string }{
		{name: "credential copy", match: "--user 0:0", step: "seed codex credentials"},
		{name: "role home link", match: "ln -s", step: "link codex credentials into the role home"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("WORKER_DOCKER_OVERFLOW_MATCH", test.match)
			t.Setenv("WORKER_DOCKER_OVERFLOW_BYTES", strconv.Itoa(worker.MaxCapturedOutputBytes+1))
			err := runtime.SeedCodexCredentials(context.Background(), worker.CredentialSeedRequest{
				RunID: "run-credential-overflow", AuthPath: authPath,
			})
			assertCaptureLimitFailure(t, err, authPath, "test-only", "credential projection failed")
			if !strings.HasPrefix(err.Error(), test.step+": ") {
				t.Fatalf("credential seed error = %q, want the %q step named", err, test.step)
			}
		})
	}
}

// TestDockerRuntimeSeedsOnlyTheCopilotTokenIntoTheProjection verifies the
// Copilot token is streamed into its own file in the shared credential volume,
// replaced atomically, never linked into the role home, and never written
// back to the host source.
func TestDockerRuntimeSeedsOnlyTheCopilotTokenIntoTheProjection(t *testing.T) {
	stub, logPath, _ := writeDockerStub(t)
	authPath := filepath.Join(t.TempDir(), "copilot-token")
	contents := []byte("github_pat_test-only\n")
	if err := os.WriteFile(authPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	var seeder worker.CopilotCredentialSeeder = runtime
	if err := runtime.Start(context.Background(), worker.StartRequest{
		RunID: "run-copilot-auth", WorktreePath: makeDirectory(t, "worktree"), GitMetadataPath: makeDirectory(t, "git-metadata"),
		Role: "implementation", Image: "ghcr.io/example/factory-worker", ImageDigest: testWorkerDigest,
	}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := seeder.SeedCopilotCredentials(context.Background(), worker.CredentialSeedRequest{RunID: "run-copilot-auth", AuthPath: authPath}); err != nil {
		t.Fatalf("SeedCopilotCredentials() error = %v", err)
	}
	lines := readStubLog(t, logPath)
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, filepath.Dir(authPath)) || strings.Contains(joined, "github_pat_test-only") || strings.Contains(joined, "docker.sock") {
		t.Fatalf("credential seed leaked the host source or the token: %q", joined)
	}
	after, err := os.ReadFile(authPath)
	if err != nil || string(after) != string(contents) {
		t.Fatal("credential seed wrote back to the host source")
	}
	projection := worker.CredentialPath + "/" + worker.CopilotCredentialFile
	var replacedAtomically bool
	for _, line := range lines {
		if !strings.HasPrefix(line, "exec ") {
			continue
		}
		if strings.Contains(line, "chown") || strings.Contains(line, "ln -s") || strings.Contains(line, "$HOME") {
			t.Fatalf("Copilot seed touched the role home or needs a dropped capability: %q", line)
		}
		replacedAtomically = replacedAtomically || strings.Contains(line, "--user 0:0") &&
			strings.Contains(line, "chmod 0444") && strings.Contains(line, "mv -f") && strings.Contains(line, "\""+projection+"\"")
	}
	if !replacedAtomically {
		t.Fatalf("Copilot seed calls = %#v, want one atomic root-owned replacement of %s", lines, projection)
	}
}

// TestDockerRuntimeRefusesAnUnsafeCopilotCredentialSource verifies Copilot
// applies the shared source checks and accepts exactly one token line, which
// the in-worker launcher exports unchanged.
func TestDockerRuntimeRefusesAnUnsafeCopilotCredentialSource(t *testing.T) {
	stub, logPath, _ := writeDockerStub(t)
	root := t.TempDir()
	empty := filepath.Join(root, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(empty, link); err != nil {
		t.Fatal(err)
	}
	multiline := filepath.Join(root, "multiline")
	if err := os.WriteFile(multiline, []byte("github_pat_one\ngithub_pat_two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spaced := filepath.Join(root, "spaced")
	if err := os.WriteFile(spaced, []byte("token=github_pat one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The pinned CLI ignores COPILOT_GITHUB_TOKEN without a supported type
	// prefix and rejects classic tokens (observed offline with 1.0.98).
	classic := filepath.Join(root, "classic")
	if err := os.WriteFile(classic, []byte("ghp_classictoken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unprefixed := filepath.Join(root, "unprefixed")
	if err := os.WriteFile(unprefixed, []byte("plainvalue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	for _, test := range []struct{ name, path, problem string }{
		{name: "classic token", path: classic, problem: "supported Copilot token"},
		{name: "no token type", path: unprefixed, problem: "supported Copilot token"},
		{name: "relative", path: "copilot-token", problem: "absolute safe path"},
		{name: "symlink", path: link, problem: "symbolic link"},
		{name: "empty", path: empty, problem: "empty"},
		{name: "multiple lines", path: multiline, problem: "one token"},
		{name: "whitespace", path: spaced, problem: "one token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := runtime.SeedCopilotCredentials(context.Background(), worker.CredentialSeedRequest{RunID: "run-copilot-auth", AuthPath: test.path})
			if err == nil || !strings.Contains(err.Error(), test.problem) {
				t.Fatalf("SeedCopilotCredentials() error = %v, want %q refusal", err, test.problem)
			}
			for _, content := range []string{"classictoken", "plainvalue", "pat_one", "pat_two", "pat one"} {
				if strings.Contains(err.Error(), content) {
					t.Fatalf("refusal leaked credential content: %v", err)
				}
			}
		})
	}
	if lines := readStubLog(t, logPath); len(lines) != 0 {
		t.Fatalf("Docker was invoked for a refused source: %#v", lines)
	}
}
