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

// releaseImage is the pinned worker image of the release check tests.
var releaseImage = worker.ImageReference{
	Name:   "ghcr.io/example/factory-worker",
	Digest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
}

// fakeReleaseChecker answers the worker diagnosis seams with a fixed image
// release label.
type fakeReleaseChecker struct {
	fakeWorkerDoctorChecker
	release    string
	releaseErr error
}

// ImageRelease implements the worker release diagnosis seam.
func (f *fakeReleaseChecker) ImageRelease(context.Context, worker.ImageReference) (string, error) {
	return f.release, f.releaseErr
}

// TestReleaseCheckRequiresTheWorkerOfTheCoordinatorRelease proves that a
// release coordinator accepts only a worker image built from its own release's
// base image, and that a development coordinator accepts any worker image.
func TestReleaseCheckRequiresTheWorkerOfTheCoordinatorRelease(t *testing.T) {
	tests := []struct {
		name        string
		coordinator string
		label       string
		status      doctor.Status
		code        string
	}{
		{"same release", "v1.2.0", "v1.2.0", doctor.StatusPassed, "worker.release.ready"},
		{"older worker", "v1.2.0", "v1.1.0", doctor.StatusFailed, "worker.release.mismatch"},
		{"unlabeled worker", "v1.2.0", "", doctor.StatusFailed, "worker.release.mismatch"},
		{"development coordinator", "", "v1.1.0", doctor.StatusPassed, "worker.release.unchecked_development"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checker := &fakeReleaseChecker{release: test.label}
			readiness := worker.NewReadiness(checker, releaseImage)

			result := worker.ReleaseCheck(readiness, checker, test.coordinator)(context.Background())

			if result.Status != test.status || result.Code != test.code {
				t.Fatalf("result = %#v, want %s %s", result, test.status, test.code)
			}
			if result.Status == doctor.StatusFailed && (!strings.Contains(result.Action, "factory version --json") || result.Origin != doctor.OriginRepository) {
				t.Errorf("mismatch = %#v, want a repository finding that names the release base image", result)
			}
		})
	}
}

// TestReleaseCheckSkipsAnUnavailableImage proves that the release check
// reports an image nobody could inspect as not checked, and that a failed
// label probe is a finding without its command output.
func TestReleaseCheckSkipsAnUnavailableImage(t *testing.T) {
	unavailable := &fakeReleaseChecker{fakeWorkerDoctorChecker: fakeWorkerDoctorChecker{imageErr: errors.New("missing")}}
	result := worker.ReleaseCheck(worker.NewReadiness(unavailable, releaseImage), unavailable, "v1.2.0")(context.Background())
	if result.Status != doctor.StatusSkipped || result.Code != "worker.release.not_checked" {
		t.Fatalf("result = %#v, want a skipped check", result)
	}

	secret := "label-probe-secret"
	failing := &fakeReleaseChecker{releaseErr: errors.New(secret)}
	result = worker.ReleaseCheck(worker.NewReadiness(failing, releaseImage), failing, "v1.2.0")(context.Background())
	if result.Status != doctor.StatusFailed || result.Code != "worker.release.unreadable" || strings.Contains(result.Problem+result.Action, secret) {
		t.Fatalf("result = %#v, want a bounded unreadable finding", result)
	}
}

// TestDockerRuntimeReadsTheImageReleaseLabel proves that the runtime reads the
// release label of the exact pinned image without pulling, and reports an
// absent label as no release.
func TestDockerRuntimeReadsTheImageReleaseLabel(t *testing.T) {
	root := t.TempDir()
	stub := filepath.Join(root, "docker")
	log := filepath.Join(root, "log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > '" + log + "'\nprintf '%s\\n' \"$STUB_LABEL\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runtime := &worker.DockerRuntime{DockerBinary: stub}
	for label, want := range map[string]string{"v1.2.0": "v1.2.0", "<no value>": ""} {
		t.Setenv("STUB_LABEL", label)
		got, err := runtime.ImageRelease(context.Background(), releaseImage)
		if err != nil || got != want {
			t.Fatalf("ImageRelease with label %q = %q, %v; want %q", label, got, err, want)
		}
	}
	argv, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	wantArgv := "image inspect --format {{index .Config.Labels \"" + worker.ReleaseLabel + "\"}} " + releaseImage.Name + "@" + releaseImage.Digest + "\n"
	if string(argv) != wantArgv {
		t.Fatalf("docker argv = %q, want %q", argv, wantArgv)
	}
}
