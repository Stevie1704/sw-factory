package worker_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestStartupChecksReportDockerAndWorkerImageIndependently verifies the
// worker contributor reports every prerequisite even when Docker is unavailable.
func TestStartupChecksReportDockerAndWorkerImageIndependently(t *testing.T) {
	checker := &fakeWorkerDoctorChecker{dockerErr: errors.New("docker unavailable")}
	report := doctor.Run(context.Background(), worker.StartupChecks(checker, worker.ImageReference{
		Name:   "ghcr.io/example/factory-worker",
		Digest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	})...)
	if report.Ready() || len(report.Failures()) != 1 {
		t.Fatalf("worker report = %#v, want one Docker failure", report)
	}
	if len(report.Results) != 2 || checker.calls != 2 {
		t.Fatalf("worker results/calls = %d/%d, want daemon and image checks", len(report.Results), checker.calls)
	}
}

// TestStartupChecksDoNotRenderWorkerCommandErrors verifies worker diagnosis
// keeps process output, which may contain repository data, out of the report.
func TestStartupChecksDoNotRenderWorkerCommandErrors(t *testing.T) {
	secret := "worker-command-secret"
	checker := &fakeWorkerDoctorChecker{dockerErr: errors.New(secret), imageErr: errors.New(secret)}
	report := doctor.Run(context.Background(), worker.StartupChecks(checker, worker.ImageReference{})...)
	for _, result := range report.Results {
		if strings.Contains(result.Problem+result.Action, secret) {
			t.Fatalf("worker result exposed command error: %#v", result)
		}
	}
}

// fakeWorkerDoctorChecker records worker diagnosis calls.
type fakeWorkerDoctorChecker struct {
	dockerErr error
	imageErr  error
	calls     int
}

// CheckDocker implements the worker daemon diagnosis seam.
func (f *fakeWorkerDoctorChecker) CheckDocker(context.Context) error {
	f.calls++
	return f.dockerErr
}

// CheckImage implements the worker image diagnosis seam.
func (f *fakeWorkerDoctorChecker) CheckImage(context.Context, worker.ImageReference) error {
	f.calls++
	return f.imageErr
}

var _ worker.DoctorChecker = (*fakeWorkerDoctorChecker)(nil)

// TestDockerRuntimeChecksHarnessInsideTheHardenedProbe verifies executable
// diagnosis uses the same least-privilege boundary as a real worker.
func TestDockerRuntimeChecksHarnessInsideTheHardenedProbe(t *testing.T) {
	stub, logPath, _ := writeDockerStub(t)
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	if err := runtime.CheckHarness(context.Background(), worker.HarnessCheckRequest{
		Image: worker.ImageReference{Name: "ghcr.io/example/factory-worker", Digest: testWorkerDigest},
		Name:  "codex",
	}); err != nil {
		t.Fatalf("CheckHarness() error = %v", err)
	}
	line := findLogLine(t, readStubLog(t, logPath), " run ")
	assertContainsAll(t, line,
		"--pull=never", "--user 10001:10001", "--cap-drop ALL",
		"--security-opt no-new-privileges", "--network none",
		"codex --version", "ghcr.io/example/factory-worker@"+testWorkerDigest,
	)
}

// TestDockerRuntimeChecksHarnessAuthenticationInsideThePinnedWorker verifies
// credential input is streamed to a disposable network-isolated probe rather
// than passed as a command argument or rendered in output.
func TestDockerRuntimeChecksHarnessAuthenticationInsideThePinnedWorker(t *testing.T) {
	stub, logPath, _ := writeDockerStub(t)
	secret := "credential-value-must-not-enter-docker-args"
	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authPath, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	if err := runtime.CheckHarnessAuthentication(context.Background(), worker.HarnessAuthenticationCheckRequest{
		Image: worker.ImageReference{Name: "ghcr.io/example/factory-worker", Digest: testWorkerDigest},
		Name:  "codex", AuthPath: authPath,
	}); err != nil {
		t.Fatalf("CheckHarnessAuthentication() error = %v", err)
	}
	line := findLogLine(t, readStubLog(t, logPath), " run ")
	assertContainsAll(t, line,
		"--pull=never", "--network none", "--user 10001:10001",
		"--cap-drop ALL", "--security-opt no-new-privileges", "codex login status",
		"ghcr.io/example/factory-worker@"+testWorkerDigest,
	)
	if strings.Contains(line, secret) || strings.Contains(line, authPath) {
		t.Fatalf("credential material or host path entered Docker arguments: %q", line)
	}
}

// TestDockerRuntimeDoesNotSendContentsFromReplacedAuthenticationPath verifies
// replacing the validated path cannot redirect the credential sent to Docker.
func TestDockerRuntimeDoesNotSendContentsFromReplacedAuthenticationPath(t *testing.T) {
	stub, _, _ := writeDockerStub(t)
	inputDirectory := filepath.Join(t.TempDir(), "docker-input")
	if err := os.Mkdir(inputDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORKER_DOCKER_INPUT_DIR", inputDirectory)

	root := t.TempDir()
	safeSourcePath := filepath.Join(root, "safe-auth.json")
	authPath := filepath.Join(root, "auth.json")
	replacementPath := filepath.Join(root, "replacement")
	restorePath := filepath.Join(root, "restore")
	arbitraryHostFilePath := filepath.Join(root, "host-file")
	safeContents := []byte("expected-credential")
	arbitraryHostContents := []byte("arbitrary-host-file-contents")
	if err := os.WriteFile(safeSourcePath, safeContents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(arbitraryHostFilePath, arbitraryHostContents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(safeSourcePath, authPath); err != nil {
		t.Fatal(err)
	}
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	request := worker.HarnessAuthenticationCheckRequest{
		Image:    worker.ImageReference{Name: "ghcr.io/example/factory-worker", Digest: testWorkerDigest},
		Name:     "codex",
		AuthPath: authPath,
	}
	if err := runtime.CheckHarnessAuthentication(context.Background(), request); err != nil {
		t.Fatalf("baseline CheckHarnessAuthentication() error = %v", err)
	}

	stopReplacement := make(chan struct{})
	replacementDone := make(chan struct{})
	replacementReady := make(chan struct{})
	var replacementCount atomic.Int64
	var replacementReadyOnce sync.Once
	go func() {
		defer close(replacementDone)
		for {
			select {
			case <-stopReplacement:
				return
			default:
			}

			_ = os.Remove(replacementPath)
			if err := os.Symlink(arbitraryHostFilePath, replacementPath); err != nil {
				continue
			}
			if err := os.Rename(replacementPath, authPath); err != nil {
				_ = os.Remove(replacementPath)
				continue
			}
			replacementCount.Add(1)
			replacementReadyOnce.Do(func() { close(replacementReady) })

			_ = os.Remove(restorePath)
			if err := os.Link(safeSourcePath, restorePath); err != nil {
				continue
			}
			_ = os.Rename(restorePath, authPath)
		}
	}()
	<-replacementReady

	const attempts = 512
	const workers = 8
	var checks sync.WaitGroup
	checks.Add(workers)
	for range workers {
		go func() {
			defer checks.Done()
			for range attempts / workers {
				_ = runtime.CheckHarnessAuthentication(context.Background(), request)
			}
		}()
	}
	checks.Wait()
	close(stopReplacement)
	<-replacementDone
	if replacementCount.Load() == 0 {
		t.Fatal("authentication path was not replaced during the check")
	}

	entries, err := os.ReadDir(inputDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("Docker received no authentication input")
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(inputDirectory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != string(safeContents) {
			t.Fatalf("Docker received unexpected authentication input: %q", data)
		}
	}
}
