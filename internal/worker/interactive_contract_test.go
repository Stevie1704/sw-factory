package worker_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/worker"
)

const interactiveWorkerDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestDockerRuntimeBuildsAnOpaqueInteractiveAttachCommand verifies that the
// terminal receives a host helper plus a run identity, never a Docker name.
func TestDockerRuntimeBuildsAnOpaqueInteractiveAttachCommand(t *testing.T) {
	runtime := &worker.DockerRuntime{AttachBinary: "factory-worker-attach"}
	prompt := "Implement issue #42.\nPreserve the user's quoted 'example'."
	command, err := runtime.InteractiveCommand(context.Background(), worker.InteractiveRequest{
		RunID:             "run-interactive",
		Command:           []string{"codex", "-s", "danger-full-access", prompt},
		EnvironmentPolicy: worker.EnvironmentPolicyRole,
		Role:              "implementation",
		Environment:       map[string]string{"FACTORY_INVOCATION_ID": "inv-1"},
	})
	if err != nil {
		t.Fatalf("InteractiveCommand() error = %v", err)
	}
	if command.Executable != "factory-worker-attach" {
		t.Fatalf("interactive executable = %q, want host attach helper", command.Executable)
	}
	if got := command.Args[len(command.Args)-1]; got != prompt {
		t.Fatalf("interactive prompt = %q, want opaque multiline argument %q", got, prompt)
	}
	joined := strings.Join(command.Args, " ")
	if !strings.Contains(joined, "--run-id run-interactive") || !strings.Contains(joined, "--role implementation") || !strings.Contains(joined, "-- codex -s danger-full-access") || !strings.Contains(joined, "FACTORY_INVOCATION_ID=inv-1") {
		t.Fatalf("interactive args = %q, want run, environment, and command", joined)
	}
	for _, argument := range command.Args {
		if strings.HasPrefix(argument, "factory-worker-") && argument != "factory-worker-attach" {
			t.Fatalf("interactive command exposed a Docker container name: %q", joined)
		}
	}
}

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
		RunID:             "run-report-mounts",
		WorktreePath:      worktree,
		GitMetadataPath:   gitMetadata,
		InvocationPath:    invocation,
		ResultPath:        results,
		CredentialStoreID: "registration",
		Role:              "implementation",
		Image:             "ghcr.io/example/factory-worker",
		ImageDigest:       interactiveWorkerDigest,
	}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	lines := readStubLog(t, logPath)
	runLine := findLogLine(t, lines, " run ")
	if !strings.Contains(runLine, "dst=/invocation,readonly") || !strings.Contains(runLine, "dst=/results") {
		t.Fatalf("worker mounts = %q, want read-only invocation and writable results", runLine)
	}
	if !strings.Contains(runLine, "factory-role-implementation") || !strings.Contains(runLine, "dst="+worker.CredentialPath) {
		t.Fatalf("worker mounts = %q, want role session and separate credential volumes", runLine)
	}
}

