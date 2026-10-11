package swfactory_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// releaseBaseImage is the worker base image the packaged test release names.
var releaseBaseImage = "ghcr.io/stevie1704/sw-factory-base@sha256:" + strings.Repeat("ef", 32)

// releaseRevision is the source revision the packaged test release names.
var releaseRevision = strings.Repeat("0a", 20)

// TestPackagedReleaseOnboardsWithoutGoOrCheckout packages a release for the
// host platform, installs it under a path with spaces, moves the installation,
// and proves that the moved binary reports the release and gives an
// onboarding harness the release resources with no Go toolchain on PATH and no
// reference to the build checkout.
func TestPackagedReleaseOnboardsWithoutGoOrCheckout(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a release archive")
	}
	checkout, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dist := filepath.Join(root, "release dist")
	platform := runtime.GOOS + "/" + runtime.GOARCH
	pack := exec.Command("./scripts/package-release.sh")
	// An explicit revision keeps the test independent of Git metadata, which
	// the worker gate's sanitized source copy does not carry.
	pack.Env = append(os.Environ(), "VERSION=v9.8.7", "REVISION="+releaseRevision, "WORKER_BASE_IMAGE="+releaseBaseImage, "PLATFORMS="+platform, "DIST_DIR="+dist)
	if output, err := pack.CombinedOutput(); err != nil {
		t.Fatalf("package-release.sh: %v\n%s", err, output)
	}

	name := "factory_v9.8.7_" + runtime.GOOS + "_" + runtime.GOARCH
	archive := filepath.Join(dist, name+".tar.gz")
	verifyChecksums(t, dist, name+".tar.gz", "release.json")
	verifyReleaseManifest(t, filepath.Join(dist, "release.json"))

	installed := filepath.Join(root, "installed factory")
	if err := os.MkdirAll(installed, 0o755); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("tar", "-xzf", archive, "-C", installed).CombinedOutput(); err != nil {
		t.Fatalf("extract: %v\n%s", err, output)
	}
	moved := filepath.Join(root, "moved install", name)
	if err := os.MkdirAll(filepath.Dir(moved), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(installed, name), moved); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(moved, "bin", "factory")
	if _, err := os.Stat(filepath.Join(moved, "README.md")); err != nil {
		t.Errorf("archive lacks the installation README: %v", err)
	}
	if data, err := os.ReadFile(binary); err != nil || bytes.Contains(data, []byte(checkout)) {
		t.Fatalf("release binary is unreadable or names the build checkout %q: %v", checkout, err)
	}

	environment := hostWithoutGo(t, root)
	encoded := runInstalled(t, environment, root, binary, "version", "--json")
	var identity struct {
		Version         string `json:"version"`
		Build           string `json:"build"`
		WorkerBaseImage string `json:"worker_base_image"`
	}
	if err := json.Unmarshal([]byte(encoded), &identity); err != nil || identity.Version != "v9.8.7" || identity.Build != "release" || identity.WorkerBaseImage != releaseBaseImage {
		t.Fatalf("version --json = %q (%v), want release v9.8.7 naming %s", encoded, err, releaseBaseImage)
	}

	target := filepath.Join(root, "target repo")
	if output, err := exec.Command("git", "init", "-q", target).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}
	runInstalled(t, environment, target, binary, "onboard")
	recorded, err := os.ReadFile(filepath.Join(root, "harness record"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(recorded), "present\n") || !strings.Contains(string(recorded), "The worker base image is "+releaseBaseImage) {
		t.Fatalf("onboarding harness record = %q, want the release resources and base image", recorded)
	}
}

// verifyChecksums proves that checksums.txt lists the SHA-256 of each named
// release file.
func verifyChecksums(t *testing.T, dist string, names ...string) {
	t.Helper()
	listing, err := os.ReadFile(filepath.Join(dist, "checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dist, name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if !strings.Contains(string(listing), hex.EncodeToString(sum[:])+"  "+name+"\n") {
			t.Errorf("checksums.txt lacks the SHA-256 of %s:\n%s", name, listing)
		}
	}
}

// verifyReleaseManifest proves that release.json names the release, its
// worker base image, and the documented support matrix.
func verifyReleaseManifest(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		SchemaVersion   int      `json:"schema_version"`
		Version         string   `json:"version"`
		Revision        string   `json:"revision"`
		WorkerBaseImage string   `json:"worker_base_image"`
		HostPlatforms   []string `json:"host_platforms"`
		WorkerPlatforms []string `json:"worker_platforms"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("release.json = %s: %v", data, err)
	}
	wantHosts := "darwin/amd64 darwin/arm64 linux/amd64 linux/arm64"
	if manifest.SchemaVersion != 1 || manifest.Version != "v9.8.7" || manifest.Revision != releaseRevision || manifest.WorkerBaseImage != releaseBaseImage ||
		strings.Join(manifest.HostPlatforms, " ") != wantHosts || strings.Join(manifest.WorkerPlatforms, " ") != "linux/amd64 linux/arm64" {
		t.Fatalf("release.json = %s, want v9.8.7 at %s, %s, hosts %s, workers linux/amd64 linux/arm64", data, releaseRevision, releaseBaseImage, wantHosts)
	}
}

// hostWithoutGo returns an operator environment whose PATH holds only a fake
// Claude Code harness, Git, and the base system commands, with a home
// directory whose path contains spaces. The harness records whether the
// procedure and scripts exist in its resource directory, and its prompt.
func hostWithoutGo(t *testing.T, root string) []string {
	t.Helper()
	bin := filepath.Join(root, "operator bin")
	home := filepath.Join(root, "operator home")
	for _, directory := range []string{bin, home} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	record := filepath.Join(root, "harness record")
	script := "#!/bin/sh\n{ if test -f \"$3/docs/repository-initialization.md\" && test -x \"$3/scripts/smoke-skills.sh\"; then echo present; else echo absent; fi; " +
		"printf '%s' \"$1\"; } > '" + record + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	environment := []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + home}
	lookup := exec.Command("/bin/sh", "-c", "command -v go")
	lookup.Env = environment
	if found, _ := lookup.Output(); len(found) != 0 {
		t.Skipf("the base system PATH resolves a Go toolchain at %s", found)
	}
	return environment
}

// runInstalled runs the installed binary in one directory and returns its
// standard output, failing the test on a nonzero exit.
func runInstalled(t *testing.T, environment []string, directory, binary string, args ...string) string {
	t.Helper()
	command := exec.Command(binary, args...)
	command.Dir = directory
	command.Env = environment
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, stderr.String())
	}
	return string(output)
}
