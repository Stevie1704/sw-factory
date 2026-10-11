package main_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildFactory builds the factory command with extra link flags into a fresh
// directory outside the checkout and returns the binary path.
func buildFactory(t *testing.T, ldflags string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the factory binary")
	}
	goCommand, err := exec.LookPath("go")
	if err != nil {
		goCommand = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	binary := filepath.Join(t.TempDir(), "factory")
	build := exec.Command(goCommand, "build", "-ldflags", ldflags, "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, output)
	}
	return binary
}

// runStandalone runs a copied binary in an empty directory outside any Git
// checkout, with no environment: no PATH, HOME, configuration, or network
// settings.
func runStandalone(t *testing.T, binary string, args ...string) (string, int) {
	t.Helper()
	command := exec.Command(binary, args...)
	command.Dir = t.TempDir()
	command.Env = []string{}
	output, err := command.CombinedOutput()
	if exitError, ok := err.(*exec.ExitError); ok {
		return string(output), exitError.ExitCode()
	}
	if err != nil {
		t.Fatalf("run %v: %v", args, err)
	}
	return string(output), 0
}

// TestStandaloneReleaseBinaryServesGuideAndVersion verifies that a copied
// release binary answers every guide and version command without a checkout,
// configuration, external executables, or network, and never prints the
// build-machine checkout path.
func TestStandaloneReleaseBinaryServesGuideAndVersion(t *testing.T) {
	buildPath := "/build-machine/sw-factory-checkout"
	binary := buildFactory(t, "-X github.com/Stevie1704/sw-factory/internal/version.release=v9.8.7 "+
		"-X github.com/Stevie1704/sw-factory/internal/cli.factoryCheckout="+buildPath)
	copied := filepath.Join(t.TempDir(), "factory")
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copied, data, 0o755); err != nil {
		t.Fatal(err)
	}

	text, code := runStandalone(t, copied, "version")
	if code != 0 || !strings.HasPrefix(text, "factory v9.8.7 (release, revision ") {
		t.Fatalf("version = %q (exit %d), want release v9.8.7", text, code)
	}
	encoded, code := runStandalone(t, copied, "version", "--json")
	var identity struct {
		SchemaVersion int    `json:"schema_version"`
		Version       string `json:"version"`
		Build         string `json:"build"`
		Revision      string `json:"revision"`
	}
	if code != 0 || json.Unmarshal([]byte(encoded), &identity) != nil {
		t.Fatalf("version --json = %q (exit %d), want JSON", encoded, code)
	}
	if identity.SchemaVersion != 1 || identity.Version != "v9.8.7" || identity.Build != "release" || !strings.Contains(text, "revision "+identity.Revision) {
		t.Fatalf("version --json = %+v disagrees with text %q", identity, text)
	}

	for _, args := range [][]string{{"guide"}, {"guide", "setup"}, {"guide", "debug"}, {"guide", "reporting"}} {
		output, code := runStandalone(t, copied, args...)
		if code != 0 || !strings.Contains(output, "Installed release: factory v9.8.7") {
			t.Fatalf("%v = %q (exit %d), want the topic with the release identity", args, output, code)
		}
		if strings.Contains(output, buildPath) {
			t.Fatalf("%v prints the build-machine path %q", args, buildPath)
		}
	}
	if output, code := runStandalone(t, copied, "guide", "nonexistent"); code != 2 || !strings.Contains(output, "available topics") {
		t.Fatalf("guide nonexistent = %q (exit %d), want discovery and exit 2", output, code)
	}
}

// TestStandaloneDevelopmentBinaryIsMarked verifies that a build without a
// release version reports itself as a development build.
func TestStandaloneDevelopmentBinaryIsMarked(t *testing.T) {
	binary := buildFactory(t, "")
	output, code := runStandalone(t, binary, "version")
	if code != 0 || !strings.HasPrefix(output, "factory development build (revision ") {
		t.Fatalf("version = %q (exit %d), want a development build", output, code)
	}
}