// TestDockerRuntimeRecreatesAnExistingWorkerForInvocationMounts verifies that
// a gate-created worker is safely rebuilt before a visible invocation starts.
func TestDockerRuntimeRecreatesAnExistingWorkerForInvocationMounts(t *testing.T) {
	stub, logPath, _ := writeDockerStub(t)
	worktree := makeDirectory(t, "worktree")
	gitMetadata := makeDirectory(t, "git-metadata")
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	base := worker.StartRequest{
		RunID:             "run-reconfigure",
		WorktreePath:      worktree,
		GitMetadataPath:   gitMetadata,
		CredentialStoreID: "registration",
		Role:              "gate",
		Image:             "ghcr.io/example/factory-worker",
		ImageDigest:       interactiveWorkerDigest,
	}
	if err := runtime.Start(context.Background(), base); err != nil {
		t.Fatalf("base Start() error = %v", err)
	}
	inspectionPath := filepath.Join(t.TempDir(), "inspection.json")
	inspection, marshalErr := json.Marshal(map[string]any{
		"State":  map[string]bool{"Running": true},
		"Config": map[string]string{"Image": "ghcr.io/example/factory-worker@" + interactiveWorkerDigest},
		"Mounts": []map[string]string{
			{"Type": "bind", "Source": worktree, "Destination": worker.WorktreePath},
			{"Type": "bind", "Source": gitMetadata, "Destination": worker.GitMetadataPath},
			{"Type": "volume", "Name": testRoleVolumeName(base.RunID, base.Role), "Destination": "/home/factory"},
		},
	})
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if err := os.WriteFile(inspectionPath, inspection, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORKER_DOCKER_INSPECT_JSON_FILE", inspectionPath)
	if err := runtime.Start(context.Background(), worker.StartRequest{
		RunID:             base.RunID,
		WorktreePath:      worktree,
		GitMetadataPath:   gitMetadata,
		InvocationPath:    makeDirectory(t, "invocation"),
		ResultPath:        makeDirectory(t, "results"),
		CredentialStoreID: base.CredentialStoreID,
		Role:              "implementation",
		Image:             base.Image,
		ImageDigest:       base.ImageDigest,
	}); err != nil {
		t.Fatalf("visible Start() error = %v", err)
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
	replacement := findLogLines(lines, " run ")[1]
	if !strings.Contains(replacement, "dst="+worker.CredentialPath) {
		t.Fatalf("replacement worker mounts = %q, want the factory credential projection", replacement)
	}
}

// testRoleVolumeName mirrors only the stable test fixture identity needed to
// build a structured Docker inspect response without exposing adapter code.
func testRoleVolumeName(runID, role string) string {
	role = strings.ToLower(role)
	digest := sha256.Sum256([]byte(runID + "\x00" + role))
	return "factory-role-" + role + "-" + hex.EncodeToString(digest[:])[:16]
}

// TestDockerRuntimeSeedsOnlyTheCodexCredentialFile verifies that seeding reads
// one explicit host file and does not mount or copy the host harness directory.
func TestDockerRuntimeSeedsOnlyTheCodexCredentialFile(t *testing.T) {
	stub, logPath, _ := writeDockerStub(t)
	worktree := makeDirectory(t, "worktree")
	gitMetadata := makeDirectory(t, "git-metadata")
	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"access_token":"test-only"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	if err := runtime.Start(context.Background(), worker.StartRequest{
		RunID:           "run-auth",
		WorktreePath:    worktree,
		GitMetadataPath: gitMetadata,
		Role:            "implementation",
		Image:           "ghcr.io/example/factory-worker",
		ImageDigest:     interactiveWorkerDigest,
	}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := runtime.SeedCodexCredentials(context.Background(), worker.CredentialSeedRequest{RunID: "run-auth", AuthPath: authPath}); err != nil {
		t.Fatalf("SeedCodexCredentials() error = %v", err)
	}
	lines := readStubLog(t, logPath)
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, filepath.Dir(authPath)) || strings.Contains(joined, "docker.sock") || !strings.Contains(joined, worker.CredentialPath+"/auth.json") {
		t.Fatalf("credential seed leaked host harness path or Docker socket: %q", joined)
	}
}

// TestDockerRuntimeSeedsCredentialsWithoutPrivilegedOperations verifies that
// credential seeding never calls chown and writes each path as the user that
// owns it. The worker drops every capability, so a seed that relies on uid 0
// being privileged fails at run time while still passing a naive argument
// check.
func TestDockerRuntimeSeedsCredentialsWithoutPrivilegedOperations(t *testing.T) {
	stub, logPath, _ := writeDockerStub(t)
	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"access_token":"test-only"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	if err := runtime.Start(context.Background(), worker.StartRequest{
		RunID:           "run-auth-privileges",
		WorktreePath:    makeDirectory(t, "worktree"),
		GitMetadataPath: makeDirectory(t, "git-metadata"),
		Role:            "implementation",
		Image:           "ghcr.io/example/factory-worker",
		ImageDigest:     interactiveWorkerDigest,
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
			t.Fatalf("credential seed requires a capability the worker drops: %q", line)
		}
		switch {
		case strings.Contains(line, "--user 0:0"):
			if strings.Contains(line, "$HOME/.codex") {
				t.Fatalf("uid 0 cannot write the factory-owned role home: %q", line)
			}
			if strings.Contains(line, "cat > \""+worker.CredentialPath+"/auth.json\"") {
				wroteCredential = true
			}
		case strings.Contains(line, "--user "+worker.WorkerUser):
			if strings.Contains(line, "ln -s") && strings.Contains(line, "$HOME/.codex/auth.json") {
				linkedRoleHome = true
			}
		}
	}
	if !wroteCredential {
		t.Fatal("credential file was not written as the owner of the credential volume")
	}
	if !linkedRoleHome {
		t.Fatal("role home symlink was not created as the worker user")
	}
}

// TestDockerRuntimeSeedsOnlyTheClaudeCredentialFile verifies Claude Code is
// seeded on the same narrow terms as Codex: one explicit host file streamed
// into the factory-managed credential volume, with no host harness directory
// mounted and no write back to the host source.
func TestDockerRuntimeSeedsOnlyTheClaudeCredentialFile(t *testing.T) {
	stub, logPath, _ := writeDockerStub(t)
	authPath := filepath.Join(t.TempDir(), ".credentials.json")
	contents := []byte(`{"claudeAiOauth":{"accessToken":"test-only"}}`)
	if err := os.WriteFile(authPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	if err := runtime.Start(context.Background(), worker.StartRequest{
		RunID:           "run-claude-auth",
		WorktreePath:    makeDirectory(t, "worktree"),
		GitMetadataPath: makeDirectory(t, "git-metadata"),
		Role:            "implementation",
		Image:           "ghcr.io/example/factory-worker",
		ImageDigest:     interactiveWorkerDigest,
	}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := runtime.SeedClaudeCredentials(context.Background(), worker.CredentialSeedRequest{RunID: "run-claude-auth", AuthPath: authPath}); err != nil {
		t.Fatalf("SeedClaudeCredentials() error = %v", err)
	}
	lines := readStubLog(t, logPath)
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, filepath.Dir(authPath)) || strings.Contains(joined, "docker.sock") {
		t.Fatalf("credential seed leaked the host harness path or Docker socket: %q", joined)
	}
	if !strings.Contains(joined, worker.CredentialPath+"/claude-credentials.json") {
		t.Fatalf("credential seed did not target the factory-managed credential volume: %q", joined)
	}
	after, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(contents) {
		t.Fatal("credential seed wrote back to the host source")
	}
	var wroteCredential, linkedRoleHome bool
	for _, line := range lines {
		if !strings.HasPrefix(line, "exec ") {
			continue
		}
		if strings.Contains(line, "chown") {
			t.Fatalf("credential seed requires a capability the worker drops: %q", line)
		}
		switch {
		case strings.Contains(line, "--user 0:0"):
			if strings.Contains(line, "$HOME/.claude") {
				t.Fatalf("uid 0 cannot write the factory-owned role home: %q", line)
			}
			if strings.Contains(line, "cat > \""+worker.CredentialPath+"/claude-credentials.json\"") {
				wroteCredential = true
			}
		case strings.Contains(line, "--user "+worker.WorkerUser):
			if strings.Contains(line, "ln -s") && strings.Contains(line, "$HOME/.claude/.credentials.json") {
				linkedRoleHome = true
			}
		}
	}
	if !wroteCredential {
		t.Fatal("credential file was not written as the owner of the credential volume")
	}
	if !linkedRoleHome {
		t.Fatal("role home symlink was not created as the worker user")
	}
}

// TestDockerRuntimeRefusesAnUnsafeClaudeCredentialSource verifies the Claude
// seam applies the same source checks as the Codex seam.
func TestDockerRuntimeRefusesAnUnsafeClaudeCredentialSource(t *testing.T) {
	stub, _, _ := writeDockerStub(t)
	root := t.TempDir()
	regular := filepath.Join(root, "regular.json")
	if err := os.WriteFile(regular, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.json")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	for _, test := range []struct {
		name    string
		path    string
		problem string
	}{
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

// TestDockerRuntimeRedactsCredentialSourceAndDockerErrors verifies that a
// missing source or Docker-side failure cannot expose the host auth path or
// credential bytes through the worker seam.
func TestDockerRuntimeRedactsCredentialSourceAndDockerErrors(t *testing.T) {
	stub, _, _ := writeDockerStub(t)
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	authRoot := t.TempDir()
	authPath := filepath.Join(authRoot, "auth.json")
	missingErr := runtime.SeedCodexCredentials(context.Background(), worker.CredentialSeedRequest{
		RunID:    "run-redacted-source",
		AuthPath: authPath,
	})
	if missingErr == nil {
		t.Fatal("SeedCodexCredentials() accepted a missing credential source")
	}
	if strings.Contains(missingErr.Error(), authPath) {
		t.Fatalf("missing-source error leaked host credential path: %v", missingErr)
	}

	if err := os.WriteFile(authPath, []byte("credential-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORKER_DOCKER_EXEC_ERROR", authPath+": credential-secret")
	dockerErr := runtime.SeedCodexCredentials(context.Background(), worker.CredentialSeedRequest{
		RunID:    "run-redacted-docker",
		AuthPath: authPath,
	})
	if dockerErr == nil {
		t.Fatal("SeedCodexCredentials() accepted a Docker projection failure")
	}
	if strings.Contains(dockerErr.Error(), authPath) || strings.Contains(dockerErr.Error(), "credential-secret") {
		t.Fatalf("Docker projection error leaked credential details: %v", dockerErr)
	}
}
