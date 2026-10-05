package worker_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestWorkerBuildsDefaultToTheApprovedGoToolchain verifies that both image
// stages default to the go.mod toolchain line, so a plain docker build cannot
// fall back to an older Go release than local and CI builds use.
func TestWorkerBuildsDefaultToTheApprovedGoToolchain(t *testing.T) {
	approved := approvedGoToolchain(t)
	defaultArg := regexp.MustCompile(`(?m)^ARG GO_VERSION=(\S+)$`)
	for _, definition := range []string{"base.Dockerfile", "Dockerfile"} {
		defaults := defaultArg.FindAllStringSubmatch(readRepositoryFile(t, "worker/"+definition), -1)
		if len(defaults) == 0 {
			t.Fatalf("%s declares no GO_VERSION default", definition)
		}
		for _, match := range defaults {
			if "go"+match[1] != approved {
				t.Errorf("%s defaults GO_VERSION to %s, want the approved %s", definition, match[1], approved)
			}
		}
	}
}

// TestBuildPathsSelectTheApprovedGoToolchain verifies that the local build,
// CI, and the worker build script read the toolchain from go.mod instead of
// carrying a version of their own.
func TestBuildPathsSelectTheApprovedGoToolchain(t *testing.T) {
	approvedGoToolchain(t)
	if !strings.Contains(readRepositoryFile(t, "Makefile"), "export GOTOOLCHAIN") {
		t.Error("Makefile does not export the approved GOTOOLCHAIN")
	}
	workflow := readRepositoryFile(t, ".github/workflows/check.yml")
	if strings.Contains(workflow, "go-version:") || !strings.Contains(workflow, "go-version-file: go.mod") {
		t.Error("CI must select Go only through go-version-file: go.mod")
	}
	if regexp.MustCompile(`GO_VERSION:-[0-9]`).MatchString(readRepositoryFile(t, "scripts/build-worker.sh")) {
		t.Error("scripts/build-worker.sh carries its own Go version default")
	}
}

// approvedGoToolchain returns the go.mod toolchain line, such as go1.27.1.
func approvedGoToolchain(t *testing.T) string {
	t.Helper()
	match := regexp.MustCompile(`(?m)^toolchain (go1\.\d+\.\d+)$`).FindStringSubmatch(readRepositoryFile(t, "go.mod"))
	if match == nil {
		t.Fatal("go.mod must name the approved patch release in a toolchain line")
	}
	return match[1]
}

// readRepositoryFile reads a file relative to the repository root.
func readRepositoryFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile("../" + path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}
