package worker_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
