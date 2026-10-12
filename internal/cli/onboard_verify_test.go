package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/worker"
)

// verifyRuntime is a worker runtime whose commands exit with exitCode.
type verifyRuntime struct {
	exitCode int
	cleaned  int
}

func (r *verifyRuntime) Start(context.Context, worker.StartRequest) error   { return nil }
func (r *verifyRuntime) Resume(context.Context, worker.ResumeRequest) error { return nil }
func (r *verifyRuntime) Stop(context.Context, string) error                 { return nil }
func (r *verifyRuntime) Inspect(context.Context, string) (worker.Inspection, error) {
	return worker.Inspection{Exists: true, Running: true}, nil
}
func (r *verifyRuntime) RunCommand(context.Context, worker.CommandRequest) (worker.CommandResult, error) {
	return worker.CommandResult{ExitCode: r.exitCode}, nil
}
func (r *verifyRuntime) Cleanup(context.Context, worker.CleanupRequest) error {
	r.cleaned++
	return nil
}

// useVerifyRuntime replaces the proof runtime and the cache home for a test
// and returns the user cache directory.
func useVerifyRuntime(t *testing.T, runtime *verifyRuntime) string {
	t.Helper()
	original := proofRuntimeForCLI
	t.Cleanup(func() { proofRuntimeForCLI = original })
	proofRuntimeForCLI = func() worker.CleanupRuntime { return runtime }
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	userCache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	return userCache
}

// verifyCandidate creates a Git checkout with the repository's own
// factory.yaml as an uncommitted file.
func verifyCandidate(t *testing.T, contents []byte) string {
	t.Helper()
	repository := t.TempDir()
	if output, err := exec.Command("git", "init", "-q", repository).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	if err := os.WriteFile(filepath.Join(repository, "factory.yaml"), contents, 0o644); err != nil {
		t.Fatal(err)
	}
	return repository
}

// TestOnboardVerifyWritesTheProofReport verifies the exit status follows the
// proof result and the report is one JSON document.
func TestOnboardVerifyWritesTheProofReport(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "factory.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		exitCode int
		status   int
		result   string
	}{{0, 0, "passed"}, {1, 1, "failed"}} {
		useVerifyRuntime(t, &verifyRuntime{exitCode: test.exitCode})
		repository := verifyCandidate(t, contents)
		t.Chdir(filepath.Dir(repository))
		var output, errorsOutput bytes.Buffer

		status := Run(context.Background(), []string{"onboard", "verify", "--repository", filepath.Base(repository), "--json"}, &output, &errorsOutput)

		var document struct {
			SchemaVersion int    `json:"schema_version"`
			Result        string `json:"result"`
			Repository    string `json:"repository"`
			EvidenceID    string `json:"evidence_id"`
		}
		if err := json.Unmarshal(output.Bytes(), &document); err != nil {
			t.Fatalf("output is not JSON: %v: %q (stderr %q)", err, output.String(), errorsOutput.String())
		}
		if status != test.status || document.Result != test.result || document.SchemaVersion != 1 || !filepath.IsAbs(document.Repository) || document.EvidenceID == "" {
			t.Fatalf("status = %d, document = %#v; want status %d with result %q", status, document, test.status, test.result)
		}
	}
}

// TestOnboardVerifyCleanupReportsTheRemovedProofs verifies the cleanup path.
func TestOnboardVerifyCleanupReportsTheRemovedProofs(t *testing.T) {
	runtime := &verifyRuntime{}
	cache := useVerifyRuntime(t, runtime)
	leftover := filepath.Join(cache, "sw-factory", "onboard-proof", "onboard-proof-0123456789abcdef")
	if err := os.MkdirAll(leftover, 0o700); err != nil {
		t.Fatal(err)
	}
	var output, errorsOutput bytes.Buffer

	status := Run(context.Background(), []string{"onboard", "verify", "--cleanup", "--json"}, &output, &errorsOutput)

	if status != 0 || !strings.Contains(output.String(), `"removed":["onboard-proof-0123456789abcdef"]`) || runtime.cleaned != 1 {
		t.Fatalf("status = %d, output = %q, stderr = %q, cleaned %d", status, output.String(), errorsOutput.String(), runtime.cleaned)
	}
}

// TestOnboardVerifyRefusesIncompleteArguments verifies usage errors exit 2.
func TestOnboardVerifyRefusesIncompleteArguments(t *testing.T) {
	useVerifyRuntime(t, &verifyRuntime{})
	for name, args := range map[string][]string{
		"no repository":        {"onboard", "verify", "--json"},
		"no json":              {"onboard", "verify", "--repository", "."},
		"cleanup and verify":   {"onboard", "verify", "--cleanup", "--repository", ".", "--json"},
		"positional arguments": {"onboard", "verify", "--json", "extra"},
	} {
		var output, errorsOutput bytes.Buffer
		if status := Run(context.Background(), args, &output, &errorsOutput); status != 2 {
			t.Errorf("%s: status = %d, want 2", name, status)
		}
	}
}
