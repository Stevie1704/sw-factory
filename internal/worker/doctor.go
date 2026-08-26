package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/doctor"
)

// ImageReference identifies the immutable worker image required by a run.
type ImageReference struct {
	// Name is the repository image name without a mutable tag.
	Name string
	// Digest is the full sha256 image digest.
	Digest string
}

// HarnessCheckRequest identifies one harness executable that must be present
// in the immutable worker image.
type HarnessCheckRequest struct {
	// Image is the worker image to inspect.
	Image ImageReference
	// Name is the safe executable name to probe.
	Name string
}

// HarnessAuthenticationCheckRequest identifies one harness auth source to
// validate inside a disposable pinned worker image.
type HarnessAuthenticationCheckRequest struct {
	// Image is the worker image to inspect.
	Image ImageReference
	// Name is the built-in harness executable to authenticate.
	Name string
	// AuthPath is the one explicit host credential file streamed to the probe.
	AuthPath string
}

// DoctorChecker is the worker-owned Docker and image diagnosis seam.
type DoctorChecker interface {
	CheckDocker(context.Context) error
	CheckImage(context.Context, ImageReference) error
}

// HarnessChecker is the worker execution seam used by the harness module to
// probe each pinned harness executable inside the worker image.
type HarnessChecker interface {
	CheckHarness(context.Context, HarnessCheckRequest) error
}

// HarnessAuthenticationChecker is the worker execution seam for checking a
// configured harness credential without exposing it to the coordinator.
type HarnessAuthenticationChecker interface {
	CheckHarnessAuthentication(context.Context, HarnessAuthenticationCheckRequest) error
}

// StartupChecks returns independent Docker daemon and worker-image checks.
func StartupChecks(checker DoctorChecker, image ImageReference) []doctor.Check {
	return []doctor.Check{
		func(ctx context.Context) doctor.Result {
			if checker == nil {
				return doctor.Failure("docker daemon", "the Docker diagnosis adapter is unavailable", "configure the Docker worker runtime")
			}
			if err := checker.CheckDocker(ctx); err != nil {
				return doctor.Failure("docker daemon", "the coordinator cannot reach a usable Docker daemon", "start Docker and verify the coordinator account can access it")
			}
			return doctor.Success("docker daemon")
		},
		func(ctx context.Context) doctor.Result {
			if checker == nil {
				return doctor.Failure("worker image", "the Docker diagnosis adapter is unavailable", "configure the Docker worker runtime")
			}
			if err := checker.CheckImage(ctx, image); err != nil {
				return doctor.Failure("worker image", "the configured worker image@sha256 digest is not available locally", "build or load the configured worker image at the exact digest; doctor never pulls images")
			}
			return doctor.Success("worker image")
		},
	}
}

// CheckDocker verifies the Docker executable and daemon without displaying
// Docker's version or diagnostic output.
func (r *DockerRuntime) CheckDocker(ctx context.Context) error {
	binary := strings.TrimSpace(r.DockerBinary)
	if binary == "" {
		binary = "docker"
	}
	if _, err := exec.LookPath(binary); err != nil {
		return errors.New("Docker executable is unavailable")
	}
	if _, err := r.runDocker(ctx, []string{"version", "--format", "{{.Server.Version}}"}); err != nil {
		return errors.New("Docker daemon is unavailable")
	}
	return nil
}

// CheckImage verifies the exact digest-qualified worker image exists without
// pulling or changing any local image.
func (r *DockerRuntime) CheckImage(ctx context.Context, image ImageReference) error {
	if err := validateImageReference(image); err != nil {
		return err
	}
	result, err := r.runDocker(ctx, []string{"image", "inspect", "--format", "{{json .}}", imageReference(image.Name, image.Digest)})
	if err != nil {
		return errors.New("worker image inspection failed")
	}
	if strings.TrimSpace(result.Stdout) == "" {
		return errors.New("worker image inspection returned no image")
	}
	return nil
}

// CheckHarness verifies one harness executable and its version command inside
// the exact worker image, using a disposable no-pull container.
func (r *DockerRuntime) CheckHarness(ctx context.Context, request HarnessCheckRequest) error {
	if err := validateImageReference(request.Image); err != nil {
		return err
	}
	if !validName(request.Name) {
		return errors.New("harness executable name is unsafe")
	}
	command := "command -v " + request.Name + " >/dev/null 2>&1 && " + request.Name + " --version >/dev/null 2>&1"
	if _, err := r.runDocker(ctx, []string{
		"run", "--rm", "--pull=never", "--entrypoint", "/bin/sh",
		imageReference(request.Image.Name, request.Image.Digest), "-c", command,
	}); err != nil {
		return errors.New("harness executable is not usable in the worker image")
	}
	return nil
}

// CheckHarnessAuthentication streams one explicit credential file into a
// disposable, network-isolated worker and runs that harness's local auth
// status command. Neither credential contents nor process output is returned.
func (r *DockerRuntime) CheckHarnessAuthentication(ctx context.Context, request HarnessAuthenticationCheckRequest) error {
	if err := validateImageReference(request.Image); err != nil {
		return err
	}
	roleHome, credentialFile, command, err := harnessAuthenticationCommand(request.Name)
	if err != nil {
		return err
	}
	data, err := readAuthenticationSource(request.AuthPath)
	if err != nil {
		return err
	}
	authFile := roleHome + "/" + credentialFile
	writeCommand := "umask 077; mkdir -p " + roleHome + "; cat > " + authFile + "; chmod 0400 " + authFile
	probe := writeCommand + " && " + command
	if _, err := r.runDockerWithInput(ctx, []string{
		"run", "--rm", "--pull=never", "--network", "none", "--user", WorkerUser,
		"--entrypoint", "/bin/sh", imageReference(request.Image.Name, request.Image.Digest), "-c", probe,
	}, data); err != nil {
		return errors.New("harness authentication is not usable in the worker image")
	}
	return nil
}

// harnessAuthenticationCommand returns the fixed in-worker auth file location
// and status command for a supported harness. It never incorporates user text
// into a shell command.
func harnessAuthenticationCommand(name string) (string, string, string, error) {
	switch name {
	case "codex":
		return "/home/factory/.codex", "auth.json", "codex login status >/dev/null 2>&1", nil
	case "claude":
		return "/home/factory/.claude", ".credentials.json", "claude auth status >/dev/null 2>&1", nil
	default:
		return "", "", "", fmt.Errorf("unsupported harness authentication probe %q", name)
	}
}

// readAuthenticationSource validates and reads one private credential file for
// a worker-side probe without returning its bytes in an error or result.
func readAuthenticationSource(path string) ([]byte, error) {
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
		return nil, errors.New("harness authentication path must be absolute and safe")
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o400 == 0 || info.Size() == 0 {
		return nil, errors.New("harness authentication source is unavailable or unsafe")
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil, errors.New("harness authentication source cannot be read")
	}
	return data, nil
}

// validateImageReference validates the image name and immutable digest before
// either value crosses into a Docker command.
func validateImageReference(image ImageReference) error {
	if err := validateImageName(image.Name); err != nil {
		return err
	}
	if !validDigest(image.Digest) {
		return errors.New("worker image digest must be a sha256 digest with 64 lowercase hexadecimal characters")
	}
	return nil
}

var _ DoctorChecker = (*DockerRuntime)(nil)
var _ HarnessChecker = (*DockerRuntime)(nil)
var _ HarnessAuthenticationChecker = (*DockerRuntime)(nil)
